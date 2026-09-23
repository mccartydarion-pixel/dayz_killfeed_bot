//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// hmPlayerSeq guarantees a unique dayz_player_id per hmSeedPlayer call even when called many
// times in a tight loop within the same clock tick (Windows' time.Now() resolution is coarser
// than a Go loop iteration, so UnixNano() alone can collide).
var hmPlayerSeq int64

// Champion Phase 5 (docs/HEATMAPS.md) integration tests: grid aggregation, the exact-timestamp
// coordinate-recovery join for kills/deaths/intrusions, player-activity dedupe sampling, time-range
// filtering, and cross-tenant isolation - all against a real PostgreSQL. Reuses saas_repository_
// integration_test.go's saasIntegrationDB/newSaaSFixture (same package).

func newHeatmapWorld(t *testing.T) (*database.DB, *HeatmapRepository, saasFixture) {
	t.Helper()
	db := saasIntegrationDB(t)
	fx := newSaaSFixture(t, db)
	return db, NewHeatmapRepository(db.Pool), fx
}

func hmSeedPlayer(t *testing.T, db *database.DB, guildID int64, name string) int64 {
	t.Helper()
	var id int64
	seq := atomic.AddInt64(&hmPlayerSeq, 1)
	if err := db.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		guildID, fmt.Sprintf("hm-%d-%d-%d", guildID, time.Now().UnixNano(), seq), name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func hmSeedKill(t *testing.T, db *database.DB, guildID, serverID, killerID, victimID int64, at time.Time) {
	t.Helper()
	fp := fmt.Sprintf("hm-kill-%d", time.Now().UnixNano())
	if _, err := db.Pool.Exec(context.Background(), `
INSERT INTO kills(guild_id,server_id,session_id,event_fingerprint,killer_player_id,victim_player_id,event_time)
VALUES($1,$2,'s',$3,$4,$5,$6)`, guildID, serverID, fp, killerID, victimID, at); err != nil {
		t.Fatal(err)
	}
}

func hmSeedDeath(t *testing.T, db *database.DB, guildID, serverID, playerID int64, at time.Time) {
	t.Helper()
	fp := fmt.Sprintf("hm-death-%d", time.Now().UnixNano())
	if _, err := db.Pool.Exec(context.Background(), `
INSERT INTO deaths(guild_id,server_id,session_id,event_fingerprint,player_id,death_type,event_time)
VALUES($1,$2,'s',$3,$4,'DEATH',$5)`, guildID, serverID, fp, playerID, at); err != nil {
		t.Fatal(err)
	}
}

func hmSeedLocation(t *testing.T, db *database.DB, guildID, serverID, playerID int64, x, z float64, eventType string, at time.Time) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `
INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at)
VALUES($1,$2,$3,'p',$4,$5,$6,$7) ON CONFLICT DO NOTHING`, guildID, serverID, playerID, x, z, eventType, at); err != nil {
		t.Fatal(err)
	}
}

// --- kills (task section 35: kills counted, and the coordinate is the exact-timestamp match) ---

func TestAggregateKillsUsesExactTimestampMatchAndExcludesUnmatched(t *testing.T) {
	db, repo, fx := newHeatmapWorld(t)
	killer := hmSeedPlayer(t, db, fx.GuildRowID, "Killer")
	victim := hmSeedPlayer(t, db, fx.GuildRowID, "Victim")
	base := time.Now().UTC().Truncate(time.Second)

	// A kill WITH a matching KILL-type location row at the exact event_time - must be counted.
	hmSeedKill(t, db, fx.GuildRowID, fx.ServerRowID, killer, victim, base)
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, killer, 2625, 4625, "KILL", base)

	// A second kill with NO matching location row at all - must be silently excluded, never fabricated.
	hmSeedKill(t, db, fx.GuildRowID, fx.ServerRowID, killer, victim, base.Add(time.Minute))

	cells, err := repo.AggregateKills(context.Background(), fx.GuildRowID, fx.ServerRowID, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 {
		t.Fatalf("expected exactly 1 aggregated cell (the unmatched kill must be excluded), got %+v", cells)
	}
	if cells[0].CellX != 10 || cells[0].CellZ != 18 || cells[0].Count != 1 {
		t.Fatalf("expected cell (10,18) count 1, got %+v", cells[0])
	}
}

// --- deaths (task section 36: respawn/unconscious excluded) ------------------------------------

func TestAggregateDeathsExcludesNonDeathEventTypes(t *testing.T) {
	db, repo, fx := newHeatmapWorld(t)
	player := hmSeedPlayer(t, db, fx.GuildRowID, "Victim")
	base := time.Now().UTC().Truncate(time.Second)

	hmSeedDeath(t, db, fx.GuildRowID, fx.ServerRowID, player, base)
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, player, 500, 500, "DEATH", base)

	// RESPAWN/UNCONSCIOUS location rows for the SAME player at DIFFERENT times, with no
	// corresponding deaths row - must never be counted as a death.
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, player, 9000, 9000, "RESPAWN", base.Add(time.Minute))
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, player, 9500, 9500, "UNCONSCIOUS", base.Add(2*time.Minute))

	cells, err := repo.AggregateDeaths(context.Background(), fx.GuildRowID, fx.ServerRowID, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range cells {
		total += c.Count
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 counted death (respawn/unconscious excluded), got total=%d cells=%+v", total, cells)
	}
}

