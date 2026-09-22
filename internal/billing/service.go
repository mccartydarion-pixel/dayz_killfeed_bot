package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Errors the HTTP layer maps to the stable API contract (docs/BILLING.md "Billing API security").
var (
	ErrUnknownPlan          = errors.New("unknown or non-purchasable plan")
	ErrPlanNotSold          = errors.New("this plan is not sold on the requested billing interval")
	ErrNoActiveSubscription = errors.New("organization has no active billing subscription")
	ErrNoBillingCustomer    = errors.New("organization has not started billing yet")
	ErrInvalidReturnPath    = errors.New("invalid return path")
	ErrInvalidSignature     = errors.New("invalid webhook signature")
)

// Store is the persistence Service needs; *repository.SubscriptionRepository implements it.
type Store interface {
	GetForOrganization(ctx context.Context, organizationID int64) (*repository.Subscription, error)
	EnsureTrial(ctx context.Context, organizationID int64, trialEndsAt time.Time) (*repository.Subscription, error)
	SetProviderCustomer(ctx context.Context, organizationID int64, provider, customerID string) error
	ApplyProviderState(ctx context.Context, organizationID int64, s repository.ProviderState) (*repository.Subscription, error)
	GetByProviderCustomerID(ctx context.Context, provider, customerID string) (*repository.Subscription, error)
	GetByProviderSubscriptionID(ctx context.Context, provider, subscriptionID string) (*repository.Subscription, error)
	RecordWebhookEventOnce(ctx context.Context, provider, eventID, eventType string, organizationID *int64) (bool, error)
}

// Service is the billing orchestration layer: HTTP handlers call this, never the Provider or Store
// directly (docs/BILLING.md).
type Service struct {
	store          Store
	catalog        *Catalog
	provider       Provider // nil = billing not configured (ErrProviderNotConfigured on every action)
	webhookSecret  string
	allowedOrigins []string
	successPath    string // default returnPath when the caller doesn't send one
	cancelPath     string
	portalPath     string
}

// Options configures a Service. Every path defaults to a sane value if empty, so a caller only
// needs to set what it wants to change.
type Options struct {
	AllowedOrigins     []string
	WebhookSecret      string
	DefaultSuccessPath string
	DefaultCancelPath  string
	DefaultPortalPath  string
}

func NewService(store Store, catalog *Catalog, provider Provider, opt Options) *Service {
	if catalog == nil {
		catalog = &Catalog{byKey: map[string]Plan{}, byPrice: map[string]pricedPlan{}}
	}
	origins := opt.AllowedOrigins
	if len(origins) == 0 {
		origins = []string{DefaultOrigin}
	}
	s := &Service{store: store, catalog: catalog, provider: provider, webhookSecret: opt.WebhookSecret, allowedOrigins: origins,
		successPath: opt.DefaultSuccessPath, cancelPath: opt.DefaultCancelPath, portalPath: opt.DefaultPortalPath}
	if s.successPath == "" {
		s.successPath = "/billing?checkout=success"
	}
	if s.cancelPath == "" {
		s.cancelPath = "/billing?checkout=cancelled"
	}
	if s.portalPath == "" {
		s.portalPath = "/billing"
	}
	return s
}

// Configured reports whether a real (or fake, for tests) Provider is wired - false means every
// billing action fails with ErrProviderNotConfigured rather than a nil-pointer panic.
func (s *Service) Configured() bool { return s.provider != nil }

// Catalog exposes the loaded plan catalog (read-only) for callers that need it directly (e.g. the
// admin API listing every plan, public or not).
func (s *Service) Catalog() *Catalog { return s.catalog }

// Plans returns the public plan catalog exactly as the pricing page should render it.
func (s *Service) Plans() []Plan { return s.catalog.PublicPlans() }

// --- subscription summary -----------------------------------------------------------------------

// Summary is the website's subscription view (docs/BILLING.md "Current subscription" /
// "Website handoff"). It never carries a Stripe id - those stay server-side.
type Summary struct {
	Plan                  string
	Status                string
	BillingInterval       string
	TrialEndsAt           *time.Time
	CurrentPeriodStart    *time.Time
	CurrentPeriodEnd      *time.Time
	CancelAtPeriodEnd     bool
	Entitlements          []string
	HasBillingCustomer    bool // a Portal session can be created
	HasActiveSubscription bool // checkout has completed at least once (change/cancel/reactivate apply)
}

