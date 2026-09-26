package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// A server holds at most one C.A.S.E. subscription with a single price, so
// Watch and Pro are mutually exclusive by construction: changing tier swaps
// that one price on the SAME Stripe subscription, never adds a second one.
type CaseTierChangeKind string

const (
	// CaseTierUpgrade invoices the proration immediately. The new tier's
	// capabilities unlock only when that invoice is paid (invoice.paid).
	CaseTierUpgrade CaseTierChangeKind = "UPGRADE"
	// CaseTierRestore returns, within an already-paid period, to the tier
	// that period was paid for (undoing a scheduled downgrade): no charge.
	CaseTierRestore CaseTierChangeKind = "RESTORE"
	// CaseTierDowngrade creates no proration or credit. The paid tier stays
	// until paid_through; the renewal invoice bills the lower price.
	CaseTierDowngrade CaseTierChangeKind = "DOWNGRADE"
)

// caseProrationWindow bounds how old a confirmed preview may be. Stripe
// charges exactly the previewed proration for the same proration_date.
const caseProrationWindow = 15 * time.Minute

var (
	ErrCaseTierUnchanged   = errors.New("C.A.S.E. server is already on that tier")
	ErrCaseCancelScheduled = errors.New("C.A.S.E. cancellation is scheduled; reactivate before changing tier")
	ErrCaseChangePending   = errors.New("a C.A.S.E. tier change is awaiting payment")
	ErrCasePreviewExpired  = errors.New("C.A.S.E. tier change preview expired")
)

type CaseProrationPreview struct {
	AmountDue int64
	Currency  string
}

type CaseTierChangeInput struct {
	SubscriptionID, FromPriceID, NewPriceID string
	Charge                                  bool
	ProrationDate                           int64
	IdempotencyKey                          string
}

type CaseTierChangeResult struct {
	State   *SubscriptionState
	Pending bool // Stripe holds the change until its proration invoice is paid
}

// CaseTierPreview is what the owner confirms. Amounts are Stripe-calculated
// (upgrade) or zero; the browser never supplies a price or an amount.
type CaseTierPreview struct {
	Kind                   CaseTierChangeKind
	CurrentTier, TargetTier casebilling.Tier
	AmountDueNowCents      int64
	Currency               string
	ProrationDate          int64
	// EffectiveAt is when the target tier's access takes effect: nil for an
	// upgrade (on payment), the paid-through end for a downgrade, now for
	// a restore.
	EffectiveAt            *time.Time
	NextRenewalAmountCents int64
	CurrentPeriodEnd       *time.Time
}

type CaseTierChangeOutcome struct {
	Kind    CaseTierChangeKind
	Pending bool
	Row     *repository.CaseAddonSubscription
}

type caseTierPlan struct {
	row                *repository.CaseAddonSubscription
	current            *SubscriptionState
	kind               CaseTierChangeKind
	target             casebilling.Plan
	fromPrice, toPrice string
}

// planCaseTierChange loads and validates everything a tier change depends on
// without any Stripe write. Organization OWNER/ADMIN is enforced by the HTTP
// layer; the installation is resolved inside the organization scope here.
func (s *Service) planCaseTierChange(ctx context.Context, orgID, installationID int64, rawTier string, now time.Time) (*caseTierPlan, error) {
	if s.caseStore == nil || s.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	provider, ok := s.provider.(CaseProvider)
	if !ok || provider == nil {
		return nil, ErrProviderNotConfigured
	}
	if orgID <= 0 || installationID <= 0 {
		return nil, ErrCaseNotManaged
	}
	target, known := casebilling.Lookup(rawTier)
	if !known || s.casePrices[target.Tier] == "" ||
		casebilling.Rank(target.Tier) > casebilling.Rank(s.caseVerifiedThrough) {
		return nil, ErrCaseNotPurchasable
	}
	row, err := s.caseStore.GetScoped(ctx, orgID, installationID)
	if err != nil {
		return nil, err
	}
	// Tier changes apply to a paid ACTIVE add-on only: not a pending
	// checkout, a founder trial, or a PAST_DUE/CANCELED subscription.
	if row == nil || row.Status != repository.SubscriptionActive || row.Provider != "stripe" ||
		row.ProviderSubscriptionID == "" || row.ProviderCustomerID == "" || row.ProviderPriceID == "" {
		return nil, ErrCaseNotManaged
	}
	if row.SelectedGameServerID == nil || *row.SelectedGameServerID != row.GameServerID {
		return nil, ErrCaseNotManaged // repointed installation: the add-on belongs to another server
	}
	if row.CancelAtPeriodEnd {
		return nil, ErrCaseCancelScheduled
	}
	current, err := s.provider.GetSubscription(ctx, row.ProviderSubscriptionID)
	if err != nil {
		return nil, err
	}
	if err := s.validateCaseBinding(row, current, orgID, installationID); err != nil {
		return nil, err
	}
	if current.StripeStatus != "active" {
		return nil, ErrCaseNotManaged
	}
	if current.CancelAtPeriodEnd {
		return nil, ErrCaseCancelScheduled
	}
	if current.PendingUpdate {
		return nil, ErrCaseChangePending
	}
	currentRank, targetRank := casebilling.Rank(casebilling.Tier(row.Tier)), casebilling.Rank(target.Tier)
	if currentRank == 0 {
		return nil, repository.ErrCaseWebhookMismatch
	}
	paidTier := casebilling.Tier(row.PaidTier)
	if paidTier == "" {
		paidTier = casebilling.Tier(row.Tier)
	}
	paidValid := row.PaidThrough != nil && row.PaidThrough.After(now)
	plan := &caseTierPlan{row: row, current: current, target: target,
		fromPrice: row.ProviderPriceID, toPrice: s.casePrices[target.Tier]}
	switch {
	case targetRank == currentRank:
		return nil, ErrCaseTierUnchanged
	case targetRank < currentRank:
		plan.kind = CaseTierDowngrade
	case paidValid && targetRank <= casebilling.Rank(paidTier):
		plan.kind = CaseTierRestore
	default:
		plan.kind = CaseTierUpgrade
		// An upgrade is a new sale: it needs sales enabled and the same
		// active paid base customer the add-on is billed to. Downgrades and
		// restores never charge and stay available with sales paused.
		if !s.caseEnabled {
			return nil, ErrCaseDisabled
		}
		base, err := s.casePaidBase(ctx, orgID, now)
		if err != nil {
			return nil, err
		}
		if base.ProviderCustomerID != row.ProviderCustomerID {
			return nil, repository.ErrCaseWebhookMismatch
		}
	}
	if err := provider.ValidateCasePrice(ctx, plan.toPrice, target.AmountCents, target.Tier); err != nil {
		return nil, fmt.Errorf("validate case Stripe price: %w", err)
	}
	return plan, nil
}

