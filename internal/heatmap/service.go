// Package heatmap implements Champion Phase 5 (docs/HEATMAPS.md): PvP kill/death, player-activity,
// and zone-intrusion heatmap datasets aggregated from Phase 3/4's already-persisted data. This
// package is deliberately independent of internal/killfeed - it is a pure, cacheable read path
// over internal/repository, never a hot-path or event-pipeline component, so it has no reason to
// live alongside the ADM parser code.
package heatmap

import (
	"context"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Type is a supported heatmap dataset (task section 1). HITS is explicitly not implemented this
// phase (task: "Only implement HITS if hit events contain reliable persisted coordinates" - they
// do, via the same player_location_events pipeline as kills/deaths, but adding a fifth type was
// judged unnecessary scope for an "optional" item given the four required types already cover the
// task's core goal - see docs/HEATMAPS.md "Deferred").
type Type string

const (
	TypePvPKills       Type = "PVP_KILLS"
	TypePvPDeaths      Type = "PVP_DEATHS"
	TypePlayerActivity Type = "PLAYER_ACTIVITY"
	TypeZoneIntrusions Type = "ZONE_INTRUSIONS"
)

var validTypes = map[Type]bool{TypePvPKills: true, TypePvPDeaths: true, TypePlayerActivity: true, TypeZoneIntrusions: true}

// ValidType reports whether t is one of the four supported heatmap types.
func ValidType(t Type) bool { return validTypes[t] }

// validResolutions is the fixed, bounded set of grid resolutions (task section 6: "Do not allow
// arbitrary values that could cause expensive queries").
var validResolutions = map[int]bool{50: true, 100: true, 250: true, 500: true, 1000: true}

// ValidResolution reports whether r is one of the supported grid resolutions.
func ValidResolution(r int) bool { return validResolutions[r] }

const (
	// MaxCustomRangeDays bounds any from/to span (task section 8).
	MaxCustomRangeDays = 30
	// MaxCells bounds the response size (task section 20) - a request that would return more is
	// rejected with an actionable message, never silently re-resolutioned.
	MaxCells = 5000
	// DefaultResolution/DefaultWindow apply when a caller omits resolution/from/to.
	DefaultResolution = 250
	DefaultWindow     = 24 * time.Hour
)

// ValidationError is a caller-fixable request problem - internal/app maps this to HTTP 400,
// everything else to 500 (task section 49).
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// ErrTooManyCells is returned when an aggregation would exceed MaxCells (task section 20:
// "reject with an actionable message. Do not silently mutate requested resolution").
var ErrTooManyCells = &ValidationError{Message: "this query would return more than 5000 cells - narrow the time range or increase the resolution"}

// Request is one heatmap query, already resolved to the caller's guild/server/installation scope
// (internal/app's handler resolves organizationID/installationID -> these via ClientAdminRepository.
// Scope, exactly like every other Client Admin route - never re-derived here).
type Request struct {
	GuildID, ServerID, InstallationID int64
	Type                              Type
	From, To                          time.Time
	Resolution                        int
	// ZoneID is only ever honored for TypeZoneIntrusions (task section 10: PVP/ACTIVITY zone
	// filtering is explicitly optional and was not built this phase to avoid delaying the core
	// implementation) - Validate rejects it for any other type rather than silently ignoring it.
	ZoneID *int64
}

// Validate applies every request-shape rule the task specifies (sections 6, 9, 10). Business-rule
// validation lives here, independent of HTTP - internal/app's handler only parses/maps, never
// re-implements these checks.
func (req Request) Validate() error {
	if !ValidType(req.Type) {
		return &ValidationError{Message: "type must be one of PVP_KILLS, PVP_DEATHS, PLAYER_ACTIVITY, ZONE_INTRUSIONS"}
	}
	if !ValidResolution(req.Resolution) {
		return &ValidationError{Message: "resolution must be one of 50, 100, 250, 500, 1000"}
	}
	if req.From.IsZero() || req.To.IsZero() {
		return &ValidationError{Message: "from and to are required"}
	}
	if !req.To.After(req.From) {
		return &ValidationError{Message: "to must be after from"}
	}
	if req.To.Sub(req.From) > MaxCustomRangeDays*24*time.Hour {
		return &ValidationError{Message: "the requested range must not exceed 30 days"}
	}
	if req.ZoneID != nil && req.Type != TypeZoneIntrusions {
		return &ValidationError{Message: "zoneId is only supported for type=ZONE_INTRUSIONS"}
	}
	return nil
}

// Cell is one aggregated grid cell, ready for the wire (task section 4).
type Cell struct {
	CellX, CellZ     int64
	CenterX, CenterZ float64
	Count            int64
	Intensity        float64
}

// Result is one heatmap query's full response shape.
type Result struct {
	Type        Type
	From, To    time.Time
	Resolution  int
	TotalEvents int64
	Cells       []Cell
}

// Store is the DB surface the service needs - satisfied by *repository.HeatmapRepository.
// Depending on this narrow interface (rather than the concrete type) lets tests substitute a fake
// without a database, matching this codebase's established pattern (internal/killfeed.LocationStore
// etc).
type Store interface {
	AggregateKills(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error)
	AggregateDeaths(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error)
	AggregateActivity(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error)
	AggregateIntrusions(ctx context.Context, installationID int64, zoneID *int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error)
}

// Service is the heatmap business-logic layer (task section 31): validation, cache, aggregation
// dispatch, intensity calculation, response shaping. The HTTP handler (internal/app/
// saas_api_heatmap.go) is limited to auth, capability check, parameter parsing, and writing the
// response - it never builds SQL or touches the cache directly.
type Service struct {
	store   Store
	cache   *Cache
	metrics *Metrics
}

// NewService constructs a Service. cache/metrics may be nil (a nil cache disables caching
// entirely - every call goes to the store; a nil metrics collects nothing) - both nil-safe
// throughout, matching this codebase's established defensive-nil style.
func NewService(store Store, cache *Cache, metrics *Metrics) *Service {
	return &Service{store: store, cache: cache, metrics: metrics}
}

// Metrics returns a point-in-time snapshot of this service's cumulative counters.
func (s *Service) Metrics() MetricsSnapshot {
	if s == nil || s.metrics == nil {
		return MetricsSnapshot{}
	}
	return s.metrics.Snapshot()
}

// Query validates req, serves from cache when possible, otherwise aggregates via Store, computes
// intensity, and caches the shaped result (task sections 5, 7, 21).
func (s *Service) Query(ctx context.Context, req Request) (*Result, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	key := cacheKeyFor(req)
	if s.cache != nil {
		if cached, ok := s.cache.get(key); ok {
			s.metrics.recordCacheHit(req.Type)
			return cached, nil
		}
		s.metrics.recordCacheMiss(req.Type)
	}

	start := time.Now()
	cells, err := s.aggregate(ctx, req)
	latency := time.Since(start)
	if err != nil {
		s.metrics.recordRequest(req.Type, 0, 0, latency, err)
		return nil, err
	}
	if len(cells) > MaxCells {
		s.metrics.recordRequest(req.Type, 0, 0, latency, ErrTooManyCells)
		return nil, ErrTooManyCells
	}

	result := shapeResult(req, cells)
	s.metrics.recordRequest(req.Type, len(result.Cells), result.TotalEvents, latency, nil)
	if s.cache != nil {
		s.cache.set(key, result)
	}
	return result, nil
}

// aggregate dispatches to the one Store method matching req.Type. limit is MaxCells+1: the store
// query itself is bounded (task section 20's "protect the API" applies to the query too, not just
// the response), and returning one row over the limit is exactly what lets Query detect "too many
// cells" without needing a separate COUNT query.
func (s *Service) aggregate(ctx context.Context, req Request) ([]repository.HeatmapCell, error) {
	const limit = MaxCells + 1
	switch req.Type {
	case TypePvPKills:
		return s.store.AggregateKills(ctx, req.GuildID, req.ServerID, req.From, req.To, req.Resolution, limit)
	case TypePvPDeaths:
		return s.store.AggregateDeaths(ctx, req.GuildID, req.ServerID, req.From, req.To, req.Resolution, limit)
	case TypePlayerActivity:
		return s.store.AggregateActivity(ctx, req.GuildID, req.ServerID, req.From, req.To, req.Resolution, limit)
	case TypeZoneIntrusions:
		return s.store.AggregateIntrusions(ctx, req.InstallationID, req.ZoneID, req.From, req.To, req.Resolution, limit)
	default:
		return nil, &ValidationError{Message: "unsupported heatmap type"}
	}
}

// shapeResult computes cell centers (task section 5's formula) and normalized intensity (task
// section 7: count/maxCellCount, range 0.0-1.0) - pure Go, over an already-bounded (<=MaxCells)
// slice, never over raw events.
func shapeResult(req Request, cells []repository.HeatmapCell) *Result {
	out := &Result{Type: req.Type, From: req.From, To: req.To, Resolution: req.Resolution, Cells: []Cell{}}
	if len(cells) == 0 {
		return out
	}
	var maxCount, total int64
	for _, c := range cells {
		if c.Count > maxCount {
			maxCount = c.Count
		}
		total += c.Count
	}
	out.TotalEvents = total
	res := float64(req.Resolution)
	out.Cells = make([]Cell, 0, len(cells))
	for _, c := range cells {
		var intensity float64
		if maxCount > 0 {
			intensity = float64(c.Count) / float64(maxCount)
		}
		out.Cells = append(out.Cells, Cell{
			CellX: c.CellX, CellZ: c.CellZ,
			CenterX: float64(c.CellX)*res + res/2, CenterZ: float64(c.CellZ)*res + res/2,
			Count: c.Count, Intensity: intensity,
		})
	}
	return out
}

// --- cache (task sections 21-23) --------------------------------------------------------------

type cacheKey struct {
	GuildID, ServerID, InstallationID int64
	Type                              Type
	FromUnixNano, ToUnixNano          int64
	Resolution                        int
	ZoneID                            int64 // 0 = none
}

func cacheKeyFor(req Request) cacheKey {
	var zoneID int64
	if req.ZoneID != nil {
		zoneID = *req.ZoneID
	}
	return cacheKey{
		GuildID: req.GuildID, ServerID: req.ServerID, InstallationID: req.InstallationID,
		Type: req.Type, FromUnixNano: req.From.UnixNano(), ToUnixNano: req.To.UnixNano(),
		Resolution: req.Resolution, ZoneID: zoneID,
	}
}

// cacheEntry's expiresAt makes the TTL self-expiring on read (get checks it) - no background
// sweep goroutine is needed for a cache this short-lived (task: "short TTL is enough for this
// phase").
type cacheEntry struct {
	result    *Result
	expiresAt time.Time
}

// maxCacheEntries bounds the cache (task section 22: "must be bounded... do not let arbitrary
// query combinations create unbounded memory growth") - mirrors internal/killfeed.ZoneCache's
// exact bounded-map-with-opportunistic-eviction pattern.
const maxCacheEntries = 2000

// Cache is a thread-safe, bounded, short-TTL cache of shaped heatmap results, keyed by every
// dimension that changes the query (task section 21).
type Cache struct {
	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
	ttl     time.Duration
}

// NewCache constructs a Cache with the given TTL (task's suggested 30-60s).
func NewCache(ttl time.Duration) *Cache {
	return &Cache{entries: make(map[cacheKey]cacheEntry), ttl: ttl}
}

func (c *Cache) get(k cacheKey) (*Result, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.result, true
}

func (c *Cache) set(k cacheKey, r *Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCacheEntries {
		c.evictExpiredLocked()
	}
	c.entries[k] = cacheEntry{result: r, expiresAt: time.Now().Add(c.ttl)}
}

func (c *Cache) evictExpiredLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// Len returns the current entry count (task's heatmap_cache_entries metric).
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
