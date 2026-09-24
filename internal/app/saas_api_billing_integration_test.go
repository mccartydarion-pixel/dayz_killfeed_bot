//go:build integration

package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"
	"github.com/yourname/dayz-killfeed/internal/adminrepo"
	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// End-to-end Champion Billing tests over the real routes and a real PostgreSQL (docs/BILLING.md):
// plans, the subscription summary, checkout authorization and safe redirects, the real Stripe
// webhook endpoint (signature verification, idempotency, tenant attribution), the customer portal,
// plan changes, cancel/reactivate, tenant isolation, rate limits, audit and admin visibility. Every
// Stripe call goes through billing.FakeProvider (newFactionWorld's w.billingProvider) - never a
// live API.

const billingTestCatalogJSON = `[
  {"key":"PRO","name":"Pro","description":"Pro plan","features":["killfeed","leaderboards"],"limits":{"installations":5},
   "monthly":{"amountCents":1999,"currency":"usd","stripePriceId":"price_test_pro_month"},
   "yearly":{"amountCents":19990,"currency":"usd","stripePriceId":"price_test_pro_year"},
   "isPublic":true,"sortOrder":2,"popular":true,"trialDays":14},
  {"key":"STARTER","name":"Starter","monthly":{"amountCents":999,"currency":"usd","stripePriceId":"price_test_starter_month"},
   "isPublic":true,"sortOrder":1,"trialDays":7},
  {"key":"LEGACY","name":"Legacy","monthly":{"amountCents":500,"currency":"usd","stripePriceId":"price_test_legacy_month"},"isPublic":false,"sortOrder":0}
]`

// approvedPricingCatalogJSON is the exact, commercially approved LOW/MEDIUM/HIGH catalog (Champion
// Billing Phase 1.2), byte-identical to internal/billing's own copy (internal/billing/pricing_catalog_test.go)
// - duplicated here rather than exported because every other catalog fixture in this file is local to
// it too. Stripe Price ids are test-mode ids (Phase 1.2: "do not switch Champion to live Stripe billing
// yet") - only the matching STRIPE_SECRET_KEY, never these ids, determines which Stripe account/mode
// they actually resolve against.
const approvedPricingCatalogJSON = `[
  {
    "key": "LOW", "name": "Low Tier", "description": "For smaller DayZ communities with up to 32 player slots.",
    "features": ["Killfeed","Faction Hub","Leaderboards","Champion Points Economy","Champion Shop","Embed Designer","Discord Integration","Nitrado Integration"],
    "limits": {"installations": 1, "maxSlots": 32},
    "monthly": {"amountCents": 599, "currency": "usd", "stripePriceId": "price_1UIQiD65uHRSytQgoMRICl5h"},
    "isPublic": true, "sortOrder": 1, "popular": false, "trialDays": 0
  },
  {
    "key": "MEDIUM", "name": "Medium Tier", "description": "For growing DayZ communities with 33 to 64 player slots.",
    "features": ["Killfeed","Faction Hub","Leaderboards","Champion Points Economy","Champion Shop","Embed Designer","Discord Integration","Nitrado Integration"],
    "limits": {"installations": 1, "maxSlots": 64},
    "monthly": {"amountCents": 999, "currency": "usd", "stripePriceId": "price_1UIQiD65uHRSytQghSQQOVpG"},
    "isPublic": true, "sortOrder": 2, "popular": true, "trialDays": 0
  },
  {
    "key": "HIGH", "name": "High Tier", "description": "For large DayZ communities with 65 to 128 player slots.",
    "features": ["Killfeed","Faction Hub","Leaderboards","Champion Points Economy","Champion Shop","Embed Designer","Discord Integration","Nitrado Integration"],
    "limits": {"installations": 1, "maxSlots": 128},
    "monthly": {"amountCents": 1499, "currency": "usd", "stripePriceId": "price_1UIQiD65uHRSytQgydtA4Pzj"},
    "isPublic": true, "sortOrder": 3, "popular": false, "trialDays": 0
  }
]`

