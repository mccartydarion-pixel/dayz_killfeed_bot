package discord

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Deterministic Champion V2 preview fixtures. Each builder returns the exact
// authoritative input a publisher would render, so a developer can see what
// every default card contains (run: go test ./internal/discord -run
// TestPresentationFixturePreview -v) and the structural tests below pin it.

var fixtureTime = time.Date(2026, 9, 23, 18, 42, 7, 0, time.UTC)

func fptr(v float64) *float64 { return &v }
func iptr(v int) *int         { return &v }

func fixtureKill(weapon string, dist float64, zone string) *killfeed.Event {
	return &killfeed.Event{
		Type:         killfeed.EventPlayerKill,
		Timestamp:    fixtureTime,
		Killer:       &killfeed.PlayerRef{Name: "WilliamAle--10", ID: "kid-1"},
		Victim:       &killfeed.PlayerRef{Name: "Semillita-azul-_", ID: "vid-1"},
		Weapon:       weapon,
		Distance:     fptr(dist),
		HitZone:      zone,
		KillerStats:  &killfeed.CombatRecord{Kills: 9, Deaths: 0},
		VictimStats:  &killfeed.CombatRecord{Kills: 2, Deaths: 2},
		KillerStreak: iptr(1),
		Encounters:   &killfeed.HeadToHead{KillerWins: 4, VictimWins: 0},
	}
}

func FixtureStandardKill() *killfeed.Event {
	ev := fixtureKill("M4-A1", 11.7, "Torso")
	ev.Damage = fptr(34.2)
	ev.Ammo = "Bullet_556x45"
	return ev
}

func FixtureHeadshotKill() *killfeed.Event { return fixtureKill("M4-A1", 87.4, "Head") }

func FixtureLongshotKill() *killfeed.Event { return fixtureKill("SCR 17", 173.8, "Torso") }

func FixtureExtremeRangeKill() *killfeed.Event { return fixtureKill("M70 Tundra", 247.3, "Head") }

func FixtureBountyKill() *killfeed.Event {
	ev := fixtureKill("M4-A1", 64.2, "Torso")
	ev.BountyClaimed = true
	ev.BountyPoints = 12500
	return ev
}

func FixtureKillingSpreeKill() *killfeed.Event {
	ev := fixtureKill("M4-A1", 42.0, "Torso")
	ev.KillingSpree = true
	ev.KillerStreak = iptr(5)
	return ev
}

func FixtureStreakEndedKill() *killfeed.Event {
	ev := fixtureKill("M4-A1", 42.0, "Torso")
	ev.StreakEnded = true
	ev.EndedStreakCount = iptr(8)
	return ev
}

func FixtureDeath() *killfeed.Event {
	return &killfeed.Event{
		Type:        killfeed.EventPlayerDeath,
		Timestamp:   fixtureTime,
		Player:      &killfeed.PlayerRef{Name: "Ceiyxe"},
		PlayerStats: &killfeed.CombatRecord{Kills: 1, Deaths: 50},
	}
}

func FixtureSuicide() *killfeed.Event {
	return &killfeed.Event{
		Type:        killfeed.EventSuicideAction,
		Timestamp:   fixtureTime,
		Player:      &killfeed.PlayerRef{Name: "Ceiyxe"},
		Weapon:      "M4-A1",
		Cause:       killfeed.DeathCauseSuicide,
		PlayerStats: &killfeed.CombatRecord{Kills: 1, Deaths: 50},
	}
}

func FixtureSeasonSnapshot() LeaderboardSnapshot {
	names := []string{"IIIIIIIIIIII-I", "Its_H14METIYO", "zTonii99", "WilliamAle--10", "KikiduritoR2", "Ceiyxe", "MmeyAFK_7", "superflame_1738", "Cool-Creeper65", "Semillita-azul-_"}
	kills := []string{"20", "16", "11", "9", "8", "7", "6", "5", "4", "3"}
	var s LeaderboardSnapshot
	for i := range names {
		s.TopKills = append(s.TopKills, repository.LeaderboardEntry{DisplayName: names[i], Value: kills[i]})
	}
	longest := []string{"298.4m", "214.7m", "198.3m", "177.1m", "98.3m"}
	kd := []string{"4.25", "3.80", "2.10", "1.75", "1.42"}
	for i := 0; i < 5; i++ {
		s.TopLongest = append(s.TopLongest, repository.LeaderboardEntry{DisplayName: names[i], Value: longest[i]})
		s.TopKD = append(s.TopKD, repository.LeaderboardEntry{DisplayName: names[i], Value: kd[i]})
	}
	return s
}

func FixturePlayerLeaderboard() []presentation.RankedEntry {
	s := FixtureSeasonSnapshot()
	out := make([]presentation.RankedEntry, 0, len(s.TopKills))
	for i, e := range s.TopKills {
		out = append(out, presentation.RankedEntry{Rank: i + 1, Name: e.DisplayName, Value: e.Value})
	}
	return out
}

type namedFixture struct {
	name  string
	embed *discordgo.MessageEmbed
}

func allFixtureEmbeds() []namedFixture {
	return []namedFixture{
		{"standard kill", BuildKillEmbed(FixtureStandardKill())},
		{"headshot", BuildKillEmbed(FixtureHeadshotKill())},
		{"longshot", BuildKillEmbed(FixtureLongshotKill())},
		{"extreme range", BuildKillEmbed(FixtureExtremeRangeKill())},
		{"bounty claim", BuildKillEmbed(FixtureBountyKill())},
		{"killing spree", BuildKillEmbed(FixtureKillingSpreeKill())},
		{"streak ended", BuildKillEmbed(FixtureStreakEndedKill())},
		{"death", BuildDeathEmbed(FixtureDeath())},
		{"suicide", BuildDeathEmbed(FixtureSuicide())},
		{"season leaderboard", BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())},
		{"player leaderboard", presentation.BuildPlayerLeaderboardEmbed("Kills", FixturePlayerLeaderboard(), "Lifetime")},
	}
}

// renderPreview is a readable dump of one card (author/title/desc/fields/footer).
func renderPreview(e *discordgo.MessageEmbed) string {
	var b strings.Builder
	if e.Author != nil {
		fmt.Fprintf(&b, "  author: %s\n", e.Author.Name)
	}
	fmt.Fprintf(&b, "  title:  %s\n  color:  #%06X\n", e.Title, e.Color)
	for _, l := range strings.Split(e.Description, "\n") {
		fmt.Fprintf(&b, "  | %s\n", l)
	}
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "  [%s] inline=%v\n", f.Name, f.Inline)
		for _, l := range strings.Split(f.Value, "\n") {
			fmt.Fprintf(&b, "      %s\n", l)
		}
	}
	if e.Footer != nil {
		fmt.Fprintf(&b, "  footer: %s\n", e.Footer.Text)
	}
	return b.String()
}

// TestPresentationFixturePreview logs every fixture card and its field count
// (the numbers used in the V2 before/after report).
func TestPresentationFixturePreview(t *testing.T) {
	for _, f := range allFixtureEmbeds() {
		t.Logf("FIELDCOUNT %-20s %d\n%s", f.name, len(f.embed.Fields), renderPreview(f.embed))
	}
}
