package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// subscriptionEvent builds a signed-ready customer.subscription.* payload whose embedded snapshot
// says status/priceID - which the handler must NOT trust over Stripe's current state.
func subscriptionEvent(eventID string, eventType, subscriptionID, customerID, status, priceID string, orgID int64) []byte {
	return []byte(`{"id":"` + eventID + `","type":"` + eventType + `","data":{"object":{
		"id":"` + subscriptionID + `","status":"` + status + `","customer":"` + customerID + `","metadata":{"champion_organization_id":"` + itoa(orgID) + `"},
		"items":{"data":[{"current_period_start":1,"current_period_end":2,"price":{"id":"` + priceID + `","recurring":{"interval":"month"}}}]}}}}`)
}

func putStripeState(provider *FakeProvider, sub *repository.Subscription, status, priceID string) {
	provider.Put(SubscriptionState{SubscriptionID: sub.ProviderSubscriptionID, CustomerID: sub.ProviderCustomerID, PriceID: priceID, StripeStatus: status,
		StripeInterval: "month", CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
}

// A failed handling must not leave the processed-event marker behind: the 5xx makes Stripe retry
// the SAME event id, and that retry has to be processed, not acknowledged as a duplicate.
func TestWebhookHandlingFailureReleasesEventSoStripeRetryIsProcessed(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	sub := activeOrg(t, ctx, svc, store, provider, 70, "PRO", "price_pro_month")
	putStripeState(provider, sub, "past_due", "price_pro_month")
	payload := subscriptionEvent("evt_retry", EventSubscriptionUpdated, sub.ProviderSubscriptionID, sub.ProviderCustomerID, "past_due", "price_pro_month", 70)
	sig := sign(t, "whsec_test", payload)

	store.applyErr = errors.New("database unavailable")
	if err := svc.HandleWebhook(ctx, payload, sig); err == nil {
		t.Fatal("a failed state write must surface as an error (5xx) so Stripe retries")
	}
	if store.events[[2]string{repository.ProviderStripe, "evt_retry"}] {
		t.Fatal("a failed event must not stay recorded as processed")
	}
	if got, _ := store.GetForOrganization(ctx, 70); got.Status != repository.SubscriptionActive {
		t.Fatalf("nothing should have been applied: %+v", got)
	}

	store.applyErr = nil
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatalf("Stripe's retry must be processed: %v", err)
	}
	if got, _ := store.GetForOrganization(ctx, 70); got.Status != repository.SubscriptionPastDue {
		t.Fatalf("the retried event was not applied: %+v", got)
	}
	if !store.events[[2]string{repository.ProviderStripe, "evt_retry"}] {
		t.Fatal("a successfully processed event must be recorded")
	}
}

// The same guarantee when the failure is the Stripe re-fetch rather than the database.
func TestWebhookStripeFetchFailureReleasesEvent(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	sub := activeOrg(t, ctx, svc, store, provider, 71, "PRO", "price_pro_month")
	payload := subscriptionEvent("evt_fetch_fail", EventSubscriptionUpdated, "sub_unknown_to_fake", sub.ProviderCustomerID, "active", "price_pro_month", 71)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(ctx, payload, sig); err == nil {
		t.Fatal("a failed Stripe fetch must surface as an error")
	}
	if store.events[[2]string{repository.ProviderStripe, "evt_fetch_fail"}] {
		t.Fatal("a failed event must not stay recorded as processed")
	}
}

// A redelivery of an event that WAS processed successfully is acknowledged without reprocessing.
func TestWebhookSuccessfulSubscriptionEventDuplicateIsNotReprocessed(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	sub := activeOrg(t, ctx, svc, store, provider, 72, "PRO", "price_pro_month")
	putStripeState(provider, sub, "past_due", "price_pro_month")
	payload := subscriptionEvent("evt_once", EventSubscriptionUpdated, sub.ProviderSubscriptionID, sub.ProviderCustomerID, "past_due", "price_pro_month", 72)
	sig := sign(t, "whsec_test", payload)
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatal(err)
	}
	// Move the stored row on; a reprocessed duplicate would overwrite it with Stripe's past_due.
	if _, err := store.ApplyProviderState(ctx, 72, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: sub.ProviderCustomerID,
		ProviderSubscriptionID: sub.ProviderSubscriptionID, Plan: "STARTER", Status: repository.SubscriptionActive}); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleWebhook(ctx, payload, sig); err != nil {
		t.Fatalf("a duplicate must be acknowledged: %v", err)
	}
	if got, _ := store.GetForOrganization(ctx, 72); got.Plan != "STARTER" || got.Status != repository.SubscriptionActive {
		t.Fatalf("a duplicate of a processed event must not be reprocessed: %+v", got)
	}
}

