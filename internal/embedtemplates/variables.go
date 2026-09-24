package embedtemplates

// VariableDefinition documents one approved {{variable}} of a route. The backend owns
// this dictionary (labels, descriptions, categories, examples, availability); the
// website renders it rather than keeping its own copy. Every variable comes from data
// already attached to the event being rendered - never a lookup, never a fabrication -
// and an optional variable is simply absent (a field that uses it is omitted).
type VariableDefinition struct {
	Name         string `json:"name"`
	Label        string `json:"label"`
	Description  string `json:"description"`
	Category     string `json:"category"`
	Example      string `json:"example"`
	Optional     bool   `json:"optional"`
	Availability string `json:"availability"`
	Format       string `json:"format"` // text | integer | decimal | distance | label | points | timestamp
	// Source names the authoritative runtime field the value comes from (docs only).
	Source string `json:"source"`
}

// Categories, in display order.
const (
	CatPlayers     = "Players"
	CatCombat      = "Combat"
	CatKillerStats = "Killer Stats"
	CatVictimStats = "Victim Stats"
	CatHeadToHead  = "Head To Head"
	CatStreaks     = "Streaks"
	CatEventStory  = "Event Story"
	CatBounty      = "Bounty"
	CatCompetitive = "Competitive"
	CatEconomy     = "Economy"
	CatActivity    = "Activity"
	CatServer      = "Server"
)

const always = "Always present."

func serverName() VariableDefinition {
	return VariableDefinition{Name: "server_name", Label: "Server Name", Description: "The DayZ server's display name.", Category: CatServer, Example: "Champions Deathmatch", Optional: true, Availability: "Present when the server has a display name.", Format: "text", Source: "game server display name"}
}

func timestamp() VariableDefinition {
	return VariableDefinition{Name: "timestamp", Label: "Timestamp", Description: "When Champion published the card (UTC).", Category: CatServer, Example: "2026-09-23 12:00:00 UTC", Availability: always, Format: "timestamp", Source: "publish time"}
}

func v(name, label, cat, desc, example, format, source, availability string, optional bool) VariableDefinition {
	return VariableDefinition{Name: name, Label: label, Category: cat, Description: desc, Example: example, Format: format, Source: source, Availability: availability, Optional: optional}
}

const (
	statsAvail  = "Present when Champion loaded the player's combat record for this kill."
	hitDataNote = "Only present when the kill event carries hit data. Standard ADM kill lines do not include a hit zone or damage, so this is usually absent."
	// finalHitNote: the kill line itself has no hit data; Champion attaches the lethal hit only when
	// the ADM hit line immediately before the kill agrees on every point (killfeed/final_hit.go).
	finalHitNote = "Present when Champion reliably matched the lethal hit: the ADM hit line immediately before the kill, marked (DEAD), with the same boot file, players, second, weapon and distance. Otherwise absent - a line or field using it is omitted."
)

