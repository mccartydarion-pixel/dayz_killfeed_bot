package presentation

import (
	"strings"
	"testing"
	"time"
)

func TestChampionEmbedUsesSharedHierarchy(t *testing.T) {
	embed := NewChampionEmbed("Player profile", ChampionGold)
	if embed.Author == nil || embed.Author.Name != AuthorName {
		t.Fatalf("brand belongs in the author line: %#v", embed.Author)
	}
	if embed.Title != "Player profile" {
		t.Fatalf("title is the section only: %q", embed.Title)
	}
	if embed.Footer != nil {
		t.Fatalf("a card has no footer until its builder has a server or a context to name: %#v", embed.Footer)
	}
	if embed.Color != ChampionGold {
		t.Fatalf("unexpected semantic color: %x", embed.Color)
	}
}

func TestFooterVariants(t *testing.T) {
	if ChampionSlogan != "Every kill tells a story" {
		t.Fatalf("slogan: %q", ChampionSlogan)
	}
	if got := SeasonFooterText("Season 3"); got != "Season 3" {
		t.Fatalf("season footer: %q", got)
	}
	if SeasonFooterText("  ") != ChampionSlogan {
		t.Fatal("no season -> plain slogan")
	}
	if AutoRefreshFooter().Text != "Updates automatically" {
		t.Fatal("auto-refresh footer")
	}
	// Discord does not render <t:...> in footers; the time goes in the timestamp.
	if f := UpdatedFooter(time.Unix(1700000000, 0)); f.Text != FooterLiveIntel || strings.Contains(f.Text, "<t:") {
		t.Fatalf("updated footer: %q", f.Text)
	}
	for _, text := range []string{ChampionSlogan, FooterAutoRefresh, FooterLiveIntel, FooterStaffOnly, SeasonFooterText("S")} {
		if strings.Contains(text, "\n") || strings.Contains(strings.ToLower(text), "killfeed") {
			t.Fatalf("footers are one line and never repeat the author brand: %q", text)
		}
		if why := CheckFooter(text); why != "" {
			t.Fatalf("footer %q %s", text, why)
		}
	}
}

// One footer format: "Server name · short context", either part optional.
func TestFooterFormat(t *testing.T) {
	for _, c := range []struct{ server, context, want string }{
		{"Example Server", "Season 3", "Example Server · Season 3"},
		{"Example Server", "", "Example Server"},
		{"", "Staff only", "Staff only"},
		{"  ", "  ", ""},
		{"@everyone #general\nServer", "Map vote", "everyone general Server · Map vote"}, // stored data is cleaned
	} {
		if got := FooterText(c.server, c.context); got != c.want {
			t.Errorf("FooterText(%q, %q) = %q, want %q", c.server, c.context, got, c.want)
		}
	}
	if Footer("", "") != nil {
		t.Fatal("nothing to say -> no footer at all")
	}
	if f := Footer("Example Server", "Season 3"); f == nil || f.Text != "Example Server · Season 3" {
		t.Fatalf("footer: %#v", f)
	}
	if SeasonFooter("") == nil || SeasonFooter("").Text != ChampionSlogan {
		t.Fatal("a kill card without a season falls back to the slogan")
	}
}

func TestBrandAuthor(t *testing.T) {
	if AuthorName != "Champions® Killfeed" || ChampionAuthor().Name != AuthorName || BrandAuthor(" ").Name != AuthorName {
		t.Fatalf("plain brand line: %q", AuthorName)
	}
	if got := BrandAuthor("Battle Pass").Name; got != "Champions® Battle Pass" {
		t.Fatalf("product author: %q", got)
	}
}

func TestTimestampMarkup(t *testing.T) {
	at := time.Unix(1790000000, 0)
	if got := Timestamp(at, 'R'); got != "<t:1790000000:R>" {
		t.Fatalf("relative: %q", got)
	}
	if got := TimestampWithRelative(at); got != "<t:1790000000:F> (<t:1790000000:R>)" {
		t.Fatalf("with relative: %q", got)
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

// The palette is six colours with six meanings; every older name resolves to one of them.
func TestSemanticPaletteIsDistinct(t *testing.T) {
	palette := Palette()
	if len(palette) != 6 {
		t.Fatalf("the palette is six colours, got %d", len(palette))
	}
	seen := map[int]bool{}
	for _, color := range palette {
		if seen[color] {
			t.Fatalf("palette contains duplicate color %06X", color)
		}
		seen[color] = true
		if PaletteName(color) == "" || !InPalette(color) {
			t.Fatalf("palette colour %06X has no name", color)
		}
	}
	for name, color := range map[string]int{"ChampionGold": ChampionGold, "EventGold": EventGold, "FactionGold": FactionGold, "CombatRed": CombatRed,
		"SuccessGreen": SuccessGreen, "WarningAmber": WarningAmber, "ErrorRed": ErrorRed, "InfoSteel": InfoSteel, "Steel": Steel, "NeutralGraphite": NeutralGraphite} {
		if !InPalette(color) {
			t.Errorf("%s (%06X) is not a palette colour", name, color)
		}
	}
	if InPalette(0x123456) || PaletteName(0) != "" {
		t.Fatal("a stray colour must not pass for a palette colour")
	}
	// The values are the website's design tokens.
	if Crimson != 0xD60F1D || Gold != 0xF0C65E {
		t.Fatalf("crimson/gold must match the website: %06X %06X", Crimson, Gold)
	}
}

func TestChampionSloganDoesNotContainDecorativeSpam(t *testing.T) {
	if strings.Count(ChampionSlogan, "🏆") > 1 {
		t.Fatal("slogan should remain restrained")
	}
}
