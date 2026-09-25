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
	GetScoped(ctx context.Context, organizationID, installationID int64) (*repository.CaseAddonSubscription, error)
	SaveCaseCancelFlag(ctx context.Context, organizationID, installationID int64, subscriptionID string, cancel bool) error
	GetPendingCaseCheckout(ctx context.Context, organizationID, installationID int64) (*repository.CaseCheckoutReservation,error)
	ResetExpiredCaseCheckout(ctx context.Context, organizationID, installationID, addonID, attempt int64, sessionID string) error
	SaveCaseTierChange(ctx context.Context, organizationID, installationID int64, subscriptionID, fromPrice, toPrice, toTier string) error
}

// CaseProvider is a separately extended Stripe boundary; the normal base
// Provider interface and its existing checkout path remain unchanged.
type CaseProvider interface {
	ValidateCasePrice(ctx context.Context, priceID string, expectedAmount int64, tier casebilling.Tier) error
	CreateCaseCheckoutSession(ctx context.Context, in CaseCheckoutInput) (*CheckoutSession, error)
	GetCaseCheckoutSession(ctx context.Context, id string) (*CaseCheckoutSessionState,error)
	// PreviewCaseTierChange is read-only: what an immediately invoiced
	// proration for swapping fromPrice to newPrice would charge right now.
	PreviewCaseTierChange(ctx context.Context, subscriptionID, fromPriceID, newPriceID string, prorationDate int64) (*CaseProrationPreview,error)
	// ChangeCaseTier swaps the add-on subscription's single price. Charge
	// invoices the proration now and leaves the change PENDING at Stripe
	// unless that payment succeeds; otherwise no proration is created.
	ChangeCaseTier(ctx context.Context, in CaseTierChangeInput) (*CaseTierChangeResult,error)
}

type CaseCheckoutInput struct {
	CustomerID, PriceID string
	OrganizationID, InstallationID, GameServerID, AddonID int64
	Attempt int64
	Tier casebilling.Tier
	SuccessURL, CancelURL string
}

type CaseOptions struct {
	Enabled bool // sales flag, independent of access for existing paid subscribers
	AccessEnabled bool // explicit premium access rollout; defaults false
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
	if opts.Enabled && !opts.AccessEnabled { return fmt.Errorf("case sales cannot be enabled while premium access is disabled") }
	if opts.Enabled || opts.AccessEnabled {
		if opts.VerifiedThrough == "" { return fmt.Errorf("case release must name independently verified tier") }
		if opts.Enabled {
			if s.provider == nil { return ErrProviderNotConfigured }
			if _, ok := s.provider.(CaseProvider); !ok { return fmt.Errorf("case provider not supported") }
			if ids[casebilling.Watch] == "" { return fmt.Errorf("case Watch Stripe price must be configured") }
		}
	}
	s.caseStore = store
	s.casePrices = ids
	s.caseEnabled = opts.Enabled
	s.caseAccessEnabled = opts.AccessEnabled
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
	base,err:=s.casePaidBase(ctx,orgID,time.Now())
	if err!=nil{return nil,err}
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
		AddonID:pending.ID,Attempt:pending.Attempt,Tier:plan.Tier,SuccessURL:success,CancelURL:cancel,
	})
	if err!=nil{return nil,fmt.Errorf("create case Stripe checkout: %w",err)}
	if err:=s.caseStore.StoreCaseCheckout(ctx,pending.ID,session.ID,session.URL);err!=nil{return nil,err}
	return &CheckoutResult{URL:session.URL},nil
}

