package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func killEv(victim, killer, weapon string, dist float64, hitZone string) *killfeed.Event {
	d := dist
	return &killfeed.Event{
		Type:      killfeed.EventPlayerKill,
		Timestamp: time.Date(2026, 9, 8, 16, 40, 12, 0, time.UTC),
		Victim:    &killfeed.PlayerRef{Name: victim, ID: "vid-1"},
		Killer:    &killfeed.PlayerRef{Name: killer, ID: "kid-1"},
		Weapon:    weapon,
		Distance:  &d,
		HitZone:   hitZone,
	}
}

func TestBuildKillEmbedStandard(t *testing.T) {
	// 62.2m is outside the close/long/extreme bands, so it must use STANDARD.
	ev := killEv("MmeyAFK_7", "Ceiyxe", "SCR 17", 62.2, "Torso")
	embed := BuildKillEmbed(ev)

	if embed.Title != "🏆 CHAMPION KILLFEED" {
		t.Fatalf("expected standard title, got %q", embed.Title)
	}
	if embed.Color != ColorChampionGold {
		t.Fatalf("expected gold color, got %x", embed.Color)
	}
	if !strings.Contains(embed.Description, "Ceiyxe") || !strings.Contains(embed.Description, "MmeyAFK_7") {
		t.Fatalf("expected killer+victim in description, got %q", embed.Description)
	}
	if embed.Footer == nil || embed.Footer.Text == "" {
		t.Fatal("expected a footer")
	}
	if embed.Author == nil || embed.Author.Name != "🏆 CHAMPION KILLFEED" {
		t.Fatal("expected Champion author branding")
	}
}

func TestBuildKillEmbedHeadshot(t *testing.T) {
	ev := killEv("V", "K", "M4-A1", 87.4, "Head")
	embed := BuildKillEmbed(ev)
	if embed.Title != "🎯 CHAMPION • HEADSHOT" {
		t.Fatalf("expected headshot title, got %q", embed.Title)
	}
	if embed.Color != ColorHeadshotRed {
		t.Fatalf("expected headshot red, got %x", embed.Color)
	}
	if !strings.Contains(embed.Description, "🎯 Headshot") {
		t.Fatalf("expected headshot badge, got %q", embed.Description)
	}
}

func TestBuildKillEmbedLongRange(t *testing.T) {
	ev := killEv("V", "K", "SCR 17", 147.6, "Torso")
	embed := BuildKillEmbed(ev)
	if embed.Title != "🎯 CHAMPION • LONG SHOT" {
		t.Fatalf("expected long range title, got %q", embed.Title)
	}
	if embed.Color != ColorLongRange {
		t.Fatalf("expected long-range color, got %x", embed.Color)
	}
}

func TestBuildKillEmbedExtremeRange(t *testing.T) {
	ev := killEv("V", "K", "SCR 17", 324.8, "Torso")
	embed := BuildKillEmbed(ev)
	if embed.Title != "👑 CHAMPION • EXTREME RANGE" {
		t.Fatalf("expected extreme range title, got %q", embed.Title)
	}
	if embed.Color != ColorExtremeRange {
		t.Fatalf("expected extreme-range color, got %x", embed.Color)
	}
}

func TestBuildKillEmbedCloseRange(t *testing.T) {
	ev := killEv("V", "K", "AKM", 4.2, "Torso")
	embed := BuildKillEmbed(ev)
	if embed.Title != "🔥 CHAMPION • CLOSE QUARTERS" {
		t.Fatalf("expected close range title, got %q", embed.Title)
	}
	if embed.Color != ColorCloseRange {
		t.Fatalf("expected close-range color, got %x", embed.Color)
	}
}

