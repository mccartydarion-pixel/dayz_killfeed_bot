package presentation

import (
	"strings"
	"testing"
	"time"
)

func TestChampionEmbedUsesSharedHierarchy(t *testing.T) {
	embed := NewChampionEmbed("PLAYER PROFILE", ChampionGold)
	if embed.Author == nil || embed.Author.Name != AuthorName {
		t.Fatalf("brand belongs in the author line: %#v", embed.Author)
	}
	if embed.Title != "PLAYER PROFILE" {
		t.Fatalf("title is the section only: %q", embed.Title)
	}
	if embed.Footer == nil || embed.Footer.Text != ChampionSlogan {
		t.Fatalf("unexpected footer: %#v", embed.Footer)
	}
	if embed.Color != ChampionGold {
		t.Fatalf("unexpected semantic color: %x", embed.Color)
	}
}

func TestFooterVariants(t *testing.T) {
	if ChampionSlogan != "EVERY KILL TELLS A STORY" {
		t.Fatalf("slogan: %q", ChampionSlogan)
	}
	if got := SeasonFooterText("Season 3"); got != "CHAMPION • Season 3 • EVERY KILL TELLS A STORY" {
		t.Fatalf("season footer: %q", got)
	}
	if SeasonFooterText("  ") != ChampionSlogan {
		t.Fatal("no season -> plain slogan")
	}
	if AutoRefreshFooter().Text != "CHAMPION • AUTO-REFRESH" {
		t.Fatal("auto-refresh footer")
	}
	// Discord does not render <t:...> in footers; the time goes in the timestamp.
	if f := UpdatedFooter(time.Unix(1700000000, 0)); f.Text != FooterLiveIntel || strings.Contains(f.Text, "<t:") {
		t.Fatalf("updated footer: %q", f.Text)
	}
	for _, text := range []string{ChampionSlogan, FooterAutoRefresh, FooterLiveIntel, SeasonFooterText("S")} {
		if strings.Contains(text, "\n") || strings.Contains(text, "KILLFEED") {
			t.Fatalf("footers are one line and never repeat the author brand: %q", text)
		}
	}
}

func TestStampEmbed(t *testing.T) {
	e := NewFeedEmbed("X", ChampionGold)
	StampEmbed(e, time.Time{})
	if e.Timestamp != "" {
		t.Fatal("zero time leaves the timestamp unset")
	}
	StampEmbed(e, time.Date(2026, 9, 23, 18, 42, 7, 0, time.UTC))
	if e.Timestamp != "2026-09-23T18:42:07.000Z" {
		t.Fatalf("timestamp: %q", e.Timestamp)
	}
}

func TestSemanticPaletteIsDistinct(t *testing.T) {
	colors := []int{ChampionGold, CombatRed, SuccessGreen, WarningAmber, InfoSteel, NeutralGraphite, ErrorRed, EventGold}
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
