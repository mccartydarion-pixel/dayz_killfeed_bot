package discord

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type rankServerSelection struct {
	id    int64
	err   error
	guild int64
}

func (s *rankServerSelection) ConnectedServerID(_ context.Context, guild int64) (int64, error) {
	s.guild = guild
	return s.id, s.err
}

type rankStandingsSource struct {
	server int64
	limit  int
	rows   []repository.ServerStanding
	err    error
}

func (s *rankStandingsSource) ServerStandings(_ context.Context, server int64, limit int) ([]repository.ServerStanding, error) {
	s.server, s.limit = server, limit
	return s.rows, s.err
}

func TestServerSeasonRankReaderUsesSelectedServerOnly(t *testing.T) {
	selected := &rankServerSelection{id: 22}
	source := &rankStandingsSource{rows: []repository.ServerStanding{
		{Name: "Alpha", Tier: "DIAMOND", RP: 6053},
		{Name: "Bravo", Tier: "GOLD", RP: 900},
	}}
	reader := ServerSeasonRankReader{Servers: selected, Ranked: source}
	got, err := reader.TopCurrentRanks(context.Background(), 7, 15)
	if err != nil {
		t.Fatal(err)
	}
	if selected.guild != 7 || source.server != 22 || source.limit != 15 {
		t.Fatalf("scope guild=%d server=%d limit=%d", selected.guild, source.server, source.limit)
	}
	if len(got) != 2 || got[0].DisplayName != "Alpha" || got[0].Rank != "DIAMOND • 6,053 RP" || got[1].Rank != "GOLD • 900 RP" {
		t.Fatalf("rank projection: %+v", got)
	}
}

func TestServerSeasonRankReaderWaitsForUnambiguousActiveSeason(t *testing.T) {
	selected := &rankServerSelection{err: linking.ErrMultipleConnectedServers}
	source := &rankStandingsSource{}
	reader := ServerSeasonRankReader{Servers: selected, Ranked: source}
	if _, err := reader.TopCurrentRanks(context.Background(), 7, 15); !errors.Is(err, repository.ErrRankedIneligible) {
		t.Fatalf("unselected multi-server guild: %v", err)
	}
	if source.server != 0 {
		t.Fatal("queried standings without a selected server")
	}
	selected.err, selected.id = nil, 22
	source.err = repository.ErrRankedIneligible
	if _, err := reader.TopCurrentRanks(context.Background(), 7, 15); !errors.Is(err, repository.ErrRankedIneligible) {
		t.Fatalf("no active season: %v", err)
	}
}

type rankErrorReader struct{ err error }

func (r rankErrorReader) TopCurrentRanks(context.Context, int64, int) ([]RankEntry, error) {
	return nil, r.err
}

func TestV3RankEmbedAppearsOnlyForActiveSeason(t *testing.T) {
	f := newRoutedFixture(t)
	s := newV3Scheduler(f, newFixtureStats())
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "leaderboards")
	s.SetRankSource(rankErrorReader{err: repository.ErrRankedIneligible})
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.api.embedsIn("leaderboards")); got != 5 {
		t.Fatalf("no season embeds=%d", got)
	}
	s.SetRankSource(fixtureRanks{rows: []RankEntry{{DisplayName: "Alpha", Rank: "DIAMOND • 6,053 RP"}}})
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	embeds := f.api.embedsIn("leaderboards")
	if len(embeds) != 6 || !strings.Contains(embeds[3].Description, "Selected public server") || !strings.Contains(embeds[3].Fields[0].Value, "6,053 RP") {
		t.Fatalf("active rank embed: %+v", embeds)
	}
}
