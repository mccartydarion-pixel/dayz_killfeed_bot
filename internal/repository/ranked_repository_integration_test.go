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
	if _, err := db.Pool.Exec(ctx, `UPDATE ranked_seasons SET status='ARCHIVED' WHERE id=$1`, season); err != nil {
		t.Fatal(err)
	}
	second := newSeason(start.Add(10 * time.Minute))
	check(second, newKill(6, start.Add(11*time.Minute)), "AWARDED", 100)
	if _, err := repo.RecordServerKill(ctx, season, newKill(5, firstAt.Add(10*time.Minute))); !errors.Is(err, ErrRankedIneligible) {
		t.Fatalf("archived season accepted kill: %v", err)
	}
}
