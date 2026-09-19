package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// hookStore is the minimal factionstats.Store the persisted-event hook test needs.
type hookStore struct {
	computes atomic.Int64
	scope    repository.HubStatsScope
}

func (h *hookStore) Scope(context.Context, int64, int64, int64) (repository.HubStatsScope, error) {
	return h.scope, nil
}
func (h *hookStore) ListScopes(context.Context, int64, int) ([]repository.HubStatsScope, error) {
	return nil, nil
}
func (h *hookStore) ScopesForPlayers(context.Context, int64, int64, []int64) ([]repository.HubStatsScope, error) {
	return []repository.HubStatsScope{h.scope}, nil
}
func (h *hookStore) ComputeStats(context.Context, repository.HubStatsScope) (*repository.HubStatsResult, error) {
	h.computes.Add(1)
	return &repository.HubStatsResult{Members: []repository.HubMemberStats{}}, nil
}
func (h *hookStore) Unlocks(context.Context, int64) (map[string]repository.HubUnlock, error) {
	return map[string]repository.HubUnlock{}, nil
}
func (h *hookStore) InsertUnlock(context.Context, repository.HubStatsScope, string, time.Time, map[string]any) (bool, error) {
	return true, nil
}
func (h *hookStore) NthEventTime(context.Context, repository.HubStatsScope, string, int) (*time.Time, error) {
	return nil, nil
}
func (h *hookStore) Activity(context.Context, repository.HubStatsScope, int, *repository.HubActivityCursor) ([]repository.HubActivityRow, bool, error) {
	return nil, false, nil
}

// The killfeed's persisted-kill and persisted-death hooks must tell the faction stats service, on
// every exit path (the adapter here has no streak repository, so ProcessPersistedKill returns early),
// without ever blocking or failing the kill.
func TestPersistedKillAndDeathHooksNotifyFactionStats(t *testing.T) {
	store := &hookStore{scope: repository.HubStatsScope{FactionID: 1, OrganizationID: 1, InstallationID: 1, GuildID: 7, ServerID: 9, CreatedAt: time.Now()}}
	svc := factionstats.NewService(store, factionstats.Options{CacheTTL: time.Hour})
	p := &persistenceStoreAdapter{factionStats: svc}
	ctx := context.Background()
	get := func() {
		t.Helper()
		if _, err := svc.GetFactionStats(ctx, 1, 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	get()
	get()
	if store.computes.Load() != 1 {
		t.Fatalf("warm cache: %d computes", store.computes.Load())
	}
	p.ProcessPersistedKill(ctx, 1, repository.KillRecord{GuildID: 7, ServerID: 9, KillerPlayerID: 5}, nil)
	get()
	if store.computes.Load() != 2 {
		t.Fatalf("a persisted kill must invalidate the guild+server's cached stats: %d computes", store.computes.Load())
	}
	p.ProcessPersistedDeath(ctx, repository.DeathRecord{GuildID: 7, ServerID: 9, PlayerID: 5}, nil)
	get()
	if store.computes.Load() != 3 {
		t.Fatalf("a persisted death must invalidate too: %d computes", store.computes.Load())
	}
	// Another server's events do not touch this faction's cache.
	p.ProcessPersistedKill(ctx, 2, repository.KillRecord{GuildID: 7, ServerID: 10, KillerPlayerID: 5}, nil)
	get()
	if store.computes.Load() != 3 {
		t.Fatalf("an event on another server must not invalidate: %d computes", store.computes.Load())
	}
	// The kill queued the killer for evaluation; the worker resolves and evaluates the faction once.
	if n := svc.ProcessPending(ctx); n != 1 {
		t.Fatalf("queued kill evaluation: %d", n)
	}
	// Without the feature wired (no database) the hooks are no-ops, never a panic.
	(&persistenceStoreAdapter{}).ProcessPersistedKill(ctx, 3, repository.KillRecord{GuildID: 7, KillerPlayerID: 5}, nil)
	(&persistenceStoreAdapter{}).ProcessPersistedDeath(ctx, repository.DeathRecord{GuildID: 7, PlayerID: 5}, nil)
}
