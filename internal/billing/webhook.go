package billing

import (
	"encoding/json"
	"fmt"
	"time"

	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
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

	// Payment reversals (Phase 6.26B). Handled only for C.A.S.E. invoices; base billing ignores them.
	EventChargeRefunded          = "charge.refunded"
	EventChargeRefundUpdated     = "charge.refund.updated"
	EventDisputeCreated          = "charge.dispute.created"
	EventDisputeUpdated          = "charge.dispute.updated"
	EventDisputeClosed           = "charge.dispute.closed"
	EventDisputeFundsReinstated  = "charge.dispute.funds_reinstated"
	EventInvoiceVoided           = "invoice.voided"
	EventInvoiceUncollectible    = "invoice.marked_uncollectible"
)

// isCaseReversalEvent reports the event types reconciled against C.A.S.E. invoice coverage.
func isCaseReversalEvent(t string) bool {
	switch t {
	case EventChargeRefunded, EventChargeRefundUpdated, EventDisputeCreated, EventDisputeUpdated,
		EventDisputeClosed, EventDisputeFundsReinstated, EventInvoiceVoided, EventInvoiceUncollectible:
		return true
	}
	return false
}

// webhookPaymentRef is the minimal shape of a charge, refund or dispute object: only its id and the
// PaymentIntent it belongs to. Amounts and statuses are always re-read live from Stripe.
type webhookPaymentRef struct {
	ID            string `json:"id"`
	PaymentIntent jsonID `json:"payment_intent"`
}

// webhookSubscription is the minimal shape read out of a customer.subscription.* event's object -
// deliberately not the full generated stripe.Subscription (this repository only reads price,
// status, period and metadata from a webhook; everything richer goes through GetSubscription,
// which the reconciler already calls with the real SDK type).
type webhookSubscription struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	CanceledAt        int64             `json:"canceled_at"`
	TrialStart        int64             `json:"trial_start"`
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

