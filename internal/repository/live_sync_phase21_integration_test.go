//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/livesync"
)

// Champion Live Sync phase 2.1 against real PostgreSQL.

func TestRecordADMSessionReportsRejectionOfAnOlderBoot(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	p := w.seedPlayer("Ceiyxe")
	w.connect(p, time.Now())
	const bootA = "dayzps/config/DayZServer_PS4_x64_2026-09-24_08-08-14.ADM"
	const bootB = "dayzps/config/DayZServer_PS4_x64_2026-09-24_09-17-09.ADM"
	const old = "dayzps/config/DayZServer_PS4_x64_2026-09-21_08-17-52.ADM"
	startA, startB, startOld := time.Date(2026, 9, 24, 8, 8, 14, 0, time.UTC), time.Date(2026, 9, 24, 9, 17, 9, 0, time.UTC), time.Date(2026, 9, 21, 8, 17, 52, 0, time.UTC)

	if ok, err := w.repo.RecordADMSession(ctx, w.guildID, w.serverID, bootA, &startA); err != nil || !ok {
		t.Fatalf("first session accepted: %v %v", ok, err)
	}
	w.observe(p, bootA, 100, "CONNECT", 1, 1, 1)
	w.observe(p, bootA, 200, "PLAYER_LIST", 4621.1, 319.6, 8397.2)
	if ok, err := w.repo.RecordADMSession(ctx, w.guildID, w.serverID, bootB, &startB); err != nil || !ok {
		t.Fatalf("a newer boot is accepted: %v %v", ok, err)
	}
	// The old session's position is no longer current once the newer boot is accepted.
	if cur, _ := w.repo.CurrentLocation(ctx, w.guildID, w.serverID, p); cur != nil {
		t.Fatalf("old-session location must be UNKNOWN after a new boot: %+v", cur)
	}
	// A days-old ADM (the 2026-09-24 13:19 listing-gap incident) is refused, and reported as refused.
	if ok, err := w.repo.RecordADMSession(ctx, w.guildID, w.serverID, old, &startOld); err != nil || ok {
		t.Fatalf("an older boot must be reported as rejected: %v %v", ok, err)
	}
	s, _ := w.repo.CurrentADMSession(ctx, w.guildID, w.serverID)
	if s == nil || s.ADMFile != bootB || s.EndedAt != nil {
		t.Fatalf("the accepted boot is unchanged: %+v", s)
	}
	// Re-recording the accepted boot (engine restart) is accepted and keeps it current.
	if ok, err := w.repo.RecordADMSession(ctx, w.guildID, w.serverID, bootB, &startB); err != nil || !ok {
		t.Fatalf("re-recording the same boot: %v %v", ok, err)
	}
}

func TestCommandLineCleanupRemovesOnlyThoseRecords(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	repo := NewLiveSyncRepository(w.db.Pool)
	const file = "dayzps/config/DayZServer_PS4_x64_2026-09-24_08-08-14.RPT"
	now := time.Now()
	mk := func(offset int64, category, evidence, parser string) livesync.StoredRecord {
		env := livesync.Record{Offset: offset, Category: category, Status: livesync.StatusParsed, Evidence: evidence}.
			Envelope(livesync.Scope{ServerID: w.serverID}, livesync.FamilyRPT, file, "", now, now)
		env.Parser = parser
		return livesync.StoredRecord{Envelope: env, Delivery: livesync.DeliveryBackfill}
	}
	src := livesync.SourceState{Family: livesync.FamilyRPT, SourceFile: file, RemotePath: file, Checkpoint: 500, AttachedAt: now}
	cli := `== "C:\SERVICES\<service>_local\dayzps\DayZServer_PS4_x64.exe" -ip=<ip> -port=15200 -config=serverDZ_Private.cfg`
	if _, err := repo.CommitSource(ctx, w.guildID, w.serverID, src, []livesync.StoredRecord{
		mk(100, livesync.CategoryLogHeader, cli, "cls-1.1"),                                     // the leak: removed
		mk(200, livesync.CategoryLogHeader, "=====================================", "cls-1.1"), // separator: kept
		mk(300, livesync.CategoryConfigWarning, "Warning Message: -port=1 in text", "cls-1.1"),  // other category: kept
		mk(400, livesync.CategoryLogHeader, "", "cls-1.2"),                                      // new parser: kept
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := repo.CommandLineHeaderRecords(ctx, w.guildID, w.serverID); n != 1 {
		t.Fatalf("diagnostic sees the stored command line: %d", n)
	}
	tag, err := w.db.Pool.Exec(ctx, database.LiveSyncCommandLineCleanupSQL)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() < 1 {
		t.Fatalf("cleanup removed nothing")
	}
	got, _ := repo.RecentRecords(ctx, w.guildID, w.serverID, livesync.FamilyRPT, 10)
	if len(got) != 3 {
		t.Fatalf("only the command-line record is removed: %d left", len(got))
	}
	for _, r := range got {
		if r.SourceOffset == 100 {
			t.Fatal("the command-line record must be gone")
		}
	}
	if n, _ := repo.CommandLineHeaderRecords(ctx, w.guildID, w.serverID); n != 0 {
		t.Fatalf("no command-line evidence remains: %d", n)
	}
}