// withApprovedPricingCatalog swaps w.a.Billing to a fresh Service loaded with the real approved
// catalog (and its own fresh FakeProvider), leaving every other fixture (organizations, users,
// installations) untouched. Only the tests in this section use it - every other billing test keeps
// using the synthetic billingTestCatalogJSON the fixture wires by default.
func (w *factionWorld) withApprovedPricingCatalog(t *testing.T) *billing.FakeProvider {
	t.Helper()
	cat, err := billing.LoadCatalog(approvedPricingCatalogJSON)
	if err != nil {
		t.Fatalf("approved catalog must parse cleanly: %v", err)
	}
	provider := billing.NewFakeProvider()
	w.a.Billing = billing.NewService(w.a.SaaSSubscriptions, cat, provider, billing.Options{WebhookSecret: "whsec_test"})
	w.billingProvider = provider
	return provider
}

func (w *factionWorld) billingPath(f installationFixture, suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/billing%s", f.OrgID, suffix)
}

var billingIDSeq atomic.Int64

// uniqID returns a fresh id for anything this file's tests key lookups on: Stripe event ids
// (billing_webhook_events dedupes by (provider, event_id) with no per-test scope), and Stripe
// object ids (provider_customer_id/provider_subscription_id have no uniqueness constraint either -
// GetByProviderCustomerID/GetByProviderSubscriptionID would happily match a leftover row from an
// earlier organization). This throwaway database is not recreated between runs of this binary and
// organizations are never deleted at test cleanup, so a literal id would collide.
func uniqID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), billingIDSeq.Add(1))
}

func signStripePayload(t *testing.T, secret string, payload []byte) string {
	t.Helper()
	now := time.Now()
	sig := stripewebhook.ComputeSignature(now, payload, secret)
	return fmt.Sprintf("t=%d,v1=%x", now.Unix(), sig)
}

func (w *factionWorld) postWebhook(payload []byte, sig string) *apiResult {
	w.t.Helper()
	req, err := http.NewRequest(http.MethodPost, w.base+"/api/saas/billing/webhook", bytes.NewReader(payload))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Stripe-Signature", sig)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	return &apiResult{Status: resp.StatusCode}
}

// checkoutCompletedPayload/subscriptionPayload build minimal, realistic webhook bodies. Field
// shapes match internal/billing/webhook.go's webhookCheckoutSession/webhookSubscription.
func checkoutCompletedPayload(eventID string, orgID int64, subscriptionID, customerID string) []byte {
	return []byte(fmt.Sprintf(`{"id":"%s","type":"checkout.session.completed","data":{"object":{
		"id":"cs_test","mode":"subscription","client_reference_id":"%d","customer":"%s","subscription":"%s",
		"metadata":{"champion_plan_key":"PRO","champion_organization_id":"%d"}}}}`, eventID, orgID, customerID, subscriptionID, orgID))
}

func subscriptionEventPayload(eventID, eventType, subscriptionID, customerID, status, priceID string, cancelAtPeriodEnd bool, orgMeta int64) []byte {
	meta := "{}"
	if orgMeta != 0 {
		meta = fmt.Sprintf(`{"champion_organization_id":"%d"}`, orgMeta)
	}
	now := time.Now().Unix()
	return []byte(fmt.Sprintf(`{"id":"%s","type":"%s","data":{"object":{
		"id":"%s","status":"%s","cancel_at_period_end":%v,"customer":"%s","metadata":%s,
		"items":{"data":[{"current_period_start":%d,"current_period_end":%d,"price":{"id":"%s","recurring":{"interval":"month"}}}]}}}}`,
		eventID, eventType, subscriptionID, status, cancelAtPeriodEnd, customerID, meta, now, now+2592000, priceID))
}

