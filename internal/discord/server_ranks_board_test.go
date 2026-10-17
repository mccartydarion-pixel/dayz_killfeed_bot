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
	servers := SingleGuild(func(context.Context) (int64, []int64, error) { return f.guild, []int64{1}, nil })
	board := NewServerRanksBoards(f.resolver, servers, f.panels, reader)
	board.SetServerNameFunc(func(int64) string { return "Champions" })
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
	restarted := NewServerRanksBoards(f.resolver, servers, NewRoutePanels(f.api, f.store), reader)
	restarted.SyncOnce(ctx)
	if f.api.liveIn("ranks-b") != 1 || f.api.sendCount() != 2 {
		t.Fatal("restart posted a duplicate server ranks panel")
	}
}

// Two guilds, each with its own server and SERVER_RANKS channel: each server gets
// its own panel in its own guild, and a server connected after startup is picked
// up on the next sync without a restart.
func TestServerRanksBoardsServeEveryGuild(t *testing.T) {
	f := newRoutedFixture(t)
	reader := &fixtureServerRanks{err: repository.ErrRankedIneligible}
	guilds := []GuildServers{{GuildRowID: 7, ServerIDs: []int64{1}}, {GuildRowID: 8, ServerIDs: []int64{3}}}
	f.resolver.set(7, 1, routeKeyServerRanks, "ranks-7")
	f.resolver.set(8, 3, routeKeyServerRanks, "ranks-8")
	board := NewServerRanksBoards(f.resolver, func(context.Context) ([]GuildServers, error) { return guilds, nil }, f.panels, reader)
	ctx := context.Background()
	board.SyncOnce(ctx)
	if f.api.liveIn("ranks-7") != 1 || f.api.liveIn("ranks-8") != 1 {
		t.Fatalf("expected one panel per guild, live=%v", f.api.live())
	}
	if f.store.messageFor(7, routeKeyServerRanks+":1", "ranks-7") == "" || f.store.messageFor(8, routeKeyServerRanks+":3", "ranks-8") == "" {
		t.Fatal("each panel must be recorded under its own guild")
	}
	// Guild 8 connects a second server later: its panel appears on the next sync.
	guilds[1].ServerIDs = []int64{3, 4}
	f.resolver.set(8, 4, routeKeyServerRanks, "ranks-8b")
	board.SyncOnce(ctx)
	if f.api.liveIn("ranks-8b") != 1 || f.api.sendCount() != 3 {
		t.Fatalf("a server added later must get its panel, live=%v", f.api.live())
	}
}
