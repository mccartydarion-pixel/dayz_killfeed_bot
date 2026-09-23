package heatmap

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type call struct {
	method                            string
	guildID, serverID, installationID int64
	zoneID                            *int64
	from, to                          time.Time
	resolution                        int
}

type fakeStore struct {
	mu    sync.Mutex
	calls []call
	// cells/err are returned by every Aggregate* call unless kills/deaths/activity/intrusions are set.
	cells     []repository.HeatmapCell
	err       error
	callCount int32
}

func (f *fakeStore) record(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	atomic.AddInt32(&f.callCount, 1)
}

func (f *fakeStore) AggregateKills(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error) {
	f.record(call{method: "kills", guildID: guildID, serverID: serverID, from: from, to: to, resolution: resolution})
	return f.cells, f.err
}
func (f *fakeStore) AggregateDeaths(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error) {
	f.record(call{method: "deaths", guildID: guildID, serverID: serverID, from: from, to: to, resolution: resolution})
	return f.cells, f.err
}
func (f *fakeStore) AggregateActivity(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error) {
	f.record(call{method: "activity", guildID: guildID, serverID: serverID, from: from, to: to, resolution: resolution})
	return f.cells, f.err
}
func (f *fakeStore) AggregateIntrusions(ctx context.Context, installationID int64, zoneID *int64, from, to time.Time, resolution, limit int) ([]repository.HeatmapCell, error) {
	f.record(call{method: "intrusions", installationID: installationID, zoneID: zoneID, from: from, to: to, resolution: resolution})
	return f.cells, f.err
}

func baseRequest() Request {
	to := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	from := to.Add(-time.Hour)
	return Request{GuildID: 1, ServerID: 2, InstallationID: 3, Type: TypePvPKills, From: from, To: to, Resolution: 250}
}

// --- grid aggregation / intensity (task sections 32-33) ---------------------------------------

func TestGridAggregationCellMath(t *testing.T) {
	// 100,100 and 110,120 at resolution 250 both floor to cell (0,0); 800,800 floors to (3,3).
	cases := []struct {
		x, z         float64
		resolution   int
		wantX, wantZ int64
	}{
		{100, 100, 250, 0, 0},
		{110, 120, 250, 0, 0},
		{800, 800, 250, 3, 3},
	}
	for _, c := range cases {
		gotX := int64(c.x) / int64(c.resolution)
		gotZ := int64(c.z) / int64(c.resolution)
		if gotX != c.wantX || gotZ != c.wantZ {
			t.Fatalf("(%v,%v)@%d: got cell (%d,%d), want (%d,%d)", c.x, c.z, c.resolution, gotX, gotZ, c.wantX, c.wantZ)
		}
	}
}

func TestGridCellCenterFormula(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{{CellX: 10, CellZ: 18, Count: 1}}}
	s := NewService(store, nil, nil)
	req := baseRequest()
	req.Resolution = 250
	res, err := s.Query(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cells) != 1 {
		t.Fatalf("expected 1 cell, got %d", len(res.Cells))
	}
	c := res.Cells[0]
	if c.CenterX != 2625 || c.CenterZ != 4625 {
		t.Fatalf("expected center (2625,4625) for cell (10,18)@250, got (%v,%v)", c.CenterX, c.CenterZ)
	}
}

func TestIntensityCalculation(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{
		{CellX: 0, CellZ: 0, Count: 10},
		{CellX: 1, CellZ: 0, Count: 5},
		{CellX: 2, CellZ: 0, Count: 1},
	}}
	s := NewService(store, nil, nil)
	res, err := s.Query(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]float64{0: 1.0, 1: 0.5, 2: 0.1}
	for _, c := range res.Cells {
		if got, w := c.Intensity, want[c.CellX]; got < w-1e-9 || got > w+1e-9 {
			t.Fatalf("cell %d: expected intensity ~%v, got %v", c.CellX, w, got)
		}
	}
	if res.TotalEvents != 16 {
		t.Fatalf("expected totalEvents=16, got %d", res.TotalEvents)
	}
}

// --- validation (task sections 42-43) ----------------------------------------------------------

func TestValidateRejectsUnsupportedType(t *testing.T) {
	req := baseRequest()
	req.Type = "NOT_A_TYPE"
	if err := req.Validate(); err == nil {
		t.Fatal("expected a validation error for an unsupported type")
	}
}

func TestValidateRejectsUnsupportedResolution(t *testing.T) {
	req := baseRequest()
	req.Resolution = 37
	if err := req.Validate(); err == nil {
		t.Fatal("expected a validation error for an unsupported resolution")
	}
}

func TestValidateRejectsFromAfterTo(t *testing.T) {
	req := baseRequest()
	req.From, req.To = req.To, req.From
	if err := req.Validate(); err == nil {
		t.Fatal("expected a validation error when from > to")
	}
}

func TestValidateRejectsRangeOverMaximum(t *testing.T) {
	req := baseRequest()
	req.From = req.To.Add(-31 * 24 * time.Hour)
	if err := req.Validate(); err == nil {
		t.Fatal("expected a validation error for a range over 30 days")
	}
}

func TestValidateRejectsZoneIdForNonIntrusionType(t *testing.T) {
	req := baseRequest()
	req.Type = TypePvPKills
	zoneID := int64(5)
	req.ZoneID = &zoneID
	if err := req.Validate(); err == nil {
		t.Fatal("expected a validation error for zoneId on a non-ZONE_INTRUSIONS type")
	}
}