func TestBillingPlansEndpoint(t *testing.T) {
	w := newFactionWorld(t)
	page := w.getJSON("/api/saas/billing/plans", w.players[0])
	items, ok := page["items"].([]any)
	if !ok || len(items) != 2 { // LEGACY is private
		t.Fatalf("expected 2 public plans, got %v", page)
	}
	names := map[string]bool{}
	for _, it := range items {
		m := it.(map[string]any)
		names[m["key"].(string)] = true
		for _, banned := range []string{"stripePriceId", "stripePriceID"} {
			if _, leaked := m[banned]; leaked {
				t.Fatalf("plans API must never expose a Stripe price id: %v", m)
			}
		}
	}
	if !names["PRO"] || !names["STARTER"] || names["LEGACY"] {
		t.Fatalf("public plans: %v", names)
	}
	pro := items[0].(map[string]any)
	for _, it := range items {
		if it.(map[string]any)["key"] == "PRO" {
			pro = it.(map[string]any)
		}
	}
	// The catalog JSON still says 14: Stripe trial days are always 0 (Onboarding V2).
	if pro["popular"] != true || pro["trialDays"].(float64) != 0 {
		t.Fatalf("pro: %v", pro)
	}
	monthly := pro["monthly"].(map[string]any)
	if monthly["amountCents"].(float64) != 1999 || monthly["currency"] != "usd" {
		t.Fatalf("pro monthly: %v", monthly)
	}
	// Unauthenticated / unsynced.
	w.expect(w.do(http.MethodGet, "/api/saas/billing/plans", "", nil), http.StatusUnauthorized, "no acting user")
	w.expect(w.do(http.MethodGet, "/api/saas/billing/plans", "never-synced", nil), http.StatusUnauthorized, "unsynced")
}

func TestBillingSubscriptionSummaryDefaultsAndEntitlements(t *testing.T) {
	w := newFactionWorld(t)
	sum := w.getJSON(w.billingPath(w.a1, "/subscription"), w.admin)
	if sum["plan"] != "TRIAL" || sum["status"] != "TRIAL" || sum["cancelAtPeriodEnd"] != false || sum["hasBillingCustomer"] != false || sum["hasActiveSubscription"] != false {
		t.Fatalf("%v", sum)
	}
	if sum["trialStatus"] != "ACTIVE" || sum["billingRequired"] != false || sum["trialDaysRemaining"].(float64) != 14 || sum["intendedPlan"] != nil {
		t.Fatalf("onboarding fields: %v", sum)
	}
	if sum["canManageBilling"] != true {
		t.Fatal("an org ADMIN must be able to manage billing")
	}
	ents, ok := sum["entitlements"].([]any)
	if !ok || len(ents) == 0 {
		t.Fatalf("expected entitlements: %v", sum)
	}
	for _, banned := range []string{"stripeCustomerId", "stripeSubscriptionId", "providerCustomerId", "providerSubscriptionId"} {
		raw := w.do(http.MethodGet, w.billingPath(w.a1, "/subscription"), w.admin, nil).Body
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("subscription summary must never contain %q", banned)
		}
	}
	// A MEMBER can read it but cannot manage billing.
	memberSum := w.getJSON(w.billingPath(w.a1, "/subscription"), w.member)
	if memberSum["canManageBilling"] != false {
		t.Fatal("a MEMBER must not be able to manage billing")
	}
	// A faction leader (no organization role) cannot read another org's, and a non-member can't read this one either.
	outsider := w.players[0]
	w.expect(w.do(http.MethodGet, w.billingPath(w.a1, "/subscription"), outsider, nil), http.StatusForbidden, "non-member")
}

func TestBillingCheckoutAuthorizationAndValidation(t *testing.T) {
	w := newFactionWorld(t)
	leader := w.players[0]
	w.createFaction(w.a1, leader, "Billing Faction", "BIL", "OPEN") // a faction role grants nothing

	body := map[string]any{"planKey": "PRO", "interval": "MONTHLY"}
	for _, actor := range []string{w.member, leader, w.players[1]} {
		r := w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), actor, body)
		if r.Status != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d %s", actor, r.Status, r.Body)
		}
	}
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), "", body), http.StatusUnauthorized, "no acting user")

	// Validation, as OWNER/ADMIN.
	for name, bad := range map[string]map[string]any{
		"unknown plan":                         {"planKey": "NOPE", "interval": "MONTHLY"},
		"private plan":                         {"planKey": "LEGACY", "interval": "MONTHLY"},
		"unsold interval":                      {"planKey": "STARTER", "interval": "YEARLY"},
		"unsafe return path (absolute)":        {"planKey": "PRO", "interval": "MONTHLY", "returnPath": "https://evil.example/steal"},
		"unsafe return path (scheme-relative)": {"planKey": "PRO", "interval": "MONTHLY", "returnPath": "//evil.example"},
	} {
		r := w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, bad)
		if r.Status != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", name, r.Status, r.Body)
		}
	}
	// A successful checkout.
	res := w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "checkout").JSON(t)
	url, _ := res["checkoutUrl"].(string)
	if url == "" || !strings.Contains(url, "checkout.stripe.example") {
		t.Fatalf("checkoutUrl: %v", res)
	}
	// The Stripe customer id is persisted (never returned to the client).
	sub, err := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	if err != nil || sub == nil || sub.ProviderCustomerID == "" {
		t.Fatalf("expected a persisted stripe customer: %+v %v", sub, err)
	}
	// The OWNER may also check out (not only ADMIN).
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.a1.OwnerDiscordID, body), http.StatusOK, "owner checkout")
}