// Stripe does not guarantee delivery order. An older customer.subscription.updated delivered after
// the deletion must not resurrect the subscription, and an older plan snapshot must not undo a
// newer plan change: the handler applies Stripe's current state, not the event's snapshot.
func TestWebhookOutOfOrderSubscriptionEventDoesNotOverwriteNewerState(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()

	// Plan change: newer event (PRO) arrives first, the older STARTER snapshot arrives second.
	sub := activeOrg(t, ctx, svc, store, provider, 73, "STARTER", "price_starter_month")
	putStripeState(provider, sub, "active", "price_pro_month")
	newer := subscriptionEvent("evt_plan_new", EventSubscriptionUpdated, sub.ProviderSubscriptionID, sub.ProviderCustomerID, "active", "price_pro_month", 73)
	older := subscriptionEvent("evt_plan_old", EventSubscriptionUpdated, sub.ProviderSubscriptionID, sub.ProviderCustomerID, "active", "price_starter_month", 73)
	for _, p := range [][]byte{newer, older} {
		if err := svc.HandleWebhook(ctx, p, sign(t, "whsec_test", p)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := store.GetForOrganization(ctx, 73); got.Plan != "PRO" || got.ProviderPriceID != "price_pro_month" {
		t.Fatalf("stale plan snapshot overwrote the newer plan: %+v", got)
	}

	// Cancellation: deleted arrives first, then a delayed older "active" update.
	sub2 := activeOrg(t, ctx, svc, store, provider, 74, "PRO", "price_pro_month")
	putStripeState(provider, sub2, "canceled", "price_pro_month")
	deleted := subscriptionEvent("evt_del", EventSubscriptionDeleted, sub2.ProviderSubscriptionID, sub2.ProviderCustomerID, "canceled", "price_pro_month", 74)
	stale := subscriptionEvent("evt_stale_active", EventSubscriptionUpdated, sub2.ProviderSubscriptionID, sub2.ProviderCustomerID, "active", "price_pro_month", 74)
	for _, p := range [][]byte{deleted, stale} {
		if err := svc.HandleWebhook(ctx, p, sign(t, "whsec_test", p)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := store.GetForOrganization(ctx, 74); got.Status != repository.SubscriptionCanceled {
		t.Fatalf("a stale update resurrected a deleted subscription: %+v", got)
	}
}

// A deletion for a subscription Stripe no longer returns still cancels (the event is the fact).
func TestWebhookSubscriptionDeletedForUnknownStripeSubscriptionStillCancels(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	sub := activeOrg(t, ctx, svc, store, provider, 75, "PRO", "price_pro_month")
	// Stored row references a subscription the fake provider no longer knows.
	if _, err := store.ApplyProviderState(ctx, 75, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: sub.ProviderCustomerID,
		ProviderSubscriptionID: "sub_purged_75", Plan: "PRO", Status: repository.SubscriptionActive}); err != nil {
		t.Fatal(err)
	}
	p := subscriptionEvent("evt_del_purged", EventSubscriptionDeleted, "sub_purged_75", sub.ProviderCustomerID, "canceled", "price_pro_month", 75)
	if err := svc.HandleWebhook(ctx, p, sign(t, "whsec_test", p)); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetForOrganization(ctx, 75); got.Status != repository.SubscriptionCanceled {
		t.Fatalf("%+v", got)
	}
}