// --- player activity dedupe (task sections 11, 37) ----------------------------------------------

func TestAggregateActivityDedupesPerPlayerPerMinutePerCell(t *testing.T) {
	db, repo, fx := newHeatmapWorld(t)
	p1 := hmSeedPlayer(t, db, fx.GuildRowID, "Alice")
	p2 := hmSeedPlayer(t, db, fx.GuildRowID, "Bob")
	base := time.Now().UTC().Truncate(time.Minute).Add(time.Second) // safely inside one minute bucket

	// 20 noisy pings from the SAME player, SAME minute, SAME cell - must contribute exactly once.
	for i := 0; i < 20; i++ {
		hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, p1, 100, 100, "HIT", base.Add(time.Duration(i)*time.Millisecond*100))
	}
	// A second player in the same cell, same minute - a separate contribution.
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, p2, 110, 120, "HIT", base)
	// The SAME first player, a different minute, same cell - a separate contribution (new bucket).
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, p1, 100, 100, "HIT", base.Add(90*time.Second))

	cells, err := repo.AggregateActivity(context.Background(), fx.GuildRowID, fx.ServerRowID, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range cells {
		total += c.Count
	}
	if total != 3 {
		t.Fatalf("expected 3 contributions (p1@min1, p2@min1, p1@min2), got %d (cells=%+v)", total, cells)
	}
}

// --- zone intrusions (task sections 14, 38) ------------------------------------------------------

func TestAggregateIntrusionsOneContributionPerIntrusionEntry(t *testing.T) {
	db, repo, fx := newHeatmapWorld(t)
	zones := NewZoneRepository(db.Pool)
	zone, err := zones.CreateZone(context.Background(), fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Zone", ZoneTypeRestricted, 0, 0, 500, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	player := hmSeedPlayer(t, db, fx.GuildRowID, "Intruder")
	base := time.Now().UTC().Truncate(time.Second)

	it, err := zones.CreateIntrusion(context.Background(), zone.ID, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, player, "Intruder", false, base, true)
	if err != nil {
		t.Fatal(err)
	}
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, player, 250, 250, "HIT", base)
	// Repeated inside-zone location updates AFTER entry - must never inflate the intrusion count
	// (the heatmap counts zone_intrusions rows, never location events).
	for i := 1; i <= 10; i++ {
		hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, player, 251, 251, "HIT", base.Add(time.Duration(i)*time.Second))
	}
	if err := zones.MarkExited(context.Background(), it.ID, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	cells, err := repo.AggregateIntrusions(context.Background(), fx.InstallationID, nil, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range cells {
		total += c.Count
	}
	if total != 1 {
		t.Fatalf("expected exactly 1 intrusion contribution regardless of subsequent inside-zone updates, got %d", total)
	}

	// Zone filter: a different, nonexistent zone id must exclude it.
	otherZone := zone.ID + 999999
	filtered, err := repo.AggregateIntrusions(context.Background(), fx.InstallationID, &otherZone, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected 0 cells for an unrelated zoneId filter, got %+v", filtered)
	}
}

// --- time range (task section 34) -----------------------------------------------------------

func TestAggregateExcludesEventsOutsideTimeRange(t *testing.T) {
	db, repo, fx := newHeatmapWorld(t)
	player := hmSeedPlayer(t, db, fx.GuildRowID, "Victim")
	base := time.Now().UTC().Truncate(time.Second)

	hmSeedDeath(t, db, fx.GuildRowID, fx.ServerRowID, player, base)
	hmSeedLocation(t, db, fx.GuildRowID, fx.ServerRowID, player, 100, 100, "DEATH", base)

	// A range entirely before the death - must return nothing.
	cells, err := repo.AggregateDeaths(context.Background(), fx.GuildRowID, fx.ServerRowID, base.Add(-2*time.Hour), base.Add(-time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 0 {
		t.Fatalf("expected 0 cells for a range excluding the event, got %+v", cells)
	}
}

// --- cross-tenant isolation (task section 26) -----------------------------------------------

func TestAggregateKillsCrossTenantIsolation(t *testing.T) {
	db, repo, fxA := newHeatmapWorld(t)
	_, _, fxB := newHeatmapWorld(t)
	killer := hmSeedPlayer(t, db, fxA.GuildRowID, "Killer")
	victim := hmSeedPlayer(t, db, fxA.GuildRowID, "Victim")
	base := time.Now().UTC().Truncate(time.Second)

	hmSeedKill(t, db, fxA.GuildRowID, fxA.ServerRowID, killer, victim, base)
	hmSeedLocation(t, db, fxA.GuildRowID, fxA.ServerRowID, killer, 100, 100, "KILL", base)

	cellsB, err := repo.AggregateKills(context.Background(), fxB.GuildRowID, fxB.ServerRowID, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cellsB) != 0 {
		t.Fatalf("installation B must never see installation A's kill data, got %+v", cellsB)
	}

	cellsA, err := repo.AggregateKills(context.Background(), fxA.GuildRowID, fxA.ServerRowID, base.Add(-time.Hour), base.Add(time.Hour), 250, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cellsA) != 1 {
		t.Fatalf("expected installation A to see its own kill, got %+v", cellsA)
	}
}
