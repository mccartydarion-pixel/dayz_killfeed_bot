package billing

import (
	"encoding/json"
	"fmt"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// VerifyWebhookEvent checks the Stripe-Signature header against payload using secret and returns
// the parsed event on success (docs/BILLING.md "Webhook verification"). It rejects anything the
// signature doesn't cover - the payload is never trusted before this succeeds. Needs no Provider:
// signature verification is pure HMAC, so it runs identically for the fake and real provider,
// which is what lets webhook tests exercise the real cryptography without any network call.
//
// IgnoreAPIVersionMismatch is set deliberately: a Stripe *account's* configured API version (which
// stamps every event's top-level api_version) is independent of the stripe-go SDK version this
// repository happens to build against, and this package only reads a handful of stable,
// long-unchanged JSON fields (ids, status, price, period, metadata - see webhookSubscription /
// webhookCheckoutSession / webhookInvoice) rather than the full generated stripe.Subscription type.
// Requiring an exact version match would make every webhook fail the moment stripe-go is upgraded
// ahead of the Stripe dashboard's pinned version, for no safety benefit (the HMAC signature is the
// actual trust boundary, not the version string).
func VerifyWebhookEvent(payload []byte, sigHeader, secret string) (stripe.Event, error) {
	if secret == "" {
		return stripe.Event{}, fmt.Errorf("webhook signing secret is not configured")
	}
	return webhook.ConstructEventWithOptions(payload, sigHeader, secret, webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
}

// Relevant Stripe event types (docs/BILLING.md "Webhook events"). Anything else is acknowledged
// (200, so Stripe stops retrying it) and otherwise ignored.
const (
	EventCheckoutCompleted    = "checkout.session.completed"
	EventSubscriptionCreated  = "customer.subscription.created"
	EventSubscriptionUpdated  = "customer.subscription.updated"
	EventSubscriptionDeleted  = "customer.subscription.deleted"
	EventInvoicePaid          = "invoice.paid"
	EventInvoicePaymentFailed = "invoice.payment_failed"
)

// webhookSubscription is the minimal shape read out of a customer.subscription.* event's object -
// deliberately not the full generated stripe.Subscription (this repository only reads price,
// status, period and metadata from a webhook; everything richer goes through GetSubscription,
// which the reconciler already calls with the real SDK type).
type webhookSubscription struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	CanceledAt        int64             `json:"canceled_at"`
	TrialEnd          int64             `json:"trial_end"`
	Customer          jsonID            `json:"customer"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			CurrentPeriodStart int64 `json:"current_period_start"`
			CurrentPeriodEnd   int64 `json:"current_period_end"`
			Price              struct {
				ID        string `json:"id"`
				Recurring struct {
					Interval string `json:"interval"`
				} `json:"recurring"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// jsonID accepts either a bare Stripe id string or an expanded object with an "id" field - a
// webhook's "customer"/"subscription" fields are usually the former, but Stripe's event payloads
// are not guaranteed to never expand them, so this never panics or silently reads "" from a JSON
// object where a string was expected.
type jsonID string

func (j *jsonID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*j = jsonID(s)
		return nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*j = jsonID(obj.ID)
	return nil
}

// webhookCheckoutSession is the minimal shape read out of a checkout.session.completed event.
type webhookCheckoutSession struct {
	ID                string            `json:"id"`
	Mode              string            `json:"mode"`
	ClientReferenceID string            `json:"client_reference_id"`
	Customer          jsonID            `json:"customer"`
	Subscription      jsonID            `json:"subscription"`
	Metadata          map[string]string `json:"metadata"`
}

// webhookInvoice is the minimal shape read out of an invoice.* event.
type webhookInvoice struct {
	ID           string `json:"id"`
	Customer     jsonID `json:"customer"`
	Subscription jsonID `json:"subscription"`
}

// ParsedEvent is one webhook event, decoded into exactly the fields Champion's reconciliation
// needs, with everything else (payment card data, line item detail, tax) left out.
type ParsedEvent struct {
	ID      string
	Type    string
	Session *webhookCheckoutSession
	Sub     *webhookSubscription
	Invoice *webhookInvoice
}

// ParseEvent decodes a verified stripe.Event's object into the typed shape for its event type.
// An event type this package doesn't handle yet returns a ParsedEvent with every pointer nil - the
// caller acknowledges it (200) and does nothing further, per docs/BILLING.md "ignore unsupported
// events safely".
func ParseEvent(e stripe.Event) (ParsedEvent, error) {
	out := ParsedEvent{ID: e.ID, Type: string(e.Type)}
	switch out.Type {
	case EventCheckoutCompleted:
		var s webhookCheckoutSession
		if err := json.Unmarshal(e.Data.Raw, &s); err != nil {
			return out, fmt.Errorf("parse checkout.session.completed: %w", err)
		}
		out.Session = &s
	case EventSubscriptionCreated, EventSubscriptionUpdated, EventSubscriptionDeleted:
		var s webhookSubscription
		if err := json.Unmarshal(e.Data.Raw, &s); err != nil {
			return out, fmt.Errorf("parse %s: %w", out.Type, err)
		}
		out.Sub = &s
	case EventInvoicePaid, EventInvoicePaymentFailed:
		var inv webhookInvoice
		if err := json.Unmarshal(e.Data.Raw, &inv); err != nil {
			return out, fmt.Errorf("parse %s: %w", out.Type, err)
		}
		out.Invoice = &inv
	}
	return out, nil
}

// State converts a parsed subscription event's fields into the same ProviderState-shaped values
// GetSubscription/normalizeSubscription would produce, so webhook handling and explicit
// reconciliation share one downstream code path (Service.applySubscriptionState).
func (s *webhookSubscription) state() *SubscriptionState {
	out := &SubscriptionState{SubscriptionID: s.ID, CustomerID: string(s.Customer), StripeStatus: s.Status, CancelAtPeriodEnd: s.CancelAtPeriodEnd}
	if len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		out.CurrentPeriodStart = time.Unix(item.CurrentPeriodStart, 0).UTC()
		out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		out.PriceID = item.Price.ID
		out.StripeInterval = item.Price.Recurring.Interval
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