func TestExtremeRangeBeatsHeadshot(t *testing.T) {
	// 250m headshot: EXTREME_RANGE primary, headshot as a secondary badge.
	ev := killEv("V", "K", "M4-A1", 250.0, "Head")
	embed := BuildKillEmbed(ev)
	if embed.Title != "👑 CHAMPION • EXTREME RANGE" {
		t.Fatalf("expected extreme range to win priority, got %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "🎯 Headshot") {
		t.Fatalf("expected secondary headshot badge, got %q", embed.Description)
	}
}

func TestHeadshotBeatsCloseRange(t *testing.T) {
	// 8m headshot: HEADSHOT primary, close-range as a secondary badge.
	ev := killEv("V", "K", "M4-A1", 8.0, "Head")
	embed := BuildKillEmbed(ev)
	if embed.Title != "🎯 CHAMPION • HEADSHOT" {
		t.Fatalf("expected headshot to win over close range, got %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "🔥 Close Range") {
		t.Fatalf("expected secondary close-range badge, got %q", embed.Description)
	}
}

func TestDistanceRoundingAndPrecision(t *testing.T) {
	ev := killEv("V", "K", "M4-A1", 62.1978, "Torso")
	embed := BuildKillEmbed(ev)
	var distField *discordgo.MessageEmbedField
	for _, f := range embed.Fields {
		if strings.Contains(f.Name, "Distance") {
			distField = f
		}
	}
	if distField == nil {
		t.Fatal("expected a distance field")
	}
	if distField.Value != "62.2m" {
		t.Fatalf("expected distance rounded to 62.2m, got %q", distField.Value)
	}
	// Internal precision retained on the event.
	if *ev.Distance != 62.1978 {
		t.Fatalf("expected full precision retained internally, got %v", *ev.Distance)
	}
}

func TestMentionAndIDSuppression(t *testing.T) {
	ev := killEv("@everyone fan", "bad#actor", "M4-A1", 10, "Torso")
	embed := BuildKillEmbed(ev)
	if strings.Contains(embed.Description, "@everyone") || strings.Contains(embed.Description, "#") {
		t.Fatalf("expected mentions neutralized, got %q", embed.Description)
	}
	// No player IDs anywhere in the embed.
	for _, f := range embed.Fields {
		if strings.Contains(f.Value, "vid-1") || strings.Contains(f.Value, "kid-1") {
			t.Fatalf("player ID leaked into embed field: %q", f.Value)
		}
	}
	if strings.Contains(embed.Description, "vid-1") || strings.Contains(embed.Description, "kid-1") {
		t.Fatal("player ID leaked into description")
	}
}

func TestEmbedLengthSafety(t *testing.T) {
	longName := strings.Repeat("VeryLongPlayerNameWithUnicode🎯", 20)
	ev := killEv(longName, longName, strings.Repeat("W", 500), 5, "Torso")
	embed := BuildKillEmbed(ev)
	if embed == nil {
		t.Fatal("expected an embed even for extreme inputs")
	}
	if len(embed.Description) > maxDescLen {
		t.Fatalf("description exceeds limit: %d", len(embed.Description))
	}
	for _, f := range embed.Fields {
		if len([]rune(f.Value)) > maxWeaponLen+2 {
			t.Fatalf("field value not safely truncated: %d runes", len([]rune(f.Value)))
		}
	}
}

func TestNoRawADMAndNoCoordinates(t *testing.T) {
	ev := killEv("V", "K", "M4-A1", 10, "Torso")
	ev.Raw = `16:40:12 | Player "V" (DEAD) (id=vid-1 pos=<1.0, 2.0, 3.0>) killed by ...`
	ev.Victim.Position = &killfeed.Position{X: 1.0, Y: 2.0, Z: 3.0}
	embed := BuildKillEmbed(ev)
	if strings.Contains(embed.Description, "pos=") || strings.Contains(embed.Description, "DEAD") {
		t.Fatalf("raw ADM/coords leaked into embed: %q", embed.Description)
	}
}

func TestTimestampSetOnlyWhenAbsolute(t *testing.T) {
	ev := killEv("V", "K", "M4-A1", 10, "Torso")
	embed := BuildKillEmbed(ev)
	if embed.Timestamp == "" {
		t.Fatal("expected embed timestamp from valid absolute event time")
	}

	// When only time-of-day exists (no absolute time), omit the timestamp.
	ev2 := &killfeed.Event{
		Type:      killfeed.EventPlayerKill,
		TimeOfDay: "16:40:12",
		Victim:    &killfeed.PlayerRef{Name: "V"},
		Killer:    &killfeed.PlayerRef{Name: "K"},
	}
	embed2 := BuildKillEmbed(ev2)
	if embed2.Timestamp != "" {
		t.Fatalf("expected no timestamp when only time-of-day is known, got %q", embed2.Timestamp)
	}
}