func TestBillingCheckoutNeverGrantsAStripeTrial(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	before, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID)
	body := map[string]any{"planKey": "PRO", "interval": "MONTHLY"} // PRO's catalog JSON says trialDays 14
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "first checkout")
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "retried checkout")
	n := 0
	for _, c := range w.billingProvider.Calls {
		if c.Method == "CreateCheckoutSession" {
			n++
			if d := c.Arg.(billing.CheckoutInput).TrialDays; d != 0 {
				t.Fatalf("checkout asked Stripe for a %d-day trial", d)
			}
		}
	}
	if n != 2 {
		t.Fatalf("checkout sessions: %d", n)
	}
	// Creating a session changes nothing about the running no-card trial; the webhook applies state.
	mid, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID)
	if mid.Status != repository.SubscriptionTrial || mid.ProviderSubscriptionID != "" || !mid.TrialEndsAt.Equal(*before.TrialEndsAt) {
		t.Fatalf("checkout must not touch the trial: %+v", mid)
	}
	state := w.billingProvider.CompleteCheckout(mid.ProviderCustomerID, "price_test_pro_month", 0)
	if state.StripeStatus != "active" {
		t.Fatalf("a zero-day checkout converts straight to active: %+v", state)
	}
	payload := checkoutCompletedPayload(uniqID("evt_convert"), w.a1.OrgID, state.SubscriptionID, state.CustomerID)
	w.expect(w.postWebhook(payload, signStripePayload(t, "whsec_test", payload)), http.StatusOK, "webhook")
	sum := w.getJSON(w.billingPath(w.a1, "/subscription"), w.admin)
	if sum["status"] != "ACTIVE" || sum["plan"] != "PRO" || sum["hasActiveSubscription"] != true || sum["trialStatus"] != "CONVERTED" || sum["billingRequired"] != false {
		t.Fatalf("%v", sum)
	}
}

func TestBillingWebhookRejectsBadSignatureAndIsIdempotent(t *testing.T) {
	w := newFactionWorld(t)
	subscriptionID, customerID := uniqID("sub"), uniqID("cus")
	sub, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	payload := checkoutCompletedPayload(uniqID("evt_bad"), w.a1.OrgID, subscriptionID, customerID)

	w.expect(w.postWebhook(payload, "t=1,v1=deadbeef"), http.StatusBadRequest, "bad signature")
	after, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	if after.Status != sub.Status || after.ProviderSubscriptionID != "" {
		t.Fatal("a badly-signed webhook must never change any subscription")
	}

	// Idempotency: redeliver the same, correctly signed event several times.
	sig := signStripePayload(t, "whsec_test", payload)
	for i := 0; i < 4; i++ {
		w.expect(w.postWebhook(payload, sig), http.StatusOK, fmt.Sprintf("delivery %d", i))
	}
	got, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	if got.ProviderSubscriptionID != subscriptionID || got.Status != repository.SubscriptionActive {
		t.Fatalf("%+v", got)
	}
	// Redelivering the SAME event id after Stripe's state moved on must not reapply the old state.
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_starter_month", StripeStatus: "past_due", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	changed := subscriptionEventPayload(uniqID("evt_new"), "customer.subscription.updated", subscriptionID, customerID, "past_due", "price_test_starter_month", false, w.a1.OrgID)
	w.expect(w.postWebhook(changed, signStripePayload(t, "whsec_test", changed)), http.StatusOK, "a genuinely new event")
	updated, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	if updated.Status != repository.SubscriptionPastDue || updated.Plan != "STARTER" {
		t.Fatalf("a new event id must be processed: %+v", updated)
	}
	w.expect(w.postWebhook(payload, sig), http.StatusOK, "redeliver the old evt_bad again")
	stillUpdated, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	if stillUpdated.Status != repository.SubscriptionPastDue || stillUpdated.Plan != "STARTER" {
		t.Fatalf("the old event must still be a no-op: %+v", stillUpdated)
	}
}

