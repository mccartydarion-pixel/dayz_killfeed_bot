package billing

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// handleCaseReversal reconciles a refund, dispute or void against C.A.S.E. invoice coverage.
//
// Attribution is a verified chain, never a customer id alone: event -> PaymentIntent ->
// InvoicePayment -> live invoice -> the invoice's subscription -> an add-on row that already stores
// that exact subscription id, and the invoice's customer must equal the add-on's customer. The
// resulting status is computed from live Stripe state (charges and disputes of the PaymentIntent), so
// duplicate and out-of-order deliveries converge on the same answer.
//
// handled=false means "not a C.A.S.E. invoice": the caller continues with base handling (today:
// acknowledged and ignored). An error makes Stripe retry.
func (s *Service) handleCaseReversal(ctx context.Context, e ParsedEvent) (bool, error) {
	if s.caseStore == nil || s.provider == nil {
		return false, nil
	}
	p, ok := s.provider.(CaseProvider)
	if !ok {
		return false, nil
	}
	var invoiceID, paymentIntentID string
	switch {
	case e.Payment != nil:
		paymentIntentID = string(e.Payment.PaymentIntent)
		if paymentIntentID == "" {
			return false, nil
		}
		id, err := p.InvoiceForPaymentIntent(ctx, paymentIntentID)
		if err != nil {
			return false, err
		}
		if id == "" {
			return false, nil // not an invoice payment
		}
		invoiceID = id
	case e.Invoice != nil && (e.Type == EventInvoiceVoided || e.Type == EventInvoiceUncollectible):
		invoiceID = e.Invoice.ID
	default:
		return false, nil
	}
	inv, err := p.GetCaseInvoice(ctx, invoiceID)
	if err != nil {
		return false, err
	}
	if inv == nil || inv.SubscriptionID == "" {
		return false, nil
	}
	row, err := s.caseStore.GetByCaseSubscriptionID(ctx, inv.SubscriptionID)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, nil // not a stored C.A.S.E. subscription: base or unrelated, never guessed
	}
	if inv.CustomerID == "" || inv.CustomerID != row.ProviderCustomerID {
		slog.Warn("component=case_billing", "event", "case_reversal_unattributed", "reason", "customer_mismatch",
			"stripe_event_id", e.ID, "type", e.Type, "organization_id", row.OrganizationID, "installation_id", row.InstallationID)
		return true, nil // a C.A.S.E. subscription id with a foreign customer: acknowledge, change nothing
	}

	in := repository.CaseReversalState{
		EventID: e.ID, EventType: e.Type, AddonID: row.ID, OrganizationID: row.OrganizationID,
		InstallationID: row.InstallationID, SubscriptionID: inv.SubscriptionID, CustomerID: row.ProviderCustomerID,
		InvoiceID: inv.ID, PaymentIntentID: paymentIntentID,
	}
	if e.Payment != nil {
		st, err := p.CasePaymentState(ctx, paymentIntentID)
		if err != nil {
			return false, err
		}
		in.Status = caseInvoiceCoverageStatus(inv.AmountPaid, *st)
		in.AmountRefunded = st.AmountRefunded
		in.DisputeStatus = caseDisputeSummary(st.DisputeStatuses)
	} else {
		in.Status = CoverageVoided // an unpaid invoice never granted coverage; a paid one cannot be voided
	}
	// Adopt the invoice into the ledger if it paid for coverage but predates it, so the reversal
	// revokes exactly that invoice's coverage.
	if inv.Status == "paid" {
		if start, end, tier := caseCoverageFromLines(inv.Lines, inv.SubscriptionID, s.caseTierForPrice, true); !end.IsZero() {
			in.Adopt = &repository.CaseInvoiceCoverage{InvoiceID: inv.ID, SubscriptionID: inv.SubscriptionID,
				PaymentIntentID: paymentIntentID, Tier: string(tier), Currency: inv.Currency,
				PeriodStart: start, PeriodEnd: end, AmountPaid: inv.AmountPaid}
		}
	}
	if in.Backfill, err = s.caseCoverageBackfill(ctx, inv.SubscriptionID); err != nil {
		return false, err
	}
	applied, err := s.caseStore.ApplyCaseInvoiceReversal(ctx, in)
	if err != nil {
		return false, fmt.Errorf("apply case reversal: %w", err)
	}
	event := "case_coverage_reversal_applied"
	if !applied {
		event = "case_webhook_duplicate"
	}
	slog.Info("component=case_billing", "event", event, "outcome", map[bool]string{true: "applied", false: "no_op"}[applied],
		"organization_id", row.OrganizationID, "installation_id", row.InstallationID, "coverage_status", in.Status,
		"stripe_event_id", e.ID, "type", e.Type)
	return true, nil
}
