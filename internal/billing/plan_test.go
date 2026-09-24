package billing

import (
	"strings"
	"testing"
)

func TestLoadCatalogEmptyIsValid(t *testing.T) {
	for _, raw := range []string{"", "   ", "[]"} {
		c, err := LoadCatalog(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if !c.Empty() || len(c.PublicPlans()) != 0 || len(c.Plans()) != 0 {
			t.Fatalf("%q: expected an empty catalog, got %+v", raw, c.Plans())
		}
		if _, ok := c.Get("PRO"); ok {
			t.Fatalf("%q: no plan should resolve from an empty catalog", raw)
		}
	}
}

const sampleCatalog = `[
  {"key":"pro","name":"Pro","description":"desc","features":["killfeed","leaderboards"],"limits":{"installations":3},
   "monthly":{"amountCents":1999,"currency":"usd","stripePriceId":"price_pro_month"},
   "yearly":{"amountCents":19990,"currency":"usd","stripePriceId":"price_pro_year"},
   "isPublic":true,"sortOrder":2,"popular":true,"trialDays":14},
  {"key":"starter","name":"Starter","monthly":{"amountCents":999,"currency":"usd","stripePriceId":"price_starter_month"},"sortOrder":1,"trialDays":7},
  {"key":"legacy","name":"Legacy","monthly":{"amountCents":500,"currency":"usd","stripePriceId":"price_legacy_month"},"isPublic":false,"sortOrder":0}
]`

func TestLoadCatalogParsesAndSorts(t *testing.T) {
	c, err := LoadCatalog(sampleCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if c.Empty() {
		t.Fatal("expected a non-empty catalog")
	}
	all := c.Plans()
	if len(all) != 3 {
		t.Fatalf("expected 3 plans, got %d", len(all))
	}
	// sorted by SortOrder: legacy(0), starter(1), pro(2)
	if all[0].Key != "LEGACY" || all[1].Key != "STARTER" || all[2].Key != "PRO" {
		t.Fatalf("sort order: %v", []string{all[0].Key, all[1].Key, all[2].Key})
	}
	pub := c.PublicPlans()
	if len(pub) != 2 {
		t.Fatalf("expected 2 public plans (legacy is private), got %d", len(pub))
	}
	for _, p := range pub {
		if p.Key == "LEGACY" {
			t.Fatal("legacy must not appear in the public plan list")
		}
	}
	pro, ok := c.Get("pro") // case-insensitive
	if !ok || pro.Name != "Pro" || pro.TrialDays != 0 || !pro.Popular || pro.Limits["installations"] != 3 {
		t.Fatalf("pro: %+v ok=%v", pro, ok)
	}
	if m := pro.Price("monthly"); m == nil || m.AmountCents != 1999 || m.Currency != "usd" || m.StripePriceID != "price_pro_month" {
		t.Fatalf("pro monthly: %+v", m)
	}
	if y := pro.Price("YEARLY"); y == nil || y.AmountCents != 19990 {
		t.Fatalf("pro yearly: %+v", y)
	}
	starter, _ := c.Get("STARTER")
	if starter.Price("YEARLY") != nil {
		t.Fatal("starter is not sold yearly - Price must return nil, never fabricate one")
	}
	if starter.Price("WEEKLY") != nil {
		t.Fatal("an unsupported interval must return nil")
	}
	// reverse lookup
	if p, interval, ok := c.PlanForPrice("price_pro_year"); !ok || p.Key != "PRO" || interval != "YEARLY" {
		t.Fatalf("PlanForPrice: %+v %s %v", p, interval, ok)
	}
	if _, _, ok := c.PlanForPrice("price_unknown"); ok {
		t.Fatal("an unknown price id must not resolve")
	}
	// a private plan still resolves by Get (billing.Service checks IsPublic itself before selling it)
	legacy, ok := c.Get("legacy")
	if !ok || legacy.IsPublic {
		t.Fatalf("legacy: %+v ok=%v", legacy, ok)
	}
}

func TestLoadCatalogRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]string{
		"not json":                             `{not json`,
		"missing key":                          `[{"name":"x","monthly":{"amountCents":1,"currency":"usd","stripePriceId":"p1"}}]`,
		"duplicate key":                        `[{"key":"pro","monthly":{"amountCents":1,"currency":"usd","stripePriceId":"p1"}},{"key":"PRO","monthly":{"amountCents":2,"currency":"usd","stripePriceId":"p2"}}]`,
		"negative amount":                      `[{"key":"pro","monthly":{"amountCents":-1,"currency":"usd","stripePriceId":"p1"}}]`,
		"missing currency":                     `[{"key":"pro","monthly":{"amountCents":1,"stripePriceId":"p1"}}]`,
		"missing stripe price":                 `[{"key":"pro","monthly":{"amountCents":1,"currency":"usd"}}]`,
		"negative trial":                       `[{"key":"pro","trialDays":-1,"monthly":{"amountCents":1,"currency":"usd","stripePriceId":"p1"}}]`,
		"reused stripe price":                  `[{"key":"pro","monthly":{"amountCents":1,"currency":"usd","stripePriceId":"shared"}},{"key":"elite","monthly":{"amountCents":2,"currency":"usd","stripePriceId":"shared"}}]`,
		"same price two intervals of one plan": `[{"key":"pro","monthly":{"amountCents":1,"currency":"usd","stripePriceId":"shared"},"yearly":{"amountCents":2,"currency":"usd","stripePriceId":"shared"}}]`,
	}
	for name, raw := range cases {
		if _, err := LoadCatalog(raw); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestLoadCatalogPlanWithNoPriceIsInfrastructureOnly(t *testing.T) {
	// A plan may be defined with no interval priced at all (pure metadata - a "coming soon" row);
	// LoadCatalog must not invent a price for it, and Price() must say so for every interval.
	c, err := LoadCatalog(`[{"key":"enterprise","name":"Enterprise","description":"contact us"}]`)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.Get("enterprise")
	if !ok {
		t.Fatal("expected the plan to exist")
	}
	if p.Price("MONTHLY") != nil || p.Price("YEARLY") != nil {
		t.Fatalf("a plan with no configured price must never resolve one: %+v", p)
	}
}

func TestLoadCatalogDoesNotChokeOnWhitespace(t *testing.T) {
	if _, err := LoadCatalog("  " + sampleCatalog + "  \n"); err != nil {
		t.Fatal(err)
	}
}

func TestPlansAreImmutableCopies(t *testing.T) {
	c, err := LoadCatalog(sampleCatalog)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Plans()
	got[0].Name = "MUTATED"
	again := c.Plans()
	if again[0].Name == "MUTATED" {
		t.Fatal("Plans() must return a copy, not the internal slice")
	}
}

func TestFeaturesSliceIsNotSharedWithInput(t *testing.T) {
	raw := `[{"key":"pro","features":["a","b"],"monthly":{"amountCents":1,"currency":"usd","stripePriceId":"p1"}}]`
	c, err := LoadCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.Get("pro")
	if strings.Join(p.Features, ",") != "a,b" {
		t.Fatalf("features: %v", p.Features)
	}
}