// CasePreviewTierChange is read-only at Stripe (an invoice preview at most).
func (s *Service) CasePreviewTierChange(ctx context.Context, orgID, installationID int64, rawTier string) (*CaseTierPreview, error) {
	now := time.Now().UTC()
	plan, err := s.planCaseTierChange(ctx, orgID, installationID, rawTier, now)
	if err != nil {
		return nil, err
	}
	out := &CaseTierPreview{
		Kind: plan.kind, CurrentTier: casebilling.Tier(plan.row.Tier), TargetTier: plan.target.Tier,
		Currency: plan.target.Currency, ProrationDate: now.Unix(),
		NextRenewalAmountCents: plan.target.AmountCents, CurrentPeriodEnd: plan.row.CurrentPeriodEnd,
	}
	switch plan.kind {
	case CaseTierUpgrade:
		preview, err := s.provider.(CaseProvider).PreviewCaseTierChange(ctx,
			plan.row.ProviderSubscriptionID, plan.fromPrice, plan.toPrice, out.ProrationDate)
		if err != nil {
			return nil, fmt.Errorf("preview case tier change: %w", err)
		}
		if preview == nil || preview.AmountDue < 0 {
			return nil, fmt.Errorf("preview case tier change: invalid provider preview")
		}
		out.AmountDueNowCents = preview.AmountDue
		if preview.Currency != "" {
			out.Currency = preview.Currency
		}
	case CaseTierDowngrade:
		out.EffectiveAt = plan.row.PaidThrough
		if out.EffectiveAt == nil || !out.EffectiveAt.After(now) {
			t := now
			out.EffectiveAt = &t
		}
	case CaseTierRestore:
		t := now
		out.EffectiveAt = &t
	}
	return out, nil
}

// CaseChangeTier applies a previewed change. prorationDate must come from a
// recent preview so an upgrade charges exactly what the owner confirmed; the
// same confirmation retried reuses one Stripe idempotency key.
func (s *Service) CaseChangeTier(ctx context.Context, orgID, installationID int64, rawTier string, prorationDate int64) (*CaseTierChangeOutcome, error) {
	now := time.Now().UTC()
	confirmed := time.Unix(prorationDate, 0)
	if prorationDate <= 0 || confirmed.Before(now.Add(-caseProrationWindow)) || confirmed.After(now.Add(time.Minute)) {
		return nil, ErrCasePreviewExpired
	}
	plan, err := s.planCaseTierChange(ctx, orgID, installationID, rawTier, now)
	if err != nil {
		return nil, err
	}
	res, err := s.provider.(CaseProvider).ChangeCaseTier(ctx, CaseTierChangeInput{
		SubscriptionID: plan.row.ProviderSubscriptionID, FromPriceID: plan.fromPrice, NewPriceID: plan.toPrice,
		Charge: plan.kind == CaseTierUpgrade, ProrationDate: prorationDate,
		IdempotencyKey: fmt.Sprintf("champion-case-tier-%d-%s-%s-%d", plan.row.ID, plan.fromPrice, plan.toPrice, prorationDate),
	})
	if err != nil {
		return nil, fmt.Errorf("change case Stripe tier: %w", err)
	}
	if res == nil || res.State == nil || res.State.SubscriptionID != plan.row.ProviderSubscriptionID ||
		res.State.CustomerID != plan.row.ProviderCustomerID {
		return nil, repository.ErrCaseWebhookMismatch
	}
	out := &CaseTierChangeOutcome{Kind: plan.kind, Row: plan.row}
	if res.Pending {
		// Stripe keeps the old price until the proration is paid; nothing
		// local changes and the webhook applies the tier if payment lands.
		out.Pending = true
		return out, nil
	}
	if res.State.PriceID != plan.toPrice {
		return nil, repository.ErrCaseWebhookMismatch
	}
	if err := s.caseStore.SaveCaseTierChange(ctx, orgID, installationID, plan.row.ProviderSubscriptionID,
		plan.fromPrice, plan.toPrice, string(plan.target.Tier)); err != nil {
		return nil, fmt.Errorf("persist case tier change: %w", err)
	}
	row := *plan.row
	row.Tier, row.ProviderPriceID = string(plan.target.Tier), plan.toPrice
	out.Row = &row
	return out, nil
}
