package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const stagingCatalog = `[{"key":"LOW","name":"Low","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base_low"},"isPublic":true}]`

func basePrice(id string, amount int64, caseKind bool) stripePrice {
	p := mockPrice(expectedPrice{priceID: id, productID: "prod_base", cents: amount})
	p.Product.Metadata = map[string]string{}
	if caseKind {
		p.Product.Metadata["champion_product_kind"] = "CASE_ADDON"
	}
	return p
}

// stub serves GETs only; any other method fails the test (read-only proof).
func stub(t *testing.T, prices map[string]stripePrice, endpoints []webhookEndpoint) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("preflight issued %s %s", r.Method, r.URL.Path)
		}
		if user, _, _ := r.BasicAuth(); user != "sk_test_stub" {
			t.Errorf("missing test key auth")
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			p, ok := prices[strings.TrimPrefix(r.URL.Path, "/v1/prices/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(p)
		case r.URL.Path == "/v1/webhook_endpoints":
			json.NewEncoder(w).Encode(map[string]any{"data": endpoints})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestBaseCatalogMustLiveInSameSandbox(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prices  map[string]stripePrice
		catalog string
		wantErr string
	}{
		"present":          {map[string]stripePrice{"price_base_low": basePrice("price_base_low", 599, false)}, stagingCatalog, ""},
		"other account":    {map[string]stripePrice{}, stagingCatalog, "does not exist in this sandbox"},
		"wrong amount":     {map[string]stripePrice{"price_base_low": basePrice("price_base_low", 999, false)}, stagingCatalog, "does not match"},
		"is a case product": {map[string]stripePrice{"price_base_low": basePrice("price_base_low", 599, true)}, stagingCatalog, "does not match"},
		"reuses case price": {nil, strings.Replace(stagingCatalog, "price_base_low", "price_watch", 1), "reuses a C.A.S.E. price"},
		"empty catalog":    {nil, `[]`, "no plans"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := stub(t, tc.prices, nil)
			defer srv.Close()
			var out bytes.Buffer
			err := verifyBaseCatalog(ctx, srv.Client(), srv.URL, "sk_test_stub", tc.catalog, specs(), &out)
			if tc.wantErr == "" {
				if err != nil || !strings.Contains(out.String(), "PASS BASE LOW") {
					t.Fatalf("err=%v out=%s", err, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "sk_test_stub") {
				t.Fatal("error leaked the key")
			}
		})
	}
}

func TestStagingWebhookEndpointVerification(t *testing.T) {
	ctx := context.Background()
	live, test := true, false
	const want = "https://case-qa.example/api/saas/billing/webhook"
	good := webhookEndpoint{ID: "we_1", URL: want, Status: "enabled", Livemode: &test, EnabledEvents: requiredWebhookEvents}
	missing := good
	missing.EnabledEvents = requiredWebhookEvents[:4]
	disabled := good
	disabled.Status = "disabled"
	liveEp := good
	liveEp.Livemode = &live
	wildcard := good
	wildcard.EnabledEvents = []string{"*"}
	for name, tc := range map[string]struct {
		eps     []webhookEndpoint
		url     string
		wantErr string
	}{
		"ok":             {[]webhookEndpoint{good}, want, ""},
		"wildcard":       {[]webhookEndpoint{wildcard}, want, ""},
		"missing events": {[]webhookEndpoint{missing}, want, "missing events"},
		"disabled":       {[]webhookEndpoint{disabled}, want, "disabled or not test mode"},
		"live endpoint":  {[]webhookEndpoint{liveEp}, want, "disabled or not test mode"},
		"absent":         {nil, want, "no sandbox webhook endpoint"},
		"wrong path":     {nil, "https://case-qa.example/webhook", "must be https"},
		"plain http":     {nil, "http://case-qa.example/api/saas/billing/webhook", "must be https"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := stub(t, nil, tc.eps)
			defer srv.Close()
			var out bytes.Buffer
			err := verifyWebhookEndpoint(ctx, srv.Client(), srv.URL, "sk_test_stub", tc.url, &out)
			if tc.wantErr == "" {
				if err != nil || !strings.Contains(out.String(), "PASS WEBHOOK") {
					t.Fatalf("err=%v out=%s", err, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
		})
	}
}
