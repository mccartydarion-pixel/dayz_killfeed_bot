package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
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
	if embed.Title != "💀 CHAMPION • PLAYER ELIMINATED" {
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
	if !fieldsContain(embed.Fields, "KILL DETAILS", "Damage") {
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
	if !fieldsContain(embed.Fields, "KILL DETAILS", "1.0 • 2.0 • 3.0") {
		t.Fatalf("expected coordinates in explicit mode: %#v", embed.Fields)
	}
}

// TestKillEmbedStatFieldsOnlyRenderWhenPresent proves KILLER STATS / VICTIM
// STATS / HEAD-TO-HEAD each render only when the Event actually carries that
// data (a guild without the stats/analytics repositories wired still gets a
// working embed), and match the mockup's field format when it does.
func TestKillEmbedStatFieldsOnlyRenderWhenPresent(t *testing.T) {
	base := &killfeed.Event{Killer: &killfeed.PlayerRef{Name: "K"}, Victim: &killfeed.PlayerRef{Name: "V"}}
	embed := BuildKillEmbed(base)
	for _, name := range []string{"KILLER STATS", "VICTIM STATS", "HEAD-TO-HEAD"} {
		for _, f := range embed.Fields {
			if f.Name == name {
				t.Fatalf("expected no %s field without source data, got %#v", name, f)
			}
		}
	}

	streak := 4
	full := &killfeed.Event{
		Killer:       &killfeed.PlayerRef{Name: "K"},
		Victim:       &killfeed.PlayerRef{Name: "V"},
		KillerStats:  &killfeed.CombatRecord{Kills: 24, Deaths: 8},
		VictimStats:  &killfeed.CombatRecord{Kills: 12, Deaths: 16},
		KillerStreak: &streak,
		Encounters:   &killfeed.HeadToHead{KillerWins: 3, VictimWins: 1},
	}
	embed = BuildKillEmbed(full)
	if !fieldsContain(embed.Fields, "KILLER STATS", "Kills: 24") || !fieldsContain(embed.Fields, "KILLER STATS", "K/D: 3.00") || !fieldsContain(embed.Fields, "KILLER STATS", "Streak: 4") {
		t.Fatalf("unexpected KILLER STATS field: %#v", embed.Fields)
	}
	if !fieldsContain(embed.Fields, "VICTIM STATS", "Kills: 12") || !fieldsContain(embed.Fields, "VICTIM STATS", "K/D: 0.75") {
		t.Fatalf("unexpected VICTIM STATS field: %#v", embed.Fields)
	}
	if fieldsContain(embed.Fields, "VICTIM STATS", "Streak") {
		t.Fatal("victim stats must not show a streak")
	}
	if !fieldsContain(embed.Fields, "HEAD-TO-HEAD", "3 - 1") {
		t.Fatalf("unexpected HEAD-TO-HEAD field: %#v", embed.Fields)
	}
}

// fieldsContain reports whether any field named name has a value containing substr.
func fieldsContain(fields []*discordgo.MessageEmbedField, name, substr string) bool {
	for _, f := range fields {
		if f.Name == name && strings.Contains(f.Value, substr) {
			return true
		}
	}
	return false
}
