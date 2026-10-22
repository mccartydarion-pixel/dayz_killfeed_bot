package discord

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// hubLookupFake serves a fixed rivalry. Player IDs are distinctive five-digit numbers so a
// leaked raw ID is easy to detect.
type hubLookupFake struct {
	rivalry repository.HubRivalry
}

func (f *hubLookupFake) FactionsByTag(context.Context, int64, string) ([]repository.HubFactionRef, error) {
	return nil, nil
}
func (f *hubLookupFake) MemberFactions(context.Context, int64, int64) ([]repository.HubFactionRef, error) {
	return nil, nil
}
func (f *hubLookupFake) Rivalry(context.Context, int64, int64, int64) (repository.HubRivalry, error) {
	return f.rivalry, nil
}
func (f *hubLookupFake) GuildInstallations(context.Context, int64) ([]repository.HubInstallationRef, error) {
	return nil, nil
}

type failingPlayerNames struct{}

func (failingPlayerNames) DisplayNamesByID(context.Context, int64, []int64) (map[int64]string, error) {
	return nil, errors.New("db down")
}

var playerIDPlaceholder = regexp.MustCompile(`Player \d+`)

func assertNoRawPlayerIDs(t *testing.T, text string) {
	t.Helper()
	if playerIDPlaceholder.MatchString(text) {
		t.Fatalf("rivalry text leaks a raw player ID:\n%s", text)
	}
	for _, id := range []string{"80123", "80456"} {
		if strings.Contains(text, id) {
			t.Fatalf("rivalry text leaks raw ID %s:\n%s", id, text)
		}
	}
}

func TestRivalryTextResolvesPlayerNames(t *testing.T) {
	a := repository.HubFactionRef{ID: 1, Tag: "RED", Name: "Red Army"}
	b := repository.HubFactionRef{ID: 2, Tag: "BLU", Name: "Blue Team"}
	hub := &hubLookupFake{rivalry: repository.HubRivalry{AKills: 12, BKills: 7, Wars: 2, LongestPlayerID: 80123, Longest: 412.34, TopPlayerID: 80456, TopKills: 9}}
	h := &WarCommandHandler{hub: hub, players: completionPlayersFake{80123: "Sniper_Joe", 80456: "*Bold*Killer"}}

	text, err := h.rivalryText(context.Background(), 1, a, b)
	if err != nil {
		t.Fatal(err)
	}
	assertNoRawPlayerIDs(t, text)
	for _, want := range []string{"🎯 Longest rivalry kill\nSniper\\_Joe — 412.3m", "🔥 Most active killer\n\\*Bold\\*Killer — 9 rivalry kills", "RED +5", "📊 Total encounters\n19", "🏆 Wars\n2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestRivalryTextOmitsUnresolvedPlayers(t *testing.T) {
	a := repository.HubFactionRef{ID: 1, Tag: "RED", Name: "Red Army"}
	b := repository.HubFactionRef{ID: 2, Tag: "BLU", Name: "Blue Team"}
	held := repository.HubRivalry{LongestPlayerID: 80123, Longest: 300, TopPlayerID: 80456, TopKills: 4}
	cases := map[string]*WarCommandHandler{
		"unknown ids":     {hub: &hubLookupFake{rivalry: held}, players: completionPlayersFake{}},
		"lookup fails":    {hub: &hubLookupFake{rivalry: held}, players: failingPlayerNames{}},
		"no player store": {hub: &hubLookupFake{rivalry: held}},
		"no kills":        {hub: &hubLookupFake{}, players: completionPlayersFake{0: "Ghost"}},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			text, err := h.rivalryText(context.Background(), 1, a, b)
			if err != nil {
				t.Fatal(err)
			}
			assertNoRawPlayerIDs(t, text)
			if strings.Contains(text, "Longest rivalry kill") || strings.Contains(text, "Most active killer") {
				t.Fatalf("unresolved holder line rendered:\n%s", text)
			}
			if !strings.Contains(text, "Champion rivalry") {
				t.Fatalf("rivalry header missing:\n%s", text)
			}
		})
	}
}