// routeVariableDefinitions is the single source of truth for every route's approved
// variables; routeVariables (the name lists validation uses) is derived from it. The
// Phase 1-4 names are kept (a stored template stays valid) and only ever widened.
var routeVariableDefinitions = map[string][]VariableDefinition{
	"KILLFEED": {
		v("killer", "Killer", CatPlayers, "The player who got the kill.", "WilliamAle--10", "text", "Event.Killer.Name", always, false),
		v("victim", "Victim", CatPlayers, "The player who was killed.", "Semillita-azul-_", "text", "Event.Victim.Name", always, false),
		v("weapon", "Weapon", CatCombat, "The weapon used.", "M4-A1", "text", "Event.Weapon", "Present when the kill line names a weapon.", true),
		v("weapon_category", "Weapon Category", CatCombat, "The weapon's class from Champion's weapon classifier.", "Assault Rifle", "label", "presentation.WeaponCategory(Event.Weapon)", "Present when the classifier recognizes the weapon.", true),
		v("ammo", "Ammo", CatCombat, "The ammunition type.", "5.56x45", "text", "Event.Ammo", "Present when the kill line names the ammunition.", true),
		v("distance", "Distance", CatCombat, "Kill distance in meters.", "11.7m", "distance", "Event.Distance", "Present when the kill line has a distance.", true),
		v("range", "Range", CatCombat, "Champion's range class for the kill.", "CLOSE QUARTERS", "label", "presentation.RangeClass(Event.Distance)", "Present when the distance is known or the kill was melee.", true),
		v("hit_zone", "Hit Zone", CatCombat, "The body part of the lethal hit.", "Torso", "text", "correlated lethal hit (Event.FinalHit)", finalHitNote, true),
		v("damage", "Damage", CatCombat, "Damage of the lethal hit (that one hit, not a total).", "28.7", "decimal", "correlated lethal hit (Event.FinalHit)", finalHitNote, true),
		v("killer_kills", "Killer Kills", CatKillerStats, "The killer's all-time kills after this kill.", "9", "integer", "Event.KillerStats.Kills", statsAvail, true),
		v("killer_deaths", "Killer Deaths", CatKillerStats, "The killer's all-time deaths.", "0", "integer", "Event.KillerStats.Deaths", statsAvail, true),
		v("killer_kd", "Killer K/D", CatKillerStats, "The killer's all-time K/D after the confirmed kill.", "9.00", "decimal", "Event.KillerStats.KD()", statsAvail, true),
		v("killer_streak", "Killer Streak", CatKillerStats, "The killer's current kill streak including this kill (same value as streak).", "1", "integer", "Event.KillerStreak", "Present when the streak is known and above zero.", true),
		v("victim_kills", "Victim Kills", CatVictimStats, "The victim's all-time kills.", "2", "integer", "Event.VictimStats.Kills", statsAvail, true),
		v("victim_deaths", "Victim Deaths", CatVictimStats, "The victim's all-time deaths after this death.", "2", "integer", "Event.VictimStats.Deaths", statsAvail, true),
		v("victim_kd", "Victim K/D", CatVictimStats, "The victim's all-time K/D after this death.", "1.00", "decimal", "Event.VictimStats.KD()", statsAvail, true),
		v("h2h_killer_wins", "H2H Killer Wins", CatHeadToHead, "How many times the killer has killed this victim.", "4", "integer", "Event.Encounters.KillerWins", "Present when Champion loaded the head-to-head record.", true),
		v("h2h_victim_wins", "H2H Victim Wins", CatHeadToHead, "How many times this victim has killed the killer.", "0", "integer", "Event.Encounters.VictimWins", "Present when Champion loaded the head-to-head record.", true),
		v("h2h_score", "H2H Score", CatHeadToHead, "The head-to-head record, killer first.", "4–0", "text", "Event.Encounters", "Present when Champion loaded the head-to-head record.", true),
		v("streak", "Streak", CatStreaks, "The killer's current kill streak including this kill.", "1", "integer", "Event.KillerStreak", "Present when the streak is known and above zero.", true),
		v("ended_streak", "Ended Streak", CatStreaks, "The victim's streak this kill ended.", "8", "integer", "Event.EndedStreakCount", "Only when this kill ended a meaningful streak.", true),
		v("killing_spree", "Killing Spree", CatStreaks, "Set when this kill reached a killing-spree milestone.", "KILLING SPREE", "label", "Event.KillingSpree", "Only on killing-spree kills.", true),
		v("streak_ended", "Streak Ended", CatStreaks, "Set when this kill ended the victim's streak.", "STREAK ENDED", "label", "Event.StreakEnded", "Only when this kill ended a meaningful streak.", true),
		v("headshot", "Headshot", CatEventStory, "Set when the confirmed hit zone is the head.", "HEADSHOT", "label", "Event.HitZone == Head", hitDataNote, true),
		v("special_kill", "Special Kill", CatEventStory, "The kill's highlight line from Champion's story engine.", "PRECISION FINISH", "label", "BuildPresentation(Event).Hero", "Only for non-standard kills (long range, headshot, streaks, bounty, events ...).", true),
		v("kill_type", "Kill Type", CatEventStory, "The kill's classification from Champion's story engine.", "HEADSHOT", "label", "BuildPresentation(Event).Title", always, false),
		v("story_title", "Story Title", CatEventStory, "The kill's title as the default card shows it, with its icon.", "🎯 HEADSHOT", "label", "BuildPresentation(Event).Icon + Title", always, false),
		v("bounty_amount", "Bounty Amount", CatBounty, "Champion Points paid for the claimed bounty (a number - write {{bounty_amount}} pts).", "500", "points", "Event.BountyPoints", "Only when this kill claimed a bounty.", true),
		v("bounty_target", "Bounty Target", CatBounty, "Set when the victim had an active bounty.", "MOST WANTED", "label", "Event.BountyTarget", "Only when the victim had an active bounty.", true),
		v("bounty_claimed", "Bounty Claimed", CatBounty, "Set when this kill claimed a bounty.", "BOUNTY CLAIMED", "label", "Event.BountyClaimed", "Only when this kill claimed a bounty.", true),
		v("season_name", "Season", CatCompetitive, "The active season.", "Season 3", "text", "Event.SeasonName", "Present while a season is active.", true),
		v("war_badge", "War Badge", CatCompetitive, "The faction war this kill counted for.", "WAR KILL", "label", "Event.WarBadge", "Only for kills in an active faction war.", true),
		v("event_badges", "Event Badges", CatCompetitive, "Active competitive events this kill counted for, joined by •.", "Double Kill • NWAF Event", "text", "Event.ActiveEventBadges", "Only when the kill counted for an active event.", true),
		serverName(), timestamp(),
	},
	"HITFEED": {
		v("attacker", "Attacker", CatPlayers, "The player who landed the hits.", "WilliamAle--10", "text", "hit encounter attacker", always, false),
		v("killer", "Killer (alias)", CatPlayers, "Same as attacker (kept for older templates).", "WilliamAle--10", "text", "hit encounter attacker", always, false),
		v("victim", "Victim", CatPlayers, "The player who was hit.", "Semillita-azul-_", "text", "hit encounter victim", always, false),
		v("weapon", "Weapon", CatCombat, "The weapon used.", "M4-A1", "text", "hit encounter weapon", "Present when the hit lines name a weapon.", true),
		v("ammo", "Ammo", CatCombat, "The ammunition type.", "5.56x45", "text", "hit encounter ammo", "Present when the hit lines name the ammunition.", true),
		v("distance", "Distance", CatCombat, "Distance of the hits in meters.", "87m", "distance", "hit encounter distance", "Present when the hit lines have a distance.", true),
		v("hit_zone", "Hit Zone", CatCombat, "The body part hit last.", "Torso", "text", "hit encounter zone", "Present when the hit lines name a zone.", true),
		v("damage", "Damage", CatCombat, "Total damage across the grouped hits.", "142", "integer", "hit encounter damage sum", "Only when every grouped hit reported its damage (a partial sum is never shown).", true),
		v("hits", "Hits", CatCombat, "How many hits this card groups: consecutive hits by the same attacker on the same victim with the same weapon.", "3", "integer", "hit encounter count", always, false),
		serverName(), timestamp(),
	},
	"PVE_FEED": {
		v("victim", "Victim", CatPlayers, "The player who died.", "Semillita-azul-_", "text", "PvE notice name", always, false),
		v("cause", "Cause", CatCombat, "The proven non-PvP cause.", "suicide", "label", "PvE notice cause", "Present when the ADM line proves a cause (today: suicides).", true),
		serverName(), timestamp(),
	},
	"BOUNTY_TRACKING": {
		v("target", "Target", CatPlayers, "The player the bounty is on.", "Semillita-azul-_", "text", "bounty event target", always, false),
		v("victim", "Victim (alias)", CatPlayers, "Same as target (kept for older templates).", "Semillita-azul-_", "text", "bounty event target", always, false),
		v("hunter", "Hunter", CatPlayers, "The player who claimed the bounty.", "WilliamAle--10", "text", "bounty event hunter", "Only on CLAIMED events.", true),
		v("killer", "Killer (alias)", CatPlayers, "Same as hunter (kept for older templates).", "WilliamAle--10", "text", "bounty event hunter", "Only on CLAIMED events.", true),
		v("amount", "Amount", CatBounty, "Champion Points on the bounty (a number - write {{amount}} pts).", "500", "points", "bounty event amount", always, false),
		v("total", "Total Paid", CatBounty, "Champion Points paid out on the claim (a number).", "500", "points", "bounty event amount", "Only on CLAIMED events.", true),
		v("count", "Bounties Claimed", CatBounty, "How many bounties the claim paid out.", "2", "integer", "bounty event count", "Only on CLAIMED events.", true),
		v("weapon", "Weapon", CatCombat, "The weapon of the claiming kill.", "M4-A1", "text", "bounty event weapon", "Only on CLAIMED events that name a weapon.", true),
		v("distance", "Distance", CatCombat, "Distance of the claiming kill.", "87m", "distance", "bounty event distance", "Only on CLAIMED events with a known distance.", true),
		v("status", "Status", CatBounty, "What happened to the bounty.", "claimed", "label", "bounty event kind", always, false),
		serverName(),
	},
	"ECONOMY": {
		v("player", "Player", CatPlayers, "The player the transaction belongs to.", "WilliamAle--10", "text", "economy event player", always, false),
		v("amount", "Amount", CatEconomy, "Champion Points moved (a number - write {{amount}} pts).", "250", "points", "economy event amount", always, false),
		v("balance", "Balance", CatEconomy, "The player's Champion Points balance afterwards (a number).", "1,250", "points", "economy event balance", "Only for rewards (bounty payouts, system rewards).", true),
		v("transaction_type", "Transaction Type", CatEconomy, "What kind of transaction it was.", "Bounty reward", "label", "economy event type", always, false),
		serverName(),
	},
	"CONNECTIONS": {
		v("player", "Player", CatPlayers, "The player who joined or left.", "WilliamAle--10", "text", "connection notice name", always, false),
		v("event", "Event", CatActivity, "joined or left.", "joined", "label", "connection notice kind", always, false),
		v("event_type", "Event Type", CatActivity, "connected or disconnected.", "connected", "label", "connection notice kind", always, false),
		v("session", "Session Length", CatActivity, "How long the player was online.", "1h 12m", "text", "connection notice session", "Only on disconnects when the session length is known.", true),
		serverName(), timestamp(),
	},
	// Routes without runtime custom rendering keep their stored vocabulary.
	"BOUNTY": {v("victim", "Target", CatPlayers, "Reserved for the bounty board.", "Semillita-azul-_", "text", "reserved", "Not rendered at runtime (persistent board).", true), serverName(), timestamp()},
	"SHOP": {
		v("player", "Player", CatPlayers, "Reserved: shop events render through ECONOMY.", "WilliamAle--10", "text", "reserved", "Not rendered at runtime.", true),
		v("item", "Item", CatEconomy, "Reserved.", "Field Rations", "text", "reserved", "Not rendered at runtime.", true),
		v("amount", "Amount", CatEconomy, "Reserved.", "75 pts", "points", "reserved", "Not rendered at runtime.", true),
		v("balance", "Balance", CatEconomy, "Reserved.", "1,175 pts", "points", "reserved", "Not rendered at runtime.", true),
	},
	"BUILD_FEED": {
		v("player", "Player", CatPlayers, "Reserved.", "WilliamAle--10", "text", "reserved", "Not rendered at runtime.", true),
		v("structure", "Structure", CatActivity, "Reserved.", "Sea Chest", "text", "reserved", "Not rendered at runtime.", true),
		serverName(),
	},
	"ADMIN_ALERTS":       {v("event", "Event", CatActivity, "Reserved.", "ADM STALE", "text", "reserved", "Not rendered at runtime.", true), v("player", "Player", CatPlayers, "Reserved.", "WilliamAle--10", "text", "reserved", "Not rendered at runtime.", true), serverName(), timestamp()},
	"ADMIN_LOGS":         {v("event", "Event", CatActivity, "Reserved.", "ADM DOWNLOADED", "text", "reserved", "Not rendered at runtime.", true), v("player", "Player", CatPlayers, "Reserved.", "WilliamAle--10", "text", "reserved", "Not rendered at runtime.", true), serverName(), timestamp()},
	"HEATMAPS":           {serverName(), timestamp()},
	"LINK_GAMERTAG":      {serverName(), timestamp()},
	"STATS_LEADERBOARDS": {serverName(), timestamp()},
	"AUTO_LEADERBOARD":   {serverName(), timestamp()},
	"SERVER_STATUS":      {serverName(), timestamp()},
}

// VariableDefinitions returns a copy of a route's variable metadata (nil if unknown).
func VariableDefinitions(routeKey string) []VariableDefinition {
	defs, ok := routeVariableDefinitions[routeKey]
	if !ok {
		return nil
	}
	return append([]VariableDefinition(nil), defs...)
}

// AllVariableDefinitions returns a copy of every route's variable metadata.
func AllVariableDefinitions() map[string][]VariableDefinition {
	out := make(map[string][]VariableDefinition, len(routeVariableDefinitions))
	for k, defs := range routeVariableDefinitions {
		out[k] = append([]VariableDefinition(nil), defs...)
	}
	return out
}