// casePaidBase is the prerequisite for any C.A.S.E. sale: an active, paid
// Stripe base subscription whose customer the add-on is billed to.
func (s *Service) casePaidBase(ctx context.Context, orgID int64, now time.Time) (*repository.Subscription,error) {
	base,err:=s.store.GetForOrganization(ctx,orgID)
	if err!=nil{return nil,err}
	if base==nil || base.Status!=repository.SubscriptionActive ||
		base.Provider!=repository.ProviderStripe || base.ProviderCustomerID=="" ||
		base.ProviderSubscriptionID=="" || base.CurrentPeriodEnd==nil || !base.CurrentPeriodEnd.After(now) {
		return nil,ErrCaseBaseRequired
	}
	return base,nil
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

var ErrCaseNotManaged = errors.New("C.A.S.E. subscription not found or cannot be managed")

// CaseSetCancellation changes ONLY the add-on subscription for the selected
// installation. Cancellation is available even with purchasing disabled:
// turning off sales must never lock customers into recurring charges.
func (s *Service) CaseSetCancellation(ctx context.Context, organizationID, installationID int64, cancel bool) (*repository.CaseAddonSubscription,error) {
	if s.caseStore==nil || s.provider==nil { return nil,ErrProviderNotConfigured }
	if organizationID<=0 || installationID<=0 { return nil,ErrCaseNotManaged }
	row,err:=s.caseStore.GetScoped(ctx,organizationID,installationID)
	if err!=nil{return nil,err}
	if row==nil || row.Provider!="stripe" || row.ProviderSubscriptionID=="" ||
		row.ProviderCustomerID=="" || row.ProviderPriceID=="" {return nil,ErrCaseNotManaged}
	if row.Status!="ACTIVE" && row.Status!="TRIAL" { return nil,ErrCaseNotManaged }
	current,err:=s.provider.GetSubscription(ctx,row.ProviderSubscriptionID)
	if err!=nil{return nil,err}
	if err:=s.validateCaseBinding(row,current,organizationID,installationID);err!=nil{return nil,err}
	if current.StripeStatus!="active" && current.StripeStatus!="trialing" {return nil,ErrCaseNotManaged}
	if current.CancelAtPeriodEnd!=cancel {
		current,err=s.provider.SetCancelAtPeriodEnd(ctx,row.ProviderSubscriptionID,cancel)
		if err!=nil{return nil,fmt.Errorf("update case Stripe cancellation: %w",err)}
		if current==nil || current.SubscriptionID!=row.ProviderSubscriptionID ||
			current.CustomerID!=row.ProviderCustomerID ||
			current.PriceID!=row.ProviderPriceID ||
			current.CancelAtPeriodEnd!=cancel {
			return nil,repository.ErrCaseWebhookMismatch
		}
	}
	if err:=s.caseStore.SaveCaseCancelFlag(ctx,organizationID,installationID,row.ProviderSubscriptionID,current.CancelAtPeriodEnd);err!=nil {
		return nil,fmt.Errorf("persist case cancellation: %w",err)
	}
	row.CancelAtPeriodEnd=current.CancelAtPeriodEnd
	return row,nil
}

// validateCaseBinding proves Stripe's live subscription is exactly this
// organization/installation/server's add-on before any Stripe mutation. The
// current tier is proven by the configured price; champion_case_tier only
// has to name the originally purchased tier (it is not rewritten on a change).
func (s *Service) validateCaseBinding(row *repository.CaseAddonSubscription, current *SubscriptionState, organizationID, installationID int64) error {
	if row==nil || current==nil {return ErrCaseNotManaged}
	_,purchased:=casebilling.Lookup(current.Metadata["champion_case_tier"])
	if current.SubscriptionID!=row.ProviderSubscriptionID || current.CustomerID!=row.ProviderCustomerID ||
		current.PriceID!=row.ProviderPriceID ||
		current.Metadata["champion_product_kind"]!=caseProductKind ||
		current.Metadata["champion_case_addon_id"]!=strconv.FormatInt(row.ID,10) ||
		current.Metadata["champion_organization_id"]!=strconv.FormatInt(organizationID,10) ||
		current.Metadata["champion_installation_id"]!=strconv.FormatInt(installationID,10) ||
		current.Metadata["champion_game_server_id"]!=strconv.FormatInt(row.GameServerID,10) ||
		!purchased || string(s.caseTierForPrice(current.PriceID))!=row.Tier {
		return repository.ErrCaseWebhookMismatch
	}
	return nil
}

// CaseCheckoutSessionState is a read-only Stripe Checkout projection for
// recovering only a session whose provider status is unequivocally expired.
type CaseCheckoutSessionState struct {
	ID,Status,CustomerID,SubscriptionID string
	Metadata map[string]string
}

var ErrCaseCheckoutNotExpired = errors.New("C.A.S.E. checkout session is not safely expired")

// RecoverCaseCheckout does NOT create a Stripe session or charge a customer.
// It only permits a new checkout attempt after Stripe confirms that the
// previous session is expired with no attached subscription and still bound
// to the same organization, server, tier and customer.
func (s *Service) RecoverCaseCheckout(ctx context.Context, organizationID, installationID int64) error {
	if s.caseStore==nil || s.provider==nil{return ErrProviderNotConfigured}
	p,ok:=s.provider.(CaseProvider)
	if !ok{return ErrProviderNotConfigured}
	row,err:=s.caseStore.GetPendingCaseCheckout(ctx,organizationID,installationID)
	if err!=nil{return err}
	st,err:=p.GetCaseCheckoutSession(ctx,row.SessionID)
	if err!=nil{return fmt.Errorf("retrieve pending case checkout: %w",err)}
	if st==nil || st.ID!=row.SessionID || st.Status!="expired" ||
		st.CustomerID!=row.ProviderCustomerID || st.SubscriptionID!="" {
		return ErrCaseCheckoutNotExpired
	}
	expected:=CaseMetadata(CaseCheckoutInput{
		AddonID:row.ID,OrganizationID:row.OrganizationID,
		InstallationID:row.InstallationID,GameServerID:row.GameServerID,
		Tier:casebilling.Tier(row.Tier),
	})
	for key,value:=range expected {
		if st.Metadata[key]!=value{return repository.ErrCaseWebhookMismatch}
	}
	return s.caseStore.ResetExpiredCaseCheckout(ctx,organizationID,installationID,
		row.ID,row.Attempt,row.SessionID)
}
