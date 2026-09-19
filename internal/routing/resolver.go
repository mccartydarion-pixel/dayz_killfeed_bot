// Package routing resolves a Champion feature (a stable route key such as
// KILLFEED) to the Discord channel an installation configured for it - the
// runtime side of the SaaS installation_channel_routes table
// (internal/repository/saas_channel_routes_repository.go).
//
// Resolution is always keyed by (guild, DayZ server), never by guild alone:
// one Discord guild can host several DayZ servers, each with its own
// installation and its own routes.
package routing

import (
	"context"
	"sync"
	"time"
)

// Stable route keys - identical to the SaaS API's route_key vocabulary
// (internal/app/saas_api_channel_routes.go's championRouteBlueprint). Runtime
// publishers migrated onto the resolver so far: KILLFEED, LINK_GAMERTAG,
// STATS_LEADERBOARDS, AUTO_LEADERBOARD, ADMIN_LOGS, HITFEED and CONNECTIONS (see
// docs/SAAS_RUNTIME_ROUTING.md); the rest are declared here so later
// migrations reuse one vocabulary.
const (
	RouteKillfeed          = "KILLFEED"
	RoutePveFeed           = "PVE_FEED"
	RouteLinkGamertag      = "LINK_GAMERTAG"
	RouteStatsLeaderboards = "STATS_LEADERBOARDS"
	RouteAutoLeaderboard   = "AUTO_LEADERBOARD"
	RouteHitfeed           = "HITFEED"
	RouteBounty            = "BOUNTY"
	RouteBountyTracking    = "BOUNTY_TRACKING"
	RouteHeatmaps          = "HEATMAPS"
	RouteEconomy           = "ECONOMY"
	RouteCasino            = "CASINO"
	RouteShop              = "SHOP"
	RouteConnections       = "CONNECTIONS"
	RouteBuildFeed         = "BUILD_FEED"
	RouteAdminAlerts       = "ADMIN_ALERTS"
	RouteAdminLogs         = "ADMIN_LOGS"
)

// RouteStore is the persistence lookup the Resolver caches in front of -
// implemented by repository.ChannelRouteRepository.ResolveChannel.
type RouteStore interface {
	ResolveChannel(ctx context.Context, guildRowID, serverID int64, routeKey string) (channelID string, found bool, err error)
}

// DefaultTTL bounds how stale a cached route can be when nothing invalidates
// it explicitly: a route changed by another process (or a direct DB edit)
// takes effect within this window. In-process changes made through the SaaS
// API call InvalidateAll and take effect on the very next lookup.
const DefaultTTL = 30 * time.Second

// maxEntries caps the cache; it holds at most (servers x route keys)
// entries in practice, so hitting this means something is wrong - pruning
// expired entries first, then dropping everything, keeps memory bounded
// without ever serving a wrong answer.
const maxEntries = 4096

type cacheKey struct {
	guildRowID, serverID int64
	routeKey             string
}

type cacheEntry struct {
	channelID string
	found     bool
	expires   time.Time
}

// Resolver is a short-lived read-through cache over a RouteStore. It never
// caches errors, and both hits and misses expire after the TTL, so there is
// no permanent stale state. A nil *Resolver resolves nothing.
type Resolver struct {
	store RouteStore
	ttl   time.Duration
	now   func() time.Time

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

// NewResolver builds a Resolver; ttl <= 0 uses DefaultTTL.
func NewResolver(store RouteStore, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Resolver{store: store, ttl: ttl, now: time.Now, entries: make(map[cacheKey]cacheEntry)}
}

// Resolve returns the channel configured for routeKey on the installation
// owning (guildRowID, serverID) - guildRowID is the internal guilds.id and
// serverID the game_servers.id, exactly the identifiers a server worker
// already holds. found=false means no route is configured (callers apply
// their own legacy fallback); a non-nil error means the lookup itself
// failed, which callers should also treat as "fall back", never as fatal.
func (r *Resolver) Resolve(ctx context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	if r == nil || r.store == nil || guildRowID <= 0 || serverID <= 0 || routeKey == "" {
		return "", false, nil
	}
	key := cacheKey{guildRowID, serverID, routeKey}
	now := r.now()

	r.mu.Lock()
	if e, ok := r.entries[key]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.channelID, e.found, nil
	}
	r.mu.Unlock()

	channelID, found, err := r.store.ResolveChannel(ctx, guildRowID, serverID, routeKey)
	if err != nil {
		return "", false, err
	}

	r.mu.Lock()
	if len(r.entries) >= maxEntries {
		for k, e := range r.entries {
			if !now.Before(e.expires) {
				delete(r.entries, k)
			}
		}
		if len(r.entries) >= maxEntries {
			r.entries = make(map[cacheKey]cacheEntry)
		}
	}
	r.entries[key] = cacheEntry{channelID: channelID, found: found, expires: now.Add(r.ttl)}
	r.mu.Unlock()
	return channelID, found, nil
}

// InvalidateAll drops every cached entry so the next lookup re-reads the
// store. Called after any in-process route or installation-server change.
func (r *Resolver) InvalidateAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries = make(map[cacheKey]cacheEntry)
	r.mu.Unlock()
}
