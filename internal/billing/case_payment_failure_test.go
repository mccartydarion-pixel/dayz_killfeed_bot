package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// Phase 6.24: a failed renewal leaves the add-on PAST_DUE (Stripe past_due, dunning continues).
// The customer must still be able to schedule cancellation - a failed payment may never strand a
// subscriber in further charge attempts - and doing so must not grant any access.
func TestPastDueCaseAddonCanBeCancelledWithoutGrantingAccess(t *testing.T) {
	h := newCaseTierHarness(t, casebilling.Pro, true)
	ctx := context.Background()
	h.store.row.Status = "PAST_DUE"
	sub, _ := h.provider.GetSubscription(ctx, "sub_case")
	sub.StripeStatus = "past_due"
	h.provider.Put(*sub)
	if got := h.access(t); len(got) != 0 {
		t.Fatalf("PAST_DUE must grant nothing: %v", got)
	}
	row, err := h.s.CaseSetCancellation(ctx, 10, 20, true)
	if err != nil {
		t.Fatalf("PAST_DUE add-on could not be cancelled: %v", err)
	}
	atStripe, _ := h.provider.GetSubscription(ctx, "sub_case")
	if !row.CancelAtPeriodEnd || !h.store.row.CancelAtPeriodEnd || !atStripe.CancelAtPeriodEnd {
		t.Fatalf("cancellation not recorded at Stripe and locally: row=%v stripe=%v", row.CancelAtPeriodEnd, atStripe.CancelAtPeriodEnd)
	}
	if got := h.access(t); len(got) != 0 {
		t.Fatalf("scheduling cancellation granted access: %v", got)
	}
	// Reactivating (undoing the scheduled cancel) is equally a flag change only.
	if _, err := h.s.CaseSetCancellation(ctx, 10, 20, false); err != nil {
		t.Fatalf("PAST_DUE add-on could not be reactivated: %v", err)
	}
	if got := h.access(t); len(got) != 0 {
		t.Fatalf("reactivation granted access while PAST_DUE: %v", got)
	}
	// A CANCELED add-on still cannot be managed.
	h.store.row.Status = "CANCELED"
	sub.StripeStatus = "canceled"
	h.provider.Put(*sub)
	if _, err := h.s.CaseSetCancellation(ctx, 10, 20, true); !errors.Is(err, ErrCaseNotManaged) {
		t.Fatalf("CANCELED add-on must stay unmanageable: %v", err)
	}
}
