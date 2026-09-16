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

// TestTopByKDRuns is the "top by KD" regression test: the auto-refreshing
// leaderboard panel (every 3 hours, internal/discord/leaderboard_scheduler.go)
// silently failed every single refresh - including its very first one on
// startup - because TopByKD's ORDER BY referenced its own correlated-subquery
// aliases directly. With a real "kills" table also in scope via those
// subqueries, Postgres could not resolve a bare "kills" in ORDER BY
// ("column \"kills\" does not exist", confirmed against production logs).
// This proves the fixed query actually executes and ranks correctly.
func TestTopByKDRuns(t *testing.T) {
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
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	guilds := NewGuildRepository(db.Pool)
	players := NewPlayerRepository(db.Pool)
	kills := NewKillRepository(db.Pool)
	deaths := NewDeathRepository(db.Pool)
	stats := NewStatsRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-kd-guild-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}

	newPlayer := func(name string) int64 {
		id, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-%s-%d", name, suffix), name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// sharpshooter: 4 kills, 1 death -> KD 4.00. bruiser: 2 kills, 2 deaths -> KD 1.00.
	sharpshooter := newPlayer(fmt.Sprintf("Sharpshooter-%d", suffix))
	bruiser := newPlayer(fmt.Sprintf("Bruiser-%d", suffix))

	insertKill := func(killer, victim int64, n int) {
		for i := 0; i < n; i++ {
			if err := kills.InsertKill(ctx, KillRecord{GuildID: guildID, KillerPlayerID: killer, VictimPlayerID: victim, Fingerprint: fmt.Sprintf("kd-test-%d-%d-%d", suffix, killer, i), WeaponDisplay: "Test Weapon"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	insertDeath := func(player int64, n int) {
		for i := 0; i < n; i++ {
			if err := deaths.InsertDeath(ctx, DeathRecord{GuildID: guildID, PlayerID: player, Fingerprint: fmt.Sprintf("kd-test-death-%d-%d-%d", suffix, player, i), DeathType: DeathTypeUnknown}); err != nil {
				t.Fatal(err)
			}
		}
	}

	insertKill(sharpshooter, bruiser, 4)
	insertDeath(sharpshooter, 1)
	insertKill(bruiser, sharpshooter, 2)
	insertDeath(bruiser, 2)

	entries, err := stats.TopByKD(ctx, guildID, 10, 1)
	if err != nil {
		t.Fatalf("TopByKD returned an error (this is the regression this test guards against): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 ranked players, got %d: %+v", len(entries), entries)
	}
	if entries[0].Value != "4.00" {
		t.Fatalf("expected the 4.00 KD player ranked first, got %+v", entries[0])
	}
	if entries[1].Value != "1.00" {
		t.Fatalf("expected the 1.00 KD player ranked second, got %+v", entries[1])
	}
}
