package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// PvP and PvE deaths (internal/deathstats) as Discord shows them.

func TestPlayerProfileShowsBothKDsAndTheDeathSplit(t *testing.T) {
	seen := time.Unix(1790000000, 0)
	cases := []struct {
		name string
		prof repository.PlayerProfile
		want []string
	}{
		{"a mix", repository.PlayerProfile{DisplayName: "Ace", Kills: 10, Deaths: 8, PvPDeaths: 3, LastSeen: seen},
			[]string{"**Deaths**\n8\nPvP 3 • PvE 5\n\n", "**K/D (PvP)**\n3.33\n\n", "**K/D (overall)**\n1.25\n\n"}},
		{"no deaths", repository.PlayerProfile{DisplayName: "Ace", Kills: 9, LastSeen: seen},
			[]string{"**Deaths**\n0\nPvP 0 • PvE 0\n\n", "**K/D (PvP)**\n9.00\n\n", "**K/D (overall)**\n9.00\n\n"}},
		{"only PvE deaths", repository.PlayerProfile{DisplayName: "Ace", Kills: 9, Deaths: 3, LastSeen: seen},
			[]string{"**Deaths**\n3\nPvP 0 • PvE 3\n\n", "**K/D (PvP)**\n9.00\n\n", "**K/D (overall)**\n3.00\n\n"}},
		{"only PvP deaths", repository.PlayerProfile{DisplayName: "Ace", Kills: 9, Deaths: 3, PvPDeaths: 3, LastSeen: seen},
			[]string{"**Deaths**\n3\nPvP 3 • PvE 0\n\n", "**K/D (PvP)**\n3.00\n\n", "**K/D (overall)**\n3.00\n\n"}},
		{"thousands", repository.PlayerProfile{DisplayName: "Ace", Kills: 5000, Deaths: 2500, PvPDeaths: 1250, LastSeen: seen},
			[]string{"PvP 1,250 • PvE 1,250", "**K/D (PvP)**\n4.00\n\n", "**K/D (overall)**\n2.00\n\n"}},
	}
	for _, c := range cases {
		got := formatPlayerProfile(&c.prof)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: profile is missing %q:\n%s", c.name, w, got)
			}
		}
		// The rest of the profile is as it was.
		for _, w := range []string{"🏆 **Champion player profile**", "**Player**\nAce\n\n", "**Longest Kill**\n—\n\n", "**Last Seen**\n<t:1790000000:R>"} {
			if !strings.Contains(got, w) {
				t.Errorf("%s: profile lost %q:\n%s", c.name, w, got)
			}
		}
		// PvP K/D comes before the overall one, and the old bare "K/D" label is gone.
		if strings.Index(got, "K/D (PvP)") > strings.Index(got, "K/D (overall)") || strings.Contains(got, "**K/D**") {
			t.Errorf("%s: K/D order or label:\n%s", c.name, got)
		}
	}
}

