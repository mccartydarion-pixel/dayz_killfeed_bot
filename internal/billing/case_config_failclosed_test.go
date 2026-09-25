package billing

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// Staging readiness: every unsafe or incomplete C.A.S.E. configuration must
// either refuse to start or leave nothing purchasable; none may sell.
func TestCaseConfigurationFailsClosed(t *testing.T) {
	prices := map[casebilling.Tier]string{casebilling.Watch: "price_watch", casebilling.Pro: "price_pro"}
	cases := []struct {
		name        string
		provider    Provider
		webhook     string
		keyMode     StripeKeyMode
		requireTest bool
		opts        CaseOptions
		wantErr     bool
	}{
		{"all defaults (production today)", NewFakeProvider(), "whsec_x", StripeKeyUnknown, false, CaseOptions{PriceIDs: prices}, false},
		{"sales without access", NewFakeProvider(), "whsec_x", StripeKeyUnknown, false, CaseOptions{Enabled: true, VerifiedThrough: casebilling.Pro, PriceIDs: prices}, true},
		{"access without verified tier", NewFakeProvider(), "whsec_x", StripeKeyUnknown, false, CaseOptions{AccessEnabled: true, PriceIDs: prices}, true},
		{"sales without Stripe key", nil, "whsec_x", StripeKeyUnknown, false, CaseOptions{Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro, PriceIDs: prices}, true},
		{"sales without Watch price", NewFakeProvider(), "whsec_x", StripeKeyUnknown, false, CaseOptions{Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro, PriceIDs: map[casebilling.Tier]string{casebilling.Pro: "price_pro"}}, true},
		{"unknown verified tier", NewFakeProvider(), "whsec_x", StripeKeyUnknown, false, CaseOptions{AccessEnabled: true, VerifiedThrough: "CASE_GOLD", PriceIDs: prices}, true},
		{"duplicate price ids", NewFakeProvider(), "whsec_x", StripeKeyUnknown, false, CaseOptions{PriceIDs: map[casebilling.Tier]string{casebilling.Watch: "price_x", casebilling.Pro: "price_x"}}, true},
		// Charging without a way to confirm payment would bill customers who
		// can never be activated: sales require the webhook signing secret.
		{"sales without webhook secret", NewFakeProvider(), "", StripeKeyUnknown, false, CaseOptions{Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro, PriceIDs: prices}, true},
		// A non-production environment must never run on a live key.
		{"staging with live key", NewFakeProvider(), "whsec_x", StripeKeyLive, true, CaseOptions{PriceIDs: prices}, true},
		{"staging with unknown key mode", NewFakeProvider(), "whsec_x", StripeKeyUnknown, true, CaseOptions{PriceIDs: prices}, true},
		{"staging test key, sales on", NewFakeProvider(), "whsec_x", StripeKeyTest, true, CaseOptions{Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro, PriceIDs: prices}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(newFakeStore(), nil, tc.provider, Options{WebhookSecret: tc.webhook})
			err := s.ConfigureCaseAddons(&caseTestStore{}, CaseOptions{
				Enabled: tc.opts.Enabled, AccessEnabled: tc.opts.AccessEnabled, VerifiedThrough: tc.opts.VerifiedThrough,
				PriceIDs: tc.opts.PriceIDs, StripeKeyMode: tc.keyMode, RequireTestMode: tc.requireTest,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("configure error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if !tc.opts.Enabled {
				for _, p := range s.CasePlans() {
					if p.Purchasable {
						t.Fatalf("%s purchasable with sales off", p.Tier)
					}
				}
				r := httptest.NewRequest("POST", "/", nil)
				if _, err := s.CaseCheckout(context.Background(), r, 10, CaseCheckoutRequest{InstallationID: 20, Tier: casebilling.Watch}, nil); !errors.Is(err, ErrCaseDisabled) {
					t.Fatalf("checkout with sales off: %v", err)
				}
				for _, c := range tc.provider.(*FakeProvider).Calls {
					if c.Method == "CreateCaseCheckoutSession" || c.Method == "ValidateCasePrice" {
						t.Fatalf("disabled checkout called Stripe: %s", c.Method)
					}
				}
			}
		})
	}
}

func TestStripeKeyModeClassification(t *testing.T) {
	for key, want := range map[string]StripeKeyMode{
		"sk_test_abc": StripeKeyTest, "rk_test_abc": StripeKeyTest,
		"sk_live_abc": StripeKeyLive, "rk_live_abc": StripeKeyLive,
		"": StripeKeyUnknown, "pk_test_abc": StripeKeyUnknown, "whsec_abc": StripeKeyUnknown, " sk_test_abc": StripeKeyTest,
	} {
		if got := ClassifyStripeKey(key); got != want {
			t.Errorf("%q: %v, want %v", key, got, want)
		}
	}
}