func TestBillingWebhookNeverCrossesTenants(t *testing.T) {
	w := newFactionWorld(t)
	subscriptionID, customerID := uniqID("sub"), uniqID("cus")
	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: subscriptionID, CustomerID: customerID, PriceID: "price_test_pro_month", StripeStatus: "active", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	payload := checkoutCompletedPayload(uniqID("evt_iso"), w.a1.OrgID, subscriptionID, customerID)
	w.expect(w.postWebhook(payload, signStripePayload(t, "whsec_test", payload)), http.StatusOK, "webhook")

	bSub, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.b1.OrgID)
	if bSub.ProviderSubscriptionID != "" || bSub.Status != repository.SubscriptionTrial {
		t.Fatalf("org B must be untouched by org A's webhook: %+v", bSub)
	}
	// An event that cannot be attributed to any organization is acknowledged and changes nothing.
	ghostSub := uniqID("sub_ghost")
	orphan := subscriptionEventPayload(uniqID("evt_orphan"), "customer.subscription.updated", ghostSub, uniqID("cus_ghost"), "active", "price_test_pro_month", false, 0)
	w.expect(w.postWebhook(orphan, signStripePayload(t, "whsec_test", orphan)), http.StatusOK, "unattributable event is still acked")
	for _, f := range []installationFixture{w.a1, w.b1} {
		sub, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), f.OrgID)
		if sub.ProviderSubscriptionID == ghostSub {
			t.Fatalf("an unattributable event must not attach to any organization: %+v", sub)
		}
	}
}

func TestBillingWebhookInvoicePaymentFailedDoesNotDeleteAnything(t *testing.T) {
	w := newFactionWorld(t)
	sub, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	state := w.billingProvider.CompleteCheckout(sub.ProviderCustomerID, "price_test_pro_month", 0)
	activated := checkoutCompletedPayload(uniqID("evt_pay"), w.a1.OrgID, state.SubscriptionID, state.CustomerID)
	w.expect(w.postWebhook(activated, signStripePayload(t, "whsec_test", activated)), http.StatusOK, "activate")

	w.billingProvider.Put(billing.SubscriptionState{SubscriptionID: state.SubscriptionID, CustomerID: state.CustomerID, PriceID: "price_test_pro_month", StripeStatus: "past_due", StripeInterval: "month",
		CurrentPeriodStart: time.Now(), CurrentPeriodEnd: time.Now().AddDate(0, 1, 0)})
	invoice := []byte(fmt.Sprintf(`{"id":"%s","type":"invoice.payment_failed","data":{"object":{"id":"%s","customer":"%s","subscription":"%s"}}}`, uniqID("evt_invoice_fail"), uniqID("in"), state.CustomerID, state.SubscriptionID))
	w.expect(w.postWebhook(invoice, signStripePayload(t, "whsec_test", invoice)), http.StatusOK, "payment failed")

	after := w.getJSON(w.billingPath(w.a1, "/subscription"), w.admin)
	if after["status"] != "PAST_DUE" || after["plan"] != "PRO" {
		t.Fatalf("%v", after)
	}
	// Nothing about the organization/installation itself is touched by a payment failure.
	org, err := w.a.SaaSOrganizations.GetByID(context.Background(), w.a1.OrgID)
	if err != nil || org == nil {
		t.Fatal("the organization must still exist")
	}
	inst, err := w.a.SaaSInstallations.GetScoped(context.Background(), w.a1.OrgID, w.a1.InstallationID)
	if err != nil || inst == nil {
		t.Fatal("the installation must still exist")
	}
}

func TestBillingPortalRequiresAnExistingCustomer(t *testing.T) {
	w := newFactionWorld(t)
	r := w.do(http.MethodPost, w.billingPath(w.a1, "/portal"), w.admin, map[string]any{})
	if r.Status != http.StatusConflict || r.errCode(t) != "NO_BILLING_CUSTOMER" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, map[string]any{"planKey": "STARTER", "interval": "MONTHLY"}), http.StatusOK, "checkout")
	res := w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/portal"), w.admin, map[string]any{}), http.StatusOK, "portal").JSON(t)
	if u, _ := res["portalUrl"].(string); u == "" {
		t.Fatalf("%v", res)
	}
	// A MEMBER cannot open the portal.
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/portal"), w.member, map[string]any{}), http.StatusForbidden, "member portal")
}

