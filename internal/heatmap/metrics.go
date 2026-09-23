package heatmap

import (
	"sync"
	"time"
)

// Metrics is a thread-safe, mutex-guarded counter set (task section 24), mirroring
// internal/killfeed.IntrusionEngine's own Metrics()/mutex-guarded-snapshot-struct convention.
// nil-safe throughout: a nil *Metrics receiver on every method here is a no-op, so a Service built
// with metrics=nil (NewService's documented "collects nothing" mode) never needs a nil check at
// every call site.
type Metrics struct {
	mu      sync.Mutex
	overall counters
	byType  map[Type]*counters
}

type counters struct {
	Requests, RequestErrors         int64
	CacheHits, CacheMisses          int64
	CellsReturned, EventsAggregated int64
	RequestLatencyMs                int64
}

// NewMetrics constructs an empty Metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{byType: make(map[Type]*counters)}
}

// MetricsSnapshot is a point-in-time copy of Metrics' counters (task's exact metric list, section
// 24): heatmap_requests, heatmap_request_latency_ms (cumulative - divide by Requests for an
// average), heatmap_cells_returned, heatmap_events_aggregated, heatmap_query_errors,
// heatmap_cache_hit, heatmap_cache_miss, plus CacheEntries (task section 23's
// heatmap_cache_entries, sourced from the Cache itself, not tracked here).
type MetricsSnapshot struct {
	Requests, RequestErrors         int64
	CacheHits, CacheMisses          int64
	CacheEntries                    int64
	CellsReturned, EventsAggregated int64
	RequestLatencyMs                int64
}

func (m *Metrics) bucket(t Type) *counters {
	c, ok := m.byType[t]
	if !ok {
		c = &counters{}
		m.byType[t] = c
	}
	return c
}

func (m *Metrics) recordRequest(t Type, cells int, events int64, latency time.Duration, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := latency.Milliseconds()
	m.overall.Requests++
	m.overall.RequestLatencyMs += ms
	b := m.bucket(t)
	b.Requests++
	b.RequestLatencyMs += ms
	if err != nil {
		m.overall.RequestErrors++
		b.RequestErrors++
		return
	}
	m.overall.CellsReturned += int64(cells)
	m.overall.EventsAggregated += events
	b.CellsReturned += int64(cells)
	b.EventsAggregated += events
}

func (m *Metrics) recordCacheHit(t Type) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overall.CacheHits++
	m.bucket(t).CacheHits++
}

func (m *Metrics) recordCacheMiss(t Type) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.overall.CacheMisses++
	m.bucket(t).CacheMisses++
}

// Snapshot returns the overall (every type combined) counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return MetricsSnapshot{
		Requests: m.overall.Requests, RequestErrors: m.overall.RequestErrors,
		CacheHits: m.overall.CacheHits, CacheMisses: m.overall.CacheMisses,
		CellsReturned: m.overall.CellsReturned, EventsAggregated: m.overall.EventsAggregated,
		RequestLatencyMs: m.overall.RequestLatencyMs,
	}
}

// ByType returns t's own counters (task section 24: "break down by heatmap type where safe" -
// safe here, since these are pure counts, never player-identifying).
func (m *Metrics) ByType(t Type) MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.byType[t]
	if !ok {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		Requests: c.Requests, RequestErrors: c.RequestErrors,
		CacheHits: c.CacheHits, CacheMisses: c.CacheMisses,
		CellsReturned: c.CellsReturned, EventsAggregated: c.EventsAggregated,
		RequestLatencyMs: c.RequestLatencyMs,
	}
}