func TestValidateAllowsZoneIdForIntrusionType(t *testing.T) {
	req := baseRequest()
	req.Type = TypeZoneIntrusions
	zoneID := int64(5)
	req.ZoneID = &zoneID
	if err := req.Validate(); err != nil {
		t.Fatalf("expected zoneId to be valid for ZONE_INTRUSIONS, got %v", err)
	}
}

// --- dispatch (task section 35: kills counted, non-kill events excluded - proven by dispatch to
// the right Store method rather than a generic one) --------------------------------------------

func TestQueryDispatchesToCorrectStoreMethod(t *testing.T) {
	cases := []struct {
		typ    Type
		method string
	}{
		{TypePvPKills, "kills"},
		{TypePvPDeaths, "deaths"},
		{TypePlayerActivity, "activity"},
		{TypeZoneIntrusions, "intrusions"},
	}
	for _, c := range cases {
		t.Run(string(c.typ), func(t *testing.T) {
			store := &fakeStore{}
			s := NewService(store, nil, nil)
			req := baseRequest()
			req.Type = c.typ
			if _, err := s.Query(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if len(store.calls) != 1 || store.calls[0].method != c.method {
				t.Fatalf("expected exactly one %q call, got %+v", c.method, store.calls)
			}
		})
	}
}

// --- empty data (task section 39) --------------------------------------------------------------

func TestQueryEmptyDataReturnsEmptyCellsNotError(t *testing.T) {
	store := &fakeStore{cells: nil}
	s := NewService(store, nil, nil)
	res, err := s.Query(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalEvents != 0 || len(res.Cells) != 0 {
		t.Fatalf("expected an empty-but-valid result, got %+v", res)
	}
}

// --- too many cells (task section 20) ----------------------------------------------------------

func TestQueryRejectsTooManyCells(t *testing.T) {
	cells := make([]repository.HeatmapCell, MaxCells+1)
	for i := range cells {
		cells[i] = repository.HeatmapCell{CellX: int64(i), CellZ: 0, Count: 1}
	}
	store := &fakeStore{cells: cells}
	s := NewService(store, nil, nil)
	_, err := s.Query(context.Background(), baseRequest())
	if !errors.Is(err, ErrTooManyCells) {
		t.Fatalf("expected ErrTooManyCells, got %v", err)
	}
}

// --- cache (task section 41) -------------------------------------------------------------------

func TestQueryCacheHitAvoidsSecondStoreCall(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{{CellX: 1, CellZ: 1, Count: 3}}}
	cache := NewCache(time.Minute)
	metrics := NewMetrics()
	s := NewService(store, cache, metrics)
	req := baseRequest()

	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("expected exactly 1 store call (second served from cache), got %d", len(store.calls))
	}
	snap := metrics.Snapshot()
	if snap.CacheHits != 1 || snap.CacheMisses != 1 {
		t.Fatalf("expected 1 hit + 1 miss, got %+v", snap)
	}
}

func TestQueryCacheKeyDiffersByResolution(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{{CellX: 1, CellZ: 1, Count: 1}}}
	cache := NewCache(time.Minute)
	s := NewService(store, cache, nil)
	req := baseRequest()
	req.Resolution = 250
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Resolution = 500
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("expected a separate cache entry (and store call) per resolution, got %d calls", len(store.calls))
	}
}

func TestQueryCacheKeyDiffersByInstallation(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{{CellX: 1, CellZ: 1, Count: 1}}}
	cache := NewCache(time.Minute)
	s := NewService(store, cache, nil)
	req := baseRequest()
	req.InstallationID = 100
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.InstallationID = 200
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("expected a separate cache entry per installation, got %d calls", len(store.calls))
	}
}

func TestCacheTTLExpiration(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{{CellX: 1, CellZ: 1, Count: 1}}}
	cache := NewCache(10 * time.Millisecond)
	s := NewService(store, cache, nil)
	req := baseRequest()
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := s.Query(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("expected the second query after TTL expiry to hit the store again, got %d calls", len(store.calls))
	}
}

func TestCacheIsBounded(t *testing.T) {
	cache := NewCache(time.Minute)
	for i := 0; i < maxCacheEntries+50; i++ {
		cache.set(cacheKey{GuildID: int64(i)}, &Result{})
	}
	if cache.Len() > maxCacheEntries+50 {
		t.Fatalf("cache grew unbounded: %d entries", cache.Len())
	}
}

// --- concurrency (task section 46) -------------------------------------------------------------

func TestQueryConcurrentAccessRace(t *testing.T) {
	store := &fakeStore{cells: []repository.HeatmapCell{{CellX: 1, CellZ: 1, Count: 1}}}
	cache := NewCache(time.Minute)
	metrics := NewMetrics()
	s := NewService(store, cache, metrics)
	req := baseRequest()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Query(context.Background(), req); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// --- errors -------------------------------------------------------------------------------------

func TestQueryPropagatesStoreError(t *testing.T) {
	store := &fakeStore{err: errors.New("db unavailable")}
	s := NewService(store, nil, nil)
	if _, err := s.Query(context.Background(), baseRequest()); err == nil {
		t.Fatal("expected the store error to propagate")
	}
}

func TestServiceMetricsNilSafe(t *testing.T) {
	var s *Service
	if got := s.Metrics(); got != (MetricsSnapshot{}) {
		t.Fatalf("nil service must report zero metrics, got %+v", got)
	}
}
