package routing

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	routes map[cacheKey]string
	err    error
	calls  int
}

func (f *fakeStore) ResolveChannel(ctx context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	f.calls++
	if f.err != nil {
		return "", false, f.err
	}
	id, ok := f.routes[cacheKey{guildRowID, serverID, routeKey}]
	return id, ok, nil
}

func TestRouteConstantsMatchSaaSVocabulary(t *testing.T) {
	want := []string{"KILLFEED", "PVE_FEED", "LINK_GAMERTAG", "STATS_LEADERBOARDS", "AUTO_LEADERBOARD", "HITFEED", "BOUNTY", "BOUNTY_TRACKING", "HEATMAPS", "ECONOMY", "CASINO", "SHOP", "CONNECTIONS", "BUILD_FEED", "ADMIN_ALERTS", "ADMIN_LOGS"}
	got := []string{RouteKillfeed, RoutePveFeed, RouteLinkGamertag, RouteStatsLeaderboards, RouteAutoLeaderboard, RouteHitfeed, RouteBounty, RouteBountyTracking, RouteHeatmaps, RouteEconomy, RouteCasino, RouteShop, RouteConnections, RouteBuildFeed, RouteAdminAlerts, RouteAdminLogs}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route constant %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolverCachesWithinTTL(t *testing.T) {
	store := &fakeStore{routes: map[cacheKey]string{{7, 1, RouteKillfeed}: "chan"}}
	r := NewResolver(store, time.Minute)
	for i := 0; i < 5; i++ {
		if id, ok, err := r.Resolve(context.Background(), 7, 1, RouteKillfeed); err != nil || !ok || id != "chan" {
			t.Fatalf("lookup %d = %q %v %v", i, id, ok, err)
		}
	}
	if store.calls != 1 {
		t.Fatalf("expected one store query for five lookups, got %d", store.calls)
	}
}

// After the TTL a changed route is picked up without a restart.
func TestResolverPicksUpRouteChangeAfterTTL(t *testing.T) {
	store := &fakeStore{routes: map[cacheKey]string{{7, 1, RouteKillfeed}: "old"}}
	r := NewResolver(store, 30*time.Second)
	now := time.Now()
	r.now = func() time.Time { return now }
	if id, _, _ := r.Resolve(context.Background(), 7, 1, RouteKillfeed); id != "old" {
		t.Fatalf("got %q", id)
	}
	store.routes[cacheKey{7, 1, RouteKillfeed}] = "new"
	if id, _, _ := r.Resolve(context.Background(), 7, 1, RouteKillfeed); id != "old" {
		t.Fatalf("expected the cached value inside the TTL, got %q", id)
	}
	now = now.Add(31 * time.Second)
	if id, _, _ := r.Resolve(context.Background(), 7, 1, RouteKillfeed); id != "new" {
		t.Fatalf("expected the new route after the TTL, got %q", id)
	}
}

// Explicit invalidation (used by in-process SaaS route writes) is immediate.
func TestResolverInvalidateAllIsImmediate(t *testing.T) {
	store := &fakeStore{routes: map[cacheKey]string{{7, 1, RouteKillfeed}: "old"}}
	r := NewResolver(store, time.Hour)
	r.Resolve(context.Background(), 7, 1, RouteKillfeed)
	store.routes[cacheKey{7, 1, RouteKillfeed}] = "new"
	r.InvalidateAll()
	if id, _, _ := r.Resolve(context.Background(), 7, 1, RouteKillfeed); id != "new" {
		t.Fatalf("expected the new route right after invalidation, got %q", id)
	}
}

func TestResolverCachesMissesBrieflyAndNeverCachesErrors(t *testing.T) {
	store := &fakeStore{routes: map[cacheKey]string{}}
	r := NewResolver(store, time.Minute)
	r.Resolve(context.Background(), 7, 1, RouteKillfeed)
	r.Resolve(context.Background(), 7, 1, RouteKillfeed)
	if store.calls != 1 {
		t.Fatalf("expected the miss to be cached briefly, got %d queries", store.calls)
	}

	failing := &fakeStore{err: errors.New("db down")}
	r2 := NewResolver(failing, time.Minute)
	if _, _, err := r2.Resolve(context.Background(), 7, 1, RouteKillfeed); err == nil {
		t.Fatal("expected the lookup error to surface")
	}
	failing.err = nil
	failing.routes = map[cacheKey]string{{7, 1, RouteKillfeed}: "chan"}
	if id, ok, err := r2.Resolve(context.Background(), 7, 1, RouteKillfeed); err != nil || !ok || id != "chan" {
		t.Fatalf("an error must never be cached; got %q %v %v", id, ok, err)
	}
}

// Routes are per (guild, server), never per guild alone.
func TestResolverKeepsServersInOneGuildSeparate(t *testing.T) {
	store := &fakeStore{routes: map[cacheKey]string{{7, 1, RouteKillfeed}: "chan-A", {7, 2, RouteKillfeed}: "chan-B"}}
	r := NewResolver(store, time.Minute)
	a, _, _ := r.Resolve(context.Background(), 7, 1, RouteKillfeed)
	b, _, _ := r.Resolve(context.Background(), 7, 2, RouteKillfeed)
	if a != "chan-A" || b != "chan-B" {
		t.Fatalf("got %q %q", a, b)
	}
}

func TestNilResolverResolvesNothing(t *testing.T) {
	var r *Resolver
	if id, ok, err := r.Resolve(context.Background(), 7, 1, RouteKillfeed); id != "" || ok || err != nil {
		t.Fatalf("got %q %v %v", id, ok, err)
	}
	r.InvalidateAll()
}
