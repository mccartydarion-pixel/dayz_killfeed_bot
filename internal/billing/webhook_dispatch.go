package billing

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// HandleWebhook verifies, dedupes and reconciles one Stripe webhook delivery (docs/BILLING.md
// "Webhook endpoint" / "Webhook idempotency"). It is the only entry point the HTTP handler calls;
// the raw payload is never trusted before VerifyWebhookEvent succeeds. A nil error means "return
// 200" - including for a duplicate delivery or an event type this package doesn't act on, both of
// which are intentionally not errors (a 4xx/5xx here makes Stripe retry, which is wrong for
// "already handled" and wasteful for "will never be handled").
func (s *Service) HandleWebhook(ctx context.Context, payload []byte, sigHeader string) error {
	if s.webhookSecret == "" {
		return fmt.Errorf("%w: STRIPE_WEBHOOK_SECRET is not configured", ErrProviderNotConfigured)
	}
	event, err := VerifyWebhookEvent(payload, sigHeader, s.webhookSecret)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	// Dedupe BEFORE any processing: the (provider, event_id) UNIQUE constraint makes this race-safe
	// even if Stripe redelivers the same event to two concurrent requests. organization_id is filled
	// in below on a best-effort basis for the admin-visible log; it is never required for the dedupe
	// itself.
	inserted, err := s.store.RecordWebhookEventOnce(ctx, repository.ProviderStripe, event.ID, string(event.Type), nil)
	if err != nil {
		return fmt.Errorf("record webhook event: %w", err)
	}
	if !inserted {
		slog.Info("component=billing", "event", "billing_webhook_duplicate", "stripe_event_id", event.ID, "type", string(event.Type))
		return nil
	}
	parsed, err := ParseEvent(event)
	if err != nil {
		// The signature was valid but the payload for a type we do parse didn't match its expected
		// shape - log and acknowledge (200) rather than let Stripe retry a payload that will never
		// parse differently.
		slog.Warn("component=billing", "event", "billing_webhook_parse_failed", "stripe_event_id", event.ID, "type", string(event.Type), "err", err.Error())
		return nil
	}
	switch parsed.Type {
	case EventCheckoutCompleted:
		return s.onCheckoutCompleted(ctx, parsed)
	case EventSubscriptionCreated:
		return s.onSubscriptionEvent(ctx, parsed, "billing_subscription_activated")
	case EventSubscriptionUpdated:
		return s.onSubscriptionEvent(ctx, parsed, "billing_subscription_updated")
	case EventSubscriptionDeleted:
		return s.onSubscriptionDeleted(ctx, parsed)
	case EventInvoicePaid:
		return s.onInvoice(ctx, parsed, "")
	case EventInvoicePaymentFailed:
		return s.onInvoice(ctx, parsed, "billing_payment_failed")
	default:
		slog.Info("component=billing", "event", "billing_webhook_ignored", "stripe_event_id", event.ID, "type", string(event.Type))
		return nil
	}
}

func (s *Service) onCheckoutCompleted(ctx context.Context, e ParsedEvent) error {
	if e.Session == nil || e.Session.Mode != "subscription" || e.Session.Subscription == "" {
		return nil // a one-time-payment Checkout Session, or a subscription session with no
		// subscription id yet (shouldn't happen for mode=subscription) - nothing for billing to do
	}
	orgID, err := strconv.ParseInt(e.Session.ClientReferenceID, 10, 64)
	if err != nil || orgID <= 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "reason", "missing or invalid client_reference_id")
		return nil
	}
	// checkout.session.completed does not carry period/price detail - fetch the full subscription
	// once, exactly as an explicit Reconcile would, so the two code paths store identical shapes.
	st, err := s.provider.GetSubscription(ctx, string(e.Session.Subscription))
	if err != nil {
		return fmt.Errorf("get stripe subscription after checkout: %w", err)
	}
	if st.CustomerID == "" {
		st.CustomerID = string(e.Session.Customer)
	}
	planKey := e.Session.Metadata["champion_plan_key"]
	if _, err := s.applyState(ctx, orgID, st, planKey); err != nil {
		return fmt.Errorf("apply checkout completion: %w", err)
	}
	slog.Info("component=billing", "event", "billing_subscription_activated", "organization_id", orgID, "stripe_event_id", e.ID, "plan", planKey)
	return nil
}

