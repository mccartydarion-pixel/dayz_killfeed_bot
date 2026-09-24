package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const caseProductKind = "CASE_ADDON"

var (
	ErrCaseDisabled = errors.New("C.A.S.E. checkout disabled")
	ErrCaseNotPurchasable = errors.New("C.A.S.E. package not yet purchasable")
	ErrCaseBaseRequired = errors.New("active paid base subscription required")
)

// CaseStore is deliberately distinct from the existing base Store; no C.A.S.E.
// write can call ApplyProviderState on the organization's base subscription.
type CaseStore interface {
	ReserveCaseCheckout(ctx context.Context, orgID, installationID, serverID int64, tier, customerID string) (*repository.CaseCheckoutReservation, error)
	StoreCaseCheckout(ctx context.Context, id int64, sessionID, checkoutURL string) error
	GetByCaseSubscriptionID(ctx context.Context, subscriptionID string) (*repository.CaseAddonSubscription, error)
	ApplyCaseWebhook(ctx context.Context, in repository.CaseWebhookState) error
	ListByOrganization(ctx context.Context, organizationID int64) ([]repository.CaseAddonSubscription, error)
}

// CaseProvider is a separately extended Stripe boundary; the normal base
// Provider interface and its existing checkout path remain unchanged.
type CaseProvider interface {
	ValidateCasePrice(ctx context.Context, priceID string, expectedAmount int64, tier casebilling.Tier) error
	CreateCaseCheckoutSession(ctx context.Context, in CaseCheckoutInput) (*CheckoutSession, error)
}

type CaseCheckoutInput struct {
	CustomerID, PriceID string
	OrganizationID, InstallationID, GameServerID, AddonID int64
	Tier casebilling.Tier
	SuccessURL, CancelURL string
}

type CaseOptions struct {
	Enabled bool
	VerifiedThrough casebilling.Tier
	PriceIDs map[casebilling.Tier]string
}

type CaseCheckoutRequest struct {
	InstallationID int64
	Tier casebilling.Tier
	ReturnPath string
}

// ConfigureCaseAddons validates explicit price mappings before any action is
// possible. The release flag defaults off and is not inferred from Stripe
// account connection or from the mere existence of prices.
func (s *Service) ConfigureCaseAddons(store CaseStore, opts CaseOptions) error {
	if store == nil { return fmt.Errorf("case add-on store is required") }
	seen := map[string]bool{}
	ids := make(map[casebilling.Tier]string)
	for tier, id := range opts.PriceIDs {
		if _, ok := casebilling.Lookup(string(tier)); !ok { return fmt.Errorf("unknown case tier %q", tier) }
		id = strings.TrimSpace(id)
		if id == "" { continue }
		if seen[id] { return fmt.Errorf("duplicate case price id") }
		seen[id] = true
		if _, _, isBasePrice := s.catalog.PlanForPrice(id); isBasePrice { return fmt.Errorf("case price collides with base catalog") }
		ids[tier] = id
	}
	if opts.VerifiedThrough != "" {
		if _, ok := casebilling.Lookup(string(opts.VerifiedThrough)); !ok { return fmt.Errorf("unknown verified case tier") }
	}
	if opts.Enabled {
		if opts.VerifiedThrough == "" { return fmt.Errorf("case release must name independently verified tier") }
		if s.provider == nil { return ErrProviderNotConfigured }
		if _, ok := s.provider.(CaseProvider); !ok { return fmt.Errorf("case provider not supported") }
		if ids[casebilling.Watch] == "" { return fmt.Errorf("case Watch Stripe price must be configured") }
	}
	s.caseStore = store
	s.casePrices = ids
	s.caseEnabled = opts.Enabled
	s.caseVerifiedThrough = opts.VerifiedThrough
	return nil
}

func (s *Service) CasePlans() []casebilling.Plan {
	out := casebilling.Plans()
	verified := len(caseCapabilitiesForTier(s.caseVerifiedThrough))
	for i := range out {
		out[i].Purchasable = s.caseEnabled && s.casePrices[out[i].Tier] != "" &&
			len(caseCapabilitiesForTier(out[i].Tier)) <= verified
	}
	return out
}

func caseCapabilitiesForTier(t casebilling.Tier) []casebilling.Capability {
	switch t {
	case casebilling.Watch: return []casebilling.Capability{casebilling.CapWatch}
	case casebilling.Pro: return []casebilling.Capability{casebilling.CapWatch,casebilling.CapPro}
	case casebilling.Command: return []casebilling.Capability{casebilling.CapWatch,casebilling.CapPro,casebilling.CapCommand}
	default: return nil
	}
}

