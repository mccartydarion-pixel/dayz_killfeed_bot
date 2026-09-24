//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/livesync"
)

// Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md) against real PostgreSQL: transactional
// source checkpoints, idempotent records, session end on evidence, monotonic session selection,
// source-time conversion, the kill/death heatmap source join and record retention.

func lsRecord(serverID int64, file string, offset int64, category, status, delivery string, detected time.Time) livesync.StoredRecord {
	local := time.Date(2026, 9, 24, 5, 21, 24, 0, time.UTC)
	env := livesync.Record{Offset: offset, Category: category, Status: status, SourceLocalTime: &local,
		Payload: map[string]string{"k": "v"}, Evidence: "5:21:24.103 --- Termination successfully completed ---"}.
		Envelope(livesync.Scope{ServerID: serverID}, livesync.FamilyRPT, file, "2026-09-24T04:15:07", detected, detected)
	return livesync.StoredRecord{Envelope: env, Delivery: delivery}
}

func TestLiveSyncCommitIsTransactionalAndIdempotent(t *testing.T) {
	w := newLocationWorld(t)
	repo := NewLiveSyncRepository(w.db.Pool)
	ctx := context.Background()
	const file = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT"
	now := time.Now().UTC()
	src := livesync.SourceState{Family: livesync.FamilyRPT, SourceFile: file, RemotePath: "/games/x/noftp/" + file, Checkpoint: 120, ReadSize: 130, Active: true, AttachedAt: now, LastReadAt: &now}
	recs := []livesync.StoredRecord{
		lsRecord(w.serverID, file, 60, livesync.CategoryBootStarted, livesync.StatusParsed, livesync.DeliveryBackfill, now),
		lsRecord(w.serverID, file, 120, livesync.CategoryUnknown, livesync.StatusUnknown, livesync.DeliveryLive, now),
	}
	if n, err := repo.CommitSource(ctx, w.guildID, w.serverID, src, recs); err != nil || n != 2 {
		t.Fatalf("first commit: %d %v", n, err)
	}
	// Replaying the same bytes is a no-op.
	if n, err := repo.CommitSource(ctx, w.guildID, w.serverID, src, recs); err != nil || n != 0 {
		t.Fatalf("replay: %d %v", n, err)
	}
	// Atomicity: a batch with an invalid record commits nothing and leaves the checkpoint where it was.
	bad := src
	bad.Checkpoint = 500
	badRecs := []livesync.StoredRecord{
		lsRecord(w.serverID, file, 300, livesync.CategoryShutdownComplete, livesync.StatusParsed, livesync.DeliveryLive, now),
		lsRecord(w.serverID, file, 500, livesync.CategoryUnknown, "BOGUS", livesync.DeliveryLive, now),
	}
	if _, err := repo.CommitSource(ctx, w.guildID, w.serverID, bad, badRecs); err == nil {
		t.Fatal("an invalid record must fail the whole commit")
	}
	srcs, err := repo.LoadSources(ctx, w.guildID, w.serverID)
	if err != nil || len(srcs) != 1 || srcs[0].Checkpoint != 120 || srcs[0].Records != 2 || !srcs[0].Active {
		t.Fatalf("checkpoint must not pass a failed commit: %+v %v", srcs, err)
	}
	got, err := repo.RecentRecords(ctx, w.guildID, w.serverID, livesync.FamilyRPT, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("records: %d %v", len(got), err)
	}
	for _, r := range got {
		if r.SourceFile != file || r.SourceOffset <= 0 || r.Evidence == "" || r.Payload["k"] != "v" {
			t.Fatalf("physical identity and sanitized evidence kept: %+v", r)
		}
		if r.PersistedAt.Before(r.DetectedAt.Add(-time.Second)) {
			t.Fatalf("persisted_at is the commit time: %+v", r)
		}
	}
	// Another tenant sees nothing.
	other := newLocationWorld(t)
	if o, _ := repo.RecentRecords(ctx, other.guildID, other.serverID, "", 10); len(o) != 0 {
		t.Fatalf("cross-tenant: %+v", o)
	}
	stats, err := repo.FamilyStats(ctx, w.guildID, w.serverID, now.Add(-time.Hour))
	if err != nil || len(stats) != 1 || stats[0].Records != 2 || stats[0].Live != 1 || stats[0].Backfill != 1 || stats[0].Unknown != 1 {
		t.Fatalf("family stats: %+v %v", stats, err)
	}
}

