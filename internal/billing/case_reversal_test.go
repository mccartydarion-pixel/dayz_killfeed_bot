package billing

import (
	"context"
	"fmt"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// reversalHarness: installation 20's add-on (addon 8, sub_case, cus_case) with one paid Watch invoice
// in_case reachable only through PaymentIntent pi_case -> InvoicePayment.
func reversalHarness(t *testing.T) *caseTierHarness {
	t.Helper()
	h := newCaseTierHarness(t, "CASE_WATCH", true)
	h.store.row.CoverageBackfilled = true
	h.provider.PutCaseInvoice(CaseInvoice{ID: "in_case", CustomerID: "cus_case", SubscriptionID: "sub_case", Status: "paid", Currency: "usd", AmountPaid: 499,
		Lines: []CaseInvoiceLine{{Amount: 499, PriceID: "price_watch", SubscriptionID: "sub_case", SubscriptionItem: true,
			PeriodStart: h.now.Unix(), PeriodEnd: h.end.Unix()}}}, "pi_case")
	return h
}

func deliverSigned(t *testing.T, s *Service, payload string) {
	t.Helper()
	b := []byte(payload)
	if err := s.HandleWebhook(context.Background(), b, sign(t, "whsec_unit_case", b)); err != nil {
		t.Fatalf("webhook must be acknowledged: %v", err)
	}
}

func chargeEvent(id, typ, object, objectID, pi string) string {
	return fmt.Sprintf(`{"id":"%s","type":"%s","api_version":"2026-08-26.dahlia","data":{"object":{"id":"%s","object":"%s","payment_intent":"%s"}}}`,
		id, typ, objectID, object, pi)
}

func TestCaseRefundIsReconciledThroughTheVerifiedInvoiceChain(t *testing.T) {
	h := reversalHarness(t)
	h.provider.PutCasePaymentState("pi_case", CasePaymentState{AmountCaptured: 499, AmountRefunded: 499})
	deliverSigned(t, h.s, chargeEvent("evt_refund", EventChargeRefunded, "charge", "ch_case", "pi_case"))
	if len(h.store.reversals) != 1 {
		t.Fatalf("want one reversal, got %+v", h.store.reversals)
	}
	r := h.store.reversals[0]
	if r.Status != CoverageRefunded || r.AmountRefunded != 499 || r.AddonID != 8 || r.InvoiceID != "in_case" || r.SubscriptionID != "sub_case" ||
		r.CustomerID != "cus_case" || r.PaymentIntentID != "pi_case" || r.Adopt == nil || r.Adopt.Tier != "CASE_WATCH" || !r.Adopt.PeriodEnd.Equal(h.end) {
		t.Fatalf("reversal: %+v adopt %+v", r, r.Adopt)
	}
	if len(h.base.events) != 0 {
		t.Fatal("a C.A.S.E. reversal must not reach base billing")
	}
	// Duplicate delivery of the same event id is a no-op.
	deliverSigned(t, h.s, chargeEvent("evt_refund", EventChargeRefunded, "charge", "ch_case", "pi_case"))
	if len(h.store.reversals) != 1 {
		t.Fatal("duplicate refund event applied twice")
	}
}

func TestCasePartialRefundAndDisputeStatusesComeFromLiveState(t *testing.T) {
	h := reversalHarness(t)
	h.provider.PutCasePaymentState("pi_case", CasePaymentState{AmountRefunded: 100})
	deliverSigned(t, h.s, chargeEvent("evt_partial", EventChargeRefunded, "charge", "ch_case", "pi_case"))
	h.provider.PutCasePaymentState("pi_case", CasePaymentState{AmountRefunded: 100, DisputeStatuses: []string{"needs_response"}})
	deliverSigned(t, h.s, chargeEvent("evt_dispute_open", EventDisputeCreated, "dispute", "dp_case", "pi_case"))
	// Out of order: the closed(won) event arrives, then a stale created event. Both read live "won".
	h.provider.PutCasePaymentState("pi_case", CasePaymentState{DisputeStatuses: []string{"won"}})
	deliverSigned(t, h.s, chargeEvent("evt_dispute_won", EventDisputeClosed, "dispute", "dp_case", "pi_case"))
	deliverSigned(t, h.s, chargeEvent("evt_dispute_stale_created", EventDisputeCreated, "dispute", "dp_case", "pi_case"))
	got := []string{}
	for _, r := range h.store.reversals {
		got = append(got, r.Status)
	}
	want := []string{CoveragePartiallyRefunded, CoverageDisputed, CoverageDisputeWon, CoverageDisputeWon}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("statuses %v, want %v", got, want)
	}
	if h.store.reversals[1].DisputeStatus != "needs_response" || h.store.reversals[2].DisputeStatus != "won" {
		t.Fatalf("dispute audit status: %+v", h.store.reversals)
	}
}

