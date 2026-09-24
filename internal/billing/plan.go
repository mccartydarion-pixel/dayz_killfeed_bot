// Package billing is the Champion Billing service (docs/BILLING.md): Stripe customers, hosted
// Checkout, the Customer Portal, webhook reconciliation and the authoritative plan catalog, on top
// of the existing subscriptions table (docs/SAAS_SCHEMA.md) - there is no second subscription
// system. All Stripe-specific code lives in this package (plan.go, status.go, provider.go,
// stripe_provider.go, service.go); HTTP handlers (internal/app/saas_api_billing.go) only translate
// requests and never import the Stripe SDK.
package billing

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PriceMeta is one billing interval's approved price for a plan. AmountCents is a whole number of
// the smallest currency unit (cents for USD) - never a float, never trusted from a browser.
type PriceMeta struct {
	AmountCents   int64  `json:"amountCents"`
	Currency      string `json:"currency"`      // ISO 4217, lowercase (Stripe convention), e.g. "usd"
	StripePriceID string `json:"stripePriceId"` // the approved Stripe Price object for this plan+interval
}

// Plan is one authoritative catalog entry. Monthly/Yearly are nil when that interval is not sold
// for this plan (Phase 1 ships with no interval priced until it is configured - see LoadCatalog).
type Plan struct {
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Features    []string       `json:"features"`
	Limits      map[string]int `json:"limits"`
	Monthly     *PriceMeta     `json:"monthly,omitempty"`
	Yearly      *PriceMeta     `json:"yearly,omitempty"`
	IsPublic    bool           `json:"isPublic"`
	SortOrder   int            `json:"sortOrder"`
	Popular     bool           `json:"popular"`
	// TrialDays is the Stripe Checkout trial length. Always 0 since Onboarding V2: the only
	// trial is the one no-card 14-day trial (TrialDays const in trial.go), and a paid activation
	// never carries a second Stripe trial. Kept for API compatibility.
	TrialDays int `json:"trialDays"`
}

// Price returns the plan's price for interval ("MONTHLY"/"YEARLY"), or nil if that interval isn't
// sold for this plan.
func (p Plan) Price(interval string) *PriceMeta {
	switch strings.ToUpper(strings.TrimSpace(interval)) {
	case "MONTHLY":
		return p.Monthly
	case "YEARLY":
		return p.Yearly
	}
	return nil
}

// Catalog is the loaded, validated plan list. It is immutable after LoadCatalog returns.
type Catalog struct {
	plans   []Plan
	byKey   map[string]Plan
	byPrice map[string]pricedPlan // Stripe price id -> (plan, interval)
}

type pricedPlan struct {
	plan     Plan
	interval string
}

// Plans returns every configured plan, sorted by SortOrder then key.
func (c *Catalog) Plans() []Plan {
	if c == nil {
		return nil
	}
	out := make([]Plan, len(c.plans))
	copy(out, c.plans)
	return out
}

// PublicPlans returns only IsPublic plans, in catalog order - what the pricing page shows.
func (c *Catalog) PublicPlans() []Plan {
	if c == nil {
		return nil
	}
	out := make([]Plan, 0, len(c.plans))
	for _, p := range c.plans {
		if p.IsPublic {
			out = append(out, p)
		}
	}
	return out
}

// Get returns the plan for key (case-insensitive), and whether it exists at all (public or not -
// callers that must not let a customer buy a private/legacy plan check IsPublic separately).
func (c *Catalog) Get(key string) (Plan, bool) {
	if c == nil {
		return Plan{}, false
	}
	p, ok := c.byKey[strings.ToUpper(strings.TrimSpace(key))]
	return p, ok
}

// PlanForPrice reverse-resolves a Stripe price id (as seen on a webhook or a Checkout Session) back
// to the Champion plan key and interval it belongs to. Used only to label incoming Stripe state -
// never to decide what a customer may buy (that's always Get(key) + Price(interval)).
func (c *Catalog) PlanForPrice(stripePriceID string) (plan Plan, interval string, ok bool) {
	if c == nil || stripePriceID == "" {
		return Plan{}, "", false
	}
	pp, ok := c.byPrice[stripePriceID]
	return pp.plan, pp.interval, ok
}

// Empty reports whether no plan is configured at all (the "PRICING DECISION REQUIRED" state:
// docs/BILLING.md). The plans API and checkout both work correctly in this state - the API returns
// an empty list and checkout returns ErrUnknownPlan for anything - but callers use this to log/report
// it distinctly from "this specific key doesn't exist".
func (c *Catalog) Empty() bool { return c == nil || len(c.plans) == 0 }