// Summary loads organizationID's subscription and folds in its entitlements. It creates the
// TRIAL row on first read (mirrors the existing EnsureTrial-on-dashboard-read behaviour) so a
// freshly created organization never 404s here.
func (s *Service) Summary(ctx context.Context, organizationID int64) (*Summary, error) {
	sub, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		sub, err = s.store.EnsureTrial(ctx, organizationID, time.Now().AddDate(0, 0, 14))
		if err != nil {
			return nil, err
		}
	}
	return summaryOf(sub), nil
}

func summaryOf(sub *repository.Subscription) *Summary {
	return &Summary{
		Plan: sub.Plan, Status: sub.Status, BillingInterval: sub.BillingInterval,
		TrialEndsAt: sub.TrialEndsAt, CurrentPeriodStart: sub.CurrentPeriodStart, CurrentPeriodEnd: sub.CurrentPeriodEnd,
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd, Entitlements: entitlementStrings(sub.Plan),
		HasBillingCustomer: sub.ProviderCustomerID != "", HasActiveSubscription: sub.ProviderSubscriptionID != "",
	}
}

// --- checkout -------------------------------------------------------------------------------------

// OrgInfo is the organization detail EnsureCustomer needs; the caller (the HTTP handler) already
// has it loaded from SaaSOrganizations and passes it through rather than Service taking a dependency
// on the organizations repository.
type OrgInfo struct {
	Name  string
	Email string // "" is fine; Stripe Checkout still collects an email itself
}

// CheckoutRequest is the validated input to Checkout (docs/BILLING.md "Checkout session").
type CheckoutRequest struct {
	PlanKey    string
	Interval   string
	ReturnPath string // optional; root-relative; combined with a server-resolved, allowlisted origin
}

type CheckoutResult struct{ URL string }

// Checkout resolves the plan/price, ensures a Stripe customer, and creates a Checkout Session.
// Requires organization OWNER/ADMIN - enforced by the caller (docs/BILLING.md "Checkout
// authorization"), not here, so Service stays free of the organization-role repository dependency.
func (s *Service) Checkout(ctx context.Context, r *http.Request, organizationID int64, org OrgInfo, req CheckoutRequest) (*CheckoutResult, error) {
	if s.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	plan, ok := s.catalog.Get(req.PlanKey)
	if !ok || !plan.IsPublic {
		return nil, ErrUnknownPlan
	}
	price := plan.Price(req.Interval)
	if price == nil {
		return nil, ErrPlanNotSold
	}
	sub, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		if sub, err = s.store.EnsureTrial(ctx, organizationID, time.Now().AddDate(0, 0, 14)); err != nil {
			return nil, err
		}
	}
	customerID, err := s.provider.EnsureCustomer(ctx, CustomerInput{ExistingCustomerID: sub.ProviderCustomerID, OrganizationID: organizationID, Email: org.Email, Name: org.Name})
	if err != nil {
		return nil, fmt.Errorf("ensure stripe customer: %w", err)
	}
	if customerID != sub.ProviderCustomerID {
		if err := s.store.SetProviderCustomer(ctx, organizationID, repository.ProviderStripe, customerID); err != nil {
			return nil, err
		}
	}
	origin := ResolveOrigin(r, s.allowedOrigins)
	successURL, ok := SafeReturnURL(origin, req.ReturnPath, s.successPath)
	if !ok {
		return nil, ErrInvalidReturnPath
	}
	cancelURL, ok := SafeReturnURL(origin, "", s.cancelPath)
	if !ok {
		return nil, ErrInvalidReturnPath
	}
	// Trial abuse prevention (docs/BILLING.md "Trials"): a Stripe trial is granted at most once per
	// organization, gated by trial_consumed (set the first time a Stripe subscription id is ever
	// recorded) - not by the still-open internal TRIAL row, which an organization can otherwise sit
	// in indefinitely without ever having checked out.
	trialDays := 0
	if !sub.TrialConsumed {
		trialDays = plan.TrialDays
	}
	cs, err := s.provider.CreateCheckoutSession(ctx, CheckoutInput{
		CustomerID: customerID, PriceID: price.StripePriceID, OrganizationID: organizationID, PlanKey: plan.Key, BillingInterval: strings.ToUpper(req.Interval),
		SuccessURL: successURL, CancelURL: cancelURL, TrialDays: trialDays,
	})
	if err != nil {
		return nil, fmt.Errorf("create stripe checkout session: %w", err)
	}
	slog.Info("component=billing", "event", "billing_checkout_created", "organization_id", organizationID, "plan", plan.Key, "interval", req.Interval, "trial_days", trialDays)
	return &CheckoutResult{URL: cs.URL}, nil
}