// Never attribute by customer id alone: the customer's BASE subscription invoice (same customer) is
// not a C.A.S.E. invoice; a payment with no invoice is not either. Both fall through to base, which
// acknowledges and ignores them. A C.A.S.E. subscription id with a foreign customer changes nothing.
func TestCaseReversalRefusesAnythingButAVerifiedCaseInvoice(t *testing.T) {
	h := reversalHarness(t)
	h.provider.PutCaseInvoice(CaseInvoice{ID: "in_base", CustomerID: "cus_case", SubscriptionID: "sub_base", Status: "paid", AmountPaid: 599,
		Lines: []CaseInvoiceLine{{Amount: 599, PriceID: "price_base", SubscriptionID: "sub_base", SubscriptionItem: true, PeriodStart: h.now.Unix(), PeriodEnd: h.end.Unix()}}}, "pi_base")
	h.provider.PutCasePaymentState("pi_base", CasePaymentState{AmountRefunded: 599})
	deliverSigned(t, h.s, chargeEvent("evt_base_refund", EventChargeRefunded, "charge", "ch_base", "pi_base"))
	deliverSigned(t, h.s, chargeEvent("evt_no_invoice", EventChargeRefunded, "charge", "ch_x", "pi_without_invoice"))
	deliverSigned(t, h.s, chargeEvent("evt_no_pi", EventChargeRefunded, "charge", "ch_y", ""))
	h.provider.PutCaseInvoice(CaseInvoice{ID: "in_forged", CustomerID: "cus_other", SubscriptionID: "sub_case", Status: "paid", AmountPaid: 499,
		Lines: []CaseInvoiceLine{{Amount: 499, PriceID: "price_watch", SubscriptionID: "sub_case", SubscriptionItem: true, PeriodStart: h.now.Unix(), PeriodEnd: h.end.Unix()}}}, "pi_forged")
	h.provider.PutCasePaymentState("pi_forged", CasePaymentState{AmountRefunded: 499})
	deliverSigned(t, h.s, chargeEvent("evt_forged", EventChargeRefunded, "charge", "ch_f", "pi_forged"))
	if len(h.store.reversals) != 0 {
		t.Fatalf("unverified reversal reached C.A.S.E.: %+v", h.store.reversals)
	}
	for _, id := range []string{"evt_base_refund", "evt_no_invoice", "evt_no_pi"} {
		if !h.base.events[[2]string{repository.ProviderStripe, id}] {
			t.Errorf("%s should fall through to base handling (acknowledged, ignored)", id)
		}
	}
	if h.base.events[[2]string{repository.ProviderStripe, "evt_forged"}] {
		t.Error("a C.A.S.E. subscription with a foreign customer must be acknowledged, not handed to base")
	}
	if b := h.base.byOrg[10]; b.Plan != "LOW" || b.Status != repository.SubscriptionActive || b.ProviderSubscriptionID != "sub_base" {
		t.Fatalf("base subscription changed: %+v", b)
	}
}

func TestCaseVoidedInvoiceIsRecordedWithoutCoverage(t *testing.T) {
	h := reversalHarness(t)
	h.provider.PutCaseInvoice(CaseInvoice{ID: "in_open", CustomerID: "cus_case", SubscriptionID: "sub_case", Status: "void", AmountPaid: 0,
		Lines: []CaseInvoiceLine{{Amount: 988, PriceID: "price_pro", SubscriptionID: "sub_case", SubscriptionItem: true, PeriodStart: h.now.Unix(), PeriodEnd: h.end.Unix()}}}, "")
	deliverSigned(t, h.s, `{"id":"evt_void","type":"invoice.voided","data":{"object":{"id":"in_open","object":"invoice","status":"void","customer":"cus_case",
		"parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_case"}}}}}`)
	if len(h.store.reversals) != 1 || h.store.reversals[0].Status != CoverageVoided || h.store.reversals[0].Adopt != nil {
		t.Fatalf("void must be recorded without adopting coverage: %+v", h.store.reversals)
	}
}
