package presentation

import (
	"strings"
	"testing"
)

func TestPlayerLeaderboardIsOneCompactField(t *testing.T) {
	e := BuildPlayerLeaderboardEmbed("kills", []RankedEntry{{1, "Player", "12"}, {2, "Other", "9"}}, "Season 3")
	if len(e.Fields) != 1 || e.Fields[0].Name != "⚔️ TOP KILLERS" {
		t.Fatalf("expected one category field: %+v", e.Fields)
	}
	if e.Fields[0].Value != "🥇 Player • **12 Kills**\n🥈 Other • **9 Kills**" {
		t.Fatalf("rank rows: %q", e.Fields[0].Value)
	}
	if e.Description != "Season 3 Rankings" || e.Title != "🏆 PLAYER LEADERBOARD" {
		t.Fatalf("hero: %q / %q", e.Title, e.Description)
	}
	empty := BuildPlayerLeaderboardEmbed("kills", nil, "Lifetime")
	if len(empty.Fields) != 0 || !strings.Contains(empty.Description, EmptyRanking) {
		t.Fatalf("empty board: %#v %q", empty.Fields, empty.Description)
	}
}

func TestLeaderboardNamesSanitized(t *testing.T) {
	e := BuildPlayerLeaderboardEmbed("kills", []RankedEntry{{1, "@everyone", "1"}}, "")
	if strings.Contains(e.Fields[0].Value, "@everyone") {
		t.Fatal("mention leaked")
	}
}

func TestUnknownCategoryHasNoGenericValueLabel(t *testing.T) {
	e := BuildPlayerLeaderboardEmbed("Zombies", []RankedEntry{{1, "A", "42"}}, "")
	if strings.Contains(e.Fields[0].Value, "Value") || e.Fields[0].Name != "🏆 ZOMBIES" {
		t.Fatalf("fallback: %q / %q", e.Fields[0].Name, e.Fields[0].Value)
	}
}

func TestErrorEmbedIsCompact(t *testing.T) {
	e := BuildLeaderboardErrorEmbed("K/D")
	if e.Color != ErrorRed || len(e.Fields) != 0 || strings.Contains(e.Description, "**Category**") {
		t.Fatalf("error embed: %#v", e)
	}
}
