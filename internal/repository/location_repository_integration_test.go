//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md) integration tests: the authoritative player
// directory, location-event persistence/ordering/dedupe, latest-location derivation, freshness
// classification, retention cleanup, and cross-tenant isolation - all against a real PostgreSQL.

func newLocationTestDB(t *testing.T) (*database.DB, *LocationRepository) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db, NewLocationRepository(db.Pool)
}

// locationWorld is a small, self-contained fixture: one guild, one server, and a helper to seed
// players/kills/deaths/links/factions/warnings directly via SQL (matching this package's existing
// integration-test convention of raw fixture SQL over a second repository layer just for setup).
type locationWorld struct {
	t        *testing.T
	db       *database.DB
	repo     *LocationRepository
	guildID  int64
	serverID int64
	suffix   int64
}

func newLocationWorld(t *testing.T) *locationWorld {
	t.Helper()
	db, repo := newLocationTestDB(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var guildID int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("loc-guild-%d", suffix)).Scan(&guildID); err != nil {
		t.Fatal(err)
	}
	var serverID int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		guildID, fmt.Sprintf("loc-svc-%d", suffix)).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	return &locationWorld{t: t, db: db, repo: repo, guildID: guildID, serverID: serverID, suffix: suffix}
}

func (w *locationWorld) seedPlayer(name string) int64 {
	w.t.Helper()
	var id int64
	if err := w.db.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		w.guildID, fmt.Sprintf("dayz-%d-%d", w.suffix, time.Now().UnixNano()), name).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *locationWorld) connect(playerID int64, at time.Time) {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(), `
INSERT INTO player_server_activity(guild_id,server_id,player_id,first_seen_at,last_seen_at,currently_connected,current_session_started_at)
VALUES($1,$2,$3,$4,$4,true,$4)
ON CONFLICT(guild_id,server_id,player_id) DO UPDATE SET last_seen_at=$4,currently_connected=true,current_session_started_at=$4`,
		w.guildID, w.serverID, playerID, at); err != nil {
		w.t.Fatal(err)
	}
}

func (w *locationWorld) disconnect(playerID int64, at time.Time) {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(), `UPDATE player_server_activity SET last_seen_at=$3,currently_connected=false WHERE guild_id=$1 AND server_id=$2 AND player_id=$4`,
		w.guildID, w.serverID, at, playerID); err != nil {
		w.t.Fatal(err)
	}
}

// --- player directory (task section 1) -----------------------------------------------------------

func TestListPlayerDirectoryReturnsRealObservedData(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p1 := w.seedPlayer("Alice")
	w.connect(p1, time.Now())

	rows, err := w.repo.ListPlayerDirectory(ctx, w.guildID, w.serverID, PlayerDirectoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.PlayerID == p1 {
			found = true
			if !r.Online {
				t.Fatal("expected the connected player to be reported online")
			}
			if r.Gamertag != "Alice" {
				t.Fatalf("expected gamertag Alice, got %q", r.Gamertag)
			}
			if r.Linked {
				t.Fatal("expected Linked=false for a player with no player_links row")
			}
			if r.DiscordUserID != nil {
				t.Fatal("expected no fabricated discordUserId")
			}
		}
	}
	if !found {
		t.Fatal("expected the seeded player to appear in the directory")
	}
}

func TestPlayerDirectoryOnlineFilterNeverIncludesDisconnected(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	online := w.seedPlayer("Online1")
	offline := w.seedPlayer("Offline1")
	w.connect(online, time.Now())
	w.connect(offline, time.Now().Add(-time.Hour))
	w.disconnect(offline, time.Now().Add(-30*time.Minute))

	onlineOnly := true
	rows, err := w.repo.ListPlayerDirectory(ctx, w.guildID, w.serverID, PlayerDirectoryFilter{Online: &onlineOnly})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.PlayerID == offline {
			t.Fatal("a disconnected player must never appear in an online-only filter")
		}
	}
	sawOnline := false
	for _, r := range rows {
		if r.PlayerID == online {
			sawOnline = true
		}
	}
	if !sawOnline {
		t.Fatal("expected the connected player in the online-only result")
	}
}

func TestPlayerDirectoryCrossTenantIsolation(t *testing.T) {
	w1 := newLocationWorld(t)
	w2 := newLocationWorld(t)
	ctx := context.Background()
	p2 := w2.seedPlayer("OtherGuildPlayer")
	w2.connect(p2, time.Now())

	rows, err := w1.repo.ListPlayerDirectory(ctx, w1.guildID, w1.serverID, PlayerDirectoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.PlayerID == p2 {
			t.Fatal("a player from a different guild must never appear in this guild's directory")
		}
	}
}

// --- location persistence / ordering / dedupe (task sections 3, 14) -------------------------------

func TestInsertLocationEventsAndLatestDerivation(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p1 := w.seedPlayer("Mover")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	y1, y2 := 1.0, 2.0
	events := []LocationEventInput{
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p1, Gamertag: "Mover", X: 100, Z: 200, Y: &y1, EventType: "CONNECT", ObservedAt: base},
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p1, Gamertag: "Mover", X: 150, Z: 250, Y: &y2, EventType: "HIT", ObservedAt: base.Add(10 * time.Second)},
	}
	inserted, err := w.repo.InsertLocationEvents(ctx, events)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 2 {
		t.Fatalf("expected 2 rows inserted, got %d", inserted)
	}

	// Latest location must be query-time derived from the newest observed_at, no second truth.
	latest, err := w.repo.LatestLocation(ctx, w.guildID, w.serverID, p1)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.X != 150 || latest.Z != 250 {
		t.Fatalf("expected the latest event (X=150,Z=250), got %+v", latest)
	}
}

