// Package casebilling defines the isolated, per-server C.A.S.E. commercial
// vocabulary. It does not modify or infer entitlements from the LOW/MEDIUM/HIGH
// base subscription catalog.
package casebilling

import "strings"

type Tier string

const (
	Watch   Tier = "CASE_WATCH"
	Pro     Tier = "CASE_PRO"
	Command Tier = "CASE_COMMAND"
)

// Capability keys are additive. A tier must also pass Resolve's independent
// account, installation, payment, verification and rollout checks.
type Capability string

const (
	CapWatch   Capability = "case.watch"
	CapPro     Capability = "case.pro"
	CapCommand Capability = "case.command"
)

// Plan is public catalog *description*, not purchase authority. Stripe Price
// IDs must be configured on the backend and verified with Stripe before a plan
// can be offered for purchase. The first Phase 6 milestone ships closed.
type Plan struct {
	Tier        Tier
	Name        string
	AmountCents int64
	Currency    string
	Interval    string
	Purchasable bool
	// Description and Features are the approved public marketing copy. They describe only
	// shipped, observation-only behaviour: C.A.S.E. never claims to detect, confirm or punish
	// cheating.
	Description string
	Features    []string
}

var catalog = []Plan{
	{Tier: Watch, Name: "C.A.S.E. Watch", AmountCents: 499, Currency: "usd", Interval: "month", Purchasable: false,
		Description: "Per-server C.A.S.E. observation for staff: persisted ADM source coverage delivered privately.",
		Features: []string{
			"Staff-requested observation digest for the selected server",
			"Delivered only to your private C.A.S.E. staff channel",
			"Source observations only - no automated verdicts or enforcement",
		}},
	{Tier: Pro, Name: "C.A.S.E. Pro", AmountCents: 999, Currency: "usd", Interval: "month", Purchasable: false,
		Description: "Everything in Watch, plus bounded evidence export for staff review.",
		Features: []string{
			"Everything in C.A.S.E. Watch",
			"Bounded, source-provenanced evidence export (CSV)",
			"Source observations only - no automated verdicts or enforcement",
		}},
	{Tier: Command, Name: "C.A.S.E. Command", AmountCents: 1499, Currency: "usd", Interval: "month", Purchasable: false,
		Description: "Reserved for a future release; not yet available.",
		Features:    []string{}},
}

// Plans returns a detached snapshot, so callers cannot mutate the catalog.
func Plans() []Plan {
	out := make([]Plan, len(catalog))
	copy(out, catalog)
	for i := range out {
		out[i].Features = append([]string{}, catalog[i].Features...)
	}
	return out
}

// Lookup parses an exact stable tier key, case-insensitively. It never treats a
// LOW/MEDIUM/HIGH base-plan name or an unrecognized future key as C.A.S.E.
func Lookup(raw string) (Plan, bool) {
	key := Tier(strings.ToUpper(strings.TrimSpace(raw)))
	for _, plan := range catalog {
		if plan.Tier == key {
			plan.Features = append([]string{}, plan.Features...)
			return plan, true
		}
	}
	return Plan{}, false
}

func tierCapabilities(tier Tier) []Capability {
	switch tier {
	case Watch:
		return []Capability{CapWatch}
	case Pro:
		return []Capability{CapWatch, CapPro}
	case Command:
		return []Capability{CapWatch, CapPro, CapCommand}
	default:
		return nil
	}
}

// Rank orders tiers for upgrade/downgrade decisions; 0 is not a C.A.S.E. tier.
func Rank(tier Tier) int {
	return len(tierCapabilities(tier))
}
