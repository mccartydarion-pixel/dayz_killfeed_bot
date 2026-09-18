// Package entitlements centralizes SaaS feature-key resolution by plan, so
// gating a feature never means adding a per-installation boolean column
// (section 13 of the SaaS foundation task). It mirrors the website's own
// centralized entitlement design, sharing the same feature key vocabulary.
//
// Paid access is not enforced yet: Resolve is a lookup helper for future
// gating, not an access-control decision made during this phase. Plan tier
// definitions (which keys each paid tier actually grants) are a business
// decision for billing integration to make - Resolve returns the full
// feature set for every plan until then, so nothing in the product is
// gated by a plan name that doesn't exist yet.
package entitlements

import "strings"

// Key is one gate-able Champion feature.
type Key string

const (
	Killfeed         Key = "killfeed"
	Leaderboards     Key = "leaderboards"
	LivePlayers      Key = "live_players"
	WebsiteDashboard Key = "website_dashboard"
	DiscordActivity  Key = "discord_activity"
	SpecialKills     Key = "special_kills"
	AdvancedStats    Key = "advanced_stats"
	MultipleServers  Key = "multiple_servers"
	PrioritySupport  Key = "priority_support"
)

// allKeys is every entitlement key Champion currently defines.
var allKeys = []Key{
	Killfeed, Leaderboards, LivePlayers, WebsiteDashboard,
	DiscordActivity, SpecialKills, AdvancedStats, MultipleServers, PrioritySupport,
}

// Resolve returns the feature keys granted by plan. See the package doc:
// every known plan currently resolves to the full feature set, since paid
// access is not enforced yet - callers should still key their gating logic
// off the returned keys (not "is a plan configured"), so tightening this
// later needs no call-site changes.
func Resolve(plan string) []Key {
	_ = strings.TrimSpace(plan) // plan is accepted now so call sites don't need to change once tiers are defined
	out := make([]Key, len(allKeys))
	copy(out, allKeys)
	return out
}

// Has reports whether plan grants key.
func Has(plan string, key Key) bool {
	for _, k := range Resolve(plan) {
		if k == key {
			return true
		}
	}
	return false
}
