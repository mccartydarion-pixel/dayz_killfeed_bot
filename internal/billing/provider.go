package billing

import (
	"context"
	"errors"
	"time"
)

// Provider isolates every Stripe SDK call behind an interface (docs/BILLING.md "Billing provider"):
// no other package in this repository imports the Stripe SDK. StripeProvider (stripe_provider.go)
// is the real implementation; FakeProvider (fake_provider.go) is an in-memory one used by every
// test in this repository and by any deployment with no STRIPE_SECRET_KEY configured, so "no
// Stripe configured" is a supported, safe state rather than a nil-pointer risk.
type Provider interface {
	// EnsureCustomer returns a Stripe customer id for the organization: existingCustomerID if it is
	// non-empty (reused as-is, never re-created), otherwise a new Stripe customer is created with
	// email/name/metadata attached for support visibility.
	EnsureCustomer(ctx context.Context, in CustomerInput) (customerID string, err error)

	// CreateCheckoutSession creates a hosted Stripe Checkout Session in subscription mode and
	// returns its URL. trialDays > 0 asks Stripe to start the subscription in trial.
	CreateCheckoutSession(ctx context.Context, in CheckoutInput) (*CheckoutSession, error)

	// CreatePortalSession creates a Stripe Customer Portal session and returns its URL.
	CreatePortalSession(ctx context.Context, customerID, returnURL string) (*PortalSession, error)

	// GetSubscription fetches the current state of a Stripe subscription, normalized.
	GetSubscription(ctx context.Context, subscriptionID string) (*SubscriptionState, error)

	// ChangeSubscriptionPrice swaps a subscription's single price (plan change), applying Stripe's
	// default proration immediately (docs/BILLING.md "Upgrade" / "Downgrade": Phase 1 applies both
	// directions immediately - see that section for why "downgrade at renewal" was not built yet).
	ChangeSubscriptionPrice(ctx context.Context, subscriptionID, newPriceID string) (*SubscriptionState, error)

	// SetCancelAtPeriodEnd flips the subscription's cancel-at-period-end flag: true schedules
	// cancellation for the end of the current period (the subscription stays fully active until
	// then); false reactivates a subscription that was scheduled to cancel.
	SetCancelAtPeriodEnd(ctx context.Context, subscriptionID string, cancel bool) (*SubscriptionState, error)
}

// CustomerInput is what EnsureCustomer needs to create a new Stripe customer (ignored when an
// existing id is reused).
type CustomerInput struct {
	ExistingCustomerID string
	OrganizationID     int64
	Email              string // the acting user's Discord-verified email if the website has one; may be ""
	Name               string // the organization's display name
}

// CheckoutInput is everything CreateCheckoutSession needs. SuccessURL/CancelURL are already fully
// resolved, allowlisted absolute URLs (internal/billing.SafeReturnURL) - this layer does not
// re-validate them.
type CheckoutInput struct {
	CustomerID      string
	PriceID         string
	OrganizationID  int64
	PlanKey         string
	BillingInterval string
	SuccessURL      string
	CancelURL       string
	TrialDays       int // 0 = no trial on this Checkout Session
}

type CheckoutSession struct {
	ID  string
	URL string
}

type PortalSession struct {
	URL string
}

// SubscriptionState is Stripe's subscription state, normalized to exactly what Champion persists -
// never the raw Stripe object.
type SubscriptionState struct {
	Metadata map[string]string // server-authored product kind and C.A.S.E. binding, never client supplied
	SubscriptionID     string
	CustomerID         string
	PriceID            string
	StripeStatus       string // Stripe's raw status string; map with MapStatus before storing
	StripeInterval     string // "month"/"year"; map with MapInterval before storing
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	TrialStart         *time.Time
	TrialEnd           *time.Time
	CancelAtPeriodEnd  bool
	CanceledAt         *time.Time
}

var (
	// ErrProviderNotConfigured is returned by every Provider method on a deployment with no
	// STRIPE_SECRET_KEY (and no fake provider substituted, which is only ever a test's choice) -
	// distinct from a genuine Stripe API failure so handlers can report BILLING_UNAVAILABLE.
	ErrProviderNotConfigured = errors.New("billing provider is not configured")
	// ErrSubscriptionNotFound is returned when Stripe reports the subscription id does not exist.
	ErrSubscriptionNotFound = errors.New("stripe subscription not found")
)
