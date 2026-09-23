package embedtemplates

import "testing"

var knownCategories = map[string]bool{CatPlayers: true, CatCombat: true, CatKillerStats: true, CatVictimStats: true, CatHeadToHead: true,
	CatStreaks: true, CatEventStory: true, CatBounty: true, CatCompetitive: true, CatEconomy: true, CatActivity: true, CatServer: true}

var knownFormats = map[string]bool{"text": true, "integer": true, "decimal": true, "distance": true, "label": true, "points": true, "timestamp": true}

func TestEveryVariableHasCompleteMetadata(t *testing.T) {
	for route, defs := range routeVariableDefinitions {
		names := Variables(route)
		if len(names) != len(defs) {
			t.Fatalf("%s: name list and metadata disagree", route)
		}
		for i, d := range defs {
			if names[i] != d.Name {
				t.Errorf("%s: names derive from metadata in order", route)
			}
			if d.Label == "" || d.Description == "" || d.Example == "" || d.Availability == "" || d.Source == "" {
				t.Errorf("%s.%s: incomplete metadata %+v", route, d.Name, d)
			}
			if !knownCategories[d.Category] || !knownFormats[d.Format] {
				t.Errorf("%s.%s: unknown category %q or format %q", route, d.Name, d.Category, d.Format)
			}
		}
	}
	defs := VariableDefinitions("KILLFEED")
	defs[0].Label = "hacked"
	if VariableDefinitions("KILLFEED")[0].Label == "hacked" {
		t.Fatal("callers cannot mutate the metadata")
	}
}

// Stored templates keep validating: every variable approved before V2.1 still is.
func TestPreviouslyApprovedVariablesRemain(t *testing.T) {
	before := map[string][]string{
		"KILLFEED":        {"killer", "victim", "weapon", "distance", "ammo", "streak", "special_kill", "bounty_amount", "server_name", "timestamp"},
		"PVE_FEED":        {"victim", "cause", "server_name", "timestamp"},
		"HITFEED":         {"killer", "attacker", "victim", "weapon", "ammo", "distance", "hit_zone", "damage", "hits", "server_name"},
		"BOUNTY":          {"victim", "server_name", "timestamp"},
		"BOUNTY_TRACKING": {"killer", "victim", "target", "hunter", "amount", "total", "count", "weapon", "distance", "status", "server_name"},
		"ECONOMY":         {"player", "amount", "balance", "transaction_type", "server_name"},
		"SHOP":            {"player", "item", "amount", "balance"},
		"CONNECTIONS":     {"player", "event", "event_type", "session", "server_name", "timestamp"},
		"BUILD_FEED":      {"player", "structure", "server_name"},
		"ADMIN_ALERTS":    {"event", "player", "server_name", "timestamp"},
		"ADMIN_LOGS":      {"event", "player", "server_name", "timestamp"},
		"HEATMAPS":        {"server_name", "timestamp"}, "LINK_GAMERTAG": {"server_name", "timestamp"},
		"STATS_LEADERBOARDS": {"server_name", "timestamp"}, "AUTO_LEADERBOARD": {"server_name", "timestamp"}, "SERVER_STATUS": {"server_name", "timestamp"},
	}
	for route, names := range before {
		have := map[string]bool{}
		for _, n := range Variables(route) {
			have[n] = true
		}
		for _, n := range names {
			if !have[n] {
				t.Errorf("%s lost %s - stored templates would stop validating", route, n)
			}
		}
	}
	for _, n := range []string{"killer_kills", "killer_kd", "victim_kd", "h2h_score", "ended_streak", "range", "weapon_category", "headshot", "kill_type", "story_title", "event_badges"} {
		found := false
		for _, have := range Variables("KILLFEED") {
			found = found || have == n
		}
		if !found {
			t.Errorf("KILLFEED is missing %s", n)
		}
	}
	for _, n := range []string{"hits", "timestamp"} {
		found := false
		for _, have := range Variables("HITFEED") {
			found = found || have == n
		}
		if !found {
			t.Errorf("HITFEED is missing %s", n)
		}
	}
}

func TestUnknownVariableStillInvalid(t *testing.T) {
	cfg := Config{Enabled: true, Color: "#000000", Title: Text{Enabled: true, Template: "{{made_up_stat}}"}}
	if _, err := Validate(cfg, "KILLFEED"); err == nil {
		t.Fatal("an unapproved variable must be rejected")
	}
	cfg.Title.Template = "{{killer_kd}} {{h2h_score}}"
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Fatalf("new variables validate: %v", err)
	}
	if _, err := Validate(cfg, "HITFEED"); err == nil {
		t.Fatal("a KILLFEED-only variable is not valid on HITFEED")
	}
}