// --- customer portal --------------------------------------------------------------------------------

type PortalRequest struct{ ReturnPath string }
type PortalResult struct{ URL string }

// Portal creates a Stripe Customer Portal session for organizationID. Requires an existing Stripe
// customer (at least one checkout must have happened) and organization OWNER/ADMIN, enforced by
// the caller.
func (s *Service) Portal(ctx context.Context, r *http.Request, organizationID int64, req PortalRequest) (*PortalResult, error) {
	if s.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	sub, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.ProviderCustomerID == "" {
		return nil, ErrNoBillingCustomer
	}
	origin := ResolveOrigin(r, s.allowedOrigins)
	returnURL, ok := SafeReturnURL(origin, req.ReturnPath, s.portalPath)
	if !ok {
		return nil, ErrInvalidReturnPath
	}
	ps, err := s.provider.CreatePortalSession(ctx, sub.ProviderCustomerID, returnURL)
	if err != nil {
		return nil, fmt.Errorf("create stripe portal session: %w", err)
	}
	return &PortalResult{URL: ps.URL}, nil
}

// --- plan changes, cancel, reactivate --------------------------------------------------------------

// ChangePlan swaps the organization's active Stripe subscription to a different plan/interval.
// Applies immediately with Stripe's default proration for both an upgrade and a downgrade - see
// docs/BILLING.md "Upgrade" / "Downgrade" for why Phase 1 does not defer a downgrade to renewal.
func (s *Service) ChangePlan(ctx context.Context, organizationID int64, planKey, interval string) (*Summary, error) {
	if s.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	plan, ok := s.catalog.Get(planKey)
	if !ok || !plan.IsPublic {
		return nil, ErrUnknownPlan
	}
	price := plan.Price(interval)
	if price == nil {
		return nil, ErrPlanNotSold
	}
	sub, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.ProviderSubscriptionID == "" {
		return nil, ErrNoActiveSubscription
	}
	st, err := s.provider.ChangeSubscriptionPrice(ctx, sub.ProviderSubscriptionID, price.StripePriceID)
	if err != nil {
		if errors.Is(err, ErrSubscriptionNotFound) {
			return nil, ErrNoActiveSubscription
		}
		return nil, fmt.Errorf("change stripe subscription price: %w", err)
	}
	updated, err := s.applyState(ctx, organizationID, st, plan.Key)
	if err != nil {
		return nil, err
	}
	slog.Info("component=billing", "event", "billing_subscription_updated", "organization_id", organizationID, "plan", plan.Key, "interval", interval)
	return summaryOf(updated), nil
}

// Cancel schedules cancellation at the end of the current billing period (docs/BILLING.md
// "Cancellation"): the subscription stays fully active - and every entitlement it grants - until
// then.
func (s *Service) Cancel(ctx context.Context, organizationID int64) (*Summary, error) {
	sum, err := s.setCancelAtPeriodEnd(ctx, organizationID, true)
	if err == nil {
		slog.Info("component=billing", "event", "billing_subscription_cancelled", "organization_id", organizationID)
	}
	return sum, err
}

// Reactivate undoes a scheduled cancellation while the subscription is still active (docs/BILLING.md
// "Reactivation").
func (s *Service) Reactivate(ctx context.Context, organizationID int64) (*Summary, error) {
	sum, err := s.setCancelAtPeriodEnd(ctx, organizationID, false)
	if err == nil {
		slog.Info("component=billing", "event", "billing_subscription_reactivated", "organization_id", organizationID)
	}
	return sum, err
}

