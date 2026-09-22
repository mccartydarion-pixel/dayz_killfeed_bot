package billing

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestMapStatus(t *testing.T) {
	cases := map[string]string{
		"trialing":           repository.SubscriptionTrial,
		"active":             repository.SubscriptionActive,
		"past_due":           repository.SubscriptionPastDue,
		"incomplete":         repository.SubscriptionPastDue,
		"incomplete_expired": repository.SubscriptionPastDue,
		"unpaid":             repository.SubscriptionPastDue,
		"canceled":           repository.SubscriptionCanceled,
		"paused":             repository.SubscriptionSuspended,
		"some_future_status": repository.SubscriptionPastDue, // fail toward "needs attention", never silently active
		"":                   repository.SubscriptionPastDue,
	}
	for stripeStatus, want := range cases {
		if got := MapStatus(stripeStatus); got != want {
			t.Errorf("%q: got %s want %s", stripeStatus, got, want)
		}
	}
}

func TestMapInterval(t *testing.T) {
	cases := map[string]string{
		"month": repository.BillingIntervalMonthly,
		"year":  repository.BillingIntervalYearly,
		"week":  "",
		"day":   "",
		"":      "",
	}
	for in, want := range cases {
		if got := MapInterval(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
