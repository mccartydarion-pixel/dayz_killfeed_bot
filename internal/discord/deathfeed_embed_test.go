package discord

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestDeathEmbedPlayerStatsOnlyRendersWhenPresent(t *testing.T) {
	base := &killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: "victim1"}}
	embed := BuildDeathEmbed(base)
	for _, f := range embed.Fields {
		if f.Name == "PLAYER STATS" {
			t.Fatalf("expected no PLAYER STATS field without source data, got %#v", f)
		}
	}

	full := &killfeed.Event{
		Type:        killfeed.EventSuicideAction,
		Player:      &killfeed.PlayerRef{Name: "victim1"},
		PlayerStats: &killfeed.CombatRecord{Kills: 12, Deaths: 16},
	}
	embed = BuildDeathEmbed(full)
	if !fieldsContain(embed.Fields, "PLAYER STATS", "Kills: 12") || !fieldsContain(embed.Fields, "PLAYER STATS", "K/D: 0.75") {
		t.Fatalf("unexpected PLAYER STATS field: %#v", embed.Fields)
	}
	if fieldsContain(embed.Fields, "PLAYER STATS", "Streak") {
		t.Fatal("death embed must not show a streak")
	}
}
