package discord

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// rivalryStatsFake serves fixed rivalry stats. Player IDs are distinctive
// five-digit numbers so a leaked raw ID is easy to detect.
type rivalryStatsFake struct {
	stats         repository.RivalryStats
	longID, topID int64
	longest       float64
	topKills      int64
	noKills       bool
}

func (f *rivalryStatsFake) GetRivalry(context.Context, int64, int64, int64) (*repository.RivalryStats, error) {
	s := f.stats
	return &s, nil
}
func (f *rivalryStatsFake) GetRivalryLongestKill(context.Context, int64, int64, int64) (int64, float64, error) {
	if f.noKills {
		return 0, 0, pgx.ErrNoRows
	}
	return f.longID, f.longest, nil
}
func (f *rivalryStatsFake) GetRivalryTopKiller(context.Context, int64, int64, int64) (int64, int64, error) {
	if f.noKills {
		return 0, 0, pgx.ErrNoRows
	}
	return f.topID, f.topKills, nil
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
	a := repository.Faction{ID: 1, Tag: "RED", Name: "Red Army"}
	b := repository.Faction{ID: 2, Tag: "BLU", Name: "Blue Team"}
	stats := &rivalryStatsFake{
		stats:  repository.RivalryStats{AKills: 12, BKills: 7, TotalKills: 19, WarCount: 2},
		longID: 80123, longest: 412.34, topID: 80456, topKills: 9,
	}
	h := &WarCommandHandler{rivalryStats: stats, players: completionPlayersFake{80123: "Sniper_Joe", 80456: "*Bold*Killer"}}

	text, err := h.rivalryText(context.Background(), 1, a, b)
	if err != nil {
		t.Fatal(err)
	}
	assertNoRawPlayerIDs(t, text)
	for _, want := range []string{"🎯 Longest Rivalry Kill\nSniper\\_Joe — 412.3m", "🔥 Most Active Killer\n\\*Bold\\*Killer — 9 rivalry kills", "RED +5"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestRivalryTextOmitsUnresolvedPlayers(t *testing.T) {
	a := repository.Faction{ID: 1, Tag: "RED", Name: "Red Army"}
	b := repository.Faction{ID: 2, Tag: "BLU", Name: "Blue Team"}
	cases := map[string]*WarCommandHandler{
		"unknown ids":     {rivalryStats: &rivalryStatsFake{longID: 80123, longest: 300, topID: 80456, topKills: 4}, players: completionPlayersFake{}},
		"lookup fails":    {rivalryStats: &rivalryStatsFake{longID: 80123, longest: 300, topID: 80456, topKills: 4}, players: failingPlayerNames{}},
		"no player store": {rivalryStats: &rivalryStatsFake{longID: 80123, longest: 300, topID: 80456, topKills: 4}},
		"no kills":        {rivalryStats: &rivalryStatsFake{noKills: true}, players: completionPlayersFake{0: "Ghost"}},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			text, err := h.rivalryText(context.Background(), 1, a, b)
			if err != nil {
				t.Fatal(err)
			}
			assertNoRawPlayerIDs(t, text)
			if strings.Contains(text, "Longest Rivalry Kill") || strings.Contains(text, "Most Active Killer") {
				t.Fatalf("unresolved holder line rendered:\n%s", text)
			}
			if !strings.Contains(text, "CHAMPION RIVALRY") {
				t.Fatalf("rivalry header missing:\n%s", text)
			}
		})
	}
}
