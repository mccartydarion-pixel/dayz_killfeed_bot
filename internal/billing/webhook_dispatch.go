package billing

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

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
	// Classify signed events BEFORE the legacy base-subscription dedupe and
	// reconciler: a C.A.S.E. subscription must never replace LOW/MEDIUM/HIGH.
	parsed, err := ParseEvent(event)
	if err != nil {
		slog.Warn("component=billing", "event", "billing_webhook_parse_failed", "stripe_event_id", event.ID, "type", string(event.Type), "err", err.Error())
		return nil
	}
	// Refunds, disputes and voids reach C.A.S.E. only through a verified invoice -> stored C.A.S.E.
	// subscription chain; anything else falls through to base handling (acknowledged, ignored).
	if isCaseReversalEvent(parsed.Type) {
		handled, err := s.handleCaseReversal(ctx, parsed)
		if err != nil {
			return fmt.Errorf("reconcile C.A.S.E. payment reversal: %w", err)
		}
		if handled {
			return nil
		}
	}
	isCase, err := s.classifyCaseEvent(ctx,parsed)
	if err != nil {return fmt.Errorf("classify Stripe subscription kind: %w",err)}
	if isCase {return s.applyCaseEvent(ctx,parsed)}
	// Base billing continues through its existing idempotency and event logic.
	inserted, err := s.store.RecordWebhookEventOnce(ctx, repository.ProviderStripe, event.ID, string(event.Type), nil)
	if err != nil {
		return fmt.Errorf("record webhook event: %w", err)
	}
	if !inserted {
		slog.Info("component=billing", "event", "billing_webhook_duplicate", "stripe_event_id", event.ID, "type", string(event.Type))
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
		// Every Champion Checkout Session carries client_reference_id; one without it was created
		// outside Champion (e.g. the Stripe Dashboard) and is never bound to an organization.
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type,
			"checkout_session_id", e.Session.ID, "reason", "missing or invalid client_reference_id (session not created by Champion)")
		return nil
	}
	// Verify the session's organization binding before trusting it: Champion's own metadata (when
	// present) must name the same organization, and Champion stores the organization's Stripe
	// customer before creating any session, so a different customer means a foreign session.
	if meta := strings.TrimSpace(e.Session.Metadata["champion_organization_id"]); meta != "" && meta != strconv.FormatInt(orgID, 10) {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type,
			"checkout_session_id", e.Session.ID, "reason", "client_reference_id conflicts with champion_organization_id")
		return nil
	}
	if row, err := s.store.GetForOrganization(ctx, orgID); err != nil {
		return fmt.Errorf("load organization subscription: %w", err)
	} else if row != nil && row.ProviderCustomerID != "" && e.Session.Customer != "" && row.ProviderCustomerID != string(e.Session.Customer) {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type,
			"checkout_session_id", e.Session.ID, "organization_id", orgID, "reason", "customer_mismatch")
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
	orgID, reason, err := s.resolveOrgID(ctx, e.Sub.Metadata["champion_organization_id"], string(e.Sub.Customer), e.Sub.ID)
	if err != nil {
		return err
	}
	if orgID == 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "stripe_subscription_id", e.Sub.ID, "reason", reason)
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
	orgID, reason, err := s.resolveOrgID(ctx, e.Sub.Metadata["champion_organization_id"], string(e.Sub.Customer), e.Sub.ID)
	if err != nil {
		return err
	}
	if orgID == 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "stripe_subscription_id", e.Sub.ID, "reason", reason)
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
// and a failed payment can change status: paid can end a past_due, failed can start one), persists a
// normalized billing_transactions row (Champion Access Model Phase 2, Part D - never fabricated,
// built only from what this webhook event itself carries) and, only for a failure, additionally
// logs auditEvent. Champion does not suspend or delete anything here (docs/BILLING.md "Payment
// failure handling" - the grace-period policy is a DECISION REQUIRED).
func (s *Service) onInvoice(ctx context.Context, e ParsedEvent, auditEvent string) error {
	if e.Invoice == nil || e.Invoice.Subscription == "" {
		return nil // a one-off invoice with no subscription - not this table's concern
	}
	orgID, reason, err := s.resolveOrgID(ctx, e.Invoice.Parent.SubscriptionDetails.Metadata["champion_organization_id"], string(e.Invoice.Customer), string(e.Invoice.Subscription))
	if err != nil {
		return err
	}
	if orgID == 0 {
		slog.Warn("component=billing", "event", "billing_webhook_unattributed", "stripe_event_id", e.ID, "type", e.Type, "stripe_subscription_id", string(e.Invoice.Subscription), "reason", reason)
		return nil
	}
	st, err := s.provider.GetSubscription(ctx, string(e.Invoice.Subscription))
	if err != nil {
		return fmt.Errorf("get stripe subscription after invoice event: %w", err)
	}
	if _, err := s.applyState(ctx, orgID, st, ""); err != nil {
		return fmt.Errorf("apply invoice event: %w", err)
	}
	if err := s.recordTransaction(ctx, orgID, e); err != nil {
		return fmt.Errorf("record billing transaction: %w", err)
	}
	if auditEvent != "" {
		slog.Warn("component=billing", "event", auditEvent, "organization_id", orgID, "stripe_event_id", e.ID)
	}
	return nil
}

