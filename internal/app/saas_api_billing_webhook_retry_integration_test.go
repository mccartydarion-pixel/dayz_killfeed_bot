//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// A webhook whose handling fails must not stay recorded in billing_webhook_events: Stripe's retry
// of the SAME event id has to be processed, while a duplicate of a successfully processed event is
// still acknowledged without reprocessing. Runs over the real route and PostgreSQL.
func TestBillingWebhookFailedEventIsReprocessedOnRetry(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	subscriptionID, customerID := uniqID("sub_retry"), uniqID("cus_retry")
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	activated := checkoutCompletedPayload(uniqID("evt_retry_checkout"), w.a1.OrgID, subscriptionID, customerID)
	w.expect(w.postWebhook(activated, signStripePayload(t, "whsec_test", activated)), http.StatusOK, "activate")

	// First delivery fails: the Service's Stripe client cannot see the subscription (a transient
	// Stripe outage, simulated with an empty fake), so the handler answers 5xx.
	cat, err := billing.LoadCatalog(billingTestCatalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	working := w.a.Billing
	w.a.Billing = billing.NewService(w.a.SaaSSubscriptions, cat, billing.NewFakeProvider(), billing.Options{WebhookSecret: "whsec_test"})
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_starter_month", StripeStatus: "past_due", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	updated := subscriptionEventPayload(uniqID("evt_retry_update"), "customer.subscription.updated", subscriptionID, customerID, "past_due", "price_test_starter_month", false, w.a1.OrgID)
	sig := signStripePayload(t, "whsec_test", updated)
	w.expect(w.postWebhook(updated, sig), http.StatusInternalServerError, "failed delivery must 5xx so Stripe retries")
	w.a.Billing = working
	if got, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID); got.Status != repository.SubscriptionActive || got.Plan != "PRO" {
		t.Fatalf("a failed delivery must not change anything: %+v", got)
	}

	// Stripe retries the same event id: it must be processed, not acked as a duplicate.
	w.expect(w.postWebhook(updated, sig), http.StatusOK, "retry")
	got, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID)
	if got.Status != repository.SubscriptionPastDue || got.Plan != "STARTER" {
		t.Fatalf("the retried event was lost: %+v", got)
	}

	// A further redelivery of the now successfully processed event is a no-op, even though
	// Stripe's state moved on.
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	w.expect(w.postWebhook(updated, sig), http.StatusOK, "duplicate of a processed event")
	if again, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID); again.Status != repository.SubscriptionPastDue || again.Plan != "STARTER" {
		t.Fatalf("a duplicate of a processed event was reprocessed: %+v", again)
	}
}

// Stripe does not deliver events in order: a delayed older customer.subscription.updated arriving
// after customer.subscription.deleted must not resurrect the subscription.
func TestBillingWebhookOutOfOrderSubscriptionEventKeepsNewerState(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	subscriptionID, customerID := uniqID("sub_order"), uniqID("cus_order")
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	activated := checkoutCompletedPayload(uniqID("evt_order_checkout"), w.a1.OrgID, subscriptionID, customerID)
	w.expect(w.postWebhook(activated, signStripePayload(t, "whsec_test", activated)), http.StatusOK, "activate")

	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_pro_month", StripeStatus: "canceled", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	stale := subscriptionEventPayload(uniqID("evt_order_old"), "customer.subscription.updated", subscriptionID, customerID, "active", "price_test_pro_month", false, w.a1.OrgID)
	deleted := subscriptionEventPayload(uniqID("evt_order_new"), "customer.subscription.deleted", subscriptionID, customerID, "canceled", "price_test_pro_month", false, w.a1.OrgID)
	w.expect(w.postWebhook(deleted, signStripePayload(t, "whsec_test", deleted)), http.StatusOK, "deleted (newer) first")
	w.expect(w.postWebhook(stale, signStripePayload(t, "whsec_test", stale)), http.StatusOK, "older update second")
	if got, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID); got.Status != repository.SubscriptionCanceled {
		t.Fatalf("a stale update overwrote the newer cancellation: %+v", got)
	}
}