func (s *Service) setCancelAtPeriodEnd(ctx context.Context, organizationID int64, cancel bool) (*Summary, error) {
	if s.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	sub, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.ProviderSubscriptionID == "" {
		return nil, ErrNoActiveSubscription
	}
	st, err := s.provider.SetCancelAtPeriodEnd(ctx, sub.ProviderSubscriptionID, cancel)
	if err != nil {
		if errors.Is(err, ErrSubscriptionNotFound) {
			return nil, ErrNoActiveSubscription
		}
		return nil, fmt.Errorf("update stripe cancel_at_period_end: %w", err)
	}
	updated, err := s.applyState(ctx, organizationID, st, "")
	if err != nil {
		return nil, err
	}
	return summaryOf(updated), nil
}

// --- reconciliation ---------------------------------------------------------------------------------

// Reconcile pulls the organization's subscription straight from Stripe and overwrites Champion's
// row with it (docs/BILLING.md "Billing reconciliation") - useful for an admin tool or a periodic
// job to catch a webhook that was never delivered. It never touches any other organization's row
// and never touches installations/factions/economy/shop data. A subscription id that Stripe no
// longer recognizes (deleted outside of a webhook, e.g. from the Stripe dashboard) is reconciled to
// CANCELED rather than left stale.
func (s *Service) Reconcile(ctx context.Context, organizationID int64) (*Summary, error) {
	if s.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	sub, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.ProviderSubscriptionID == "" {
		if sub == nil {
			return nil, ErrNoActiveSubscription
		}
		return summaryOf(sub), nil // nothing to reconcile against - no Stripe subscription yet
	}
	st, err := s.provider.GetSubscription(ctx, sub.ProviderSubscriptionID)
	if errors.Is(err, ErrSubscriptionNotFound) {
		st = &SubscriptionState{SubscriptionID: sub.ProviderSubscriptionID, CustomerID: sub.ProviderCustomerID, PriceID: sub.ProviderPriceID, StripeStatus: "canceled"}
	} else if err != nil {
		return nil, fmt.Errorf("get stripe subscription: %w", err)
	}
	updated, err := s.applyState(ctx, organizationID, st, "")
	if err != nil {
		return nil, err
	}
	return summaryOf(updated), nil
}

// applyState maps a normalized Stripe state into repository.ProviderState and persists it.
// planKeyOverride wins when non-empty (a checkout/subscription-created event's own metadata, or an
// explicit ChangePlan call); otherwise the plan key is reverse-resolved from the price id via the
// catalog, and if that also fails the stored plan is left unchanged (repository.ApplyProviderState's
// CASE ... WHEN ” THEN plan).
func (s *Service) applyState(ctx context.Context, organizationID int64, st *SubscriptionState, planKeyOverride string) (*repository.Subscription, error) {
	plan := strings.ToUpper(strings.TrimSpace(planKeyOverride))
	if plan == "" {
		if p, _, ok := s.catalog.PlanForPrice(st.PriceID); ok {
			plan = p.Key
		}
	}
	ps := repository.ProviderState{
		Provider: repository.ProviderStripe, ProviderCustomerID: st.CustomerID, ProviderSubscriptionID: st.SubscriptionID, ProviderPriceID: st.PriceID,
		Plan: plan, Status: MapStatus(st.StripeStatus), BillingInterval: MapInterval(st.StripeInterval),
		TrialEndsAt: st.TrialEnd, CurrentPeriodStart: zeroToNil(st.CurrentPeriodStart), CurrentPeriodEnd: zeroToNil(st.CurrentPeriodEnd),
		CancelAtPeriodEnd: st.CancelAtPeriodEnd, CanceledAt: st.CanceledAt,
	}
	return s.store.ApplyProviderState(ctx, organizationID, ps)
}

func zeroToNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// entitlementStrings adapts internal/entitlements.Resolve to the []string the API/website expects
// (docs/SAAS_SCHEMA.md "Entitlements": Resolve is the one place feature-by-plan logic lives -
// billing never re-implements it).
func entitlementStrings(plan string) []string {
	keys := entitlements.Resolve(plan)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = string(k)
	}
	return out
}
