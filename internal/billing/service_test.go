package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeStore is an in-memory Store (one row per organization, like the real table's
// UNIQUE(organization_id)) so Service's logic is exercised without a database.
type fakeStore struct {
	mu     sync.Mutex
	byOrg  map[int64]*repository.Subscription
	events map[[2]string]bool // (provider, event_id) already recorded
	nextID int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{byOrg: map[int64]*repository.Subscription{}, events: map[[2]string]bool{}}
}

func (f *fakeStore) GetForOrganization(_ context.Context, organizationID int64) (*repository.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byOrg[organizationID]
	if !ok {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (f *fakeStore) EnsureTrial(_ context.Context, organizationID int64, trialEndsAt time.Time) (*repository.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.byOrg[organizationID]; ok {
		cp := *s
		return &cp, nil
	}
	f.nextID++
	s := &repository.Subscription{ID: f.nextID, OrganizationID: organizationID, Plan: repository.SubscriptionTrial, Status: repository.SubscriptionTrial, TrialEndsAt: &trialEndsAt, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.byOrg[organizationID] = s
	cp := *s
	return &cp, nil
}

func (f *fakeStore) SetProviderCustomer(_ context.Context, organizationID int64, provider, customerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byOrg[organizationID]
	if !ok {
		return errors.New("no subscription row")
	}
	s.Provider, s.ProviderCustomerID = provider, customerID
	return nil
}

func (f *fakeStore) ApplyProviderState(_ context.Context, organizationID int64, st repository.ProviderState) (*repository.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byOrg[organizationID]
	if !ok {
		return nil, repository.ErrSubscriptionNotFound
	}
	s.Provider, s.ProviderCustomerID, s.ProviderSubscriptionID, s.ProviderPriceID = st.Provider, st.ProviderCustomerID, st.ProviderSubscriptionID, st.ProviderPriceID
	if st.Plan != "" {
		s.Plan = st.Plan
	}
	s.Status = st.Status
	if st.BillingInterval != "" {
		s.BillingInterval = st.BillingInterval
	}
	s.TrialEndsAt, s.CurrentPeriodStart, s.CurrentPeriodEnd = st.TrialEndsAt, st.CurrentPeriodStart, st.CurrentPeriodEnd
	s.CancelAtPeriodEnd, s.CanceledAt = st.CancelAtPeriodEnd, st.CanceledAt
	if st.ProviderSubscriptionID != "" {
		s.TrialConsumed = true
	}
	s.UpdatedAt = time.Now()
	cp := *s
	return &cp, nil
}

func (f *fakeStore) GetByProviderCustomerID(_ context.Context, provider, customerID string) (*repository.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.byOrg {
		if s.Provider == provider && s.ProviderCustomerID == customerID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) GetByProviderSubscriptionID(_ context.Context, provider, subscriptionID string) (*repository.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.byOrg {
		if s.Provider == provider && s.ProviderSubscriptionID == subscriptionID {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) RecordWebhookEventOnce(_ context.Context, provider, eventID, eventType string, _ *int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := [2]string{provider, eventID}
	if f.events[k] {
		return false, nil
	}
	f.events[k] = true
	return true, nil
}

func newTestService(t *testing.T, catalogJSON string) (*Service, *fakeStore, *FakeProvider) {
	t.Helper()
	c, err := LoadCatalog(catalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	provider := NewFakeProvider()
	svc := NewService(store, c, provider, Options{WebhookSecret: "whsec_test"})
	return svc, store, provider
}

func req(t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/billing/checkout", nil)
}

func TestSummaryCreatesTrialOnFirstRead(t *testing.T) {
	svc, _, _ := newTestService(t, "")
	sum, err := svc.Summary(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != repository.SubscriptionTrial || sum.Plan != repository.SubscriptionTrial || sum.HasBillingCustomer || sum.HasActiveSubscription {
		t.Fatalf("%+v", sum)
	}
	if len(sum.Entitlements) == 0 {
		t.Fatal("expected entitlements to resolve for the TRIAL plan")
	}
}

func TestCheckoutRejectsUnknownOrPrivateOrUnsoldPlan(t *testing.T) {
	svc, _, _ := newTestService(t, sampleCatalog) // legacy is private, starter has no yearly price
	ctx := context.Background()
	for name, r := range map[string]CheckoutRequest{
		"unknown key":      {PlanKey: "NOPE", Interval: "MONTHLY"},
		"private plan":     {PlanKey: "legacy", Interval: "MONTHLY"},
		"unsold interval":  {PlanKey: "starter", Interval: "YEARLY"},
		"garbage interval": {PlanKey: "pro", Interval: "WEEKLY"},
	} {
		if _, err := svc.Checkout(ctx, req(t), 1, OrgInfo{Name: "Org"}, r); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !errors.Is(err, ErrUnknownPlan) && !errors.Is(err, ErrPlanNotSold) {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
}

func TestCheckoutNotConfiguredWithoutProvider(t *testing.T) {
	c, err := LoadCatalog(sampleCatalog)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(newFakeStore(), c, nil, Options{})
	if svc.Configured() {
		t.Fatal("no provider was given")
	}
	if _, err := svc.Checkout(context.Background(), req(t), 1, OrgInfo{}, CheckoutRequest{PlanKey: "PRO", Interval: "MONTHLY"}); !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("%v", err)
	}
	if _, err := svc.Portal(context.Background(), req(t), 1, PortalRequest{}); !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestCheckoutGrantsTrialOnceThenNeverAgain(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	if _, err := svc.Checkout(ctx, req(t), 7, OrgInfo{Name: "Org 7"}, CheckoutRequest{PlanKey: "PRO", Interval: "MONTHLY"}); err != nil {
		t.Fatal(err)
	}
	// the checkout session itself doesn't create a subscription until "paid" - simulate that via the fake.
	sub, _ := store.GetForOrganization(ctx, 7)
	state := provider.CompleteCheckout(sub.ProviderCustomerID, "price_pro_month", 14)
	if state.StripeStatus != "trialing" {
		t.Fatalf("expected the first checkout to carry a trial: %+v", state)
	}
	if _, err := store.ApplyProviderState(ctx, 7, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: state.CustomerID, ProviderSubscriptionID: state.SubscriptionID, ProviderPriceID: state.PriceID, Plan: "PRO", Status: MapStatus(state.StripeStatus)}); err != nil {
		t.Fatal(err)
	}
	// A second checkout call (e.g. the buyer abandoned and starts over) must not ask for a trial again.
	if _, err := svc.Checkout(ctx, req(t), 7, OrgInfo{Name: "Org 7"}, CheckoutRequest{PlanKey: "PRO", Interval: "MONTHLY"}); err != nil {
		t.Fatal(err)
	}
	var lastTrialDays = -1
	for _, c := range provider.Calls {
		if c.Method == "CreateCheckoutSession" {
			lastTrialDays = c.Arg.(CheckoutInput).TrialDays
		}
	}
	if lastTrialDays != 0 {
		t.Fatalf("trial abuse: the second checkout still asked Stripe for a %d-day trial", lastTrialDays)
	}
}

func TestCheckoutReusesExistingStripeCustomer(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	if _, err := svc.Checkout(ctx, req(t), 3, OrgInfo{Name: "Org"}, CheckoutRequest{PlanKey: "STARTER", Interval: "MONTHLY"}); err != nil {
		t.Fatal(err)
	}
	first, _ := store.GetForOrganization(ctx, 3)
	if first.ProviderCustomerID == "" {
		t.Fatal("expected a customer id to be persisted")
	}
	if _, err := svc.Checkout(ctx, req(t), 3, OrgInfo{Name: "Org"}, CheckoutRequest{PlanKey: "STARTER", Interval: "MONTHLY"}); err != nil {
		t.Fatal(err)
	}
	second, _ := store.GetForOrganization(ctx, 3)
	if second.ProviderCustomerID != first.ProviderCustomerID {
		t.Fatalf("expected the same Stripe customer to be reused: %s vs %s", first.ProviderCustomerID, second.ProviderCustomerID)
	}
	ensureCalls := 0
	for _, c := range provider.Calls {
		if c.Method == "EnsureCustomer" {
			ensureCalls++
		}
	}
	if ensureCalls != 2 {
		t.Fatalf("expected EnsureCustomer called twice (once per checkout), got %d", ensureCalls)
	}
}

func TestCheckoutRejectsUnsafeReturnPath(t *testing.T) {
	svc, _, _ := newTestService(t, sampleCatalog)
	if _, err := svc.Checkout(context.Background(), req(t), 1, OrgInfo{}, CheckoutRequest{PlanKey: "PRO", Interval: "MONTHLY", ReturnPath: "https://evil.example/steal"}); !errors.Is(err, ErrInvalidReturnPath) {
		t.Fatalf("%v", err)
	}
	if _, err := svc.Checkout(context.Background(), req(t), 1, OrgInfo{}, CheckoutRequest{PlanKey: "PRO", Interval: "MONTHLY", ReturnPath: "//evil.example"}); !errors.Is(err, ErrInvalidReturnPath) {
		t.Fatalf("%v", err)
	}
}

// --- Champion Billing: checkout return route fix (stale "/billing" -> "/dashboard/subscription") ---

// TestNewServiceDefaultReturnURLsPointAtDashboardSubscription proves the service's built-in defaults
// - used whenever a caller sends no returnPath at all - are the current Champion website route, not
// the retired "/billing" page that caused a production 404 on Stripe Checkout cancellation.
func TestNewServiceDefaultReturnURLsPointAtDashboardSubscription(t *testing.T) {
	svc, _, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	if _, err := svc.Checkout(ctx, req(t), 20, OrgInfo{Name: "Org"}, CheckoutRequest{PlanKey: "PRO", Interval: "MONTHLY"}); err != nil {
		t.Fatal(err)
	}
	var in CheckoutInput
	for _, c := range provider.Calls {
		if c.Method == "CreateCheckoutSession" {
			in = c.Arg.(CheckoutInput)
		}
	}
	if in.SuccessURL != DefaultOrigin+"/dashboard/subscription?checkout=success" {
		t.Errorf("default success URL = %q", in.SuccessURL)
	}
	if in.CancelURL != DefaultOrigin+"/dashboard/subscription?checkout=cancelled" {
		t.Errorf("default cancel URL = %q", in.CancelURL)
	}
	if strings.Contains(in.SuccessURL, "/billing") || strings.Contains(in.CancelURL, "/billing") {
		t.Fatalf("no route may contain the retired /billing page: success=%q cancel=%q", in.SuccessURL, in.CancelURL)
	}

	// Portal's default follows the same route.
	sub, err := activeOrgSubscription(ctx, svc, 20)
	if err != nil || sub == nil || sub.ProviderCustomerID == "" {
		t.Fatalf("expected a persisted customer id: %+v %v", sub, err)
	}
	res, err := svc.Portal(ctx, req(t), 20, PortalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var portalReturn string
	for _, c := range provider.Calls {
		if c.Method == "CreatePortalSession" {
			portalReturn = c.Arg.(struct{ CustomerID, ReturnURL string }).ReturnURL
		}
	}
	if portalReturn != DefaultOrigin+"/dashboard/subscription" {
		t.Errorf("default portal return URL = %q", portalReturn)
	}
	if res.URL == "" {
		t.Fatal("expected a portal URL")
	}
}

// activeOrgSubscription is a small helper so the default-portal-URL assertion above can read back the
// customer id Checkout just persisted, without pulling in the heavier activeOrg helper (which also
// drives a webhook-style ApplyProviderState this test doesn't need).
func activeOrgSubscription(ctx context.Context, svc *Service, orgID int64) (*repository.Subscription, error) {
	return svc.store.GetForOrganization(ctx, orgID)
}

// TestCheckoutCancelURLDerivedFromCallerReturnPath is the focused regression test for the bug: the
// website sends one returnPath for the billing page ("/dashboard/subscription?checkout=success"),
// and the cancel URL must resolve to that same page with "?checkout=cancelled" - never the stale
// "/billing" default - because a caller-supplied returnPath must influence BOTH success and cancel.
func TestCheckoutCancelURLDerivedFromCallerReturnPath(t *testing.T) {
	svc, _, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	if _, err := svc.Checkout(ctx, req(t), 21, OrgInfo{Name: "Org"}, CheckoutRequest{
		PlanKey: "PRO", Interval: "MONTHLY", ReturnPath: "/dashboard/subscription?checkout=success",
	}); err != nil {
		t.Fatal(err)
	}
	var in CheckoutInput
	for _, c := range provider.Calls {
		if c.Method == "CreateCheckoutSession" {
			in = c.Arg.(CheckoutInput)
		}
	}
	if in.SuccessURL != DefaultOrigin+"/dashboard/subscription?checkout=success" {
		t.Errorf("success URL = %q", in.SuccessURL)
	}
	if in.CancelURL != DefaultOrigin+"/dashboard/subscription?checkout=cancelled" {
		t.Errorf("cancel URL = %q, want the same page as success with checkout=cancelled", in.CancelURL)
	}
	if strings.Contains(in.CancelURL, "/billing") {
		t.Fatalf("cancel URL must never fall back to the retired /billing page: %q", in.CancelURL)
	}

	// A caller returning to a different page entirely still gets a same-page cancel, never /billing.
	if _, err := svc.Checkout(ctx, req(t), 22, OrgInfo{Name: "Org 2"}, CheckoutRequest{
		PlanKey: "STARTER", Interval: "MONTHLY", ReturnPath: "/settings/org/9",
	}); err != nil {
		t.Fatal(err)
	}
	in = CheckoutInput{}
	for _, c := range provider.Calls {
		if c.Method == "CreateCheckoutSession" {
			in = c.Arg.(CheckoutInput)
		}
	}
	if in.CancelURL != DefaultOrigin+"/settings/org/9?checkout=cancelled" {
		t.Errorf("cancel URL for a non-default returnPath = %q", in.CancelURL)
	}
}

func TestChangePlanCancelReactivateRequireAnActiveSubscription(t *testing.T) {
	svc, _, _ := newTestService(t, sampleCatalog)
	ctx := context.Background()
	if _, err := svc.ChangePlan(ctx, 9, "PRO", "MONTHLY"); !errors.Is(err, ErrNoActiveSubscription) {
		t.Fatalf("ChangePlan: %v", err)
	}
	if _, err := svc.Cancel(ctx, 9); !errors.Is(err, ErrNoActiveSubscription) {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := svc.Reactivate(ctx, 9); !errors.Is(err, ErrNoActiveSubscription) {
		t.Fatalf("Reactivate: %v", err)
	}
	if _, err := svc.Portal(ctx, req(t), 9, PortalRequest{}); !errors.Is(err, ErrNoBillingCustomer) {
		t.Fatalf("Portal: %v", err)
	}
}

// activeOrg drives a full checkout -> paid subscription for orgID and returns the resulting row.
func activeOrg(t *testing.T, ctx context.Context, svc *Service, store *fakeStore, provider *FakeProvider, orgID int64, plan, priceID string) *repository.Subscription {
	t.Helper()
	if _, err := svc.Checkout(ctx, req(t), orgID, OrgInfo{Name: "Org"}, CheckoutRequest{PlanKey: plan, Interval: "MONTHLY"}); err != nil {
		t.Fatal(err)
	}
	sub, _ := store.GetForOrganization(ctx, orgID)
	state := provider.CompleteCheckout(sub.ProviderCustomerID, priceID, 0)
	updated, err := store.ApplyProviderState(ctx, orgID, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: state.CustomerID, ProviderSubscriptionID: state.SubscriptionID,
		ProviderPriceID: state.PriceID, Plan: plan, Status: MapStatus(state.StripeStatus), CurrentPeriodStart: &state.CurrentPeriodStart, CurrentPeriodEnd: &state.CurrentPeriodEnd})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestChangePlanUpgradeAndDowngradeApplyImmediately(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 5, "STARTER", "price_starter_month")

	sum, err := svc.ChangePlan(ctx, 5, "PRO", "MONTHLY")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Plan != "PRO" || sum.Status != repository.SubscriptionActive {
		t.Fatalf("upgrade: %+v", sum)
	}
	sub, _ := store.GetForOrganization(ctx, 5)
	if sub.ProviderPriceID != "price_pro_month" {
		t.Fatalf("expected the Stripe price to change immediately: %s", sub.ProviderPriceID)
	}

	sum, err = svc.ChangePlan(ctx, 5, "STARTER", "MONTHLY")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Plan != "STARTER" {
		t.Fatalf("downgrade: %+v", sum)
	}
}

func TestChangePlanUnknownPlanIsRejectedBeforeCallingStripe(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 5, "STARTER", "price_starter_month")
	before := len(provider.Calls)
	if _, err := svc.ChangePlan(ctx, 5, "NOPE", "MONTHLY"); !errors.Is(err, ErrUnknownPlan) {
		t.Fatalf("%v", err)
	}
	if len(provider.Calls) != before {
		t.Fatal("an invalid plan must never reach the provider")
	}
}

func TestCancelThenReactivate(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 6, "PRO", "price_pro_month")

	sum, err := svc.Cancel(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CancelAtPeriodEnd || sum.Status != repository.SubscriptionActive {
		t.Fatalf("cancel-at-period-end must keep the subscription active until then: %+v", sum)
	}
	sum, err = svc.Reactivate(ctx, 6)
	if err != nil {
		t.Fatal(err)
	}
	if sum.CancelAtPeriodEnd {
		t.Fatalf("reactivate: %+v", sum)
	}
}

func TestPortalNeedsAnExistingCustomer(t *testing.T) {
	svc, store, _ := newTestService(t, sampleCatalog)
	ctx := context.Background()
	store.EnsureTrial(ctx, 11, time.Now().AddDate(0, 0, 14))
	if _, err := svc.Portal(ctx, req(t), 11, PortalRequest{}); !errors.Is(err, ErrNoBillingCustomer) {
		t.Fatalf("%v", err)
	}
}

func TestPortalReturnsAURL(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 12, "STARTER", "price_starter_month")
	res, err := svc.Portal(ctx, req(t), 12, PortalRequest{})
	if err != nil || res.URL == "" {
		t.Fatalf("%v %+v", err, res)
	}
}

// --- webhooks --------------------------------------------------------------------------------------

func sign(t *testing.T, secret string, payload []byte) string {
	t.Helper()
	now := time.Now()
	sig := stripewebhook.ComputeSignature(now, payload, secret)
	return "t=" + itoa(now.Unix()) + ",v1=" + hexEnc(sig)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func hexEnc(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out)
}

func checkoutCompletedPayload(eventID string, orgID int64, subscriptionID string) []byte {
	return []byte(`{"id":"` + eventID + `","type":"checkout.session.completed","data":{"object":{
		"id":"cs_test_1","mode":"subscription","client_reference_id":"` + itoa(orgID) + `","customer":"cus_x","subscription":"` + subscriptionID + `",
		"metadata":{"champion_plan_key":"PRO"}}}}`)
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	svc, _, _ := newTestService(t, sampleCatalog)
	payload := checkoutCompletedPayload("evt_1", 1, "sub_x")
	err := svc.HandleWebhook(context.Background(), payload, "t=1,v1=deadbeef")
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("%v", err)
	}
}

func TestWebhookRequiresConfiguredSecret(t *testing.T) {
	c, _ := LoadCatalog(sampleCatalog)
	svc := NewService(newFakeStore(), c, NewFakeProvider(), Options{}) // no WebhookSecret
	err := svc.HandleWebhook(context.Background(), checkoutCompletedPayload("evt_1", 1, "sub_x"), "t=1,v1=ab")
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestWebhookCheckoutCompletedActivatesTheRightOrganization(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	store.EnsureTrial(ctx, 40, time.Now().AddDate(0, 0, 14))
	provider.Put(SubscriptionState{SubscriptionID: "sub_40", CustomerID: "cus_40", PriceID: "price_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})

	payload := checkoutCompletedPayload("evt_100", 40, "sub_40")
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatal(err)
	}
	sub, _ := store.GetForOrganization(ctx, 40)
	if sub.Status != repository.SubscriptionActive || sub.Plan != "PRO" || sub.ProviderSubscriptionID != "sub_40" || !sub.TrialConsumed {
		t.Fatalf("%+v", sub)
	}
}

func TestWebhookIdempotentOnRedelivery(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	store.EnsureTrial(ctx, 41, time.Now().AddDate(0, 0, 14))
	provider.Put(SubscriptionState{SubscriptionID: "sub_41", CustomerID: "cus_41", PriceID: "price_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	payload := checkoutCompletedPayload("evt_dup", 41, "sub_41")
	sig := sign(t, "whsec_test", payload)

	for i := 0; i < 5; i++ {
		if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if n := len(store.events); n != 1 {
		t.Fatalf("expected exactly one recorded event, got %d", n)
	}
	// Change the subscription out from under the org, then redeliver the SAME event: it must not
	// reapply (proves idempotency is enforced before reprocessing, not just "harmless to repeat").
	sub, _ := store.GetForOrganization(ctx, 41)
	if _, err := store.ApplyProviderState(ctx, 41, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: sub.ProviderCustomerID, ProviderSubscriptionID: sub.ProviderSubscriptionID,
		Plan: "STARTER", Status: repository.SubscriptionPastDue}); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetForOrganization(ctx, 41)
	if after.Plan != "STARTER" || after.Status != repository.SubscriptionPastDue {
		t.Fatalf("a duplicate delivery must not reprocess: %+v", after)
	}
}

func TestWebhookUnknownEventTypeIsAcknowledgedAndIgnored(t *testing.T) {
	svc, store, _ := newTestService(t, sampleCatalog)
	payload := []byte(`{"id":"evt_unk","type":"customer.updated","data":{"object":{"id":"cus_1"}}}`)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(context.Background(), payload, sig); err != nil {
		t.Fatal(err)
	}
	if len(store.byOrg) != 0 {
		t.Fatal("an unhandled event type must not touch any subscription")
	}
}

func TestWebhookSubscriptionUpdatedResolvesOrgByCustomerWhenNoMetadata(t *testing.T) {
	svc, store, _ := newTestService(t, sampleCatalog)
	ctx := context.Background()
	store.EnsureTrial(ctx, 55, time.Now().AddDate(0, 0, 14))
	store.SetProviderCustomer(ctx, 55, repository.ProviderStripe, "cus_55")
	store.ApplyProviderState(ctx, 55, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: "cus_55", ProviderSubscriptionID: "sub_55", Plan: "STARTER", Status: repository.SubscriptionActive})

	payload := []byte(`{"id":"evt_200","type":"customer.subscription.updated","data":{"object":{
		"id":"sub_55","status":"past_due","cancel_at_period_end":false,"customer":"cus_55","metadata":{},
		"items":{"data":[{"current_period_start":1700000000,"current_period_end":1702592000,"price":{"id":"price_starter_month","recurring":{"interval":"month"}}}]}}}}`)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatal(err)
	}
	sub, _ := store.GetForOrganization(ctx, 55)
	if sub.Status != repository.SubscriptionPastDue {
		t.Fatalf("%+v", sub)
	}
}

func TestWebhookUnattributableEventIsAcknowledgedNotErrored(t *testing.T) {
	svc, store, _ := newTestService(t, sampleCatalog)
	payload := []byte(`{"id":"evt_300","type":"customer.subscription.updated","data":{"object":{
		"id":"sub_ghost","status":"active","customer":"cus_ghost","metadata":{},
		"items":{"data":[{"current_period_start":1,"current_period_end":2,"price":{"id":"price_x","recurring":{"interval":"month"}}}]}}}}`)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(context.Background(), payload, sig); err != nil {
		t.Fatal(err)
	}
	if len(store.byOrg) != 0 {
		t.Fatal("nothing should have been created or changed")
	}
}

func TestWebhookSubscriptionDeletedForcesCanceled(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 60, "PRO", "price_pro_month")
	sub, _ := store.GetForOrganization(ctx, 60)

	payload := []byte(`{"id":"evt_400","type":"customer.subscription.deleted","data":{"object":{
		"id":"` + sub.ProviderSubscriptionID + `","status":"canceled","customer":"` + sub.ProviderCustomerID + `","metadata":{"champion_organization_id":"60"},
		"items":{"data":[{"current_period_start":1,"current_period_end":2,"price":{"id":"price_pro_month","recurring":{"interval":"month"}}}]}}}}`)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetForOrganization(ctx, 60)
	if after.Status != repository.SubscriptionCanceled {
		t.Fatalf("%+v", after)
	}
}

func TestWebhookInvoicePaymentFailedMarksPastDueWithoutDeletingAnything(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 61, "PRO", "price_pro_month")
	sub, _ := store.GetForOrganization(ctx, 61)
	provider.Put(SubscriptionState{SubscriptionID: sub.ProviderSubscriptionID, CustomerID: sub.ProviderCustomerID, PriceID: sub.ProviderPriceID, StripeStatus: "past_due", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})

	payload := []byte(`{"id":"evt_500","type":"invoice.payment_failed","data":{"object":{"id":"in_1","customer":"` + sub.ProviderCustomerID + `","subscription":"` + sub.ProviderSubscriptionID + `"}}}`)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetForOrganization(ctx, 61)
	if after.Status != repository.SubscriptionPastDue {
		t.Fatalf("%+v", after)
	}
	if after.Plan != "PRO" {
		t.Fatal("a payment failure must not change the plan or delete anything")
	}
}

// --- reconciliation ---------------------------------------------------------------------------------

func TestReconcilePullsStripeStateIntoChampion(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	sub := activeOrg(t, ctx, svc, store, provider, 70, "STARTER", "price_starter_month")
	// Simulate a missed webhook: Stripe moved on, Champion didn't hear about it.
	provider.Put(SubscriptionState{SubscriptionID: sub.ProviderSubscriptionID, CustomerID: sub.ProviderCustomerID, PriceID: "price_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	sum, err := svc.Reconcile(ctx, 70)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Plan != "PRO" {
		t.Fatalf("reconcile must pick up the plan Stripe now reports: %+v", sum)
	}
}

func TestReconcileHandlesASubscriptionStripeNoLongerKnows(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 71, "PRO", "price_pro_month")
	// Nothing seeded for this id in the fake provider's map => ErrSubscriptionNotFound.
	sub, _ := store.GetForOrganization(ctx, 71)
	delete(provider.subs, sub.ProviderSubscriptionID)
	sum, err := svc.Reconcile(ctx, 71)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != repository.SubscriptionCanceled {
		t.Fatalf("%+v", sum)
	}
}

func TestReconcileWithNoStripeSubscriptionYetIsANoop(t *testing.T) {
	svc, store, _ := newTestService(t, sampleCatalog)
	ctx := context.Background()
	store.EnsureTrial(ctx, 72, time.Now().AddDate(0, 0, 14))
	sum, err := svc.Reconcile(ctx, 72)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Status != repository.SubscriptionTrial {
		t.Fatalf("%+v", sum)
	}
}

func TestReconcileUnknownOrganization(t *testing.T) {
	svc, _, _ := newTestService(t, sampleCatalog)
	if _, err := svc.Reconcile(context.Background(), 99999); !errors.Is(err, ErrNoActiveSubscription) {
		t.Fatalf("%v", err)
	}
}
