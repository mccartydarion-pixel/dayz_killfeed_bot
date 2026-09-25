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
	Tier         Tier
	Name         string
	AmountCents  int64
	Currency     string
	Interval     string
	Purchasable  bool
}

var catalog = []Plan{
	{Tier: Watch, Name: "C.A.S.E. Watch", AmountCents: 499, Currency: "usd", Interval: "month", Purchasable: false},
	{Tier: Pro, Name: "C.A.S.E. Pro", AmountCents: 999, Currency: "usd", Interval: "month", Purchasable: false},
	{Tier: Command, Name: "C.A.S.E. Command", AmountCents: 1499, Currency: "usd", Interval: "month", Purchasable: false},
}

// Plans returns a detached snapshot, so callers cannot mutate the catalog.
func Plans() []Plan {
	out := make([]Plan, len(catalog))
	copy(out, catalog)
	return out
}

// Lookup parses an exact stable tier key, case-insensitively. It never treats a
// LOW/MEDIUM/HIGH base-plan name or an unrecognized future key as C.A.S.E.
func Lookup(raw string) (Plan, bool) {
	key := Tier(strings.ToUpper(strings.TrimSpace(raw)))
	for _, plan := range catalog {
		if plan.Tier == key {
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
