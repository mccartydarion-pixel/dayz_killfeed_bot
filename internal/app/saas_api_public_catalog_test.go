package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/config"
)

const publicCatalogSecret = "website-secret-for-public-catalog"

// A realistic base catalog: three public plans with Stripe price ids, plus a private plan that must
// never appear publicly.
const publicCatalogJSON = `[
 {"key":"LOW","name":"Low","description":"Starter","features":["Killfeed"],"isPublic":true,"sortOrder":1,
  "monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_low_m_SECRET"}},
 {"key":"MEDIUM","name":"Medium","description":"Growing","features":["Killfeed","Leaderboards"],"isPublic":true,"sortOrder":2,"popular":true,
  "monthly":{"amountCents":999,"currency":"usd","stripePriceId":"price_med_m_SECRET"},
  "yearly":{"amountCents":9990,"currency":"usd","stripePriceId":"price_med_y_SECRET"}},
 {"key":"HIGH","name":"High","description":"Everything","features":["All"],"isPublic":true,"sortOrder":3,
  "monthly":{"amountCents":1499,"currency":"usd","stripePriceId":"price_high_m_SECRET"}},
 {"key":"FOUNDER","name":"Founder","description":"Private","features":[],"isPublic":false,"sortOrder":9,
  "monthly":{"amountCents":1,"currency":"usd","stripePriceId":"price_founder_SECRET"}}
]`

func publicCatalogApp(t *testing.T, provider billing.Provider, caseOpts *billing.CaseOptions, catalogJSON string) *App {
	t.Helper()
	catalog, err := billing.LoadCatalog(catalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	svc := billing.NewService(nil, catalog, provider, billing.Options{WebhookSecret: "whsec_public_catalog"})
	if caseOpts != nil {
		if err := svc.ConfigureCaseAddons(&replayCaseStore{seen: map[string]bool{}}, *caseOpts); err != nil {
			t.Fatal(err)
		}
	}
	return &App{Config: &config.Config{WebsiteAPISecret: publicCatalogSecret}, Billing: svc}
}

func getPublicCatalog(a *App, auth, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/saas/billing/public-plans", nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rr := httptest.NewRecorder()
	a.handlePublicCatalog(rr, req)
	return rr
}

var casePricesForPublicCatalog = map[casebilling.Tier]string{casebilling.Watch: "price_watch_SECRET", casebilling.Pro: "price_pro_SECRET"}

// Anonymous (no acting user, no Discord login) read succeeds and carries only approved fields.
func TestPublicCatalogAnonymousReadHasOnlyApprovedFields(t *testing.T) {
	a := publicCatalogApp(t, billing.NewFakeProvider(), &billing.CaseOptions{Enabled: true, AccessEnabled: true,
		VerifiedThrough: casebilling.Pro, PriceIDs: casePricesForPublicCatalog}, publicCatalogJSON)
	rr := getPublicCatalog(a, publicCatalogSecret, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("anonymous catalog: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"SECRET", "price_", "prod_", "cus_", "sub_", "whsec", "stripe", "Stripe", "FOUNDER",
		"organization", "installation", "limits", "trialDays"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public catalog leaks %q: %s", forbidden, body)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if keys := sortedKeys(raw); !reflect.DeepEqual(keys, []string{"plans", "version"}) {
		t.Fatalf("top-level fields: %v", keys)
	}
	approved := []string{"availability", "category", "description", "displayTier", "features", "key", "name", "perServer",
		"popular", "prices", "purchasable", "requiresBasePlan", "sortOrder"}
	for _, p := range raw["plans"].([]any) {
		plan := p.(map[string]any)
		if keys := sortedKeys(plan); !reflect.DeepEqual(keys, approved) {
			t.Fatalf("plan fields %v, want %v", keys, approved)
		}
		for _, pr := range plan["prices"].([]any) {
			if keys := sortedKeys(pr.(map[string]any)); !reflect.DeepEqual(keys, []string{"amountCents", "currency", "interval"}) {
				t.Fatalf("price fields %v", keys)
			}
		}
	}
	if rr.Header().Get("Cache-Control") != publicCatalogCacheControl || rr.Header().Get("ETag") == "" {
		t.Fatalf("cache headers: %v", rr.Header())
	}
	// A conditional re-read is a 304 with no body.
	if again := getPublicCatalog(a, publicCatalogSecret, rr.Header().Get("ETag")); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("conditional read: %d %q", again.Code, again.Body.String())
	}
}

// Prices and availability come from configuration; Command stays off the catalog.
func TestPublicCatalogMatchesConfiguredPlansAndReleaseGates(t *testing.T) {
	read := func(a *App) map[string]publicPlanDTO {
		rr := getPublicCatalog(a, publicCatalogSecret, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("catalog: %d %s", rr.Code, rr.Body.String())
		}
		var c publicCatalogResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out := map[string]publicPlanDTO{}
		order := []string{}
		for _, p := range c.Plans {
			out[p.Key] = p
			order = append(order, p.Key)
		}
		if strings.Join(order, ",") != "LOW,MEDIUM,HIGH,CASE_WATCH,CASE_PRO" || c.Version != 1 {
			t.Fatalf("plans/order: %v v%d", order, c.Version)
		}
		return out
	}
	// Sales open for Watch and Pro, checkout configured.
	open := read(publicCatalogApp(t, billing.NewFakeProvider(), &billing.CaseOptions{Enabled: true, AccessEnabled: true,
		VerifiedThrough: casebilling.Pro, PriceIDs: casePricesForPublicCatalog}, publicCatalogJSON))
	med := open["MEDIUM"]
	if med.Category != "BASE" || med.Name != "Medium" || med.DisplayTier != "MEDIUM" || !med.Popular || !med.Purchasable ||
		med.Availability != "AVAILABLE" || med.PerServer || med.RequiresBasePlan ||
		!reflect.DeepEqual(med.Prices, []publicPriceDTO{{"MONTHLY", 999, "usd"}, {"YEARLY", 9990, "usd"}}) ||
		!reflect.DeepEqual(med.Features, []string{"Killfeed", "Leaderboards"}) {
		t.Fatalf("MEDIUM: %+v", med)
	}
	if low := open["LOW"]; !reflect.DeepEqual(low.Prices, []publicPriceDTO{{"MONTHLY", 599, "usd"}}) {
		t.Fatalf("LOW prices: %+v", low.Prices)
	}
	watch, pro := open["CASE_WATCH"], open["CASE_PRO"]
	if watch.Category != "CASE_ADDON" || watch.DisplayTier != "WATCH" || !watch.Purchasable || watch.Availability != "AVAILABLE" ||
		!watch.PerServer || !watch.RequiresBasePlan || !reflect.DeepEqual(watch.Prices, []publicPriceDTO{{"MONTHLY", 499, "usd"}}) ||
		watch.Description == "" || len(watch.Features) == 0 {
		t.Fatalf("Watch: %+v", watch)
	}
	if !pro.Purchasable || !reflect.DeepEqual(pro.Prices, []publicPriceDTO{{"MONTHLY", 999, "usd"}}) {
		t.Fatalf("Pro: %+v", pro)
	}
	if _, listed := open["CASE_COMMAND"]; listed {
		t.Fatal("Command listed before its release gates pass")
	}
	// Production today: C.A.S.E. flags off - listed, never purchasable.
	closed := read(publicCatalogApp(t, billing.NewFakeProvider(), &billing.CaseOptions{PriceIDs: casePricesForPublicCatalog}, publicCatalogJSON))
	for _, k := range []string{"CASE_WATCH", "CASE_PRO"} {
		if p := closed[k]; p.Purchasable || p.Availability != "COMING_SOON" {
			t.Fatalf("%s advertised while disabled: %+v", k, p)
		}
	}
	// Watch verified only: Pro is not sold even with sales on.
	watchOnly := read(publicCatalogApp(t, billing.NewFakeProvider(), &billing.CaseOptions{Enabled: true, AccessEnabled: true,
		VerifiedThrough: casebilling.Watch, PriceIDs: casePricesForPublicCatalog}, publicCatalogJSON))
	if !watchOnly["CASE_WATCH"].Purchasable || watchOnly["CASE_PRO"].Purchasable || watchOnly["CASE_PRO"].Availability != "COMING_SOON" {
		t.Fatalf("verified-through gate: %+v %+v", watchOnly["CASE_WATCH"], watchOnly["CASE_PRO"])
	}
	// No Stripe provider configured: base plans are listed with prices but not purchasable.
	noCheckout := read(publicCatalogApp(t, nil, nil, publicCatalogJSON))
	for _, k := range []string{"LOW", "MEDIUM", "HIGH", "CASE_WATCH", "CASE_PRO"} {
		if p := noCheckout[k]; p.Purchasable || p.Availability == "AVAILABLE" {
			t.Fatalf("%s purchasable without checkout configured: %+v", k, p)
		}
	}
}

// Fail safe: missing service auth, missing billing, or an empty catalog never invents prices.
func TestPublicCatalogFailsSafe(t *testing.T) {
	a := publicCatalogApp(t, billing.NewFakeProvider(), nil, publicCatalogJSON)
	for _, auth := range []string{"", "wrong-secret"} {
		if rr := getPublicCatalog(a, auth, ""); rr.Code != http.StatusUnauthorized {
			t.Fatalf("service auth %q: %d", auth, rr.Code)
		}
	}
	for name, app := range map[string]*App{
		"no billing":    {Config: &config.Config{WebsiteAPISecret: publicCatalogSecret}},
		"empty catalog": publicCatalogApp(t, billing.NewFakeProvider(), nil, ""),
	} {
		rr := getPublicCatalog(app, publicCatalogSecret, "")
		if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "BILLING_UNAVAILABLE") ||
			rr.Header().Get("Cache-Control") != "no-store" || strings.Contains(rr.Body.String(), "amountCents") {
			t.Fatalf("%s: %d %s %v", name, rr.Code, rr.Body.String(), rr.Header())
		}
	}
}