func TestLocationHistoryOrderedNewestFirst(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p1 := w.seedPlayer("Walker")
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	var events []LocationEventInput
	for i := 0; i < 5; i++ {
		events = append(events, LocationEventInput{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p1, Gamertag: "Walker", X: float64(i), Z: 0, EventType: "HIT", ObservedAt: base.Add(time.Duration(i) * time.Minute)})
	}
	if _, err := w.repo.InsertLocationEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	history, err := w.repo.LocationHistory(ctx, w.guildID, w.serverID, p1, LocationHistoryFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 5 {
		t.Fatalf("expected 5 history rows, got %d", len(history))
	}
	for i := 1; i < len(history); i++ {
		if !history[i-1].ObservedAt.After(history[i].ObservedAt) && !history[i-1].ObservedAt.Equal(history[i].ObservedAt) {
			t.Fatalf("expected newest-first ordering, got %v before %v", history[i-1].ObservedAt, history[i].ObservedAt)
		}
	}
	if history[0].X != 4 {
		t.Fatalf("expected the newest event first (X=4), got X=%v", history[0].X)
	}
}

// TestInsertLocationEventsDeduplicatesOnReplay is the task's own "no duplicate location events
// after ADM replay" (section 14) proof: the durable UNIQUE(player_id, server_id, event_type,
// observed_at) constraint - not an in-memory structure - is the actual backstop, exactly mirroring
// how kills/deaths already rely on a DB-level UNIQUE constraint rather than trusting in-memory
// dedupe alone. This also stands in for "restart/recovery": a process restart that re-derives the
// same candidate from the same ADM line (because its own in-memory Deduplicator was lost) must
// never produce a second row.
func TestInsertLocationEventsDeduplicatesOnReplay(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p1 := w.seedPlayer("Replayed")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	event := LocationEventInput{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p1, Gamertag: "Replayed", X: 1, Z: 1, EventType: "KILL", ObservedAt: at}

	first, err := w.repo.InsertLocationEvents(ctx, []LocationEventInput{event})
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("expected the first insert to write 1 row, got %d", first)
	}
	// Simulate an ADM replay: the identical candidate submitted again (e.g. after a restart that
	// lost the in-memory dedupe state).
	second, err := w.repo.InsertLocationEvents(ctx, []LocationEventInput{event})
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Fatalf("expected the duplicate insert to write 0 rows (ON CONFLICT DO NOTHING), got %d", second)
	}
	history, err := w.repo.LocationHistory(ctx, w.guildID, w.serverID, p1, LocationHistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("expected exactly 1 durable row after a replayed duplicate, got %d", len(history))
	}
}

// --- freshness classification (task section 5) -----------------------------------------------

func TestClassifyFreshnessThresholds(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, LocationFreshnessLiveRecent},
		{30 * time.Second, LocationFreshnessLiveRecent},
		{60 * time.Second, LocationFreshnessLiveRecent},
		{61 * time.Second, LocationFreshnessRecent},
		{4 * time.Minute, LocationFreshnessRecent},
		{5 * time.Minute, LocationFreshnessRecent},
		{5*time.Minute + time.Second, LocationFreshnessStale},
		{time.Hour, LocationFreshnessStale},
	}
	for _, c := range cases {
		if got := ClassifyFreshness(c.age); got != c.want {
			t.Errorf("ClassifyFreshness(%s) = %s, want %s", c.age, got, c.want)
		}
	}
}

// --- retention cleanup (task section 9) -----------------------------------------------------------

func TestDeleteOlderThanRemovesOnlyStaleRowsNeverKillsOrDeaths(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p1 := w.seedPlayer("Retained")
	old := time.Now().Add(-60 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)

	// Insert one old and one recent event, each with a distinct observed_at so the UNIQUE
	// constraint doesn't collapse them.
	if _, err := w.repo.InsertLocationEvents(ctx, []LocationEventInput{
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p1, Gamertag: "Retained", X: 1, Z: 1, EventType: "HIT", ObservedAt: old},
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p1, Gamertag: "Retained", X: 2, Z: 2, EventType: "HIT", ObservedAt: recent},
	}); err != nil {
		t.Fatal(err)
	}
	// created_at is server-assigned NOW() on insert, so both rows currently look "recent" by
	// created_at - directly backdate the old row's created_at to simulate it actually having
	// aged past the retention window (created_at, not observed_at, is what retention sweeps on).
	if _, err := w.db.Pool.Exec(ctx, `UPDATE player_location_events SET created_at=$1 WHERE guild_id=$2 AND observed_at=$3`, old, w.guildID, old); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	deleted, err := w.repo.DeleteOlderThan(ctx, cutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expected exactly 1 stale row deleted, got %d", deleted)
	}
	history, err := w.repo.LocationHistory(ctx, w.guildID, w.serverID, p1, LocationHistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].X != 2 {
		t.Fatalf("expected only the recent row (X=2) to survive retention, got %+v", history)
	}
}
