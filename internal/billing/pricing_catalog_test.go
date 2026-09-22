package billing

import "testing"

// approvedPricingCatalogJSON is the exact, commercially approved LOW/MEDIUM/HIGH catalog (Champion
// Billing Phase 1.2). It must be used byte-for-byte as approved - this file only verifies it loads
// and resolves the way docs/BILLING.md's "Pricing configuration handoff" promises; it never invents
// or adjusts a price, limit or id. Stripe Price ids are test-mode (Phase 1.2: "do not switch Champion
// to live Stripe billing yet").
const approvedPricingCatalogJSON = `[
  {
    "key": "LOW",
    "name": "Low Tier",
    "description": "For smaller DayZ communities with up to 32 player slots.",
    "features": [
      "Killfeed",
      "Faction Hub",
      "Leaderboards",
      "Champion Points Economy",
      "Champion Shop",
      "Embed Designer",
      "Discord Integration",
      "Nitrado Integration"
    ],
    "limits": {
      "installations": 1,
      "maxSlots": 32
    },
    "monthly": {
      "amountCents": 599,
      "currency": "usd",
      "stripePriceId": "price_1UIQiD65uHRSytQgoMRICl5h"
    },
    "isPublic": true,
    "sortOrder": 1,
    "popular": false,
    "trialDays": 7
  },
  {
    "key": "MEDIUM",
    "name": "Medium Tier",
    "description": "For growing DayZ communities with 33 to 64 player slots.",
    "features": [
      "Killfeed",
      "Faction Hub",
      "Leaderboards",
      "Champion Points Economy",
      "Champion Shop",
      "Embed Designer",
      "Discord Integration",
      "Nitrado Integration"
    ],
    "limits": {
      "installations": 1,
      "maxSlots": 64
    },
    "monthly": {
      "amountCents": 999,
      "currency": "usd",
      "stripePriceId": "price_1UIQiD65uHRSytQghSQQOVpG"
    },
    "isPublic": true,
    "sortOrder": 2,
    "popular": true,
    "trialDays": 7
  },
  {
    "key": "HIGH",
    "name": "High Tier",
    "description": "For large DayZ communities with 65 to 128 player slots.",
    "features": [
      "Killfeed",
      "Faction Hub",
      "Leaderboards",
      "Champion Points Economy",
      "Champion Shop",
      "Embed Designer",
      "Discord Integration",
      "Nitrado Integration"
    ],
    "limits": {
      "installations": 1,
      "maxSlots": 128
    },
    "monthly": {
      "amountCents": 1499,
      "currency": "usd",
      "stripePriceId": "price_1UIQiD65uHRSytQgydtA4Pzj"
    },
    "isPublic": true,
    "sortOrder": 3,
    "popular": false,
    "trialDays": 7
  }
]`

func TestApprovedPricingCatalogLoadsAndSorts(t *testing.T) {
	cat, err := LoadCatalog(approvedPricingCatalogJSON)
	if err != nil {
		t.Fatalf("approved catalog must parse cleanly: %v", err)
	}
	plans := cat.Plans()
	if len(plans) != 3 {
		t.Fatalf("expected 3 plans, got %d: %+v", len(plans), plans)
	}
	// Public plans API order is by SortOrder: LOW(1), MEDIUM(2), HIGH(3).
	wantOrder := []string{"LOW", "MEDIUM", "HIGH"}
	for i, want := range wantOrder {
		if plans[i].Key != want {
			t.Fatalf("plan[%d]: want %s, got %s (%+v)", i, want, plans[i].Key, plans)
		}
	}
	pub := cat.PublicPlans()
	if len(pub) != 3 {
		t.Fatalf("all three plans must be public: %+v", pub)
	}
}

func TestApprovedPricingCatalogValues(t *testing.T) {
	cat, err := LoadCatalog(approvedPricingCatalogJSON)
	if err != nil {
		t.Fatalf("approved catalog must parse cleanly: %v", err)
	}

	cases := []struct {
		key           string
		amountCents   int64
		maxSlots      int
		popular       bool
		stripePriceID string
	}{
		{"LOW", 599, 32, false, "price_1UIQiD65uHRSytQgoMRICl5h"},
		{"MEDIUM", 999, 64, true, "price_1UIQiD65uHRSytQghSQQOVpG"},
		{"HIGH", 1499, 128, false, "price_1UIQiD65uHRSytQgydtA4Pzj"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			p, ok := cat.Get(c.key)
			if !ok {
				t.Fatalf("expected plan %s to exist", c.key)
			}
			if p.Monthly == nil {
				t.Fatalf("%s: expected a monthly price", c.key)
			}
			if p.Monthly.AmountCents != c.amountCents {
				t.Errorf("%s: monthly amountCents = %d, want %d", c.key, p.Monthly.AmountCents, c.amountCents)
			}
			if p.Monthly.Currency != "usd" {
				t.Errorf("%s: monthly currency = %q, want usd", c.key, p.Monthly.Currency)
			}
			if p.Monthly.StripePriceID != c.stripePriceID {
				t.Errorf("%s: monthly stripePriceId = %q, want %q", c.key, p.Monthly.StripePriceID, c.stripePriceID)
			}
			if p.Yearly != nil {
				t.Errorf("%s: yearly must be unavailable (no annual pricing approved), got %+v", c.key, p.Yearly)
			}
			if p.Price("YEARLY") != nil {
				t.Errorf("%s: Price(YEARLY) must be nil", c.key)
			}
			if p.TrialDays != 7 {
				t.Errorf("%s: trialDays = %d, want 7", c.key, p.TrialDays)
			}
			if p.Limits["maxSlots"] != c.maxSlots {
				t.Errorf("%s: limits.maxSlots = %d, want %d", c.key, p.Limits["maxSlots"], c.maxSlots)
			}
			if p.Limits["installations"] != 1 {
				t.Errorf("%s: limits.installations = %d, want 1", c.key, p.Limits["installations"])
			}
			if p.Popular != c.popular {
				t.Errorf("%s: popular = %v, want %v", c.key, p.Popular, c.popular)
			}
			if !p.IsPublic {
				t.Errorf("%s: expected isPublic = true", c.key)
			}
		})
	}
}

func TestApprovedPricingCatalogPriceIDsAreUniqueAndReverseResolve(t *testing.T) {
	cat, err := LoadCatalog(approvedPricingCatalogJSON)
	if err != nil {
		t.Fatalf("approved catalog must parse cleanly: %v", err)
	}
	for key, priceID := range map[string]string{
		"LOW":    "price_1UIQiD65uHRSytQgoMRICl5h",
		"MEDIUM": "price_1UIQiD65uHRSytQghSQQOVpG",
		"HIGH":   "price_1UIQiD65uHRSytQgydtA4Pzj",
	} {
		plan, interval, ok := cat.PlanForPrice(priceID)
		if !ok || plan.Key != key || interval != "MONTHLY" {
			t.Errorf("PlanForPrice(%q) = %+v, %q, %v; want %s, MONTHLY, true", priceID, plan, interval, ok, key)
		}
	}
}
