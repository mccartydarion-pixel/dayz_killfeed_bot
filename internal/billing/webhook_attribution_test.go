package billing

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Regression for the Phase 6.19 staging incident: a Checkout Session created OUTSIDE Champion
// (Stripe Dashboard: no client_reference_id, empty metadata, success_url https://stripe.com) was
// paid instead of Champion's own session. Its webhooks must stay unattributed, and a Stripe
// customer id must never on its own attach a subscription to an organization. Payload shapes are
// the ones Stripe API 2026-08-26.dahlia actually delivered (no top-level invoice.subscription).

// championOrg seeds an organization exactly as Champion's checkout leaves it: an existing row
// with the organization's Stripe customer saved before any Checkout Session was created.
func championOrg(t *testing.T, store *fakeStore, orgID int64, customerID string) {
	t.Helper()
	ctx := context.Background()
	store.EnsureTrial(ctx, orgID, time.Now().AddDate(0, 0, 14))
	if err := store.SetProviderCustomer(ctx, orgID, repository.ProviderStripe, customerID); err != nil {
		t.Fatal(err)
	}
}

func deliver(t *testing.T, svc *Service, payload string) {
	t.Helper()
	b := []byte(payload)
	if err := svc.HandleWebhook(context.Background(), b, sign(t, "whsec_test", b)); err != nil {
		t.Fatalf("webhook must be acknowledged, got %v", err)
	}
}

func snapshot(store *fakeStore, orgID int64) repository.Subscription {
	s, _ := store.GetForOrganization(context.Background(), orgID)
	return *s
}

func foreignSubscriptionEvent(eventID, typ, subID, customer string) string {
	return fmt.Sprintf(`{"id":"%s","type":"%s","api_version":"2026-08-26.dahlia","data":{"object":{
		"id":"%s","object":"subscription","status":"active","cancel_at_period_end":false,"customer":"%s","metadata":{},
		"items":{"data":[{"current_period_start":1790366656,"current_period_end":1792958656,"price":{"id":"price_starter_month","recurring":{"interval":"month"}}}]}}}}`,
		eventID, typ, subID, customer)
}

func dahliaInvoicePaid(eventID, invoiceID, subID, customer, orgMeta string) string {
	meta := `{}`
	if orgMeta != "" {
		meta = `{"champion_organization_id":"` + orgMeta + `"}`
	}
	return fmt.Sprintf(`{"id":"%s","type":"invoice.paid","api_version":"2026-08-26.dahlia","data":{"object":{
		"id":"%s","object":"invoice","status":"paid","customer":"%s","subscription":null,"amount_paid":599,"amount_due":599,"currency":"usd",
		"parent":{"type":"subscription_details","subscription_details":{"subscription":"%s","metadata":%s}},
		"lines":{"data":[{"amount":599,"parent":{"type":"subscription_item_details","subscription_item_details":{"subscription":"%s"}},
		"pricing":{"price_details":{"price":"price_starter_month"}},"period":{"start":1790366656,"end":1792958656}}]}}}}`,
		eventID, invoiceID, customer, subID, meta, subID)
}

