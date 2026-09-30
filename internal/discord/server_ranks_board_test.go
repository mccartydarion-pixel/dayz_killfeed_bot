package discord

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fixtureServerRanks struct {
	rows []repository.ServerStanding
	err  error
}

func (f *fixtureServerRanks) ServerStandings(context.Context, int64, int) ([]repository.ServerStanding, error) {
	return f.rows, f.err
}

func TestServerRanksBoardPersistentRouteAndSourceFailure(t *testing.T) {
	f := newRoutedFixture(t)
	reader := &fixtureServerRanks{err: repository.ErrRankedIneligible}
	f.resolver.set(f.guild, 1, routeKeyServerRanks, "ranks-a")
	board := NewServerRanksBoard(f.resolver, f.panels, reader, f.guild, 1, "Champions")
	ctx := context.Background()
	board.SyncOnce(ctx)
	key := fmt.Sprintf("%s:%d", routeKeyServerRanks, 1)
	first := f.store.messageFor(f.guild, key, "ranks-a")
	if first == "" || f.api.liveIn("ranks-a") != 1 {
		t.Fatalf("missing persistent placeholder: %v", f.api.live())
	}
	reader.rows, reader.err = []repository.ServerStanding{{Name: "A", RP: 100}}, nil
	board.SyncOnce(ctx)
	if f.store.messageFor(f.guild, key, "ranks-a") != first || f.api.sendCount() != 1 {
		t.Fatal("active standings did not edit existing panel")
	}
	reader.err = errors.New("temporary database failure")
	board.SyncOnce(ctx)
	if f.api.liveIn("ranks-a") != 1 || f.api.sendCount() != 1 {
		t.Fatal("source failure changed the last good panel")
	}
	reader.err = nil
	f.resolver.set(f.guild, 1, routeKeyServerRanks, "ranks-b")
	board.SyncOnce(ctx)
	if f.api.liveIn("ranks-a") != 0 || f.api.liveIn("ranks-b") != 1 {
		t.Fatalf("route move left duplicate panel: %v", f.api.live())
	}
	restarted := NewServerRanksBoard(f.resolver, NewRoutePanels(f.api, f.store), reader, f.guild, 1, "Champions")
	restarted.SyncOnce(ctx)
	if f.api.liveIn("ranks-b") != 1 || f.api.sendCount() != 2 {
		t.Fatal("restart posted a duplicate server ranks panel")
	}
}