// The authenticated plans endpoint and checkout keep requiring a synced acting user.
func TestPublicCatalogLeavesAuthenticatedBillingUnchanged(t *testing.T) {
	a := publicCatalogApp(t, billing.NewFakeProvider(), nil, publicCatalogJSON)
	req := httptest.NewRequest(http.MethodGet, "/api/saas/billing/plans", nil)
	req.Header.Set("Authorization", "Bearer "+publicCatalogSecret)
	rr := httptest.NewRecorder()
	a.handleBillingPlans(rr, req)
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "missing acting user") {
		t.Fatalf("authenticated plans endpoint without acting user: %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/saas/organizations/1/billing/checkout", strings.NewReader(`{"planKey":"LOW","interval":"MONTHLY"}`))
	req.SetPathValue("organizationID", "1")
	req.Header.Set("Authorization", "Bearer "+publicCatalogSecret)
	rr = httptest.NewRecorder()
	a.handleBillingCheckout(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous checkout: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPublicCatalogIsRateLimited(t *testing.T) {
	a := publicCatalogApp(t, billing.NewFakeProvider(), nil, publicCatalogJSON)
	a.saasPublicCatalogLimiter = newSaaSRateLimiter(time.Minute, 2)
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if rr := getPublicCatalog(a, publicCatalogSecret, ""); rr.Code != want {
			t.Fatalf("request %d: %d, want %d", i+1, rr.Code, want)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