func TestPickWarSidesNeedsTwoFactionsOfOneServer(t *testing.T) {
	red1 := repository.HubFactionRef{ID: 1, InstallationID: 10, Tag: "RED"}
	blu1 := repository.HubFactionRef{ID: 2, InstallationID: 10, Tag: "BLU"}
	red2 := repository.HubFactionRef{ID: 3, InstallationID: 20, Tag: "RED"}
	blu2 := repository.HubFactionRef{ID: 4, InstallationID: 20, Tag: "BLU"}

	if c, o, ok, amb := pickWarSides([]repository.HubFactionRef{red1}, []repository.HubFactionRef{blu1, blu2}); !ok || amb || c.ID != 1 || o.ID != 2 {
		t.Fatalf("a leader on one server picks that server's opponent: %+v %+v ok=%v amb=%v", c, o, ok, amb)
	}
	if _, _, ok, _ := pickWarSides([]repository.HubFactionRef{red1}, []repository.HubFactionRef{blu2}); ok {
		t.Fatal("factions of different servers cannot fight")
	}
	if _, _, ok, _ := pickWarSides([]repository.HubFactionRef{red1}, []repository.HubFactionRef{red1}); ok {
		t.Fatal("a faction cannot fight itself")
	}
	if _, _, ok, amb := pickWarSides([]repository.HubFactionRef{red1, red2}, []repository.HubFactionRef{blu1, blu2}); ok || !amb {
		t.Fatal("leading factions on two servers against a tag on both is ambiguous")
	}
}

func TestWarTextsUseFactionLabels(t *testing.T) {
	w := repository.War{ID: 7, FactionALabel: "[RED] Red Army", FactionBLabel: "[BLU] Blue Team", Status: repository.WarEnded, ScoreA: 3, ScoreB: 5, WinnerSide: "B"}
	status := warStatusText(w, 3, 5, "Sniper_Joe", 4, "", 0)
	for _, want := range []string{"Faction war #7", "[RED] Red Army — **3**", "Lead: [BLU] Blue Team +2", "Winner: [BLU] Blue Team", "Top killer: Sniper\\_Joe — 4 kills"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q:\n%s", want, status)
		}
	}
	if strings.Contains(status, "Longest kill") {
		t.Fatalf("an unresolved holder line rendered:\n%s", status)
	}
	history := warHistoryText([]repository.War{w})
	if !strings.Contains(history, "**#7** [RED] Red Army vs [BLU] Blue Team — ended (3–5)") {
		t.Fatalf("history = %q", history)
	}
	if warHistoryText(nil) != "No faction wars yet." {
		t.Fatal("empty history")
	}
}

func TestFactionInfoAndLeaderboardText(t *testing.T) {
	leader, member := "LEADER", "MEMBER"
	f := repository.HubFactionRef{ID: 1, Tag: "RED", Name: "Red Army"}
	info := factionInfoText(f, factionstats.Stats{
		Summary: factionstats.Summary{MemberCount: 3, Kills: 20, Deaths: 8, KDRatio: 2.5},
		MemberContributions: []factionstats.MemberContribution{
			{DisplayName: "Grunt", Role: &member, Status: factionstats.StatusActive},
			{DisplayName: "Boss", Role: &leader, Status: factionstats.StatusActive},
		},
	})
	for _, want := range []string{"[RED] Red Army", "👑 **Leader**\nBoss", "👥 **Members**\n3", "20 kills", "2.50 K/D"} {
		if !strings.Contains(info, want) {
			t.Fatalf("info missing %q:\n%s", want, info)
		}
	}
	lines := leaderboardLines(factionstats.Leaderboard{Metric: factionstats.LeaderboardKD, Items: []factionstats.LeaderboardEntry{
		{Rank: 1, Faction: repository.HubLeaderboardRow{Tag: "RED", Name: "Red Army"}, Value: 2.5},
	}})
	if lines != "1. [RED] Red Army — 2.50\n" {
		t.Fatalf("leaderboard lines = %q", lines)
	}
}
