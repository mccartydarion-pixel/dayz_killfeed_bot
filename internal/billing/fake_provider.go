package billing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// FakeProvider is an in-memory Provider used by every test in this repository (docs section 5:
// "no test should create real charges") and available for local development without Stripe
// credentials. It behaves like Stripe closely enough to exercise Champion's own logic (ids,
// idempotent customer reuse, cancel-at-period-end, price swaps) without any network call.
type FakeProvider struct {
	mu   sync.Mutex
	subs map[string]*SubscriptionState
	caseSessions map[string]*CaseCheckoutSessionState
	caseProration   int64
	caseDeclineNext bool
	caseInvoices    map[string]CaseInvoice
	caseInvoiceByPI map[string]string
	casePayments    map[string]CasePaymentState
	// Calls records every method invocation for tests that want to assert on call shape
	// (e.g. "checkout was created with the trial days we expected").
	Calls []FakeCall
}

type FakeCall struct {
	Method string
	Arg    any
}

func NewFakeProvider() *FakeProvider { return &FakeProvider{subs: map[string]*SubscriptionState{},caseSessions: map[string]*CaseCheckoutSessionState{}} }

// fakeIDSeq is process-global (not per-FakeProvider): a real Stripe id is globally unique, and an
// integration test's FakeProvider shares its database with every other test in the same run (no
// per-test isolation of the subscriptions table), so a per-instance counter would let two different
// tests' organizations collide on the same "cus_fake_1" and make GetByProviderCustomerID resolve to
// the wrong one.
var fakeIDSeq atomic.Int64

func (f *FakeProvider) next(prefix string) string {
	return fmt.Sprintf("%s_fake_%d_%d", prefix, time.Now().UnixNano(), fakeIDSeq.Add(1))
}

func (f *FakeProvider) EnsureCustomer(_ context.Context, in CustomerInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{"EnsureCustomer", in})
	if in.ExistingCustomerID != "" {
		return in.ExistingCustomerID, nil
	}
	return f.next("cus"), nil
}

func (f *FakeProvider) CreateCheckoutSession(_ context.Context, in CheckoutInput) (*CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{"CreateCheckoutSession", in})
	id := f.next("cs")
	// A real Checkout Session isn't "completed" until the buyer pays; the fake mirrors that by not
	// creating a subscription here - tests that want a completed checkout call CompleteCheckout.
	return &CheckoutSession{ID: id, URL: "https://checkout.stripe.example/test/" + id}, nil
}

// CompleteCheckout simulates the buyer finishing a Checkout Session: it creates the Stripe
// subscription CreateCheckoutSession would have produced, as GetSubscription would then see it.
// Tests use it to drive a realistic checkout.session.completed -> customer.subscription.created
// webhook pair without a browser.
func (f *FakeProvider) CompleteCheckout(customerID, priceID string, trialDays int) *SubscriptionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	s := &SubscriptionState{
		SubscriptionID: f.next("sub"), CustomerID: customerID, PriceID: priceID,
		StripeInterval: "month", CurrentPeriodStart: now, CurrentPeriodEnd: now.AddDate(0, 1, 0),
	}
	if trialDays > 0 {
		s.StripeStatus = "trialing"
		end := now.AddDate(0, 0, trialDays)
		s.TrialEnd = &end
		s.CurrentPeriodEnd = end
	} else {
		s.StripeStatus = "active"
	}
	f.subs[s.SubscriptionID] = s
	cp := *s
	return &cp
}

func (f *FakeProvider) CreatePortalSession(_ context.Context, customerID, returnURL string) (*PortalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{"CreatePortalSession", struct{ CustomerID, ReturnURL string }{customerID, returnURL}})
	return &PortalSession{URL: "https://billing.stripe.example/test/session/" + f.next("bps")}, nil
}

func (f *FakeProvider) GetSubscription(_ context.Context, subscriptionID string) (*SubscriptionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[subscriptionID]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	cp := *s
	return &cp, nil
}

func (f *FakeProvider) ChangeSubscriptionPrice(_ context.Context, subscriptionID, newPriceID string) (*SubscriptionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[subscriptionID]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	s.PriceID = newPriceID
	if s.StripeStatus == "trialing" {
		s.StripeStatus = "active" // a plan change ends a trial early, like real Stripe with default proration
		s.TrialEnd = nil
	}
	cp := *s
	return &cp, nil
}

func (f *FakeProvider) SetCancelAtPeriodEnd(_ context.Context, subscriptionID string, cancel bool) (*SubscriptionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subs[subscriptionID]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	s.CancelAtPeriodEnd = cancel
	if cancel {
		now := time.Now().UTC()
		s.CanceledAt = &now
	} else {
		s.CanceledAt = nil
	}
	cp := *s
	return &cp, nil
}

// Put seeds a subscription directly (tests that need GetSubscription/ChangeSubscriptionPrice to see
// a subscription they didn't create via CompleteCheckout, e.g. reconciliation tests).
func (f *FakeProvider) Put(s SubscriptionState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := s
	f.subs[s.SubscriptionID] = &cp
}

var _ Provider = (*FakeProvider)(nil)
