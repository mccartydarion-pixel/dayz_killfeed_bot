package billing

import (
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// CaseInvoiceLine is one invoice line normalized from Stripe: enough to decide which C.A.S.E. tier a
// paid invoice covered and until when. Amounts are in the smallest currency unit.
type CaseInvoiceLine struct {
	Amount           int64
	PriceID          string
	SubscriptionID   string
	SubscriptionItem bool // parent.type == subscription_item_details
	PeriodStart      int64
	PeriodEnd        int64
}

// CaseInvoice is a Stripe invoice normalized for C.A.S.E. coverage. It never carries card data.
type CaseInvoice struct {
	ID, CustomerID, SubscriptionID, Status, Currency string
	AmountPaid                                       int64
	Lines                                            []CaseInvoiceLine
}

// CasePaymentState is the live refund/dispute state of the payment behind one invoice, read from
// Stripe at processing time (never trusted from an event payload, so delivery order cannot matter).
type CasePaymentState struct {
	AmountCaptured  int64
	AmountRefunded  int64
	DisputeStatuses []string
}

// caseCoverageFromLines applies the one coverage rule to invoice lines: the subscription-item line of
// THIS subscription whose price is a configured C.A.S.E. price with the latest period end, and on the
// same end the higher tier. paidOnly skips credit/zero lines, so a credit can never read as coverage.
func caseCoverageFromLines(lines []CaseInvoiceLine, subscriptionID string, tierOf func(string) casebilling.Tier, paidOnly bool) (start, end time.Time, tier casebilling.Tier) {
	if subscriptionID == "" || tierOf == nil {
		return time.Time{}, time.Time{}, ""
	}
	for _, l := range lines {
		t := tierOf(l.PriceID)
		if t == "" || !l.SubscriptionItem || l.SubscriptionID != subscriptionID ||
			l.PeriodStart <= 0 || l.PeriodEnd <= l.PeriodStart || (paidOnly && l.Amount <= 0) {
			continue
		}
		e := time.Unix(l.PeriodEnd, 0).UTC()
		if e.After(end) || (e.Equal(end) && casebilling.Rank(t) > casebilling.Rank(tier)) {
			start, end, tier = time.Unix(l.PeriodStart, 0).UTC(), e, t
		}
	}
	return start, end, tier
}

// Coverage statuses (migration 0064). Good standing grants the invoice's tier until its period end.
const (
	CoveragePaid              = "PAID"
	CoveragePartiallyRefunded = "PARTIALLY_REFUNDED"
	CoverageRefunded          = "REFUNDED"
	CoverageDisputed          = "DISPUTED"
	CoverageDisputeWon        = "DISPUTE_WON"
	CoverageDisputeLost       = "DISPUTE_LOST"
	CoverageVoided            = "VOIDED"
)

// caseInvoiceCoverageStatus is the approved Phase 6.26B policy, computed from live Stripe state:
//   - a lost dispute revokes permanently;
//   - a full refund (refunded >= amount paid) revokes;
//   - an open dispute (any non-final status, including unknown future ones) suspends;
//   - a partial refund keeps coverage but is recorded and flagged;
//   - a won/closed-warning/prevented dispute restores;
//   - otherwise the invoice stays PAID.
func caseInvoiceCoverageStatus(amountPaid int64, st CasePaymentState) string {
	lost, open, won := false, false, false
	for _, d := range st.DisputeStatuses {
		switch d {
		case "lost":
			lost = true
		case "won", "warning_closed", "prevented":
			won = true
		default: // needs_response, under_review, warning_needs_response, warning_under_review, unknown
			open = true
		}
	}
	switch {
	case lost:
		return CoverageDisputeLost
	case amountPaid > 0 && st.AmountRefunded >= amountPaid:
		return CoverageRefunded
	case open:
		return CoverageDisputed
	case st.AmountRefunded > 0:
		return CoveragePartiallyRefunded
	case won:
		return CoverageDisputeWon
	default:
		return CoveragePaid
	}
}

// caseDisputeSummary is the dispute status recorded for audit: the most severe observed.
func caseDisputeSummary(statuses []string) string {
	best, rank := "", -1
	order := map[string]int{"won": 1, "warning_closed": 1, "prevented": 1, "lost": 4}
	for _, s := range statuses {
		r, ok := order[s]
		if !ok {
			r = 3 // open or unknown
		}
		if r > rank {
			best, rank = s, r
		}
	}
	return best
}
