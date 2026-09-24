package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	billingportalsession "github.com/stripe/stripe-go/v82/billingportal/session"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/subscription"
)

// StripeProvider is the real Provider, isolating every stripe-go call so nothing else in this
// repository imports the Stripe SDK (docs/BILLING.md). It sets the classic package-level
// stripe.Key once at construction (the same pattern stripe-go's own examples use outside of the
// deprecated client.API wrapper) and passes ctx through stripe.Params.Context on every call, so a
// request's timeout/cancellation reaches the HTTP call to Stripe.
type StripeProvider struct{}

// NewStripeProvider sets the process-wide Stripe API key and returns a Provider backed by the real
// Stripe API. secretKey must be non-empty (the caller decides whether to construct this at all -
// see Service, which falls back to a nil provider otherwise).
func NewStripeProvider(secretKey string) *StripeProvider {
	stripe.Key = secretKey
	return &StripeProvider{}
}

func withCtx(ctx context.Context) *stripe.Params { return &stripe.Params{Context: ctx} }

func stripeNotFound(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && se.HTTPStatusCode == http.StatusNotFound
}

func (p *StripeProvider) EnsureCustomer(ctx context.Context, in CustomerInput) (string, error) {
	if in.ExistingCustomerID != "" {
		return in.ExistingCustomerID, nil
	}
	params := &stripe.CustomerParams{Params: *withCtx(ctx), Name: stripe.String(in.Name)}
	if in.Email != "" {
		params.Email = stripe.String(in.Email)
	}
	params.AddMetadata("champion_organization_id", strconv.FormatInt(in.OrganizationID, 10))
	c, err := customer.New(params)
	if err != nil {
		return "", fmt.Errorf("create stripe customer: %w", err)
	}
	return c.ID, nil
}

func (p *StripeProvider) CreateCheckoutSession(ctx context.Context, in CheckoutInput) (*CheckoutSession, error) {
	params := &stripe.CheckoutSessionParams{
		Params:            *withCtx(ctx),
		Mode:              stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:          stripe.String(in.CustomerID),
		ClientReferenceID: stripe.String(strconv.FormatInt(in.OrganizationID, 10)),
		SuccessURL:        stripe.String(in.SuccessURL),
		CancelURL:         stripe.String(in.CancelURL),
		LineItems:         []*stripe.CheckoutSessionLineItemParams{{Price: stripe.String(in.PriceID), Quantity: stripe.Int64(1)}},
		SubscriptionData: &stripe.CheckoutSessionSubscriptionDataParams{
			Metadata: map[string]string{
				"champion_organization_id":  strconv.FormatInt(in.OrganizationID, 10),
				"champion_plan_key":         in.PlanKey,
				"champion_billing_interval": in.BillingInterval,
			},
		},
	}
	params.AddMetadata("champion_organization_id", strconv.FormatInt(in.OrganizationID, 10))
	params.AddMetadata("champion_plan_key", in.PlanKey)
	params.AddMetadata("champion_billing_interval", in.BillingInterval)
	if in.TrialDays > 0 {
		params.SubscriptionData.TrialPeriodDays = stripe.Int64(int64(in.TrialDays))
	}
	s, err := checkoutsession.New(params)
	if err != nil {
		return nil, fmt.Errorf("create stripe checkout session: %w", err)
	}
	return &CheckoutSession{ID: s.ID, URL: s.URL}, nil
}

func (p *StripeProvider) CreatePortalSession(ctx context.Context, customerID, returnURL string) (*PortalSession, error) {
	s, err := billingportalsession.New(&stripe.BillingPortalSessionParams{Params: *withCtx(ctx), Customer: stripe.String(customerID), ReturnURL: stripe.String(returnURL)})
	if err != nil {
		return nil, fmt.Errorf("create stripe portal session: %w", err)
	}
	return &PortalSession{URL: s.URL}, nil
}

func (p *StripeProvider) GetSubscription(ctx context.Context, subscriptionID string) (*SubscriptionState, error) {
	s, err := subscription.Get(subscriptionID, &stripe.SubscriptionParams{Params: *withCtx(ctx)})
	if err != nil {
		if stripeNotFound(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("get stripe subscription: %w", err)
	}
	return normalizeSubscription(s), nil
}

func (p *StripeProvider) ChangeSubscriptionPrice(ctx context.Context, subscriptionID, newPriceID string) (*SubscriptionState, error) {
	cur, err := subscription.Get(subscriptionID, &stripe.SubscriptionParams{Params: *withCtx(ctx)})
	if err != nil {
		if stripeNotFound(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("get stripe subscription: %w", err)
	}
	if cur.Items == nil || len(cur.Items.Data) == 0 {
		return nil, fmt.Errorf("stripe subscription %s has no items to change", subscriptionID)
	}
	itemID := cur.Items.Data[0].ID
	// Default proration ("create_prorations"): the price change is effective immediately and the
	// difference is prorated onto the next invoice - the same behaviour for an upgrade or a
	// downgrade (docs/BILLING.md "Upgrade" / "Downgrade").
	s, err := subscription.Update(subscriptionID, &stripe.SubscriptionParams{
		Params: *withCtx(ctx),
		Items:  []*stripe.SubscriptionItemsParams{{ID: stripe.String(itemID), Price: stripe.String(newPriceID)}},
	})
	if err != nil {
		return nil, fmt.Errorf("update stripe subscription price: %w", err)
	}
	return normalizeSubscription(s), nil
}

func (p *StripeProvider) SetCancelAtPeriodEnd(ctx context.Context, subscriptionID string, cancel bool) (*SubscriptionState, error) {
	s, err := subscription.Update(subscriptionID, &stripe.SubscriptionParams{Params: *withCtx(ctx), CancelAtPeriodEnd: stripe.Bool(cancel)})
	if err != nil {
		if stripeNotFound(err) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, fmt.Errorf("update stripe subscription cancel_at_period_end: %w", err)
	}
	return normalizeSubscription(s), nil
}

// normalizeSubscription converts a raw *stripe.Subscription into Champion's SubscriptionState.
// current_period_start/end live on the subscription's first item as of Stripe API version
// 2025-08-27 ("basil"), not on the subscription itself - see stripe-go v82's subscriptionitem.go.
func normalizeSubscription(s *stripe.Subscription) *SubscriptionState {
	out := &SubscriptionState{SubscriptionID: s.ID, StripeStatus: string(s.Status), CancelAtPeriodEnd: s.CancelAtPeriodEnd, Metadata: s.Metadata}
	if s.Customer != nil {
		out.CustomerID = s.Customer.ID
	}
	if s.Items != nil && len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		out.CurrentPeriodStart = time.Unix(item.CurrentPeriodStart, 0).UTC()
		out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		if item.Price != nil {
			out.PriceID = item.Price.ID
			if item.Price.Recurring != nil {
				out.StripeInterval = string(item.Price.Recurring.Interval)
			}
		}
	}
	if s.TrialEnd > 0 {
		t := time.Unix(s.TrialEnd, 0).UTC()
		out.TrialEnd = &t
	}
	if s.CanceledAt > 0 {
		t := time.Unix(s.CanceledAt, 0).UTC()
		out.CanceledAt = &t
	}
	return out
}

var _ Provider = (*StripeProvider)(nil)
