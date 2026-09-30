package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/config"
)

const publicCatalogTestSecret = "public-catalog-test-secret"

// publicCatalogTestJSON mirrors the approved catalog plus one private plan and one plan with a
// yearly price and an underscored key, so the mapping of every field is exercised.
const publicCatalogTestJSON = `[
  {"key": "LOW", "name": "Low Tier", "description": "Up to 32 slots.", "features": ["Killfeed","Leaderboards"],
   "limits": {"installations": 1, "maxSlots": 32},
   "monthly": {"amountCents": 599, "currency": "usd", "stripePriceId": "price_low_m"},
   "isPublic": true, "sortOrder": 1, "popular": false, "trialDays": 0},
  {"key": "MEDIUM", "name": "Medium Tier", "description": "33 to 64 slots.", "features": ["Killfeed"],
   "limits": {"installations": 1, "maxSlots": 64},
   "monthly": {"amountCents": 999, "currency": "usd", "stripePriceId": "price_med_m"},
   "yearly": {"amountCents": 9990, "currency": "usd", "stripePriceId": "price_med_y"},
   "isPublic": true, "sortOrder": 2, "popular": true, "trialDays": 0},
  {"key": "LEGACY_OLD", "name": "Legacy", "description": "Not sold.", "features": [],
   "limits": {"installations": 1, "maxSlots": 32},
   "monthly": {"amountCents": 100, "currency": "usd", "stripePriceId": "price_legacy_m"},
   "isPublic": false, "sortOrder": 9, "popular": false, "trialDays": 0}
]`

func newPublicCatalogApp(t *testing.T, catalogJSON string) *App {
	t.Helper()
	a := &App{Config: &config.Config{WebsiteAPISecret: publicCatalogTestSecret}}
	if catalogJSON != "" {
		cat, err := billing.LoadCatalog(catalogJSON)
		if err != nil {
			t.Fatalf("catalog must parse: %v", err)
		}
		a.Billing = billing.NewService(nil, cat, billing.NewFakeProvider(), billing.Options{WebhookSecret: "whsec_test"})
	}
	return a
}

func publicCatalogGet(a *App, authorization, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/saas/billing/public-plans", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rr := httptest.NewRecorder()
	a.handleBillingPublicPlans(rr, req)
	return rr
}

func TestPublicPlansRequiresServiceAuthOnly(t *testing.T) {
	a := newPublicCatalogApp(t, publicCatalogTestJSON)
	if rr := publicCatalogGet(a, "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: want 401, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := publicCatalogGet(a, "Bearer wrong", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: want 401, got %d", rr.Code)
	}
	// The service token alone is enough: no acting-user header, unlike /api/saas/billing/plans.
	if rr := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, ""); rr.Code != http.StatusOK {
		t.Fatalf("service bearer without acting user: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestPublicPlansReturnsWebsiteCatalogShape(t *testing.T) {
	a := newPublicCatalogApp(t, publicCatalogTestJSON)
	rr := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != publicCatalogCacheControl {
		t.Errorf("Cache-Control = %q", cc)
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("price_")) || bytes.Contains(rr.Body.Bytes(), []byte("stripePriceId")) {
		t.Fatalf("public catalog must never leak a Stripe price id: %s", rr.Body.String())
	}

	var resp publicCatalogResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != 1 {
		t.Errorf("version = %d, want 1", resp.Version)
	}
	if len(resp.Plans) != 2 {
		t.Fatalf("want the 2 public plans only (private LEGACY_OLD excluded), got %+v", resp.Plans)
	}

	low := resp.Plans[0]
	if low.Key != "LOW" || low.Category != "BASE" || low.Name != "Low Tier" || low.DisplayTier != "Low" || low.Description != "Up to 32 slots." {
		t.Errorf("LOW identity fields: %+v", low)
	}
	if len(low.Features) != 2 || low.Features[0] != "Killfeed" {
		t.Errorf("LOW features: %v", low.Features)
	}
	if len(low.Prices) != 1 || low.Prices[0] != (publicCatalogPriceDTO{Interval: "MONTHLY", AmountCents: 599, Currency: "usd"}) {
		t.Errorf("LOW prices: %+v", low.Prices)
	}
	if low.Availability != "AVAILABLE" || !low.Purchasable || low.Popular || low.SortOrder != 1 || low.PerServer || low.RequiresBasePlan {
		t.Errorf("LOW flags: %+v", low)
	}

	medium := resp.Plans[1]
	if medium.Key != "MEDIUM" || !medium.Popular || medium.SortOrder != 2 {
		t.Errorf("MEDIUM identity: %+v", medium)
	}
	if len(medium.Prices) != 2 || medium.Prices[0].Interval != "MONTHLY" || medium.Prices[1] != (publicCatalogPriceDTO{Interval: "YEARLY", AmountCents: 9990, Currency: "usd"}) {
		t.Errorf("MEDIUM prices: %+v", medium.Prices)
	}
}

func TestPublicPlansETagRevalidation(t *testing.T) {
	a := newPublicCatalogApp(t, publicCatalogTestJSON)
	first := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, "")
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" {
		t.Fatalf("first response: %d, ETag %q", first.Code, etag)
	}
	second := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, etag)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("matching If-None-Match: want 304 with empty body, got %d %q", second.Code, second.Body.String())
	}
	if second.Header().Get("ETag") != etag {
		t.Errorf("304 must repeat the ETag, got %q", second.Header().Get("ETag"))
	}
	stale := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, `"stale"`)
	if stale.Code != http.StatusOK {
		t.Fatalf("non-matching If-None-Match: want 200, got %d", stale.Code)
	}
}

func TestPublicPlansWithoutBillingIs503(t *testing.T) {
	a := newPublicCatalogApp(t, "")
	rr := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no billing service: want 503, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestPublicPlansEmptyCatalogIsAnEmptyList(t *testing.T) {
	a := newPublicCatalogApp(t, `[]`)
	rr := publicCatalogGet(a, "Bearer "+publicCatalogTestSecret, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Version int               `json:"version"`
		Plans   []json.RawMessage `json:"plans"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Plans == nil || len(resp.Plans) != 0 {
		t.Fatalf("plans must be an empty JSON array, not null: %s", rr.Body.String())
	}
}

func TestDisplayTierFromKey(t *testing.T) {
	for in, want := range map[string]string{"LOW": "Low", "MEDIUM": "Medium", "CASE_COMMAND": "Case Command", " high ": "High", "": ""} {
		if got := displayTierFromKey(in); got != want {
			t.Errorf("displayTierFromKey(%q) = %q, want %q", in, got, want)
		}
	}
}