func TestCompactCombatStatsAddsAPvPLineOnlyWhenItSaysSomething(t *testing.T) {
	cases := []struct {
		name string
		rec  killfeed.CombatRecord
		want string
	}{
		{"split not known: the strip as it always was", killfeed.CombatRecord{Kills: 9, Deaths: 3}, "**9 K** • **3 D** • **3.00 K/D**"},
		{"no deaths", killfeed.CombatRecord{Kills: 9, DeathSplitKnown: true}, "**9 K** • **0 D** • **9.00 K/D**"},
		{"only PvP deaths: both K/Ds are the same number", killfeed.CombatRecord{Kills: 9, Deaths: 3, PvPDeaths: 3, DeathSplitKnown: true}, "**9 K** • **3 D** • **3.00 K/D**"},
		{"only PvE deaths", killfeed.CombatRecord{Kills: 9, Deaths: 3, DeathSplitKnown: true}, "**9 K** • **3 D** • **3.00 K/D**\nPvP **0 D** • **9.00 K/D**"},
		{"a mix", killfeed.CombatRecord{Kills: 1284, Deaths: 310, PvPDeaths: 262, DeathSplitKnown: true}, "**1,284 K** • **310 D** • **4.14 K/D**\nPvP **262 D** • **4.90 K/D**"},
	}
	for _, c := range cases {
		rec := c.rec
		if got := compactCombatStats(&rec); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestKillAndDeathCardsCarryThePvPLine(t *testing.T) {
	ev := FixtureStandardKill()
	ev.KillerStats = &killfeed.CombatRecord{Kills: 9, Deaths: 4, PvPDeaths: 1, DeathSplitKnown: true}
	ev.VictimStats = &killfeed.CombatRecord{Kills: 2, Deaths: 2, PvPDeaths: 2, DeathSplitKnown: true}
	embed := BuildKillEmbed(ev)
	var killer, victim string
	for _, f := range embed.Fields {
		switch f.Name {
		case "Killer":
			killer = f.Value
		case "Victim":
			victim = f.Value
		}
	}
	if !strings.HasPrefix(killer, "**9 K** • **4 D** • **2.25 K/D**\nPvP **1 D** • **9.00 K/D**") {
		t.Errorf("killer field = %q", killer)
	}
	if victim != "**2 K** • **2 D** • **1.00 K/D**" {
		t.Errorf("victim field = %q", victim)
	}
	if v := presentation.CheckEmbed(embed); len(v) != 0 {
		t.Errorf("the kill card breaks the design rules: %v", v)
	}

	death := BuildDeathEmbed(&killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: "Fox"},
		PlayerStats: &killfeed.CombatRecord{Kills: 1, Deaths: 50, PvPDeaths: 20, DeathSplitKnown: true}})
	if len(death.Fields) != 1 || death.Fields[0].Value != "**1 K** • **50 D** • **0.02 K/D**\nPvP **20 D** • **0.05 K/D**" {
		t.Errorf("death card fields = %+v", death.Fields)
	}
	if v := presentation.CheckEmbed(death); len(v) != 0 {
		t.Errorf("the death card breaks the design rules: %v", v)
	}
}

func TestKillfeedVarsAddTheSplitBesideTheOldVariables(t *testing.T) {
	ev := FixtureStandardKill()
	before := killfeedVars(ev, "Northstar")
	for _, name := range []string{"killer_pvp_deaths", "killer_pve_deaths", "killer_pvp_kd", "victim_pvp_deaths", "victim_pve_deaths", "victim_pvp_kd"} {
		if _, ok := before[name]; ok {
			t.Fatalf("%s is set for a record that does not carry the split", name)
		}
	}

	ev.KillerStats = &killfeed.CombatRecord{Kills: 1284, Deaths: 310, PvPDeaths: 262, DeathSplitKnown: true}
	ev.VictimStats = &killfeed.CombatRecord{Kills: 2, Deaths: 2, DeathSplitKnown: true}
	got := killfeedVars(ev, "Northstar")
	want := map[string]string{
		// Unchanged meaning: every death and the overall K/D.
		"killer_kills": "1,284", "killer_deaths": "310", "killer_kd": "4.14",
		"victim_kills": "2", "victim_deaths": "2", "victim_kd": "1.00",
		// New.
		"killer_pvp_deaths": "262", "killer_pve_deaths": "48", "killer_pvp_kd": "4.90",
		"victim_pvp_deaths": "0", "victim_pve_deaths": "2", "victim_pvp_kd": "2.00",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("{{%s}} = %q, want %q", k, got[k], v)
		}
	}
	// Nothing else moved: every variable the old record produced is still there with its value,
	// apart from the six stat values that follow the new record.
	for k, v := range before {
		if _, changed := want[k]; changed {
			continue
		}
		if got[k] != v {
			t.Errorf("{{%s}} changed from %q to %q", k, v, got[k])
		}
	}
	if len(got) != len(before)+6 {
		t.Errorf("%d variables, want the old %d plus 6", len(got), len(before))
	}

	// Each new variable is an approved, optional KILLFEED variable, so a template may use it.
	defs := map[string]embedtemplates.VariableDefinition{}
	for _, d := range embedtemplates.VariableDefinitions("KILLFEED") {
		defs[d.Name] = d
	}
	for _, name := range []string{"killer_pvp_deaths", "killer_pve_deaths", "killer_pvp_kd", "victim_pvp_deaths", "victim_pve_deaths", "victim_pvp_kd"} {
		d, ok := defs[name]
		if !ok || !d.Optional {
			t.Errorf("%s: approved=%v optional=%v", name, ok, d.Optional)
		}
	}
}
