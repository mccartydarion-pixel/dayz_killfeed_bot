package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// These tests exercise the concurrency paths docs/BILLING.md section 37 ("Plan change concurrency")
// and the task's "focused race tests" ask for: webhook processing, reconciliation and plan
// mutation/state updates, all racing against each other. Run with -race.

// TestConcurrentWebhookDeliveryIsRaceFree redelivers the SAME event concurrently (Stripe's own retry
// behaviour) and checks the fakeStore/Service data race detector finds nothing and exactly one
// delivery actually applied the state (RecordWebhookEventOnce's uniqueness is the real guard; this
// proves the code path around it is race-safe too, not just the SQL constraint in production).
func TestConcurrentWebhookDeliveryIsRaceFree(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	store.EnsureTrial(ctx, 900, time.Now().AddDate(0, 0, 14))
	provider.Put(SubscriptionState{SubscriptionID: "sub_race", CustomerID: "cus_race", PriceID: "price_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	payload := checkoutCompletedPayload("evt_race", 900, "sub_race")
	sig := sign(t, "whsec_test", payload)

	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.HandleWebhook(ctx, payload, sig)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("delivery %d: %v", i, err)
		}
	}
	if n := len(store.events); n != 1 {
		t.Fatalf("expected exactly one recorded event despite 20 concurrent deliveries, got %d", n)
	}
	sub, _ := store.GetForOrganization(ctx, 900)
	if sub.ProviderSubscriptionID != "sub_race" || sub.Status != repository.SubscriptionActive {
		t.Fatalf("%+v", sub)
	}
}

// TestConcurrentWebhooksForDifferentOrganizationsAreRaceFree processes unrelated events for several
// organizations at once - the common case in production (many customers' webhooks arriving together).
func TestConcurrentWebhooksForDifferentOrganizationsAreRaceFree(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	const n = 15
	var wg sync.WaitGroup
	for i := int64(1); i <= n; i++ {
		wg.Add(1)
		go func(orgID int64) {
			defer wg.Done()
			store.EnsureTrial(ctx, orgID, time.Now().AddDate(0, 0, 14))
			subID, custID := idFor(orgID, "sub"), idFor(orgID, "cus")
			provider.Put(SubscriptionState{SubscriptionID: subID, CustomerID: custID, PriceID: "price_starter_month", StripeStatus: "active", StripeInterval: "month",
				CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
			payload := checkoutCompletedPayload(idFor(orgID, "evt"), orgID, subID)
			if err := svc.HandleWebhook(ctx, payload, sign(t, "whsec_test", payload)); err != nil {
				t.Errorf("org %d: %v", orgID, err)
			}
		}(i)
	}
	wg.Wait()
	for i := int64(1); i <= n; i++ {
		sub, err := store.GetForOrganization(ctx, i)
		if err != nil || sub == nil || sub.ProviderSubscriptionID != idFor(i, "sub") {
			t.Errorf("org %d: %+v %v", i, sub, err)
		}
	}
}

func idFor(orgID int64, prefix string) string {
	return prefix + "_" + itoa(orgID)
}

// TestConcurrentReconcileAndChangePlanAreRaceFree races Reconcile against ChangePlan/Cancel on the
// SAME organization: the fakeStore's own locking (and, in production, the row lock implied by
// UPDATE ... WHERE organization_id=$1 against subscriptions' UNIQUE(organization_id)) must keep the
// final state consistent and the race detector silent.
func TestConcurrentReconcileAndChangePlanAreRaceFree(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	activeOrg(t, ctx, svc, store, provider, 901, "STARTER", "price_starter_month")
	sub, _ := store.GetForOrganization(ctx, 901)
	provider.Put(SubscriptionState{SubscriptionID: sub.ProviderSubscriptionID, CustomerID: sub.ProviderCustomerID, PriceID: "price_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_, _ = svc.Reconcile(ctx, 901)
			case 1:
				_, _ = svc.ChangePlan(ctx, 901, "PRO", "MONTHLY")
			default:
				_, _ = svc.Cancel(ctx, 901)
			}
		}(i)
	}
	wg.Wait()
	// No assertion on which one "won" (that's a legitimate race in real concurrent admin actions -
	// docs/BILLING.md section 37 says the provider/webhook reconciliation must SETTLE safely, not
	// that a particular caller's request wins) - only that the store ends in a self-consistent state
	// and, above all, that -race found nothing.
	final, err := store.GetForOrganization(ctx, 901)
	if err != nil || final == nil || final.ProviderSubscriptionID == "" {
		t.Fatalf("%+v %v", final, err)
	}
}

// TestConcurrentPlanChangesOnDifferentOrganizationsAreRaceFree is the "plan mutation/state updates"
// case from many different customers acting at once.
func TestConcurrentPlanChangesOnDifferentOrganizationsAreRaceFree(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	const n = 10
	for i := int64(1); i <= n; i++ {
		activeOrg(t, ctx, svc, store, provider, 2000+i, "STARTER", "price_starter_month")
	}
	var wg sync.WaitGroup
	for i := int64(1); i <= n; i++ {
		wg.Add(1)
		go func(orgID int64) {
			defer wg.Done()
			if _, err := svc.ChangePlan(ctx, orgID, "PRO", "MONTHLY"); err != nil {
				t.Errorf("org %d: %v", orgID, err)
			}
		}(2000 + i)
	}
	wg.Wait()
	for i := int64(1); i <= n; i++ {
		sub, _ := store.GetForOrganization(ctx, 2000+i)
		if sub.Plan != "PRO" {
			t.Errorf("org %d: %+v", 2000+i, sub)
		}
	}
}
