package billing

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// The approved Phase 6.26B policy, computed from live Stripe state only.
func TestCaseInvoiceCoverageStatusPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		paid int64
		st   CasePaymentState
		want string
	}{
		"paid":                               {499, CasePaymentState{AmountCaptured: 499}, CoveragePaid},
		"full refund revokes":                {499, CasePaymentState{AmountCaptured: 499, AmountRefunded: 499}, CoverageRefunded},
		"over-refund still full":             {499, CasePaymentState{AmountRefunded: 600}, CoverageRefunded},
		"partial refund kept, flagged":       {499, CasePaymentState{AmountRefunded: 100}, CoveragePartiallyRefunded},
		"open dispute suspends":              {499, CasePaymentState{DisputeStatuses: []string{"needs_response"}}, CoverageDisputed},
		"early warning suspends":             {499, CasePaymentState{DisputeStatuses: []string{"warning_needs_response"}}, CoverageDisputed},
		"under review suspends":              {499, CasePaymentState{DisputeStatuses: []string{"under_review"}}, CoverageDisputed},
		"unknown future status fails closed": {499, CasePaymentState{DisputeStatuses: []string{"some_new_status"}}, CoverageDisputed},
		"won restores":                       {499, CasePaymentState{DisputeStatuses: []string{"won"}}, CoverageDisputeWon},
		"warning closed restores":            {499, CasePaymentState{DisputeStatuses: []string{"warning_closed"}}, CoverageDisputeWon},
		"prevented restores":                 {499, CasePaymentState{DisputeStatuses: []string{"prevented"}}, CoverageDisputeWon},
		"lost revokes":                       {499, CasePaymentState{DisputeStatuses: []string{"lost"}}, CoverageDisputeLost},
		"lost beats a later won":             {499, CasePaymentState{DisputeStatuses: []string{"won", "lost"}}, CoverageDisputeLost},
		"lost beats full refund":             {499, CasePaymentState{AmountRefunded: 499, DisputeStatuses: []string{"lost"}}, CoverageDisputeLost},
		"full refund beats open dispute":     {499, CasePaymentState{AmountRefunded: 499, DisputeStatuses: []string{"needs_response"}}, CoverageRefunded},
		"open dispute beats partial":         {499, CasePaymentState{AmountRefunded: 100, DisputeStatuses: []string{"under_review"}}, CoverageDisputed},
		"partial beats won":                  {499, CasePaymentState{AmountRefunded: 100, DisputeStatuses: []string{"won"}}, CoveragePartiallyRefunded},
		"zero-amount invoice stays paid":     {0, CasePaymentState{}, CoveragePaid},
	} {
		if got := caseInvoiceCoverageStatus(tc.paid, tc.st); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestCaseCoverageFromLinesPicksThisSubscriptionsPaidCaseLine(t *testing.T) {
	tierOf := func(p string) casebilling.Tier {
		return map[string]casebilling.Tier{"price_watch": casebilling.Watch, "price_pro": casebilling.Pro}[p]
	}
	s, e := time.Now().Add(-time.Hour).Truncate(time.Second), time.Now().Add(720*time.Hour).Truncate(time.Second)
	line := func(amount int64, price, sub string, item bool, a, b time.Time) CaseInvoiceLine {
		return CaseInvoiceLine{Amount: amount, PriceID: price, SubscriptionID: sub, SubscriptionItem: item, PeriodStart: a.Unix(), PeriodEnd: b.Unix()}
	}
	// Upgrade proration: Watch credit + Pro charge on the same period -> Pro.
	st, en, tier := caseCoverageFromLines([]CaseInvoiceLine{line(-494, "price_watch", "sub_a", true, s, e), line(988, "price_pro", "sub_a", true, s, e)}, "sub_a", tierOf, true)
	if tier != casebilling.Pro || !en.Equal(e) || !st.Equal(s) {
		t.Fatalf("proration: %v %v %v", st, en, tier)
	}
	for name, lines := range map[string][]CaseInvoiceLine{
		"other subscription":      {line(499, "price_watch", "sub_other", true, s, e)},
		"unconfigured price":      {line(599, "price_base_low", "sub_a", true, s, e)},
		"not a subscription item": {line(499, "price_watch", "sub_a", false, s, e)},
		"credit only":             {line(-499, "price_watch", "sub_a", true, s, e)},
		"bad period":              {line(499, "price_watch", "sub_a", true, e, s)},
	} {
		if _, en, tier := caseCoverageFromLines(lines, "sub_a", tierOf, true); tier != "" || !en.IsZero() {
			t.Errorf("%s: produced coverage %v %v", name, en, tier)
		}
	}
	// A failed-invoice view (paidOnly=false) still sees the credit line's period.
	if _, en, _ := caseCoverageFromLines([]CaseInvoiceLine{line(-494, "price_watch", "sub_a", true, s, e)}, "sub_a", tierOf, false); !en.Equal(e) {
		t.Fatal("paidOnly=false must consider credit lines")
	}
}
