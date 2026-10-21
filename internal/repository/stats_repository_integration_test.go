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
	// A kill is the victim's death. sharpshooter: 4 kills, 1 other death + killed twice = 3 deaths
	// -> KD 1.33. bruiser: 2 kills, 2 other deaths + killed four times = 6 deaths -> KD 0.33.
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
	if entries[0].Value != "1.33" {
		t.Fatalf("expected the 1.33 KD player ranked first, got %+v", entries[0])
	}
	if entries[1].Value != "0.33" {
		t.Fatalf("expected the 0.33 KD player ranked second, got %+v", entries[1])
	}
}

// TestAutoLeaderboardV3Queries proves the Auto Leaderboard's four ALL-TIME
// boards execute against real Postgres, span seasons (no season filter),
// rank highest-first with the documented tie breakers, honour the limit, and
// that the streak board reads the RECORD streak (it survives a streak reset).
func TestAutoLeaderboardV3Queries(t *testing.T) {
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
	streaks := NewStreakRepository(db.Pool)
	seasons := NewSeasonRepository(db.Pool)
	stats := NewStatsRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-v3-guild-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	s1, err := seasons.Start(ctx, guildID, "Season 1", time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := seasons.End(ctx, guildID, s1.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s2, err := seasons.Start(ctx, guildID, "Season 2", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	newPlayer := func(name string) int64 {
		id, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-v3-%s-%d", name, suffix), name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alpha, bravo, charlie, delta := newPlayer("Alpha"), newPlayer("Bravo"), newPlayer("Charlie"), newPlayer("Delta")

	n := 0
	kill := func(killer, victim int64, season int64, dist float64) {
		n++
		d := dist
		sid := season
		if err := kills.InsertKill(ctx, KillRecord{GuildID: guildID, KillerPlayerID: killer, VictimPlayerID: victim, SeasonID: &sid, Distance: &d,
			Fingerprint: fmt.Sprintf("v3-kill-%d-%d", suffix, n), WeaponDisplay: "Test Weapon"}); err != nil {
			t.Fatal(err)
		}
	}
	death := func(player int64, season int64) {
		n++
		sid := season
		if err := deaths.InsertDeath(ctx, DeathRecord{GuildID: guildID, PlayerID: player, SeasonID: &sid,
			Fingerprint: fmt.Sprintf("v3-death-%d-%d", suffix, n), DeathType: DeathTypeUnknown}); err != nil {
			t.Fatal(err)
		}
	}

	// Kills: alpha 3 (split across seasons), bravo 3, charlie 1 -> tie broken by name.
	kill(alpha, delta, s1.ID, 50)
	kill(alpha, delta, s1.ID, 1104.25)
	kill(alpha, delta, s2.ID, 10)
	kill(bravo, delta, s2.ID, 300)
	kill(bravo, delta, s2.ID, 20)
	kill(bravo, delta, s1.ID, 30)
	kill(charlie, delta, s2.ID, 300)
	// Deaths: delta 4 non-PvP (across seasons) plus the seven kills above = 11, charlie 1, bravo 1.
	death(delta, s1.ID)
	death(delta, s1.ID)
	death(delta, s2.ID)
	death(delta, s2.ID)
	death(charlie, s2.ID)
	death(bravo, s1.ID)
	// Streaks: charlie reaches 5 then resets (record stays 5); alpha 5 with more
	// kills (wins the tie); bravo 2.
	for i := 0; i < 5; i++ {
		if _, err := streaks.Increment(ctx, guildID, charlie); err != nil {
			t.Fatal(err)
		}
		if _, err := streaks.Increment(ctx, guildID, alpha); err != nil {
			t.Fatal(err)
		}
	}
	if err := streaks.Reset(ctx, guildID, charlie); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := streaks.Increment(ctx, guildID, bravo); err != nil {
			t.Fatal(err)
		}
	}

	type row struct{ name, value string }
	check := func(label string, got []LeaderboardEntry, err error, want []row) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %+v, want %+v", label, got, want)
		}
		for i := range want {
			if got[i].DisplayName != want[i].name || got[i].Value != want[i].value {
				t.Fatalf("%s row %d: got %+v, want %+v", label, i, got[i], want[i])
			}
		}
	}

	k, err := stats.TopByKills(ctx, guildID, 15)
	check("kills", k, err, []row{{"Alpha", "3"}, {"Bravo", "3"}, {"Charlie", "1"}})
	d, err := stats.TopByDeaths(ctx, guildID, 15)
	check("deaths", d, err, []row{{"Delta", "11"}, {"Bravo", "1"}, {"Charlie", "1"}})
	st, err := stats.TopByBestStreak(ctx, guildID, 15)
	check("streaks", st, err, []row{{"Alpha", "5"}, {"Charlie", "5"}, {"Bravo", "2"}})
	l, err := stats.TopLongestKill(ctx, guildID, 15)
	check("longest", l, err, []row{{"Alpha", "1104.2m"}, {"Bravo", "300.0m"}, {"Charlie", "300.0m"}})

	top2, err := stats.TopByDeaths(ctx, guildID, 2)
	check("deaths limit", top2, err, []row{{"Delta", "11"}, {"Bravo", "1"}})
}
