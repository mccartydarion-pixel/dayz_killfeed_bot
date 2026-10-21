package discord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeSeasonStore struct {
	active *repository.Season
	err    error
}

func (f fakeSeasonStore) GetActiveSeason(context.Context, int64) (*repository.Season, error) {
	return f.active, f.err
}
func (fakeSeasonStore) Start(context.Context, int64, string, time.Time) (*repository.Season, error) {
	return nil, nil
}
func (fakeSeasonStore) FinalizeSeason(context.Context, int64, int64, time.Time) (*repository.SeasonResult, error) {
	return nil, nil
}
func (fakeSeasonStore) GetSeasonHistory(context.Context, int64, int) ([]repository.Season, error) {
	return nil, nil
}

type fakeRankedStatus struct {
	servers []repository.GameServer
	seasons map[int64]*repository.ServerRankedSeason
	err     map[int64]error
}

func (f fakeRankedStatus) ListActiveByGuild(context.Context, int64) ([]repository.GameServer, error) {
	return f.servers, nil
}
func (f fakeRankedStatus) ActiveServerSeason(_ context.Context, _ int64, id int64) (*repository.ServerRankedSeason, error) {
	return f.seasons[id], f.err[id]
}

// The reported confusion: "/season status" said Season 1 ACTIVE while Server
// Ranks said the Ranked season had not started. Both are now shown, labelled.
func TestSeasonStatusSeparatesStatsSeasonFromRankedSeason(t *testing.T) {
	start := time.Unix(1790000000, 0)
	h := NewSeasonCommandHandler(fakeSeasonStore{active: &repository.Season{Name: "Season 1", Status: "ACTIVE", StartsAt: start}}, nil)
	h.SetRankedStatus(fakeRankedStatus{
		servers: []repository.GameServer{{ID: 1, DisplayName: "Champions Deathmatch"}, {ID: 2, DisplayName: "PvE Island"}, {ID: 3, DisplayName: "@everyone"}},
		seasons: map[int64]*repository.ServerRankedSeason{2: {RPPerKill: 1500, SameVictimCooldownMinutes: 30, Thresholds: ranked.Thresholds{1, 2, 3, 4, 5, 6, 7}, StartsAt: start}},
		err:     map[int64]error{3: errors.New("db")},
	})
	text := h.statusText(context.Background(), 7, start)
	for _, want := range []string{
		"📊 **Stats season**", "**Season 1** — active since <t:1790000000:R>", "All-time leaderboards are never reset",
		"🎖️ **Ranked (RP) season** — per server",
		"**Champions Deathmatch** — not started. Players are Unranked and no RP is awarded.",
		"**PvE Island** — active since <t:1790000000:R> · 1,500 RP per eligible kill · the same player counts again after 30 minutes",
		"status temporarily unavailable",
		"Server Admin → Server Controls → Server Ranked",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "@everyone") {
		t.Fatal("server names must be sanitised")
	}
	if strings.Index(text, "Stats season") > strings.Index(text, "Ranked (RP) season") {
		t.Fatal("stats season first, Ranked second")
	}
}

func TestSeasonStatusWithoutRankedReader(t *testing.T) {
	h := NewSeasonCommandHandler(fakeSeasonStore{}, nil)
	text := h.statusText(context.Background(), 7, time.Now())
	if !strings.Contains(text, "No active stats season.") || !strings.Contains(text, "Ranked status is unavailable.") {
		t.Fatalf("unexpected: %s", text)
	}
}

func TestServerRanksInactiveNoticeIsHonestAndDistinct(t *testing.T) {
	e := ServerRanksInactiveEmbed("Champions Deathmatch")
	for _, want := range []string{"**Champions Deathmatch**", "Ranked (RP) season has not started yet", "Unranked", "no RP is awarded", "/season status", "separate"} {
		if !strings.Contains(e.Description, want) {
			t.Errorf("inactive notice missing %q: %s", want, e.Description)
		}
	}
	if len(e.Fields) != 0 {
		t.Fatal("an inactive season never shows standings")
	}
	assertWithinLimits(t, e)
}