func (s *Service) onSubscriptionEvent(ctx context.Context, e ParsedEvent, auditEvent string) error {
	if e.Sub == nil {
		return nil
	}
	orgID, err := s.resolveOrgID(ctx, e.Sub.Metadata["champion_organization_id"], string(e.Sub.Customer), e.Sub.ID)
	if err != nil {
		return err
	}
	if orgID == 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "stripe_subscription_id", e.Sub.ID)
		return nil
	}
	planKey := e.Sub.Metadata["champion_plan_key"]
	if _, err := s.applyState(ctx, orgID, e.Sub.state(), planKey); err != nil {
		return fmt.Errorf("apply subscription event: %w", err)
	}
	slog.Info("component=billing", "event", auditEvent, "organization_id", orgID, "stripe_event_id", e.ID)
	return nil
}

func (s *Service) onSubscriptionDeleted(ctx context.Context, e ParsedEvent) error {
	if e.Sub == nil {
		return nil
	}
	orgID, err := s.resolveOrgID(ctx, e.Sub.Metadata["champion_organization_id"], string(e.Sub.Customer), e.Sub.ID)
	if err != nil {
		return err
	}
	if orgID == 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "stripe_subscription_id", e.Sub.ID)
		return nil
	}
	st := e.Sub.state()
	st.StripeStatus = "canceled" // the event name IS the fact; trust it over whatever status string Stripe attached
	if _, err := s.applyState(ctx, orgID, st, ""); err != nil {
		return fmt.Errorf("apply subscription deletion: %w", err)
	}
	slog.Info("component=billing", "event", "billing_subscription_cancelled", "organization_id", orgID, "stripe_event_id", e.ID, "reason", "deleted_on_provider")
	return nil
}

// onInvoice reconciles the full subscription on any invoice event that names one (both a successful
// and a failed payment can change status: paid can end a past_due, failed can start one) and, only
// for a failure, additionally logs auditEvent. Champion does not suspend or delete anything here
// (docs/BILLING.md "Payment failure handling" - the grace-period policy is a DECISION REQUIRED).
func (s *Service) onInvoice(ctx context.Context, e ParsedEvent, auditEvent string) error {
	if e.Invoice == nil || e.Invoice.Subscription == "" {
		return nil // a one-off invoice with no subscription - not this table's concern
	}
	orgID, err := s.resolveOrgID(ctx, "", string(e.Invoice.Customer), string(e.Invoice.Subscription))
	if err != nil {
		return err
	}
	if orgID == 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "stripe_subscription_id", string(e.Invoice.Subscription))
		return nil
	}
	st, err := s.provider.GetSubscription(ctx, string(e.Invoice.Subscription))
	if err != nil {
		return fmt.Errorf("get stripe subscription after invoice event: %w", err)
	}
	if _, err := s.applyState(ctx, orgID, st, ""); err != nil {
		return fmt.Errorf("apply invoice event: %w", err)
	}
	if auditEvent != "" {
		slog.Warn("component=billing", "event", auditEvent, "organization_id", orgID, "stripe_event_id", e.ID)
	}
	return nil
}

// resolveOrgID finds which organization a webhook event belongs to: the metadata Champion itself
// set at checkout time first (cheapest, no extra query), then a lookup by Stripe customer id, then
// by Stripe subscription id. Returns 0 (not an error) when none resolve - the caller logs and acks
// rather than failing the whole delivery over one unattributable event.
func (s *Service) resolveOrgID(ctx context.Context, metaOrgID, customerID, subscriptionID string) (int64, error) {
	if id, err := strconv.ParseInt(metaOrgID, 10, 64); err == nil && id > 0 {
		return id, nil
	}
	if customerID != "" {
		sub, err := s.store.GetByProviderCustomerID(ctx, repository.ProviderStripe, customerID)
		if err != nil {
			return 0, fmt.Errorf("resolve organization by stripe customer: %w", err)
		}
		if sub != nil {
			return sub.OrganizationID, nil
		}
	}
	if subscriptionID != "" {
		sub, err := s.store.GetByProviderSubscriptionID(ctx, repository.ProviderStripe, subscriptionID)
		if err != nil {
			return 0, fmt.Errorf("resolve organization by stripe subscription: %w", err)
		}
		if sub != nil {
			return sub.OrganizationID, nil
		}
	}
	return 0, nil
}
