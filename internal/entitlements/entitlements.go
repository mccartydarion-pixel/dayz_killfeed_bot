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
//
// One exception sits above the plan: an organization owned by a platform owner
// (CHAMPION_ADMIN_DISCORD_IDS) has every feature, whatever its plan and whether or
// not gating is enforced. See Plan and ForOrganization.
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
	HotZones          Key = "hot_zones"          // heatmap-driven hot-zone events (they pay Champion Points)
	Retention         Key = "retention"          // the retention dashboard and lapsed-player list
	FightReplay       Key = "fight_replay"       // fight replays, for staff and (opt-in) players
	FeedIdentity      Key = "feed_identity"      // feeds posted under the installation's own name and avatar
	PerkStore         Key = "perk_store"         // the perk store: the Donate tab, its control hub and the donations channel
	MapRotation       Key = "map_rotation"       // map rotation with a player vote (docs/MAP_ROTATION.md)
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
	HotZones, Retention, FightReplay, FeedIdentity, PerkStore, MapRotation,
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

// Plan is one organization's effective plan: its subscription plan key, plus whether the
// organization belongs to a platform owner (docs/ADMIN_API.md "Platform owner access"), in which
// case it has every feature whatever the key says.
//
// It can only be built by ForOrganization, and every exported question in this package takes a
// Plan rather than a plan string. That is the seam: a caller cannot ask about a plan without
// saying whose plan it is, so no gate can forget the platform-owner rule.
type Plan struct {
	key   string
	owner bool
}

// ownerOrganizations answers "is this organization owned by a platform owner". Set once at
// startup (SetOwnerOrganizations); nil means nobody is.
var ownerOrganizations atomic.Pointer[func(organizationID int64) bool]

// SetOwnerOrganizations installs the platform-owner lookup (internal/owneraccess). The function
// is called on hot paths, so it must answer from memory. Passing nil removes it.
func SetOwnerOrganizations(fn func(organizationID int64) bool) {
	if fn == nil {
		ownerOrganizations.Store(nil)
		return
	}
	ownerOrganizations.Store(&fn)
}

// OwnerOrganization reports whether organizationID belongs to a platform owner.
func OwnerOrganization(organizationID int64) bool {
	if organizationID <= 0 {
		return false
	}
	fn := ownerOrganizations.Load()
	return fn != nil && (*fn)(organizationID)
}

// ForOrganization is the effective plan of organizationID, whose subscription plan key is
// planKey ("" when it has no subscription row). Always pass the real organization id: an id of 0
// can never be a platform owner's organization.
func ForOrganization(organizationID int64, planKey string) Plan {
	return Plan{key: planKey, owner: OwnerOrganization(organizationID)}
}

// Key is the subscription plan key the plan was built from.
func (p Plan) Key() string { return p.key }

// OwnerAccess reports whether the plan is unrestricted because a platform owner owns the
// organization.
func (p Plan) OwnerAccess() bool { return p.owner }

// Resolve returns the feature keys granted by the plan, as an independent copy.
func Resolve(p Plan) []Key {
	if p.owner {
		out := make([]Key, len(allKeys))
		copy(out, allKeys)
		return out
	}
	return resolve(p.key)
}

// Has reports whether the plan grants key.
func Has(p Plan, key Key) bool {
	if p.owner {
		return known(key)
	}
	return has(p.key, key)
}

// FactionLimit is the most factions one installation may have on the plan; 0 = unlimited.
func FactionLimit(p Plan) int {
	if Has(p, UnlimitedFactions) {
		return 0
	}
	return SurvivorFactionLimit
}

// RouteAllowed reports whether the plan includes whatever the route delivers.
func RouteAllowed(p Plan, routeKey string) bool {
	key, gated := RouteFeature(routeKey)
	return !gated || Has(p, key)
}

func known(key Key) bool {
	for _, k := range allKeys {
		if k == key {
			return true
		}
	}
	return false
}

// resolve, has, factionLimit and routeAllowed answer for a bare plan key. They stay unexported
// so that code outside this package has to go through a Plan.
func resolve(plan string) []Key {
	src := allKeys
	if restricted(plan) {
		src = survivorKeys
	}
	out := make([]Key, len(src))
	copy(out, src)
	return out
}

func has(plan string, key Key) bool {
	if !restricted(plan) {
		return known(key)
	}
	for _, k := range survivorKeys {
		if k == key {
			return true
		}
	}
	return false
}

func factionLimit(plan string) int {
	if has(plan, UnlimitedFactions) {
		return 0
	}
	return SurvivorFactionLimit
}

func routeAllowed(plan, routeKey string) bool {
	key, gated := RouteFeature(routeKey)
	return !gated || has(plan, key)
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
	case "DONATION_PERKS":
		return PerkStore, true
	}
	return "", false
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
	case HotZones:
		return "Hot zones"
	case Retention:
		return "The retention dashboard"
	case FightReplay:
		return "Fight replay"
	case FeedIdentity:
		return "A custom feed identity"
	case PerkStore:
		return "The perk store"
	case MapRotation:
		return "Map rotation"
	case MultipleServers:
		return "Multiple servers"
	case PrioritySupport:
		return "Priority support"
	}
	return string(key)
}
