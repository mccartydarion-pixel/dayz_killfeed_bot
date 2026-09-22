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
	if pro["popular"] != true || pro["trialDays"].(float64) != 14 {
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

func TestBillingCheckoutTrialGrantedOnceOnly(t *testing.T) {
	w := newFactionWorld(t)
	body := map[string]any{"planKey": "PRO", "interval": "MONTHLY"}
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "first checkout")
	sub, _ := w.a.SaaSSubscriptions.GetForOrganization(context.Background(), w.a1.OrgID)
	state := w.billingProvider.CompleteCheckout(sub.ProviderCustomerID, "price_test_pro_month", 14)
	if state.StripeStatus != "trialing" {
		t.Fatalf("expected a trial on first checkout: %+v", state)
	}
	// Reconcile it into Champion via the real webhook endpoint.
	payload := checkoutCompletedPayload(uniqID("evt_trial"), w.a1.OrgID, state.SubscriptionID, state.CustomerID)
	w.expect(w.postWebhook(payload, signStripePayload(t, "whsec_test", payload)), http.StatusOK, "webhook")

	sum := w.getJSON(w.billingPath(w.a1, "/subscription"), w.admin)
	if sum["status"] != "TRIAL" || sum["hasActiveSubscription"] != true {
		t.Fatalf("%v", sum)
	}
	// A second checkout call must not ask Stripe for another trial.
	w.expect(w.do(http.MethodPost, w.billingPath(w.a1, "/checkout"), w.admin, body), http.StatusOK, "second checkout")
	var lastTrialDays float64 = -1
	for _, c := range w.billingProvider.Calls {
		if c.Method == "CreateCheckoutSession" {
			lastTrialDays = float64(c.Arg.(billing.CheckoutInput).TrialDays)
		}
	}
	if lastTrialDays != 0 {
		t.Fatalf("trial abuse: second checkout asked for a %v-day trial", lastTrialDays)
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