func (s *Service) CaseSubscriptions(ctx context.Context, orgID int64) ([]repository.CaseAddonSubscription, error) {
	if s.caseStore==nil{return nil,ErrProviderNotConfigured}
	return s.caseStore.ListByOrganization(ctx,orgID)
}

// CaseCheckout creates a separate add-on subscription on the EXISTING Stripe
// customer, never a new base subscription, and never grants access on redirect.
// If a retry races, the stable reserved add-on ID is the Stripe idempotency key.
func (s *Service) CaseCheckout(ctx context.Context, r *http.Request, orgID int64, req CaseCheckoutRequest, installations *repository.InstallationRepository) (*CheckoutResult,error) {
	if !s.caseEnabled || s.caseStore==nil {return nil,ErrCaseDisabled}
	provider,ok:=s.provider.(CaseProvider)
	if !ok{return nil,ErrProviderNotConfigured}
	plan,known:=casebilling.Lookup(string(req.Tier))
	if !known || s.casePrices[plan.Tier]=="" ||
		len(caseCapabilitiesForTier(plan.Tier))>len(caseCapabilitiesForTier(s.caseVerifiedThrough)) {
		return nil,ErrCaseNotPurchasable
	}
	if orgID<=0 || req.InstallationID<=0 || installations==nil {return nil,ErrCaseNotPurchasable}
	base,err:=s.store.GetForOrganization(ctx,orgID)
	if err!=nil{return nil,err}
	now:=time.Now()
	if base==nil || base.Status!=repository.SubscriptionActive ||
		base.Provider!=repository.ProviderStripe || base.ProviderCustomerID=="" ||
		base.ProviderSubscriptionID=="" || base.CurrentPeriodEnd==nil || !base.CurrentPeriodEnd.After(now) {
		return nil,ErrCaseBaseRequired
	}
	inst,err:=installations.GetScoped(ctx,orgID,req.InstallationID)
	if err!=nil{return nil,err}
	if inst==nil || inst.GameServerID==nil{return nil,repository.ErrCaseCheckoutConflict}
	origin:=ResolveOrigin(r,s.allowedOrigins)
	success,valid:=SafeReturnURL(origin,req.ReturnPath,"/dashboard/subscription?case_checkout=success")
	if !valid{return nil,ErrInvalidReturnPath}
	// Customer-provided return paths never control the cancellation host.
	cancel,valid:=SafeReturnURL(origin,"","/dashboard/subscription?case_checkout=cancelled")
	if !valid{return nil,ErrInvalidReturnPath}
	priceID:=s.casePrices[plan.Tier]
	if err:=provider.ValidateCasePrice(ctx,priceID,plan.AmountCents,plan.Tier);err!=nil {
		return nil,fmt.Errorf("validate case Stripe price: %w",err)
	}
	pending,err:=s.caseStore.ReserveCaseCheckout(ctx,orgID,inst.ID,*inst.GameServerID,string(plan.Tier),base.ProviderCustomerID)
	if err!=nil{return nil,err}
	if pending.CheckoutURL!="" {return &CheckoutResult{URL:pending.CheckoutURL},nil}
	session,err:=provider.CreateCaseCheckoutSession(ctx,CaseCheckoutInput{
		CustomerID:base.ProviderCustomerID,PriceID:priceID,
		OrganizationID:orgID,InstallationID:inst.ID,GameServerID:*inst.GameServerID,
		AddonID:pending.ID,Tier:plan.Tier,SuccessURL:success,CancelURL:cancel,
	})
	if err!=nil{return nil,fmt.Errorf("create case Stripe checkout: %w",err)}
	if err:=s.caseStore.StoreCaseCheckout(ctx,pending.ID,session.ID,session.URL);err!=nil{return nil,err}
	return &CheckoutResult{URL:session.URL},nil
}

// CaseMetadata returns the exact same binding fields on BOTH Checkout Session
// and Stripe Subscription; an invoice can then be classified from its parent.
func CaseMetadata(in CaseCheckoutInput) map[string]string {
	return map[string]string{
		"champion_product_kind":caseProductKind,
		"champion_case_addon_id":strconv.FormatInt(in.AddonID,10),
		"champion_organization_id":strconv.FormatInt(in.OrganizationID,10),
		"champion_installation_id":strconv.FormatInt(in.InstallationID,10),
		"champion_game_server_id":strconv.FormatInt(in.GameServerID,10),
		"champion_case_tier":string(in.Tier),
	}
}
