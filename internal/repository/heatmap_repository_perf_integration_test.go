//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestHeatmapPerformanceAtScale is Champion Phase 5's load/performance test (task sections 19,
// 45): seeds a realistic-volume synthetic dataset spread across 30 days for one server, then
// measures AggregateKills/AggregateDeaths/AggregateActivity latency over 24h/7d/30d windows.
// Results are logged (t.Logf) rather than hard-asserted into a specific millisecond budget - local
// hardware varies too much for a fixed threshold to be meaningful CI signal - but the test does
// assert each query completes well within the request's own adminTimeout-class budget (10s), so a
// genuine quadratic-scan regression would still fail it.
func TestHeatmapPerformanceAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heatmap performance test in -short mode")
	}
	db, repo, fx := newHeatmapWorld(t)
	ctx := context.Background()

	const (
		playerCount   = 200
		killCount     = 20000
		deathCount    = 20000
		locationCount = 150000
		spanDays      = 30
	)
	now := time.Now().UTC().Truncate(time.Second)
	spanStart := now.Add(-spanDays * 24 * time.Hour)

	players := make([]int64, playerCount)
	for i := range players {
		players[i] = hmSeedPlayer(t, db, fx.GuildRowID, fmt.Sprintf("Perf%d", i))
	}

	seedStart := time.Now()

	// Bulk-seed player_location_events via CopyFrom (KILL/DEATH/HIT rows spread across the full
	// window) - this is what makes coordinate recovery for kills/deaths possible, exactly like
	// production data.
	locRows := make([][]any, 0, killCount+deathCount+locationCount)
	killTimes := make([]time.Time, killCount)
	killPlayers := make([]int64, killCount)
	for i := 0; i < killCount; i++ {
		at := spanStart.Add(time.Duration(i) * spanDays * 24 * time.Hour / killCount)
		p := players[i%playerCount]
		killTimes[i] = at
		killPlayers[i] = p
		locRows = append(locRows, []any{fx.GuildRowID, fx.ServerRowID, p, "p", float64(i % 8000), float64((i * 7) % 8000), "KILL", at})
	}
	deathTimes := make([]time.Time, deathCount)
	deathPlayers := make([]int64, deathCount)
	for i := 0; i < deathCount; i++ {
		at := spanStart.Add(time.Duration(i) * spanDays * 24 * time.Hour / deathCount)
		p := players[(i+1)%playerCount]
		deathTimes[i] = at
		deathPlayers[i] = p
		locRows = append(locRows, []any{fx.GuildRowID, fx.ServerRowID, p, "p", float64((i * 3) % 8000), float64((i * 11) % 8000), "DEATH", at})
	}
	for i := 0; i < locationCount; i++ {
		at := spanStart.Add(time.Duration(i) * spanDays * 24 * time.Hour / locationCount)
		p := players[i%playerCount]
		locRows = append(locRows, []any{fx.GuildRowID, fx.ServerRowID, p, "p", float64((i * 13) % 8000), float64((i * 17) % 8000), "HIT", at})
	}
	if _, err := db.Pool.CopyFrom(ctx, pgx.Identifier{"player_location_events"},
		[]string{"guild_id", "server_id", "player_id", "gamertag", "x", "z", "event_type", "observed_at"},
		pgx.CopyFromRows(locRows)); err != nil {
		t.Fatal(err)
	}

	killRows := make([][]any, killCount)
	for i := 0; i < killCount; i++ {
		killRows[i] = []any{fx.GuildRowID, fx.ServerRowID, "s", fmt.Sprintf("perf-kill-%d", i), killPlayers[i], players[(i+1)%playerCount], killTimes[i]}
	}
	if _, err := db.Pool.CopyFrom(ctx, pgx.Identifier{"kills"},
		[]string{"guild_id", "server_id", "session_id", "event_fingerprint", "killer_player_id", "victim_player_id", "event_time"},
		pgx.CopyFromRows(killRows)); err != nil {
		t.Fatal(err)
	}

	deathRows := make([][]any, deathCount)
	for i := 0; i < deathCount; i++ {
		deathRows[i] = []any{fx.GuildRowID, fx.ServerRowID, "s", fmt.Sprintf("perf-death-%d", i), deathPlayers[i], "DEATH", deathTimes[i]}
	}
	if _, err := db.Pool.CopyFrom(ctx, pgx.Identifier{"deaths"},
		[]string{"guild_id", "server_id", "session_id", "event_fingerprint", "player_id", "death_type", "event_time"},
		pgx.CopyFromRows(deathRows)); err != nil {
		t.Fatal(err)
	}

	t.Logf("seeded %d kills, %d deaths, %d location events (%d players, %d-day span) in %s",
		killCount, deathCount, locationCount, playerCount, spanDays, time.Since(seedStart))

	// A bulk COPY does not update planner statistics itself (autovacuum's ANALYZE may not have run
	// yet by the time the very next query executes) - explicitly analyzing the three tables this
	// benchmark just loaded is standard practice after any bulk load and avoids benchmarking a
	// stale-statistics query plan instead of the real one.
	for _, tbl := range []string{"kills", "deaths", "player_location_events"} {
		if _, err := db.Pool.Exec(ctx, "ANALYZE "+tbl); err != nil {
			t.Fatal(err)
		}
	}

	windows := []struct {
		name string
		from time.Time
	}{
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"30d", now.Add(-30 * 24 * time.Hour)},
	}

	measure := func(name string, fn func() (int, error)) {
		start := time.Now()
		cells, err := fn()
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
		t.Logf("%-24s cells=%-5d latency=%s", name, cells, elapsed)
		if elapsed > 10*time.Second {
			t.Errorf("%s took %s - exceeds the 10s admin-request budget", name, elapsed)
		}
	}

	for _, w := range windows {
		w := w
		measure(fmt.Sprintf("AggregateKills[%s]", w.name), func() (int, error) {
			cells, err := repo.AggregateKills(ctx, fx.GuildRowID, fx.ServerRowID, w.from, now, 250, MaxCellsPlusOneForTest)
			return len(cells), err
		})
		measure(fmt.Sprintf("AggregateDeaths[%s]", w.name), func() (int, error) {
			cells, err := repo.AggregateDeaths(ctx, fx.GuildRowID, fx.ServerRowID, w.from, now, 250, MaxCellsPlusOneForTest)
			return len(cells), err
		})
		measure(fmt.Sprintf("AggregateActivity[%s]", w.name), func() (int, error) {
			cells, err := repo.AggregateActivity(ctx, fx.GuildRowID, fx.ServerRowID, w.from, now, 250, MaxCellsPlusOneForTest)
			return len(cells), err
		})
	}
}

// MaxCellsPlusOneForTest mirrors internal/heatmap.MaxCells+1 (the service layer's own bound) -
// duplicated as a small constant here rather than importing internal/heatmap into
// internal/repository's test package, which would invert this codebase's established layering
// (repository never depends on a higher-level package, even in tests).
const MaxCellsPlusOneForTest = 5001