func TestForeignCheckoutSessionIsNeverAttributed(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	championOrg(t, store, 1, "cus_champion")
	provider.Put(SubscriptionState{SubscriptionID: "sub_foreign", CustomerID: "cus_foreign", PriceID: "price_starter_month",
		StripeStatus: "active", StripeInterval: "month", CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	before := snapshot(store, 1)
	// The exact staging shape: client_reference_id null, metadata {}, a different new customer.
	deliver(t, svc, `{"id":"evt_foreign_cs","type":"checkout.session.completed","api_version":"2026-08-26.dahlia","data":{"object":{
		"id":"cs_test_foreign","object":"checkout.session","mode":"subscription","client_reference_id":null,"metadata":{},
		"customer":"cus_foreign","subscription":"sub_foreign","success_url":"https://stripe.com"}}}`)
	deliver(t, svc, foreignSubscriptionEvent("evt_foreign_sub", EventSubscriptionCreated, "sub_foreign", "cus_foreign"))
	deliver(t, svc, dahliaInvoicePaid("evt_foreign_inv", "in_foreign", "sub_foreign", "cus_foreign", ""))
	if after := snapshot(store, 1); after != before {
		t.Fatalf("a foreign session changed organization 1:\nbefore %+v\nafter  %+v", before, after)
	}
	if len(store.transactions) != 0 {
		t.Fatalf("a foreign invoice was recorded: %+v", store.transactions)
	}
}

// The latent flaw behind the incident: before this fix a subscription created outside Champion on
// an organization's OWN customer was attributed by customer id alone and replaced that
// organization's base subscription (id, plan, status).
func TestCustomerIDAloneNeverAttributesASubscriptionOrInvoice(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	ctx := context.Background()
	championOrg(t, store, 55, "cus_55")
	// The stray subscription really exists at Stripe (created from the Dashboard), without metadata.
	provider.Put(SubscriptionState{SubscriptionID: "sub_stray", CustomerID: "cus_55", PriceID: "price_starter_month",
		StripeStatus: "active", StripeInterval: "month", CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	store.ApplyProviderState(ctx, 55, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: "cus_55",
		ProviderSubscriptionID: "sub_55", ProviderPriceID: "price_pro_month", Plan: "PRO", Status: repository.SubscriptionActive})
	before := snapshot(store, 55)
	deliver(t, svc, foreignSubscriptionEvent("evt_stray_created", EventSubscriptionCreated, "sub_stray", "cus_55"))
	deliver(t, svc, foreignSubscriptionEvent("evt_stray_updated", EventSubscriptionUpdated, "sub_stray", "cus_55"))
	deliver(t, svc, foreignSubscriptionEvent("evt_stray_deleted", EventSubscriptionDeleted, "sub_stray", "cus_55"))
	deliver(t, svc, dahliaInvoicePaid("evt_stray_inv", "in_stray", "sub_stray", "cus_55", ""))
	if after := snapshot(store, 55); after != before {
		t.Fatalf("customer-only match rewrote the base subscription:\nbefore %+v\nafter  %+v", before, after)
	}
	if len(store.transactions) != 0 {
		t.Fatalf("customer-only invoice was recorded: %+v", store.transactions)
	}
}

func TestChampionCheckoutSessionMustMatchTheOrganizationsCustomer(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	championOrg(t, store, 70, "cus_70")
	provider.Put(SubscriptionState{SubscriptionID: "sub_70x", CustomerID: "cus_other", PriceID: "price_pro_month",
		StripeStatus: "active", StripeInterval: "month", CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	before := snapshot(store, 70)
	// client_reference_id names org 70 but the paid session belongs to another customer.
	deliver(t, svc, `{"id":"evt_cs_mismatch","type":"checkout.session.completed","data":{"object":{
		"id":"cs_x","mode":"subscription","client_reference_id":"70","customer":"cus_other","subscription":"sub_70x",
		"metadata":{"champion_organization_id":"70","champion_plan_key":"PRO"}}}}`)
	// client_reference_id and Champion's own metadata disagree.
	deliver(t, svc, `{"id":"evt_cs_conflict","type":"checkout.session.completed","data":{"object":{
		"id":"cs_y","mode":"subscription","client_reference_id":"70","customer":"cus_70","subscription":"sub_70x",
		"metadata":{"champion_organization_id":"71","champion_plan_key":"PRO"}}}}`)
	if after := snapshot(store, 70); after != before {
		t.Fatalf("unverified session binding was applied:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestMetadataConflictingWithStoredSubscriptionIsRefused(t *testing.T) {
	svc, store, _ := newTestService(t, sampleCatalog)
	ctx := context.Background()
	championOrg(t, store, 80, "cus_80")
	championOrg(t, store, 81, "cus_81")
	store.ApplyProviderState(ctx, 81, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: "cus_81",
		ProviderSubscriptionID: "sub_81", Plan: "PRO", Status: repository.SubscriptionActive})
	b80, b81 := snapshot(store, 80), snapshot(store, 81)
	deliver(t, svc, `{"id":"evt_meta_conflict","type":"customer.subscription.updated","data":{"object":{
		"id":"sub_81","status":"past_due","customer":"cus_81","metadata":{"champion_organization_id":"80"},
		"items":{"data":[{"current_period_start":1,"current_period_end":2,"price":{"id":"price_pro_month","recurring":{"interval":"month"}}}]}}}}`)
	if snapshot(store, 80) != b80 || snapshot(store, 81) != b81 {
		t.Fatal("an event whose metadata conflicts with the stored subscription changed an organization")
	}
}

// The genuine Champion flow still works with the dahlia payloads, in any delivery order: the
// invoice can arrive before checkout.session.completed and is attributed through the Champion
// metadata Stripe copies onto parent.subscription_details, with the saved customer agreeing.
func TestChampionSubscriptionAttributesInAnyOrderWithDahliaPayloads(t *testing.T) {
	svc, store, provider := newTestService(t, sampleCatalog)
	championOrg(t, store, 60, "cus_60")
	provider.Put(SubscriptionState{SubscriptionID: "sub_60", CustomerID: "cus_60", PriceID: "price_starter_month",
		StripeStatus: "active", StripeInterval: "month", CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0),
		Metadata: map[string]string{"champion_organization_id": "60", "champion_plan_key": "STARTER"}})
	deliver(t, svc, dahliaInvoicePaid("evt_60_inv", "in_60", "sub_60", "cus_60", "60"))
	if s := snapshot(store, 60); s.Status != repository.SubscriptionActive || s.ProviderSubscriptionID != "sub_60" {
		t.Fatalf("Champion invoice before checkout.session.completed not attributed: %+v", s)
	}
	if len(store.transactions) != 1 || store.transactions[0].OrganizationID != 60 || store.transactions[0].AmountCents != 599 {
		t.Fatalf("paid invoice not recorded exactly once for org 60: %+v", store.transactions)
	}
	deliver(t, svc, `{"id":"evt_60_cs","type":"checkout.session.completed","api_version":"2026-08-26.dahlia","data":{"object":{
		"id":"cs_60","mode":"subscription","client_reference_id":"60","customer":"cus_60","subscription":"sub_60",
		"metadata":{"champion_organization_id":"60","champion_plan_key":"STARTER"}}}}`)
	deliver(t, svc, `{"id":"evt_60_sub","type":"customer.subscription.created","data":{"object":{
		"id":"sub_60","status":"active","customer":"cus_60","metadata":{"champion_organization_id":"60","champion_plan_key":"STARTER"},
		"items":{"data":[{"current_period_start":1790366656,"current_period_end":1792958656,"price":{"id":"price_starter_month","recurring":{"interval":"month"}}}]}}}}`)
	if s := snapshot(store, 60); s.Status != repository.SubscriptionActive || s.Plan != "STARTER" || s.ProviderCustomerID != "cus_60" {
		t.Fatalf("Champion checkout not applied: %+v", s)
	}
}