// activeOrgA drives org A to an ACTIVE PRO subscription through the real checkout + webhook path.
func (w *factionWorld) activeOrgA(t *testing.T, plan, priceID string) {
	t.Helper()
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, map[string]any{"planKey": plan, "interval": "MONTHLY"}), http.StatusOK, "checkout")
	sub, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	state := w.billingProvider.CompleteCheckout(sub.ProviderCustomerID, priceID, 0)
	payload := checkoutCompletedPayload(uniqID("evt_active"), w.a1.OrgID, state.SubscriptionID, state.CustomerID)
	w.expect(w.postWebhook(payload, signStripePayload(t, "whsec_test", payload)), http.StatusOK, "activate")
}

func TestBillingChangePlanCancelReactivate(t *testing.T) {
	w := newFactionWorld(t)
	// Before any Stripe subscription exists.
	if r := w.do(http.MethodPost, w.billingPath(w.a1, "/plan"), w.admin, map[string]any{"planKey": "PRO", "interval": "MONTHLY"}); r.Status != http.StatusConflict || r.errCode(t) != "NO_ACTIVE_SUBSCRIPTION" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodPost, w.billingPath(w.a1, "/cancel"), w.admin, nil); r.Status != http.StatusConflict {
		t.Fatalf("%d %s", r.Status, r.Body)
	}

	w.activeOrgA(t, "STARTER", "price_test_starter_month")
	// MEMBER cannot change/cancel/reactivate.
	for _, path := range []string{"/plan", "/cancel", "/reactivate"} {
		w.expect(w.do(http.MethodPost, w.billingPath(w.a1, path), w.member, map[string]any{"planKey": "PRO", "interval": "MONTHLY"}), http.StatusForbidden, "member "+path)
	}
	// Upgrade.
	upd := w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/plan"), w.admin, map[string]any{"planKey": "PRO", "interval": "MONTHLY"}), http.StatusOK, "upgrade").JSON(t)
	if upd["plan"] != "PRO" || upd["status"] != "ACTIVE" {
		t.Fatalf("%v", upd)
	}
	// An unknown plan is rejected without touching anything.
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/plan"), w.admin, map[string]any{"planKey": "NOPE", "interval": "MONTHLY"}), http.StatusBadRequest, "unknown plan")
	// Downgrade back.
	down := w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/plan"), w.admin, map[string]any{"planKey": "STARTER", "interval": "MONTHLY"}), http.StatusOK, "downgrade").JSON(t)
	if down["plan"] != "STARTER" {
		t.Fatalf("%v", down)
	}
	// Cancel at period end.
	c := w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/cancel"), w.admin, nil), http.StatusOK, "cancel").JSON(t)
	if c["cancelAtPeriodEnd"] != true || c["status"] != "ACTIVE" {
		t.Fatalf("cancellation must keep the subscription active until period end: %v", c)
	}
	// Reactivate.
	re := w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/reactivate"), w.admin, nil), http.StatusOK, "reactivate").JSON(t)
	if re["cancelAtPeriodEnd"] != false {
		t.Fatalf("%v", re)
	}
}

func TestBillingTenantIsolation(t *testing.T) {
	w := newFactionWorld(t)
	w.activeOrgA(t, "PRO", "price_test_pro_month")
	// Org B's admin acting on org A's billing route.
	for _, path := range []string{"/checkout", "/portal", "/plan", "/cancel", "/reactivate"} {
		r := w.do(http.MethodPost, w.billingPath(w.a1, path), w.b1.OwnerDiscordID, map[string]any{"planKey": "PRO", "interval": "MONTHLY"})
		if r.Status != http.StatusForbidden {
			t.Errorf("org B's owner on org A's %s: expected 403, got %d %s", path, r.Status, r.Body)
		}
	}
	if r := w.do(http.MethodGet, w.billingPath(w.a1, "/subscription"), w.b1.OwnerDiscordID, nil); r.Status != http.StatusForbidden {
		t.Fatalf("org B reading org A's subscription: %d %s", r.Status, r.Body)
	}
	// Org B's own subscription is untouched and independent.
	bSum := w.getJSON(w.billingPath(w.b1, "/subscription"), w.b1.OwnerDiscordID)
	if bSum["status"] != "TRIAL" || bSum["plan"] != "TRIAL" {
		t.Fatalf("%v", bSum)
	}
}

