package discord

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type emptyStats struct{}

func (emptyStats) GetPlayerProfile(context.Context, int64, string) (*repository.PlayerProfile, error) {
	return nil, nil
}
func (emptyStats) TopByKills(context.Context, int64, int) ([]repository.LeaderboardEntry, error) {
	return nil, nil
}
func (emptyStats) TopByKD(context.Context, int64, int, int) ([]repository.LeaderboardEntry, error) {
	return nil, nil
}
func (emptyStats) TopLongestKill(context.Context, int64, int) ([]repository.LeaderboardEntry, error) {
	return nil, nil
}

// leaderboardFixture: a guild with a legacy leaderboard panel already live in
// "legacy-chan" (message "legacy-msg") and a scheduler routed over the fakes.
type leaderboardFixture struct {
	*routedFixture
	sched     *LeaderboardScheduler
	savedIDs  []string
	panel     *LeaderboardPanel
	legacyMsg string
}

func newLeaderboardFixture(t *testing.T) *leaderboardFixture {
	t.Helper()
	f := &leaderboardFixture{routedFixture: newRoutedFixture(t), legacyMsg: "legacy-msg"}
	_ = f.setup.Save(GuildSetup{GuildID: "g1", LeaderboardsChannelID: "legacy-chan", LeaderboardMessageID: f.legacyMsg})
	f.api.seed("legacy-chan", f.legacyMsg)
	f.panel = NewLeaderboardPanel(f.api, "legacy-chan", f.legacyMsg, DefaultLeaderboardConfig())
	f.sched = NewLeaderboardScheduler(f.panel, emptyStats{}, f.guild, DefaultLeaderboardConfig(), func(id string) {
		f.savedIDs = append(f.savedIDs, id)
		if gs, _ := f.setup.Get("g1"); gs != nil {
			gs.LeaderboardMessageID = id
			_ = f.setup.Save(*gs)
		}
	})
	f.sched.SetRouting(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, f.panels,
		NewLegacyLeaderboardRetirer(f.api, f.setup, "g1"))
	f.syncer.SetLeaderboard(f.sched)
	return f
}

func (f *leaderboardFixture) refresh() {
	f.t.Helper()
	if err := f.sched.RefreshOnce(context.Background()); err != nil {
		f.t.Fatalf("refresh: %v", err)
	}
}

// No route: today's behaviour exactly - the legacy message is edited in place.
func TestLeaderboardLegacyFallbackEditsLegacyMessage(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.refresh()
	if len(f.api.edits) != 1 || f.api.edits[0] != "legacy-chan/legacy-msg" {
		t.Fatalf("expected the legacy message edited in place, edits=%v", f.api.edits)
	}
	if f.api.sendCount() != 0 || f.store.count(f.guild, routeKeyAutoLeaderboard) != 0 {
		t.Fatalf("no route: nothing may be posted to routes, sends=%d", f.api.sendCount())
	}
}

// AUTO_LEADERBOARD route configured: it wins, and only it is published to.
func TestLeaderboardNewRouteWinsAndLegacyIsRetired(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "route-chan")
	f.refresh()

	if f.api.liveIn("route-chan") != 1 {
		t.Fatalf("expected the routed leaderboard, live=%v", f.api.live())
	}
	if f.api.liveIn("legacy-chan") != 0 {
		t.Fatalf("expected the legacy leaderboard retired (no duplicate), live=%v", f.api.live())
	}
	for _, e := range f.api.edits {
		if e == "legacy-chan/legacy-msg" {
			t.Fatal("the legacy message must not be edited once a route exists")
		}
	}
	if gs, _ := f.setup.Get("g1"); gs.LeaderboardMessageID != "" {
		t.Fatalf("expected the legacy message id cleared, got %q", gs.LeaderboardMessageID)
	}
}

// The persistent message stays ONE message across refreshes and restarts.
func TestLeaderboardRoutedMessageIsPersistentAcrossRefreshesAndRestart(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "route-chan")
	f.refresh()
	first := f.store.messageFor(f.guild, routeKeyAutoLeaderboard, "route-chan")
	f.refresh()
	f.refresh()
	if f.api.sendCount() != 1 || f.store.messageFor(f.guild, routeKeyAutoLeaderboard, "route-chan") != first {
		t.Fatalf("expected one persistent message edited in place, sends=%d", f.api.sendCount())
	}

	// "Restart": a brand new scheduler over the same durable state edits the
	// recorded message instead of posting another.
	panel := NewLeaderboardPanel(f.api, "legacy-chan", "", DefaultLeaderboardConfig())
	restarted := NewLeaderboardScheduler(panel, emptyStats{}, f.guild, DefaultLeaderboardConfig(), func(string) {})
	restarted.SetRouting(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, f.panels,
		NewLegacyLeaderboardRetirer(f.api, f.setup, "g1"))
	if err := restarted.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.api.sendCount() != 1 || f.api.liveIn("route-chan") != 1 {
		t.Fatalf("a restart must not duplicate the persistent message, sends=%d live=%v", f.api.sendCount(), f.api.live())
	}
}

