package killfeed

import (
	"context"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// ZoneSource is the DB read the zone cache falls back to on a miss/expiry.
type ZoneSource interface {
	ActiveZonesForServer(ctx context.Context, serverID int64) ([]repository.Zone, error)
}

// zoneCacheTTL is the short-fallback refresh interval (task section 26: the cache must be
// invalidated explicitly on zone CRUD, but also self-heals within this window if an invalidation
// is ever missed - e.g. a bug, or a write from a process that didn't call Invalidate).
const zoneCacheTTL = 30 * time.Second

// maxCachedServers/maxZonesPerServer bound the cache (task: "thread-safe, bounded").
const (
	maxCachedServers  = 4000
	maxZonesPerServer = 500
)

type zoneCacheEntry struct {
	zones     []repository.Zone
	expiresAt time.Time
}

// ZoneCache is a thread-safe, bounded, per-server cache of a server's enabled zones - the
// mechanism that lets the intrusion engine evaluate every location event without querying zones
// from the database per event (task section 26). Reads use only a short critical section; the DB
// fallback runs outside the lock.
type ZoneCache struct {
	source ZoneSource
	mu     sync.Mutex
	byID   map[int64]zoneCacheEntry
}

func NewZoneCache(source ZoneSource) *ZoneCache {
	return &ZoneCache{source: source, byID: make(map[int64]zoneCacheEntry)}
}

// Zones returns serverID's active zones, refreshing from source on a cache miss or TTL expiry.
func (c *ZoneCache) Zones(ctx context.Context, serverID int64) ([]repository.Zone, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	if e, ok := c.byID[serverID]; ok && time.Now().Before(e.expiresAt) {
		c.mu.Unlock()
		return e.zones, nil
	}
	c.mu.Unlock()

	if c.source == nil {
		return nil, nil
	}
	zones, err := c.source.ActiveZonesForServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if len(zones) > maxZonesPerServer {
		zones = zones[:maxZonesPerServer]
	}
	c.mu.Lock()
	if len(c.byID) >= maxCachedServers {
		c.evictExpiredLocked()
	}
	c.byID[serverID] = zoneCacheEntry{zones: zones, expiresAt: time.Now().Add(zoneCacheTTL)}
	c.mu.Unlock()
	return zones, nil
}

// evictExpiredLocked drops every already-expired entry - called only when the map has grown large,
// so normal operation never pays this cost. Caller holds c.mu.
func (c *ZoneCache) evictExpiredLocked() {
	now := time.Now()
	for k, e := range c.byID {
		if now.After(e.expiresAt) {
			delete(c.byID, k)
		}
	}
}

// Invalidate drops serverID's cached entry so the next Zones call refreshes from source
// immediately. Called by the SaaS API after any zone create/update/delete/enable-toggle.
func (c *ZoneCache) Invalidate(serverID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.byID, serverID)
	c.mu.Unlock()
}
