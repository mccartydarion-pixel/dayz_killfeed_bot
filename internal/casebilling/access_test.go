package casebilling

import (
	"reflect"
	"testing"
	"time"
)

func TestCatalogIsClosedAndIsolatedFromBasePlans(t *testing.T) {
	plans := Plans()
	want := []struct {
		key Tier
		price int64
	}{{Watch, 499}, {Pro, 999}, {Command, 1499}}
	if len(plans) != len(want) {
		t.Fatalf("catalog has %d plans, want %d", len(plans), len(want))
	}
	for i, p := range plans {
		if p.Tier != want[i].key || p.AmountCents != want[i].price || p.Currency != "usd" || p.Interval != "month" || p.Purchasable {
			t.Fatalf("plan %d not closed/approved: %+v", i, p)
		}
		got, ok := Lookup(string(p.Tier))
		if !ok || got != p {
			t.Fatalf("lookup %q: %+v %v", p.Tier, got, ok)
		}
	}
	for _, base := range []string{"LOW", "MEDIUM", "HIGH", "TRIAL", "", "CASE_UNKNOWN"} {
		if _, ok := Lookup(base); ok {
			t.Errorf("base/unknown %q must not be recognized as a security add-on", base)
		}
	}
	plans[0].Purchasable = true
	plans[0].AmountCents = 0
	if next := Plans()[0]; next.Purchasable || next.AmountCents != 499 {
		t.Fatalf("caller modified shared security catalog: %+v", next)
	}
}

func TestAccessRequiresSameServerPaidBaseAndVerifiedTier(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	end := now.Add(24 * time.Hour)
	trialEnd := now.Add(12 * time.Hour)
	in := AccessInput{
		BillingEnabled: true, VerifiedThrough: Pro,
		OrganizationID: 10, InstallationID: 20, SelectedGameServerID: 30,
		BaseStatus: "ACTIVE", AddonOrganizationID: 10, AddonInstallationID: 20, BoundGameServerID: 30,
		Tier: Pro, Status: "ACTIVE", Provider: "stripe",
		ProviderSubscriptionID: "sub_test", ProviderPriceID: "price_test", CurrentPeriodEnd: &end, PaidThrough: &end,
		TrialEndsAt: &trialEnd,
	}
	want := []Capability{CapWatch, CapPro}
	if got := Resolve(in, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("valid Pro = %v, want %v", got, want)
	}
	if !Has(in, CapWatch, now) || !Has(in, CapPro, now) || Has(in, CapCommand, now) {
		t.Fatal("Pro grants only Watch+Pro")
	}
	cases := []struct {
		name string
		change func(*AccessInput)
	}{
		{"flag off", func(x *AccessInput) { x.BillingEnabled = false }},
		{"unverified", func(x *AccessInput) { x.VerifiedThrough = "" }},
		{"pro not yet verified", func(x *AccessInput) { x.VerifiedThrough = Watch }},
		{"trial base instead of paid", func(x *AccessInput) { x.BaseStatus = "TRIAL" }},
		{"base payment issue", func(x *AccessInput) { x.BaseStatus = "PAST_DUE" }},
		{"other organization", func(x *AccessInput) { x.AddonOrganizationID++ }},
		{"other installation", func(x *AccessInput) { x.AddonInstallationID++ }},
		{"installation repointed", func(x *AccessInput) { x.SelectedGameServerID++ }},
		{"missing selected game server", func(x *AccessInput) { x.SelectedGameServerID = 0 }},
		{"unknown tier", func(x *AccessInput) { x.Tier = "CASE_FUTURE" }},
		{"unknown provider", func(x *AccessInput) { x.Provider = "manual" }},
		{"missing Stripe subscription", func(x *AccessInput) { x.ProviderSubscriptionID = "" }},
		{"missing Stripe price", func(x *AccessInput) { x.ProviderPriceID = "" }},
		{"expired period", func(x *AccessInput) { x.CurrentPeriodEnd = &now }},
		{"unpaid ACTIVE", func(x *AccessInput) { x.PaidThrough = nil }},
		{"paid coverage expired", func(x *AccessInput) { x.PaidThrough = &now }},
		{"missing period", func(x *AccessInput) { x.CurrentPeriodEnd = nil }},
		{"payment failure", func(x *AccessInput) { x.Status = "PAST_DUE" }},
		{"canceled even inside old period", func(x *AccessInput) { x.Status = "CANCELED" }},
		{"pending checkout", func(x *AccessInput) { x.Status = "PENDING" }},
		{"suspended", func(x *AccessInput) { x.Status = "SUSPENDED" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			modified := in
			tt.change(&modified)
			if got := Resolve(modified, now); len(got) != 0 {
				t.Fatalf("fail-closed expected, got %v", got)
			}
		})
	}
	in.Status = "TRIAL"
	if got := Resolve(in, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("verified Stripe trial = %v, want %v", got, want)
	}
	in.TrialEndsAt = &now
	if got := Resolve(in, now); len(got) != 0 {
		t.Fatalf("expired trial granted %v", got)
	}
	in.Status = "ACTIVE"
}

func TestCommandRequiresExplicitCommandVerification(t *testing.T) {
	now := time.Now()
	end := now.Add(time.Hour)
	in := AccessInput{
		BillingEnabled: true, VerifiedThrough: Pro,
		OrganizationID: 1, InstallationID: 2, SelectedGameServerID: 3, BaseStatus: "ACTIVE",
		AddonOrganizationID: 1, AddonInstallationID: 2, BoundGameServerID: 3,
		Tier: Command, Status: "ACTIVE", Provider: "stripe", ProviderSubscriptionID: "sub_id",
		ProviderPriceID: "price_id", CurrentPeriodEnd: &end,
	}
	if got := Resolve(in, now); len(got) != 0 {
		t.Fatalf("unverified Command granted: %v", got)
	}
	in.VerifiedThrough = Command
	want := []Capability{CapWatch, CapPro, CapCommand}
	if got := Resolve(in, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("verified Command got %v, want %v", got, want)
	}
}