func TestBillingRateLimitsAdminActions(t *testing.T) {
	w := newFactionWorld(t)
	w.a.saasBillingActionLimiter = newSaaSRateLimiter(time.Hour, 2)
	body := map[string]any{"planKey": "PRO", "interval": "MONTHLY"}
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "1")
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "2")
	if r := w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body); r.Status != http.StatusTooManyRequests {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	// Reads are never limited by the action limiter.
	for i := 0; i < 20; i++ {
		w.expect(w.do(http.MethodGet, w.billingPath(w.a1, "/subscription"), w.admin, nil), http.StatusOK, "read")
	}
}

func TestBillingAuditLogsIdsOnly(t *testing.T) {
	w := newFactionWorld(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	w.activeOrgA(t, "PRO", "price_test_pro_month")
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/cancel"), w.admin, nil), http.StatusOK, "cancel")
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/reactivate"), w.admin, nil), http.StatusOK, "reactivate")

	out := logs.String()
	for _, want := range []string{"billing_checkout_created", "billing_subscription_activated", "billing_subscription_cancelled", "billing_subscription_reactivated", "organization_id="} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log missing %q", want)
		}
	}
	for _, banned := range []string{"sub_", "cus_", "price_test", "whsec_test"} {
		if strings.Contains(out, banned) {
			t.Errorf("the audit log must never contain a Stripe id or secret, found %q", banned)
		}
	}
}