// Changing the route moves the message (no leftover copy), and the syncer
// notices the change and refreshes without waiting for the 3-hour timer.
func TestLeaderboardRouteChangeUpdatesDestination(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "chan-old")
	f.refresh()
	f.syncer.SyncOnce(context.Background()) // first observation only records the routes
	sends := f.api.sendCount()

	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "chan-new")
	f.syncer.SyncOnce(context.Background()) // detects the change -> RefreshOnce

	if f.api.liveIn("chan-old") != 0 || f.api.liveIn("chan-new") != 1 {
		t.Fatalf("expected the leaderboard to move to chan-new, live=%v", f.api.live())
	}
	if f.api.sendCount() != sends+1 {
		t.Fatalf("expected exactly one new message, sends %d -> %d", sends, f.api.sendCount())
	}
	// An unchanged route causes no further refresh/posts.
	before := f.api.sendCount()
	f.syncer.SyncOnce(context.Background())
	if f.api.sendCount() != before {
		t.Fatal("an unchanged route must not post again")
	}
}

// Route removed: routed panel retired and a fresh legacy panel posted and persisted.
func TestLeaderboardRouteRemovedReturnsToLegacy(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "route-chan")
	f.refresh()

	f.resolver.clear(f.guild, 1, routeKeyAutoLeaderboard)
	f.refresh()

	if f.api.liveIn("route-chan") != 0 || f.store.count(f.guild, routeKeyAutoLeaderboard) != 0 {
		t.Fatalf("expected the routed leaderboard retired, live=%v", f.api.live())
	}
	if f.api.liveIn("legacy-chan") != 1 {
		t.Fatalf("expected exactly one legacy leaderboard back, live=%v", f.api.live())
	}
	if len(f.savedIDs) == 0 {
		t.Fatal("expected the new legacy message id persisted")
	}
}

// A failed route lookup falls back to legacy and never tears down routed panels.
func TestLeaderboardLookupFailureFallsBackToLegacy(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.fail(f.guild, 1, routeKeyAutoLeaderboard, errors.New("db down"))
	f.resolver.fail(f.guild, 2, routeKeyAutoLeaderboard, errors.New("db down"))
	f.refresh()
	if len(f.api.edits) != 1 || f.api.edits[0] != "legacy-chan/legacy-msg" {
		t.Fatalf("lookup failure must fall back to the legacy message, edits=%v", f.api.edits)
	}

	// With a routed panel already live, a failing lookup leaves it alone.
	g := newLeaderboardFixture(t)
	g.resolver.set(g.guild, 1, routeKeyAutoLeaderboard, "route-chan")
	g.refresh()
	g.resolver.clear(g.guild, 1, routeKeyAutoLeaderboard)
	g.resolver.fail(g.guild, 1, routeKeyAutoLeaderboard, errors.New("db down"))
	g.refresh()
	if g.api.liveIn("route-chan") != 1 || g.store.count(g.guild, routeKeyAutoLeaderboard) != 1 {
		t.Fatalf("a failed lookup must not retire the routed leaderboard, live=%v", g.api.live())
	}
}

// Two servers in one guild with different AUTO_LEADERBOARD routes each get the
// guild-wide board in their own channel; a shared channel gets exactly one.
func TestLeaderboardTwoServersSameGuild(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "chan-A")
	f.resolver.set(f.guild, 2, routeKeyAutoLeaderboard, "chan-B")
	f.refresh()
	if f.api.liveIn("chan-A") != 1 || f.api.liveIn("chan-B") != 1 {
		t.Fatalf("expected a leaderboard per server channel, live=%v", f.api.live())
	}

	g := newLeaderboardFixture(t)
	g.resolver.set(g.guild, 1, routeKeyAutoLeaderboard, "shared")
	g.resolver.set(g.guild, 2, routeKeyAutoLeaderboard, "shared")
	g.refresh()
	if g.api.liveIn("shared") != 1 {
		t.Fatalf("servers sharing a channel must share one leaderboard, live=%v", g.api.live())
	}
}

// Another guild's route (cross-org) never publishes here.
func TestLeaderboardCrossGuildRouteIgnored(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(99, 1, routeKeyAutoLeaderboard, "foreign-chan")
	f.refresh()
	if f.api.liveIn("foreign-chan") != 0 || len(f.api.edits) != 1 || f.api.edits[0] != "legacy-chan/legacy-msg" {
		t.Fatalf("foreign route must be ignored, live=%v edits=%v", f.api.live(), f.api.edits)
	}
}

// If routed panels cannot be recorded the refresh errors instead of quietly
// publishing to the legacy channel next to a configured route.
func TestLeaderboardRoutedStoreFailureDoesNotDoublePublish(t *testing.T) {
	f := newLeaderboardFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "route-chan")
	f.store.listErr = errors.New("db down")
	if err := f.sched.RefreshOnce(context.Background()); err == nil {
		t.Fatal("expected the refresh to report the failure")
	}
	if len(f.api.edits) != 0 || f.api.sendCount() != 0 {
		t.Fatalf("nothing may be published while the route panel store is failing, edits=%v sends=%d", f.api.edits, f.api.sendCount())
	}
}