// recordTransaction normalizes e.Invoice into one billing_transactions row. Status/amount come
// straight from the event: paid uses amount_paid, failed uses amount_due (paid is always 0 on a
// failed invoice) - never a value re-derived or guessed from subscription state.
func (s *Service) recordTransaction(ctx context.Context, orgID int64, e ParsedEvent) error {
	inv := e.Invoice
	status := repository.TransactionPaid
	amount := inv.AmountPaid
	var paidAt, failedAt *time.Time
	now := time.Now().UTC()
	if e.Type == EventInvoicePaymentFailed {
		status = repository.TransactionFailed
		amount = inv.AmountDue
		failedAt = &now
	} else {
		paidAt = &now
	}
	periodStart, periodEnd := inv.period()
	t := repository.BillingTransaction{
		OrganizationID: orgID, Provider: repository.ProviderStripe, ProviderInvoiceID: inv.ID,
		ProviderPaymentIntentID: string(inv.PaymentIntent), ProviderSubscriptionID: string(inv.Subscription),
		Status: status, AmountCents: amount, Currency: strings.ToLower(inv.Currency),
		PaidAt: paidAt, FailedAt: failedAt, StripeEventID: e.ID,
	}
	if !periodStart.IsZero() {
		t.PeriodStart = &periodStart
	}
	if !periodEnd.IsZero() {
		t.PeriodEnd = &periodEnd
	}
	return s.store.RecordBillingTransaction(ctx, t)
}

// resolveOrgID finds which organization a webhook event belongs to. Attribution requires a binding
// Champion itself authored: the champion_organization_id metadata Champion sets server-side on its
// Checkout Session's subscription, or the subscription id already stored on that organization's row.
// A Stripe customer id alone NEVER attributes an event: a subscription created outside Champion (for
// example from the Stripe Dashboard) on an organization's customer would otherwise overwrite that
// organization's base subscription. The customer id is only a consistency check - a stored customer
// that differs from the event's refuses the event. Returns 0 plus a reason (not an error) when the
// event cannot be attributed; the caller logs and acknowledges it.
func (s *Service) resolveOrgID(ctx context.Context, metaOrgID, customerID, subscriptionID string) (int64, string, error) {
	var bySub *repository.Subscription
	if subscriptionID != "" {
		row, err := s.store.GetByProviderSubscriptionID(ctx, repository.ProviderStripe, subscriptionID)
		if err != nil {
			return 0, "", fmt.Errorf("resolve organization by stripe subscription: %w", err)
		}
		bySub = row
	}
	metaID, err := strconv.ParseInt(strings.TrimSpace(metaOrgID), 10, 64)
	if err != nil || metaID <= 0 {
		metaID = 0
	}
	if bySub != nil && metaID != 0 && bySub.OrganizationID != metaID {
		return 0, "metadata_conflicts_with_stored_subscription", nil
	}
	row := bySub
	orgID := metaID
	if bySub != nil {
		orgID = bySub.OrganizationID
	}
	if orgID == 0 {
		return 0, "no_champion_binding", nil // customer-only matches are deliberately not used
	}
	if row == nil {
		if row, err = s.store.GetForOrganization(ctx, orgID); err != nil {
			return 0, "", fmt.Errorf("load organization subscription: %w", err)
		}
	}
	if row != nil && customerID != "" && row.ProviderCustomerID != "" && row.ProviderCustomerID != customerID {
		return 0, "customer_mismatch", nil
	}
	return orgID, "", nil
}
