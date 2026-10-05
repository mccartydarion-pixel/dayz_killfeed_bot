package app

import (
	"encoding/json"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/playercard"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The PvP/PvE death split (internal/deathstats) in the JSON the website reads. deaths and kd keep
// their meaning; the new fields sit beside them.

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlayerStatsDTOSplitsDeaths(t *testing.T) {
	cases := []struct {
		name string
		in   repository.PlayerCombatStats
		want map[string]float64
	}{
		{"no deaths", repository.PlayerCombatStats{Kills: 9}, map[string]float64{"kills": 9, "deaths": 0, "kd": 9, "pvpDeaths": 0, "pveDeaths": 0, "pvpKd": 9}},
		{"only PvE deaths", repository.PlayerCombatStats{Kills: 9, Deaths: 3}, map[string]float64{"deaths": 3, "kd": 3, "pvpDeaths": 0, "pveDeaths": 3, "pvpKd": 9}},
		{"only PvP deaths", repository.PlayerCombatStats{Kills: 9, Deaths: 3, PvPDeaths: 3}, map[string]float64{"deaths": 3, "kd": 3, "pvpDeaths": 3, "pveDeaths": 0, "pvpKd": 3}},
		{"a mix", repository.PlayerCombatStats{Kills: 10, Deaths: 8, PvPDeaths: 4, Headshots: 2, Longshots: 1, LongestMeters: 80}, map[string]float64{"deaths": 8, "kd": 1.25, "pvpDeaths": 4, "pveDeaths": 4, "pvpKd": 2.5, "headshots": 2, "longshots": 1, "longestKillMeters": 80}},
		{"no kills", repository.PlayerCombatStats{Deaths: 5, PvPDeaths: 2}, map[string]float64{"kills": 0, "deaths": 5, "kd": 0, "pvpDeaths": 2, "pveDeaths": 3, "pvpKd": 0}},
	}
	for _, c := range cases {
		got := asMap(t, toPlayerStatsDTO(42, c.in))
		if got["installationId"] != float64(42) {
			t.Errorf("%s: installationId = %v", c.name, got["installationId"])
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, got[k], v)
			}
		}
	}
}

func TestCardDTOSplitsDeathsOnlyWhenKnown(t *testing.T) {
	a := &App{}
	plain := asMap(t, a.toCardDTO(playercard.Card{PlayerName: "Ace", Kills: 10, Deaths: 8}))
	for _, k := range []string{"pvpDeaths", "pveDeaths", "pvpKd"} {
		if _, ok := plain[k]; ok {
			t.Errorf("%s is present on a card without the split", k)
		}
	}
	if plain["deaths"] != float64(8) || plain["kd"] != 1.25 {
		t.Errorf("plain card deaths/kd = %v/%v", plain["deaths"], plain["kd"])
	}
	for _, tile := range plain["tiles"].([]any) {
		if _, ok := tile.(map[string]any)["note"]; ok {
			t.Errorf("a tile has a note without the split: %v", tile)
		}
	}

	pvp := 3
	split := asMap(t, a.toCardDTO(playercard.Card{PlayerName: "Ace", Kills: 10, Deaths: 8, PvPDeaths: &pvp}))
	if split["deaths"] != float64(8) || split["kd"] != 1.25 || split["pvpDeaths"] != float64(3) || split["pveDeaths"] != float64(5) {
		t.Errorf("split card = %v", split)
	}
	if kd := split["pvpKd"].(float64); kd < 3.333 || kd > 3.334 {
		t.Errorf("pvpKd = %v", kd)
	}
	tiles := split["tiles"].([]any)
	deaths, kd := tiles[1].(map[string]any), tiles[2].(map[string]any)
	if deaths["label"] != "DEATHS" || deaths["value"] != "8" || deaths["note"] != "PVP 3 / PVE 5" {
		t.Errorf("deaths tile = %v", deaths)
	}
	if kd["label"] != "K/D" || kd["value"] != "1.25" || kd["note"] != "PVP 3.33" {
		t.Errorf("K/D tile = %v", kd)
	}

	// Zero is a real value, not "unknown": a player who was never killed by a player still gets the split.
	zero := 0
	never := asMap(t, a.toCardDTO(playercard.Card{PlayerName: "Ace", Kills: 4, Deaths: 2, PvPDeaths: &zero}))
	if never["pvpDeaths"] != float64(0) || never["pveDeaths"] != float64(2) || never["pvpKd"] != float64(4) {
		t.Errorf("never killed by a player = %v", never)
	}
}

func TestPlayerDirectoryDTOSplitsDeaths(t *testing.T) {
	got := asMap(t, toPlayerDirectoryEntryDTO(repository.PlayerDirectoryEntry{PlayerID: 7, Gamertag: "Ace", Kills: 10, Deaths: 8, PvPDeaths: 3}))
	if got["deaths"] != float64(8) || got["pvpDeaths"] != float64(3) || got["pveDeaths"] != float64(5) {
		t.Errorf("directory entry = %v", got)
	}
	none := asMap(t, toPlayerDirectoryEntryDTO(repository.PlayerDirectoryEntry{PlayerID: 7, Gamertag: "Ace"}))
	if none["deaths"] != float64(0) || none["pvpDeaths"] != float64(0) || none["pveDeaths"] != float64(0) {
		t.Errorf("empty directory entry = %v", none)
	}
}