// catalogPlan is the JSON shape LoadCatalog reads (CHAMPION_BILLING_PLANS_JSON), one entry per plan.
// It intentionally mirrors Plan field-for-field so operators configure exactly what ships.
type catalogPlan struct {
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Features    []string       `json:"features"`
	Limits      map[string]int `json:"limits"`
	Monthly     *catalogPrice  `json:"monthly"`
	Yearly      *catalogPrice  `json:"yearly"`
	IsPublic    *bool          `json:"isPublic"`
	SortOrder   int            `json:"sortOrder"`
	Popular     bool           `json:"popular"`
	TrialDays   int            `json:"trialDays"`
}

type catalogPrice struct {
	AmountCents   int64  `json:"amountCents"`
	Currency      string `json:"currency"`
	StripePriceID string `json:"stripePriceId"`
}

// LoadCatalog parses CHAMPION_BILLING_PLANS_JSON (a JSON array of catalogPlan; see docs/BILLING.md
// "Pricing configuration handoff" for the exact shape and a worked example). An empty/unset string
// is a valid, deliberate "no plans approved yet" catalog - NOT an error - so the service starts
// cleanly before a commercial pricing decision exists; Load never invents a key, a price or a
// feature list on its own.
//
// Validation (a malformed catalog IS an error, since a broken catalog silently selling nothing, or
// selling the wrong thing, is worse than refusing to start):
//   - every plan needs a non-empty, unique (case-insensitively) Key
//   - AmountCents >= 0, Currency and StripePriceID non-empty whenever a Monthly/Yearly block is present
//   - no two plans may reuse the same Stripe price id (that price id would resolve to two plans)
func LoadCatalog(raw string) (*Catalog, error) {
	raw = strings.TrimSpace(raw)
	c := &Catalog{byKey: map[string]Plan{}, byPrice: map[string]pricedPlan{}}
	if raw == "" {
		return c, nil
	}
	var entries []catalogPlan
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON: invalid JSON: %w", err)
	}
	for i, e := range entries {
		key := strings.ToUpper(strings.TrimSpace(e.Key))
		if key == "" {
			return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON[%d]: key is required", i)
		}
		if _, dup := c.byKey[key]; dup {
			return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON[%d]: duplicate plan key %q", i, key)
		}
		p := Plan{Key: key, Name: strings.TrimSpace(e.Name), Description: strings.TrimSpace(e.Description),
			Features: append([]string{}, e.Features...), Limits: e.Limits, SortOrder: e.SortOrder, Popular: e.Popular, IsPublic: true}
		if e.IsPublic != nil {
			p.IsPublic = *e.IsPublic
		}
		if p.Name == "" {
			p.Name = key
		}
		// A configured trialDays is still validated (a typo should fail loudly) but never used:
		// Stripe trial days are always 0 (docs/BILLING.md "No-card trial").
		if e.TrialDays < 0 {
			return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON[%d] %s: trialDays must not be negative", i, key)
		}
		var err error
		if p.Monthly, err = validatePrice(e.Monthly, "monthly", key); err != nil {
			return nil, err
		}
		if p.Yearly, err = validatePrice(e.Yearly, "yearly", key); err != nil {
			return nil, err
		}
		if p.Monthly != nil && p.Yearly != nil && p.Monthly.StripePriceID == p.Yearly.StripePriceID {
			return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON %s: monthly and yearly must not share the same Stripe price %q", key, p.Monthly.StripePriceID)
		}
		for _, entry := range []struct {
			pm       *PriceMeta
			interval string
		}{{p.Monthly, "MONTHLY"}, {p.Yearly, "YEARLY"}} {
			if entry.pm == nil {
				continue
			}
			if other, dup := c.byPrice[entry.pm.StripePriceID]; dup {
				return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON %s: Stripe price %q is already used by plan %s", key, entry.pm.StripePriceID, other.plan.Key)
			}
			c.byPrice[entry.pm.StripePriceID] = pricedPlan{p, entry.interval}
		}
		c.byKey[key] = p
		c.plans = append(c.plans, p)
	}
	sort.SliceStable(c.plans, func(i, j int) bool {
		if c.plans[i].SortOrder != c.plans[j].SortOrder {
			return c.plans[i].SortOrder < c.plans[j].SortOrder
		}
		return c.plans[i].Key < c.plans[j].Key
	})
	return c, nil
}

func validatePrice(p *catalogPrice, interval, key string) (*PriceMeta, error) {
	if p == nil {
		return nil, nil
	}
	if p.AmountCents < 0 {
		return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON %s.%s: amountCents must not be negative", key, interval)
	}
	if strings.TrimSpace(p.Currency) == "" {
		return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON %s.%s: currency is required", key, interval)
	}
	if strings.TrimSpace(p.StripePriceID) == "" {
		return nil, fmt.Errorf("CHAMPION_BILLING_PLANS_JSON %s.%s: stripePriceId is required", key, interval)
	}
	return &PriceMeta{AmountCents: p.AmountCents, Currency: strings.ToLower(strings.TrimSpace(p.Currency)), StripePriceID: strings.TrimSpace(p.StripePriceID)}, nil
}
