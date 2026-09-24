package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

const tierCatalog = `[
 {"key":"LOW","name":"Low","limits":{"installations":1},"monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_low"},"trialDays":7},
 {"key":"MEDIUM","name":"Medium","limits":{"installations":2},"monthly":{"amountCents":999,"currency":"usd","stripePriceId":"price_medium"},"trialDays":7},
 {"key":"HIGH","name":"High","monthly":{"amountCents":1499,"currency":"usd","stripePriceId":"price_high"},"trialDays":7},
 {"key":"LEGACY","isPublic":false,"monthly":{"amountCents":100,"currency":"usd","stripePriceId":"price_legacy"}}
]`

func mustCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(tierCatalog)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStartTrialNeedsNoPaymentAndNoStripe(t *testing.T) {
	store := newFakeStore() // StartTrial takes no Provider: it cannot reach Stripe
	now := time.Now()
	st, err := StartTrial(context.Background(), store, mustCatalog(t), 1, 100, "medium", now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Started || st.TrialStatus != TrialActive || st.SelectedPlan != "MEDIUM" || st.DaysRemaining != TrialDays || st.BillingRequired || st.PaymentMethodRequired {
		t.Fatalf("%+v", st)
	}
	if st.TrialEndsAt == nil || !st.TrialEndsAt.Equal(now.AddDate(0, 0, TrialDays)) || st.TrialStartedAt == nil || !st.TrialStartedAt.Equal(now) {
		t.Fatalf("trial window: %+v", st)
	}
	sub, _ := store.GetForOrganization(context.Background(), 1)
	if sub.Plan != repository.SubscriptionTrial || sub.ProviderCustomerID != "" || sub.IntendedPlan != "MEDIUM" {
		t.Fatalf("the intended plan is a preference, never the paid plan: %+v", sub)
	}
}

func TestStartTrialIsIdempotentAndNeverRestartsTheClock(t *testing.T) {
	store, cat, ctx := newFakeStore(), mustCatalog(t), context.Background()
	start := time.Now()
	first, err := StartTrial(ctx, store, cat, 1, 100, "LOW", start)
	if err != nil {
		t.Fatal(err)
	}
	// Five days later, picking another tier: same trial, same end, new selection.
	later := start.AddDate(0, 0, 5)
	again, err := StartTrial(ctx, store, cat, 1, 100, "HIGH", later)
	if err != nil {
		t.Fatal(err)
	}
	if again.Started || !again.TrialEndsAt.Equal(*first.TrialEndsAt) || again.DaysRemaining != 9 || again.SelectedPlan != "HIGH" {
		t.Fatalf("%+v", again)
	}
	// Expired: reported as such, never restarted.
	expired, err := StartTrial(ctx, store, cat, 1, 100, "", start.AddDate(0, 0, 15))
	if err != nil {
		t.Fatal(err)
	}
	if expired.Started || expired.TrialStatus != TrialExpired || !expired.BillingRequired || !expired.TrialEndsAt.Equal(*first.TrialEndsAt) {
		t.Fatalf("an expired trial cannot restart: %+v", expired)
	}
}

func TestOneTrialPerAccountAcrossOrganizations(t *testing.T) {
	store, cat, ctx := newFakeStore(), mustCatalog(t), context.Background()
	if st, _ := StartTrial(ctx, store, cat, 1, 100, "", time.Now()); !st.Started {
		t.Fatal("first trial")
	}
	st, err := StartTrial(ctx, store, cat, 2, 100, "LOW", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.Started || st.TrialStatus != TrialNotEligible || !st.BillingRequired || st.TrialEndsAt != nil || st.SelectedPlan != "LOW" {
		t.Fatalf("a second organization must not get a second trial: %+v", st)
	}
	// Another user cannot trial an organization that was already trialed either.
	if st, _ := StartTrial(ctx, store, cat, 1, 200, "", time.Now()); st.Started {
		t.Fatal("organization 1 was already trialed")
	}
}

func TestStartTrialNeverOverwritesAPaidSubscription(t *testing.T) {
	store, cat, ctx := newFakeStore(), mustCatalog(t), context.Background()
	store.EnsureTrial(ctx, 5, time.Now())
	if _, err := store.ApplyProviderState(ctx, 5, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: "cus_5", ProviderSubscriptionID: "sub_5", Plan: "MEDIUM", Status: repository.SubscriptionActive}); err != nil {
		t.Fatal(err)
	}
	st, err := StartTrial(ctx, store, cat, 5, 300, "HIGH", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := store.GetForOrganization(ctx, 5)
	if st.Started || st.TrialStatus != TrialConverted || st.BillingRequired || sub.Plan != "MEDIUM" || sub.Status != repository.SubscriptionActive || sub.IntendedPlan != "" {
		t.Fatalf("state %+v, row %+v", st, sub)
	}
}

func TestStartTrialRejectsUnknownOrPrivatePlans(t *testing.T) {
	store, cat := newFakeStore(), mustCatalog(t)
	for _, key := range []string{"NOPE", "LEGACY", "TRIAL"} {
		if _, err := StartTrial(context.Background(), store, cat, 1, 100, key, time.Now()); !errors.Is(err, ErrUnknownPlan) {
			t.Errorf("%s: %v", key, err)
		}
	}
	if sub, _ := store.GetForOrganization(context.Background(), 1); sub != nil {
		t.Fatal("a rejected plan must not start the trial")
	}
}

func TestStateOfBillingRequired(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	for name, c := range map[string]struct {
		sub      *repository.Subscription
		status   string
		required bool
	}{
		"no row":              {nil, TrialNotStarted, true},
		"running trial":       {&repository.Subscription{Status: "TRIAL", TrialEndsAt: &future}, TrialActive, false},
		"expired trial":       {&repository.Subscription{Status: "TRIAL", TrialEndsAt: &past}, TrialExpired, true},
		"not eligible":        {&repository.Subscription{Status: "INACTIVE"}, TrialNotEligible, true},
		"active":              {&repository.Subscription{Status: "ACTIVE", ProviderSubscriptionID: "sub"}, TrialConverted, false},
		"past due":            {&repository.Subscription{Status: "PAST_DUE", ProviderSubscriptionID: "sub"}, TrialConverted, false},
		"legacy stripe trial": {&repository.Subscription{Status: "TRIAL", ProviderSubscriptionID: "sub", TrialEndsAt: &past}, TrialConverted, false},
		"canceled":            {&repository.Subscription{Status: "CANCELED", ProviderSubscriptionID: "sub"}, TrialConverted, true},
		"suspended":           {&repository.Subscription{Status: "SUSPENDED", ProviderSubscriptionID: "sub"}, TrialConverted, true},
	} {
		st := StateOf(c.sub, now)
		if st.TrialStatus != c.status || st.BillingRequired != c.required {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

func TestInstallationLimit(t *testing.T) {
	cat := mustCatalog(t)
	for name, c := range map[string]struct {
		sub  *repository.Subscription
		want int
	}{
		"trial":                     {&repository.Subscription{Status: "TRIAL"}, TrialInstallations},
		"no row":                    {nil, TrialInstallations},
		"low":                       {&repository.Subscription{Status: "ACTIVE", Plan: "LOW", ProviderSubscriptionID: "s"}, 1},
		"medium":                    {&repository.Subscription{Status: "ACTIVE", Plan: "MEDIUM", ProviderSubscriptionID: "s"}, 2},
		"high, no limit":            {&repository.Subscription{Status: "ACTIVE", Plan: "HIGH", ProviderSubscriptionID: "s"}, DefaultPlanInstallations},
		"unknown paid plan":         {&repository.Subscription{Status: "ACTIVE", Plan: "GONE", ProviderSubscriptionID: "s"}, DefaultPlanInstallations},
		"intended plan is not paid": {&repository.Subscription{Status: "TRIAL", IntendedPlan: "MEDIUM"}, TrialInstallations},
	} {
		if got := InstallationLimit(c.sub, cat); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if InstallationLimit(&repository.Subscription{Plan: "MEDIUM", ProviderSubscriptionID: "s"}, nil) != DefaultPlanInstallations {
		t.Error("nil catalog")
	}
}

func TestCatalogIgnoresConfiguredStripeTrialDays(t *testing.T) {
	for _, p := range mustCatalog(t).PublicPlans() {
		if p.TrialDays != 0 {
			t.Fatalf("%s: trialDays %d", p.Key, p.TrialDays)
		}
	}
}
