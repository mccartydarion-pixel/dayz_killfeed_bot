//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

func TestRankedServerLedgerReplayCooldownAndSeasonReset(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for a non-production database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	guild, err := NewGuildRepository(db.Pool).UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServerRepository(db.Pool).UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "fixture", ProviderServiceID: fmt.Sprintf("ranked-%d", suffix), Game: "dayz", Platform: "PLAYSTATION", Status: "ACTIVE", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	players := NewPlayerRepository(db.Pool)
	attacker, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("attacker-%d", suffix), "Attacker", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	victim, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("victim-%d", suffix), "Victim", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	newSeason := func(at time.Time) int64 {
		var id int64
		err := db.Pool.QueryRow(ctx, `INSERT INTO ranked_seasons(scope,platform,server_id,status,rp_per_kill,thresholds,starts_at)
VALUES('SERVER','PLAYSTATION',$1,'ACTIVE',100,ARRAY[100,300,600,1000,1500,2100,2800]::bigint[],$2) RETURNING id`, server.ID, at).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	season := newSeason(start)
	kills := NewKillRepository(db.Pool)
	newKill := func(n int, at time.Time) int64 {
		id, err := kills.InsertKillReturning(ctx, KillRecord{GuildID: guild, ServerID: server.ID, SessionID: "ranked-test", Fingerprint: fmt.Sprintf("ranked-%d-%d", suffix, n), KillerPlayerID: attacker, VictimPlayerID: victim, EventTime: &at})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	repo := NewRankedRepository(db.Pool)
	firstAt := start.Add(time.Minute)
	first := newKill(1, firstAt)
	check := func(seasonID, killID int64, outcome string, amount int64) {
		t.Helper()
		got, err := repo.RecordServerKill(ctx, seasonID, killID)
		if err != nil || got.Outcome != outcome || got.Amount != amount {
			t.Fatalf("award(%d,%d) = %+v, %v; want %s %d", seasonID, killID, got, err, outcome, amount)
		}
	}
	check(season, first, "AWARDED", 100)
	check(season, first, "AWARDED", 100) // replay does not duplicate RP
	check(season, newKill(2, firstAt.Add(4*time.Minute)), "COOLDOWN", 0)
	check(season, newKill(3, firstAt.Add(5*time.Minute)), "AWARDED", 100)
	check(season, newKill(4, firstAt.Add(2*time.Minute)), "OUT_OF_ORDER", 0)
	var total int64
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM ranked_awards WHERE season_id=$1`, season).Scan(&total); err != nil || total != 200 {
		t.Fatalf("season total=%d err=%v, want 200", total, err)
	}
	standings, err := repo.ServerStandings(ctx, server.ID, 15)
	if err != nil || len(standings) != 1 || standings[0].PlayerID != attacker || standings[0].RP != 200 || standings[0].Tier != "ROOKIE" || standings[0].Remaining != 100 {
		t.Fatalf("server standings=%+v err=%v", standings, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE ranked_seasons SET status='ARCHIVED' WHERE id=$1`, season); err != nil {
		t.Fatal(err)
	}
	check(season, first, "AWARDED", 100) // an archived season remains replayable
	second := newSeason(start.Add(10 * time.Minute))
	check(second, newKill(6, start.Add(11*time.Minute)), "AWARDED", 100)
	standings, err = repo.ServerStandings(ctx, server.ID, 15)
	if err != nil || len(standings) != 1 || standings[0].RP != 100 {
		t.Fatalf("reset standings=%+v err=%v", standings, err)
	}
	if _, err := repo.RecordServerKill(ctx, season, newKill(5, firstAt.Add(time.Minute))); !errors.Is(err, ErrRankedIneligible) {
		t.Fatalf("archived season accepted kill: %v", err)
	}
	instant := newKill(7, start.Add(12*time.Minute))
	decision, err := repo.AwardActiveServerKill(ctx, server.ID, instant)
	if err != nil || decision.Outcome != "COOLDOWN" {
		t.Fatalf("live award cooldown = %+v, %v", decision, err)
	}
	missing := newKill(8, start.Add(17*time.Minute))
	count, err := repo.ReconcileServerAwards(ctx, server.ID)
	if err != nil || count != 1 {
		t.Fatalf("reconcile count=%d err=%v", count, err)
	}
	decision, err = repo.RecordServerKill(ctx, second, missing)
	if err != nil || decision.Outcome != "AWARDED" || decision.Amount != 100 {
		t.Fatalf("reconciled decision = %+v, %v", decision, err)
	}
	count, err = repo.ReconcileServerAwards(ctx, server.ID)
	if err != nil || count != 0 {
		t.Fatalf("replay reconciliation count=%d err=%v", count, err)
	}

	// A real ADM kill has no event_time. Its source_local_time is the
	// wall clock carried by the ADM filename and line, not a UTC instant.
	sourceUTC := start.Add(23 * time.Minute)
	sourceLocal := sourceUTC.Add(-4 * time.Hour)
	admKill, err := kills.InsertKillReturning(ctx, KillRecord{
		GuildID: guild, ServerID: server.ID, SessionID: "ranked-adm",
		Fingerprint: fmt.Sprintf("ranked-adm-%d", suffix),
		KillerPlayerID: attacker, VictimPlayerID: victim,
		SourceFile: "DayZServer_PS4_x64_2026-09-29_00-00-00.ADM",
		SourceOffset: 200, SourceLocalTime: &sourceLocal,
	})
	if err != nil { t.Fatal(err) }
	if _, err = repo.AwardActiveServerKill(ctx, server.ID, admKill); !errors.Is(err, ErrRankedIneligible) {
		t.Fatalf("ADM kill with unknown UTC offset should wait: %v", err)
	}
	if err = NewLiveSyncRepository(db.Pool).SetServerUTCOffset(ctx, guild, server.ID, -240, "ranked-integration-fixture"); err != nil {
		t.Fatal(err)
	}
	count, err = repo.ReconcileServerAwards(ctx, server.ID)
	if err != nil || count != 1 { t.Fatalf("ADM reconciliation count=%d err=%v", count, err) }
	admAward, err := repo.AwardActiveServerKill(ctx, server.ID, admKill)
	if err != nil || admAward.Outcome != "AWARDED" || admAward.Amount != 100 {
		t.Fatalf("ADM award=%+v err=%v", admAward, err)
	}
	count, err = repo.ReconcileServerAwards(ctx, server.ID)
	if err != nil || count != 0 { t.Fatalf("ADM replay count=%d err=%v", count, err) }
}
