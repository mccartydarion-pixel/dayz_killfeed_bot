package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestServicePaused(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	in := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	const org = 987654321

	cases := []struct {
		name string
		sub  *repository.Subscription
		want bool
	}{
		{"no subscription row", nil, true},
		{"trial running", &repository.Subscription{Status: repository.SubscriptionTrial, TrialEndsAt: in(48 * time.Hour)}, false},
		{"trial expired", &repository.Subscription{Status: repository.SubscriptionTrial, TrialEndsAt: ago(time.Hour)}, true},
		{"paid and active", &repository.Subscription{Status: repository.SubscriptionActive, ProviderSubscriptionID: "sub_1"}, false},
		{"payment failed within grace", &repository.Subscription{Status: repository.SubscriptionPastDue, ProviderSubscriptionID: "sub_1", CurrentPeriodStart: ago(2 * 24 * time.Hour)}, false},
		{"payment failed past grace", &repository.Subscription{Status: repository.SubscriptionPastDue, ProviderSubscriptionID: "sub_1", CurrentPeriodStart: ago(4 * 24 * time.Hour)}, true},
		{"canceled", &repository.Subscription{Status: repository.SubscriptionCanceled, ProviderSubscriptionID: "sub_1"}, true},
		{"suspended", &repository.Subscription{Status: repository.SubscriptionSuspended, ProviderSubscriptionID: "sub_1"}, true},
		{"owner grant open-ended", &repository.Subscription{Status: repository.SubscriptionActive}, false},
		{"owner grant lapsed", &repository.Subscription{Status: repository.SubscriptionActive, OwnerGrantUntil: ago(time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := servicePaused(org, tc.sub, now); got != tc.want {
				t.Fatalf("servicePaused = %v, want %v", got, tc.want)
			}
		})
	}
}