// webhookInvoice is the minimal shape read out of an invoice.* event. Amount/currency/status/
// payment_intent are stable, top-level Invoice fields; period_start/period_end come from the
// first line item's own period (an Invoice has no single top-level period - each line does),
// mirroring webhookSubscription's own items.data[0] pattern above.
type webhookInvoice struct {
	ID            string `json:"id"`
	Customer      jsonID `json:"customer"`
	Subscription  jsonID `json:"subscription"`
	// Stripe copies the subscription's metadata (Champion's champion_organization_id) onto the invoice.
	Parent struct { SubscriptionDetails struct { Subscription jsonID `json:"subscription"`; Metadata map[string]string `json:"metadata"` } `json:"subscription_details"` } `json:"parent"`
	Status        string `json:"status"`
	AmountPaid    int64  `json:"amount_paid"`
	AmountDue     int64  `json:"amount_due"`
	Currency      string `json:"currency"`
	PaymentIntent jsonID `json:"payment_intent"`
	Lines         struct {
		Data []struct {
			Amount int64 `json:"amount"` // negative for a proration credit line
			Price jsonID `json:"price"` // legacy Stripe invoice line
			Pricing struct {
				PriceDetails struct { Price jsonID `json:"price"` } `json:"price_details"`
			} `json:"pricing"`
			Parent struct {
				Type string `json:"type"`
				SubscriptionItemDetails struct { Subscription jsonID `json:"subscription"` } `json:"subscription_item_details"`
			} `json:"parent"`
			Period struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"period"`
		} `json:"data"`
	} `json:"lines"`
}

// period returns the invoice's billing period from its first line item, or zero times if the
// event carried no line items (never fabricated - a missing period stays nil downstream).
func (inv *webhookInvoice) period() (start, end time.Time) {
	if len(inv.Lines.Data) == 0 {
		return time.Time{}, time.Time{}
	}
	p := inv.Lines.Data[0].Period
	if p.Start > 0 {
		start = time.Unix(p.Start, 0).UTC()
	}
	if p.End > 0 {
		end = time.Unix(p.End, 0).UTC()
	}
	return start, end
}

// caseLine returns the subscription-item line of THIS subscription whose
// price is a configured C.A.S.E. price (tierOf != "") with the latest period
// end; on the same end the higher tier wins (an upgrade invoice holds the new
// tier's charge and the old tier's credit). paidOnly skips credit/zero lines,
// so a Pro credit on a downgrade can never be read as paid Pro coverage.
func (inv *webhookInvoice) caseLine(subscriptionID string, tierOf func(string) casebilling.Tier, paidOnly bool) (end time.Time, tier casebilling.Tier) {
	_, end, tier = caseCoverageFromLines(inv.caseLines(), subscriptionID, tierOf, paidOnly)
	return end, tier
}

// caseLines normalizes the event's invoice lines for caseCoverageFromLines (the one coverage rule).
func (inv *webhookInvoice) caseLines() []CaseInvoiceLine {
	if inv == nil {
		return nil
	}
	out := make([]CaseInvoiceLine, 0, len(inv.Lines.Data))
	for _, line := range inv.Lines.Data {
		price := string(line.Pricing.PriceDetails.Price)
		if price == "" {
			price = string(line.Price)
		}
		out = append(out, CaseInvoiceLine{Amount: line.Amount, PriceID: price,
			SubscriptionID: string(line.Parent.SubscriptionItemDetails.Subscription),
			SubscriptionItem: line.Parent.Type == "subscription_item_details",
			PeriodStart: line.Period.Start, PeriodEnd: line.Period.End})
	}
	return out
}

// ParsedEvent is one webhook event, decoded into exactly the fields Champion's reconciliation
// needs, with everything else (payment card data, line item detail, tax) left out.
type ParsedEvent struct {
	ID      string
	Type    string
	Session *webhookCheckoutSession
	Sub     *webhookSubscription
	Invoice *webhookInvoice
	Payment *webhookPaymentRef // charge.refunded, charge.refund.updated, charge.dispute.*
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
		if inv.Subscription == "" { inv.Subscription = inv.Parent.SubscriptionDetails.Subscription }
		out.Invoice = &inv
	case EventInvoiceVoided, EventInvoiceUncollectible:
		var inv webhookInvoice
		if err := json.Unmarshal(e.Data.Raw, &inv); err != nil {
			return out, fmt.Errorf("parse %s: %w", out.Type, err)
		}
		if inv.Subscription == "" { inv.Subscription = inv.Parent.SubscriptionDetails.Subscription }
		out.Invoice = &inv
	case EventChargeRefunded, EventChargeRefundUpdated, EventDisputeCreated, EventDisputeUpdated,
		EventDisputeClosed, EventDisputeFundsReinstated:
		var ref webhookPaymentRef
		if err := json.Unmarshal(e.Data.Raw, &ref); err != nil {
			return out, fmt.Errorf("parse %s: %w", out.Type, err)
		}
		out.Payment = &ref
	}
	return out, nil
}

// State converts a parsed subscription event's fields into the same ProviderState-shaped values
// GetSubscription/normalizeSubscription would produce, so webhook handling and explicit
// reconciliation share one downstream code path (Service.applySubscriptionState).
func (s *webhookSubscription) state() *SubscriptionState {
	out := &SubscriptionState{SubscriptionID: s.ID, CustomerID: string(s.Customer), StripeStatus: s.Status, CancelAtPeriodEnd: s.CancelAtPeriodEnd, Metadata: s.Metadata}
	if len(s.Items.Data) > 0 {
		item := s.Items.Data[0]
		out.CurrentPeriodStart = time.Unix(item.CurrentPeriodStart, 0).UTC()
		out.CurrentPeriodEnd = time.Unix(item.CurrentPeriodEnd, 0).UTC()
		out.PriceID = item.Price.ID
		out.StripeInterval = item.Price.Recurring.Interval
	}
	if s.TrialStart > 0 {
		t := time.Unix(s.TrialStart, 0).UTC()
		out.TrialStart = &t
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