func TestSessionEndsOnEvidenceAndSelectionOnlyMovesForward(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p := w.seedPlayer("Ceiyxe")
	w.connect(p, time.Now())
	const bootA = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	const bootB = "dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM"
	startA := time.Date(2026, 9, 24, 4, 15, 7, 0, time.UTC)
	startB := time.Date(2026, 9, 24, 5, 23, 5, 0, time.UTC)
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, bootA, &startA); err != nil {
		t.Fatal(err)
	}
	w.observe(p, bootA, 100, "CONNECT", 1, 1, 1)
	w.observe(p, bootA, 200, "PLAYER_LIST", 4621.1, 319.6, 8397.2)
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur == nil {
		t.Fatal("open session: current")
	}
	// Evidence dated before the session started ends nothing (old history read late).
	if ended, err := w.repo.EndADMSessionBefore(ctx, w.guildID, w.serverID, startA.Add(-time.Hour), "restart_log_pre_start_check", "restart.log@1"); err != nil || ended {
		t.Fatalf("older evidence: %v %v", ended, err)
	}
	// The RPT's completed shutdown ends boot A: its positions stop being CURRENT at once.
	if ended, err := w.repo.EndADMSessionBefore(ctx, w.guildID, w.serverID, time.Date(2026, 9, 24, 5, 21, 24, 0, time.UTC), "rpt_shutdown_complete", "rpt@9"); err != nil || !ended {
		t.Fatalf("shutdown evidence: %v %v", ended, err)
	}
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur != nil {
		t.Fatalf("an ended session is never current: %+v", cur)
	}
	// The engine re-noting the same file (e.g. after a Champion restart) keeps the end.
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, bootA, &startA); err != nil {
		t.Fatal(err)
	}
	s, err := w.repo.CurrentADMSession(ctx, w.guildID, w.serverID)
	if err != nil || s == nil || s.EndedAt == nil || s.EndedReason != "rpt_shutdown_complete" {
		t.Fatalf("re-recording must keep the proven end: %+v %v", s, err)
	}
	// Boot B opens a new session; an older file from a lagging mount never replaces it.
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, bootB, &startB); err != nil {
		t.Fatal(err)
	}
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, bootA, &startA); err != nil {
		t.Fatal(err)
	}
	s, _ = w.repo.CurrentADMSession(ctx, w.guildID, w.serverID)
	if s == nil || s.ADMFile != bootB || s.EndedAt != nil {
		t.Fatalf("selection only moves forward: %+v", s)
	}
	w.observe(p, bootB, 50, "CONNECT", 2, 2, 2)
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur == nil || cur.SourceFile != bootB {
		t.Fatalf("boot B current: %+v", cur)
	}
}

func TestLocationSourceUTCUsesRestartLogOffset(t *testing.T) {
	w := newLocationWorld(t)
	ls := NewLiveSyncRepository(w.db.Pool)
	ctx := context.Background()
	p := w.seedPlayer("Clock")
	const boot = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	w.observe(p, boot, 100, "PLAYER_LIST", 1, 1, 1) // source_local_time = 04:00:00 + 100s
	loc, _ := w.repo.LatestLocation(ctx, w.guildID, w.serverID, p)
	if loc == nil || loc.SourceUTC != nil {
		t.Fatalf("no offset known yet: no source UTC, never guessed: %+v", loc)
	}
	if err := ls.SetServerUTCOffset(ctx, w.guildID, w.serverID, -240, "restart.log@42"); err != nil {
		t.Fatal(err)
	}
	loc, _ = w.repo.LatestLocation(ctx, w.guildID, w.serverID, p)
	want := time.Date(2026, 9, 24, 8, 1, 40, 0, time.UTC)
	if loc == nil || loc.SourceUTC == nil || !loc.SourceUTC.Equal(want) {
		t.Fatalf("source UTC = local - offset: %+v", loc)
	}
	lat, err := ls.ADMLatency(ctx, w.guildID, w.serverID, time.Now().Add(-time.Hour))
	if err != nil || lat.Samples != 1 || lat.EventToDetectP50Sec == nil {
		t.Fatalf("ADM latency measured from stored rows: %+v %v", lat, err)
	}
}

