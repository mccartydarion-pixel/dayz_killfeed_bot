package killfeed

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type countingZoneSource struct {
	calls int
	zones []repository.Zone
}

func (s *countingZoneSource) ActiveZonesForServer(_ context.Context, serverID int64) ([]repository.Zone, error) {
	s.calls++
	var out []repository.Zone
	for _, z := range s.zones {
		if z.ServerID == serverID {
			out = append(out, z)
		}
	}
	return out, nil
}

func TestZoneCacheServesFromCacheUntilInvalidated(t *testing.T) {
	src := &countingZoneSource{zones: []repository.Zone{{ID: 1, ServerID: 5}}}
	cache := NewZoneCache(src)
	ctx := context.Background()

	if _, err := cache.Zones(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Zones(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if src.calls != 1 {
		t.Fatalf("expected 1 DB call (second read served from cache), got %d", src.calls)
	}

	cache.Invalidate(5)
	if _, err := cache.Zones(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if src.calls != 2 {
		t.Fatalf("expected a fresh DB call after Invalidate, got %d calls", src.calls)
	}
}

func TestZoneCacheNilSafe(t *testing.T) {
	var c *ZoneCache
	if zones, err := c.Zones(context.Background(), 1); err != nil || zones != nil {
		t.Fatalf("nil cache must return (nil, nil), got (%v, %v)", zones, err)
	}
	c.Invalidate(1) // must not panic
}

func TestZoneCacheDifferentServersIsolated(t *testing.T) {
	src := &countingZoneSource{zones: []repository.Zone{{ID: 1, ServerID: 5}, {ID: 2, ServerID: 6}}}
	cache := NewZoneCache(src)
	ctx := context.Background()

	z5, _ := cache.Zones(ctx, 5)
	z6, _ := cache.Zones(ctx, 6)
	if len(z5) != 1 || z5[0].ID != 1 {
		t.Fatalf("unexpected server 5 zones: %+v", z5)
	}
	if len(z6) != 1 || z6[0].ID != 2 {
		t.Fatalf("unexpected server 6 zones: %+v", z6)
	}
}
