package discord

import (
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestSignatureKillDefaultsToCompetitiveStructure(t *testing.T) {
	distance := 12.4
	damage := 3.0
	event := &killfeed.Event{
		Killer:   &killfeed.PlayerRef{Name: "superflame_1738", Position: &killfeed.Position{X: 4496, Y: 10333.5, Z: 339.2}},
		Victim:   &killfeed.PlayerRef{Name: "Cool-Creeper65"},
		Weapon:   "MeleeFist_Heavy",
		Distance: &distance,
		Damage:   &damage,
		HitZone:  "Torso",
	}
	embed := BuildKillEmbed(event)
	if embed.Title != "🥊 CHAMPION • CLOSE QUARTERS" {
		t.Fatalf("unexpected melee title: %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "superflame_1738  ➜  Cool-Creeper65") {
		t.Fatalf("missing matchup line: %q", embed.Description)
	}
	if !strings.Contains(embed.Description, "Straight One-Two") {
		t.Fatalf("missing melee story line: %q", embed.Description)
	}
	if strings.Contains(embed.Description, "4496") {
		t.Fatal("coordinates must be private by default")
	}
	if len(embed.Fields) == 0 || !strings.Contains(embed.Fields[0].Value, "Final Hit Damage") {
		t.Fatalf("missing grouped combat details: %#v", embed.Fields)
	}
}

func TestSignatureKillSpecialHeroAndAccent(t *testing.T) {
	distance := 287.4
	event := &killfeed.Event{
		Killer:   &killfeed.PlayerRef{Name: "Killer"},
		Victim:   &killfeed.PlayerRef{Name: "Victim"},
		Weapon:   "M70 Tundra",
		Distance: &distance,
		HitZone:  "Head",
	}
	embed := BuildKillEmbed(event)
	if embed.Title != "🏆 CHAMPION • EXTREME RANGE" {
		t.Fatalf("unexpected special title: %q", embed.Title)
	}
	if embed.Color != ColorExtremeRange {
		t.Fatalf("unexpected special accent: %x", embed.Color)
	}
	if !strings.Contains(embed.Description, "EXTREME RANGE ELIMINATION") || len(embed.Fields) == 0 || !strings.Contains(embed.Fields[0].Value, "287.4m") {
		t.Fatalf("missing special hero: %q", embed.Description)
	}
}

func TestSignatureKillCoordinatesCanBeExplicitlyEnabled(t *testing.T) {
	event := &killfeed.Event{Killer: &killfeed.PlayerRef{Name: "K", Position: &killfeed.Position{X: 1, Y: 2, Z: 3}}, Victim: &killfeed.PlayerRef{Name: "V"}}
	embed := BuildKillEmbedWithOptions(event, KillEmbedOptions{LocationMode: LocationCoordinates})
	if len(embed.Fields) == 0 || !strings.Contains(embed.Fields[0].Value, "1.0 • 2.0 • 3.0") {
		t.Fatalf("expected coordinates in explicit mode: %#v", embed.Fields)
	}
}
