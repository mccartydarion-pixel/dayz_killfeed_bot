package routing

import (
	"context"
	"sync/atomic"
	"testing"
)

// benchStore is a RouteStore that never misses the cache after warmup - used
// to isolate the resolver's own cache-hit cost (lock + map read) from any
// store/database latency, which is measured separately.
type benchStore struct{ calls atomic.Int64 }

func (s *benchStore) ResolveChannel(_ context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	s.calls.Add(1)
	return "chan-1", true, nil
}

// BenchmarkResolverCacheHitConcurrent measures the resolver's cache-hit path
// (Champion Performance Phase 1, section 11/12: "route resolution cache
// hit: < 5ms" target) under concurrent load representative of many server
// workers resolving routes at once. All requests hit the same (guild,
// server, routeKey) key on purpose - a single global lock around the cache
// is exactly what would bottleneck under this pattern.
func BenchmarkResolverCacheHitConcurrent(b *testing.B) {
	store := &benchStore{}
	r := NewResolver(store, DefaultTTL)
	ctx := context.Background()
	// Warm the cache once so every benchmarked call is a hit.
	if _, _, err := r.Resolve(ctx, 1, 1, RouteKillfeed); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, _, err := r.Resolve(ctx, 1, 1, RouteKillfeed); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkResolverCacheHitConcurrentManyKeys is the same but spreads load
// across many distinct (server, routeKey) keys, the more realistic shape for
// a multi-server deployment - each server's workers resolve their OWN routes,
// not a shared hot key.
func BenchmarkResolverCacheHitConcurrentManyKeys(b *testing.B) {
	store := &benchStore{}
	r := NewResolver(store, DefaultTTL)
	ctx := context.Background()
	const servers = 32
	for s := int64(1); s <= servers; s++ {
		if _, _, err := r.Resolve(ctx, 1, s, RouteKillfeed); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var i int64
		for pb.Next() {
			serverID := (i % servers) + 1
			i++
			if _, _, err := r.Resolve(ctx, 1, serverID, RouteKillfeed); err != nil {
				b.Fatal(err)
			}
		}
	})
}