func TestBillingAdminVisibilityShowsIntervalAndCancellationNoPaymentDetail(t *testing.T) {
	w := newFactionWorld(t)
	w.activeOrgA(t, "PRO", "price_test_pro_month")
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/cancel"), w.admin, nil), http.StatusOK, "cancel")

	org, err := w.a.SaaSOrganizations.GetByID(context.Background(), w.a1.OrgID)
	if err != nil || org == nil {
		t.Fatal(err)
	}
	admin := adminrepo.New(w.a.DB.Pool)
	// Search by the org's own name: the shared throwaway database accumulates rows across the whole
	// test run (and across reruns of this binary), so a plain "recent 50" page is not reliable here.
	rows, _, err := admin.ListSubscriptions(context.Background(), adminrepo.SubscriptionFilter{Limit: 50, Search: org.Name})
	if err != nil {
		t.Fatal(err)
	}
	var found *adminrepo.SubscriptionRow
	for i := range rows {
		if rows[i].OrganizationID == w.a1.OrgID {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("expected to find org A's subscription in the admin list")
	}
	if found.Plan != "PRO" || found.Status != "ACTIVE" || found.BillingInterval != "MONTHLY" || !found.CancelAtPeriodEnd {
		t.Fatalf("%+v", found)
	}
}

// --- Champion Billing Phase 1.2: the approved LOW/MEDIUM/HIGH pricing catalog ----------------------

// TestApprovedPricingCatalogPublicPlansAPI proves the real catalog's public plans API returns exactly
// LOW, MEDIUM, HIGH in that order (sortOrder 1/2/3), with the exact approved prices/limits/trial/
// popular flag, and never leaks a Stripe price id anywhere in the response.
func TestApprovedPricingCatalogPublicPlansAPI(t *testing.T) {
	w := newFactionWorld(t)
	w.withApprovedPricingCatalog(t)

	raw := w.do(http.MethodGet, "/api/saas/billing/plans", w.players[0], nil)
	w.expect(raw, http.StatusOK, "plans")
	for _, banned := range []string{"stripePriceId", "stripePriceID", "price_1UIQiD65uHRSytQgoMRICl5h", "price_1UIQiD65uHRSytQghSQQOVpG", "price_1UIQiD65uHRSytQgydtA4Pzj"} {
		if bytes.Contains(raw.Body, []byte(banned)) {
			t.Fatalf("public plans API must never leak a Stripe price id, found %q in %s", banned, raw.Body)
		}
	}

	page := w.getJSON("/api/saas/billing/plans", w.players[0])
	items, ok := page["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("expected exactly 3 public plans (LOW/MEDIUM/HIGH), got %v", page)
	}
	wantOrder := []struct {
		key         string
		amountCents float64
		maxSlots    float64
		popular     bool
	}{
		{"LOW", 599, 32, false},
		{"MEDIUM", 999, 64, true},
		{"HIGH", 1499, 128, false},
	}
	for i, want := range wantOrder {
		m := items[i].(map[string]any)
		if m["key"] != want.key {
			t.Fatalf("plan[%d]: want key %s, got %v (full order: %v)", i, want.key, m["key"], items)
		}
		if m["popular"] != want.popular {
			t.Errorf("%s: popular = %v, want %v", want.key, m["popular"], want.popular)
		}
		if m["trialDays"].(float64) != 0 {
			t.Errorf("%s: trialDays = %v, want 0 (no Stripe trial - Onboarding V2)", want.key, m["trialDays"])
		}
		if m["yearly"] != nil {
			t.Errorf("%s: yearly must be null (no annual pricing approved), got %v", want.key, m["yearly"])
		}
		monthly := m["monthly"].(map[string]any)
		if monthly["amountCents"].(float64) != want.amountCents || monthly["currency"] != "usd" {
			t.Errorf("%s: monthly = %v, want %v cents usd", want.key, monthly, want.amountCents)
		}
		limits := m["limits"].(map[string]any)
		if limits["maxSlots"].(float64) != want.maxSlots {
			t.Errorf("%s: limits.maxSlots = %v, want %v", want.key, limits["maxSlots"], want.maxSlots)
		}
		if limits["installations"].(float64) != 1 {
			t.Errorf("%s: limits.installations = %v, want 1", want.key, limits["installations"])
		}
	}
}

// TestApprovedPricingCatalogCheckoutResolvesPriceIDsServerSideOnly proves checkout resolves each
// approved plan's exact Stripe Price id purely from the server-side catalog (never from the request
// body), that MONTHLY is accepted for all three plans, and that YEARLY - which none of the three
// plans sell, since no annual pricing has been approved - returns INVALID_PLAN for all three rather
// than silently falling back to a fabricated annual price.
func TestApprovedPricingCatalogCheckoutResolvesPriceIDsServerSideOnly(t *testing.T) {
	w := newFactionWorld(t)
	provider := w.withApprovedPricingCatalog(t)

	cases := []struct {
		key     string
		priceID string
	}{
		{"LOW", "price_1UIQiD65uHRSytQgoMRICl5h"},
		{"MEDIUM", "price_1UIQiD65uHRSytQghSQQOVpG"},
		{"HIGH", "price_1UIQiD65uHRSytQgydtA4Pzj"},
	}
	for _, c := range cases {
		t.Run(c.key+"/MONTHLY", func(t *testing.T) {
			w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, map[string]any{"planKey": c.key, "interval": "MONTHLY"}), http.StatusOK, "checkout")
			var lastPriceID string
			for _, call := range provider.Calls {
				if call.Method == "CreateCheckoutSession" {
					lastPriceID = call.Arg.(billing.CheckoutInput).PriceID
				}
			}
			if lastPriceID != c.priceID {
				t.Fatalf("%s: checkout resolved Stripe price %q, want the approved %q", c.key, lastPriceID, c.priceID)
			}
		})
		t.Run(c.key+"/YEARLY", func(t *testing.T) {
			r := w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, map[string]any{"planKey": c.key, "interval": "YEARLY"})
			if r.Status != http.StatusBadRequest || r.errCode(t) != "INVALID_PLAN" {
				t.Fatalf("%s YEARLY: expected 400 INVALID_PLAN (no annual pricing approved), got %d %s", c.key, r.Status, r.Body)
			}
		})
	}
	// An unknown plan key is rejected the same way.
	r := w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, map[string]any{"planKey": "ENTERPRISE", "interval": "MONTHLY"})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "INVALID_PLAN" {
		t.Fatalf("unknown plan: expected 400 INVALID_PLAN, got %d %s", r.Status, r.Body)
	}
}