func TestHeatmapJoinsKillsAndDeathsBySourceIdentity(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	kills, deaths, heat := NewKillRepository(w.db.Pool), NewDeathRepository(w.db.Pool), NewHeatmapRepository(w.db.Pool)
	killer, victim, other := w.seedPlayer("Killer"), w.seedPlayer("Victim"), w.seedPlayer("Suicide")
	const file = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	local := time.Date(2026, 9, 24, 4, 30, 0, 0, time.UTC)
	// The engine's real shape: event_time NULL (ADM lines carry no date), location rows written from
	// the same line at the same end offset, observed at ingestion time.
	y := 300.0
	if _, err := w.repo.InsertLocationEvents(ctx, []LocationEventInput{
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: killer, Gamertag: "Killer", X: 2625, Z: 4625, Y: &y, EventType: "KILL", ObservedAt: time.Now(), SourceFile: file, SourceOffset: 900, SourceLocalTime: &local},
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: victim, Gamertag: "Victim", X: 2630, Z: 4630, Y: &y, EventType: "KILL", ObservedAt: time.Now(), SourceFile: file, SourceOffset: 900, SourceLocalTime: &local},
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: other, Gamertag: "Suicide", X: 100, Z: 100, Y: &y, EventType: "DEATH", ObservedAt: time.Now(), SourceFile: file, SourceOffset: 1200, SourceLocalTime: &local},
	}); err != nil {
		t.Fatal(err)
	}
	if err := kills.InsertKill(ctx, KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("k-%d", time.Now().UnixNano()),
		KillerPlayerID: killer, VictimPlayerID: victim, SourceFile: file, SourceOffset: 900, SourceLocalTime: &local}); err != nil {
		t.Fatal(err)
	}
	// A kill whose line carried no position (no location row at its offset) is excluded.
	if err := kills.InsertKill(ctx, KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("k2-%d", time.Now().UnixNano()),
		KillerPlayerID: killer, VictimPlayerID: victim, SourceFile: file, SourceOffset: 950}); err != nil {
		t.Fatal(err)
	}
	if err := deaths.InsertDeath(ctx, DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("d-%d", time.Now().UnixNano()),
		PlayerID: other, DeathType: DeathTypeUnknown, SourceFile: file, SourceOffset: 1200, SourceLocalTime: &local}); err != nil {
		t.Fatal(err)
	}
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	kc, err := heat.AggregateKills(ctx, w.guildID, w.serverID, from, to, 250, 100)
	if err != nil || len(kc) != 1 || kc[0].CellX != 10 || kc[0].CellZ != 18 || kc[0].Count != 1 {
		t.Fatalf("kill heatmap joins by source identity (killer's position): %+v %v", kc, err)
	}
	dc, err := heat.AggregateDeaths(ctx, w.guildID, w.serverID, from, to, 250, 100)
	if err != nil || len(dc) != 1 || dc[0].Count != 1 {
		t.Fatalf("death heatmap joins by source identity: %+v %v", dc, err)
	}
	// Other tenants never match.
	o := newLocationWorld(t)
	if c, _ := heat.AggregateKills(ctx, o.guildID, o.serverID, from, to, 250, 100); len(c) != 0 {
		t.Fatalf("cross-tenant: %+v", c)
	}
	// Schema: the kill row kept its source identity.
	var sf string
	var so int64
	if err := w.db.Pool.QueryRow(ctx, `SELECT source_file, source_offset FROM kills WHERE guild_id=$1 AND source_offset=900`, w.guildID).Scan(&sf, &so); err != nil || sf != file {
		t.Fatalf("kill source: %q %d %v", sf, so, err)
	}
}

func TestLiveSyncRetentionKeepsSignalLongerThanNoise(t *testing.T) {
	w := newLocationWorld(t)
	repo := NewLiveSyncRepository(w.db.Pool)
	ctx := context.Background()
	const file = "dayzps/config/DayZServer_PS4_x64_2026-09-20_04-15-07.RPT"
	old := time.Now().Add(-5 * 24 * time.Hour)
	src := livesync.SourceState{Family: livesync.FamilyRPT, SourceFile: file, RemotePath: file, Checkpoint: 20, AttachedAt: old}
	if _, err := repo.CommitSource(ctx, w.guildID, w.serverID, src, []livesync.StoredRecord{
		lsRecord(w.serverID, file, 10, livesync.CategoryModelWarning, livesync.StatusParsed, livesync.DeliveryLive, old),
		lsRecord(w.serverID, file, 20, livesync.CategoryShutdownComplete, livesync.StatusParsed, livesync.DeliveryLive, old),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := repo.PruneRecords(ctx, now.Add(-3*24*time.Hour), now.Add(-30*24*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.RecentRecords(ctx, w.guildID, w.serverID, "", 10)
	if len(got) != 1 || got[0].Category != livesync.CategoryShutdownComplete {
		t.Fatalf("noise pruned after 3 days, signal kept: %+v", got)
	}
}
