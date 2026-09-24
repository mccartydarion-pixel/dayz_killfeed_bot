//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

// Champion Live Sync phase 1 (docs/CHAMPION_LIVE_SYNC.md): sourced location observations,
// replay-exact dedupe, PLAYER_LIST rows and session-scoped current location - real PostgreSQL.

func (w *locationWorld) observe(playerID int64, file string, offset int64, eventType string, x, alt, z float64) int {
	w.t.Helper()
	y := alt
	local := time.Date(2026, 9, 24, 4, 0, 0, 0, time.UTC).Add(time.Duration(offset) * time.Second)
	n, err := w.repo.InsertLocationEvents(context.Background(), []LocationEventInput{{
		GuildID: w.guildID, ServerID: w.serverID, PlayerID: playerID, Gamertag: "Ceiyxe", X: x, Z: z, Y: &y,
		EventType: eventType, ObservedAt: time.Now().UTC(), SourceFile: file, SourceOffset: offset, SourceLocalTime: &local, SnapshotRef: file + "@snap",
	}})
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestCurrentLocationIsSessionScoped(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p := w.seedPlayer("Ceiyxe")
	const bootA = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	const bootB = "dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM"
	w.connect(p, time.Now())

	// No session recorded yet: nothing is current.
	if loc, err := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); err != nil || loc != nil {
		t.Fatalf("no session -> UNKNOWN: %+v %v", loc, err)
	}
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, bootA, nil); err != nil {
		t.Fatal(err)
	}
	w.observe(p, bootA, 100, "CONNECT", 4344.2, 317.4, 8533.8)
	// Four identical five-minute observations: all preserved (distinct source offsets).
	for _, off := range []int64{200, 300, 400, 500} {
		if n := w.observe(p, bootA, off, "PLAYER_LIST", 4621.1, 319.6, 8397.2); n != 1 {
			t.Fatalf("observation at %d: %d", off, n)
		}
	}
	// A replay of the same bytes is an exact no-op.
	if n := w.observe(p, bootA, 500, "PLAYER_LIST", 4621.1, 319.6, 8397.2); n != 0 {
		t.Fatalf("replay must not duplicate: %d", n)
	}
	hist, err := w.repo.LocationHistory(ctx, w.guildID, w.serverID, p, LocationHistoryFilter{EventType: "PLAYER_LIST"})
	if err != nil || len(hist) != 4 {
		t.Fatalf("four stationary observations: %d %v", len(hist), err)
	}
	cur, err := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p)
	if err != nil || cur == nil || !cur.CurrentSession || cur.X != 4621.1 || *cur.Y != 319.6 || cur.Z != 8397.2 || cur.SourceFile != bootA || cur.SourceLocalTime == nil {
		t.Fatalf("current: %+v %v", cur, err)
	}

	// A new server boot: no fresh position yet -> UNKNOWN, while the old one stays history.
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, bootB, nil); err != nil {
		t.Fatal(err)
	}
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur != nil {
		t.Fatalf("an old-session position is never current: %+v", cur)
	}
	if last, _ := w.repo.LatestLocation(ctx, w.guildID, w.serverID, p); last == nil || last.SourceFile != bootA || last.CurrentSession {
		t.Fatalf("last known location stays available as history: %+v", last)
	}

	// The player reconnects in boot B; a later player-list entry is current.
	w.observe(p, bootB, 50, "CONNECT", 1000, 300, 2000)
	w.observe(p, bootB, 150, "PLAYER_LIST", 1010, 301, 2010)
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur == nil || cur.X != 1010 {
		t.Fatalf("current in boot B: %+v", cur)
	}
	// A reconnect inside the same boot starts a new player session: only positions at or after it count.
	w.observe(p, bootB, 400, "CONNECT", 3000, 250, 4000)
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur == nil || cur.X != 3000 || cur.EventType != "CONNECT" {
		t.Fatalf("the reconnect position is the current one: %+v", cur)
	}
	// Disconnected: UNKNOWN again, whatever the file holds.
	w.disconnect(p, time.Now())
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur != nil {
		t.Fatalf("a disconnected player has no current location: %+v", cur)
	}

	// Directory: currentLocation is session-scoped, lastKnownLocation is history.
	w.connect(p, time.Now())
	rows, err := w.repo.ListPlayerDirectory(ctx, w.guildID, w.serverID, PlayerDirectoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.PlayerID == p && (r.CurrentLocation == nil || r.LastKnownLocation == nil || !r.CurrentLocation.CurrentSession) {
			t.Fatalf("directory entry: %+v", r)
		}
	}
}

func TestCurrentLocationIgnoresLegacyAndOtherTenants(t *testing.T) {
	w := newLocationWorld(t)
	other := newLocationWorld(t)
	ctx := context.Background()
	p := w.seedPlayer("Legacy")
	w.connect(p, time.Now())
	const boot = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, boot, nil); err != nil {
		t.Fatal(err)
	}
	// A legacy (unsourced) row is history only.
	y := 10.0
	if _, err := w.repo.InsertLocationEvents(ctx, []LocationEventInput{{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p, Gamertag: "Legacy", X: 1, Z: 1, Y: &y, EventType: "KILL", ObservedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur != nil {
		t.Fatalf("an unsourced row is never current: %+v", cur)
	}
	// Another tenant's session with the same file name changes nothing here.
	if err := other.repo.SetCurrentADMSession(ctx, other.guildID, other.serverID, boot, nil); err != nil {
		t.Fatal(err)
	}
	if cur, _ := other.repo.CurrentLocation(ctx, other.guildID, other.serverID, p); cur != nil {
		t.Fatalf("cross-tenant: %+v", cur)
	}
	// Schema guards: an offset without a file, or an unknown event type, is refused.
	if _, err := w.db.Pool.Exec(ctx, `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at,source_offset) VALUES($1,$2,$3,'x',1,1,'PLAYER_LIST',NOW(),5)`, w.guildID, w.serverID, p); err == nil {
		t.Fatal("source_offset without source_file must be refused")
	}
	if _, err := w.db.Pool.Exec(ctx, `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at) VALUES($1,$2,$3,'x',1,1,'TELEPORT',NOW())`, w.guildID, w.serverID, p); err == nil {
		t.Fatal("an unknown event type must be refused")
	}
}
