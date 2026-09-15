package presentation

import (
	"strings"
	"testing"
	"time"
)

func TestChampionEmbedUsesSharedHierarchy(t *testing.T) {
	embed := NewChampionEmbed("PLAYER PROFILE", ChampionGold)
	if embed.Title != "CHAMPION KILLFEED\nPLAYER PROFILE" {
		t.Fatalf("unexpected title: %q", embed.Title)
	}
	if embed.Footer == nil || embed.Footer.Text != ChampionSlogan {
		t.Fatalf("unexpected footer: %#v", embed.Footer)
	}
	if embed.Color != ChampionGold {
		t.Fatalf("unexpected semantic color: %x", embed.Color)
	}
}

func TestUpdatedFooterUsesDiscordRelativeTimestamp(t *testing.T) {
	at := time.Unix(1700000000, 0)
	footer := UpdatedFooter(at)
	if footer.Text != "CHAMPION KILLFEED • Updated <t:1700000000:R>" {
		t.Fatalf("unexpected updated footer: %q", footer.Text)
	}
}

func TestSemanticPaletteIsDistinct(t *testing.T) {
	colors := []int{ChampionGold, CombatRed, SuccessGreen, WarningAmber, InfoSteel, NeutralGraphite, ErrorRed}
	seen := map[int]bool{}
	for _, color := range colors {
		if seen[color] {
			t.Fatalf("semantic palette contains duplicate color %x", color)
		}
		seen[color] = true
	}
}

func TestChampionSloganDoesNotContainDecorativeSpam(t *testing.T) {
	if strings.Count(ChampionSlogan, "🏆") > 1 {
		t.Fatal("slogan should remain restrained")
	}
}
