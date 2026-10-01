// Package entitlements centralizes SaaS feature-key resolution by plan, so
// gating a feature never means adding a per-installation boolean column
// (section 13 of the SaaS foundation task). It mirrors the website's own
// centralized entitlement design, sharing the same feature key vocabulary.
//
// Plan gating is behind one rollout switch (SetEnforced, from
// CHAMPION_PLAN_GATING_ENABLED, default off). Off: every plan resolves to the
// full feature set, exactly as before gating existed. On: the Survivor plan
// (catalog key NORMAL) resolves to the base set and every other plan - Champion
// (PREMIUM), the no-card trial, the retired LOW/MEDIUM/HIGH plans, an
// organization with no subscription row - keeps the full set. Only a plan that is
// explicitly known to be restricted is ever restricted, so an unexpected plan
// string can never take features away from a paying customer.
//
// Subscription status (trial expired, canceled, past due) is decided elsewhere
// (internal/billing); this package answers only "which features does this plan
// include".
package entitlements

import (
	"strings"
	"sync/atomic"
)

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

	// Champion-only features.
	RankedSeasons     Key = "ranked_seasons"     // server ranked seasons and the SERVER_RANKS board
	Bounties          Key = "bounties"           // bounty board and bounty tracking feeds
	Heatmaps          Key = "heatmaps"           // heatmap API and the HEATMAPS board
	Economy           Key = "economy"            // economy wallets, the shop and the ECONOMY/SHOP feeds
	CustomEmbeds      Key = "custom_embeds"      // Embed Designer templates (writes and runtime rendering)
	UnlimitedFactions Key = "unlimited_factions" // no faction cap per installation
)

// PlanSurvivor is the catalog key of the Survivor (Normal) plan, the one plan whose
// feature set is restricted when gating is enforced.
const PlanSurvivor = "NORMAL"

// SurvivorFactionLimit is the most factions one installation may have on Survivor.
const SurvivorFactionLimit = 5

// allKeys is every entitlement key Champion currently defines.
var allKeys = []Key{
	Killfeed, Leaderboards, LivePlayers, WebsiteDashboard,
	DiscordActivity, SpecialKills, AdvancedStats, MultipleServers, PrioritySupport,
	RankedSeasons, Bounties, Heatmaps, Economy, CustomEmbeds, UnlimitedFactions,
}

// survivorKeys is the Survivor feature set: the core killfeed product on one server.
var survivorKeys = []Key{
	Killfeed, Leaderboards, LivePlayers, WebsiteDashboard,
	DiscordActivity, SpecialKills, AdvancedStats,
}

var enforced atomic.Bool

// SetEnforced turns plan gating on or off for the whole process (set once at startup).
func SetEnforced(on bool) { enforced.Store(on) }

// Enforced reports whether plan gating is on.
func Enforced() bool { return enforced.Load() }

func restricted(plan string) bool {
	return enforced.Load() && strings.EqualFold(strings.TrimSpace(plan), PlanSurvivor)
}

// Resolve returns the feature keys granted by plan, as an independent copy.
func Resolve(plan string) []Key {
	src := allKeys
	if restricted(plan) {
		src = survivorKeys
	}
	out := make([]Key, len(src))
	copy(out, src)
	return out
}

// Has reports whether plan grants key.
func Has(plan string, key Key) bool {
	if !restricted(plan) {
		for _, k := range allKeys {
			if k == key {
				return true
			}
		}
		return false
	}
	for _, k := range survivorKeys {
		if k == key {
			return true
		}
	}
	return false
}

// FactionLimit is the most factions one installation may have on plan; 0 = unlimited.
func FactionLimit(plan string) int {
	if Has(plan, UnlimitedFactions) {
		return 0
	}
	return SurvivorFactionLimit
}

// RouteFeature maps a Discord channel route key to the feature it delivers, for the
// routes that only some plans include. ok=false means the route is part of every plan.
func RouteFeature(routeKey string) (Key, bool) {
	switch routeKey {
	case "BOUNTY", "BOUNTY_TRACKING":
		return Bounties, true
	case "HEATMAPS":
		return Heatmaps, true
	case "ECONOMY", "SHOP":
		return Economy, true
	case "SERVER_RANKS":
		return RankedSeasons, true
	}
	return "", false
}

// RouteAllowed reports whether plan includes whatever the route delivers.
func RouteAllowed(plan, routeKey string) bool {
	key, gated := RouteFeature(routeKey)
	return !gated || Has(plan, key)
}

// Label is the customer-facing name of a feature, for upgrade messages.
func Label(key Key) string {
	switch key {
	case RankedSeasons:
		return "Ranked seasons"
	case Bounties:
		return "Bounties"
	case Heatmaps:
		return "Heatmaps"
	case Economy:
		return "The economy and shop"
	case CustomEmbeds:
		return "Custom embeds"
	case UnlimitedFactions:
		return "Unlimited factions"
	case MultipleServers:
		return "Multiple servers"
	case PrioritySupport:
		return "Priority support"
	}
	return string(key)
}
