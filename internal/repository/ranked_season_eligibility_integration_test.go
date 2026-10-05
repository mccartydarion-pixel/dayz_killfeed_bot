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
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// Server eligibility for a Ranked season is active + owning guild + console
// platform. game_servers.status is a display label and never gates it.
func TestStartServerSeasonEligibility(t *testing.T) {
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
	guilds, servers, repo := NewGuildRepository(db.Pool), NewServerRepository(db.Pool), NewRankedRepository(db.Pool)
	guild, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-elig-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-elig-other-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(name, status, platform string, active bool) int64 {
		s, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("%s-%d", name, suffix), Game: "DayZ", Platform: platform, Status: status, Active: active})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	rules := ranked.Thresholds{3000, 6000, 9000, 12000, 15000, 18000, 25000}
	now := time.Now().UTC()

	for _, status := range []string{"CONNECTED", "ONLINE", "OFFLINE"} {
		id := newServer("ok-"+status, status, "PLAYSTATION", true)
		season, err := repo.StartServerSeason(ctx, guild, id, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now)
		if err != nil || season.RPPerKill != 100 || season.Thresholds != rules || season.Platform != "PLAYSTATION" {
			t.Fatalf("status %s: start must succeed with the owner's rules, got %+v %v", status, season, err)
		}
	}
	xbox := newServer("xbox", "ONLINE", "XBOX", true)
	if _, err := repo.StartServerSeason(ctx, guild, xbox, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now); err != nil {
		t.Fatalf("xbox: %v", err)
	}

	inactive := newServer("inactive", "DISCONNECTED", "PLAYSTATION", false)
	pc := newServer("pc", "ONLINE", "PC", true)
	for name, id := range map[string]int64{"inactive": inactive, "pc": pc} {
		if _, err := repo.StartServerSeason(ctx, guild, id, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now); !errors.Is(err, ErrRankedServerIneligible) {
			t.Fatalf("%s: expected ErrRankedServerIneligible, got %v", name, err)
		}
	}
	// Tenant scope: a real, active server is never startable from another guild.
	crossGuild := newServer("tenant", "CONNECTED", "PLAYSTATION", true)
	if _, err := repo.StartServerSeason(ctx, other, crossGuild, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now); !errors.Is(err, ErrRankedServerIneligible) {
		t.Fatalf("cross-guild start must be rejected, got %v", err)
	}
	// Reset with nothing active is its own, explicit error.
	fresh := newServer("fresh", "CONNECTED", "PLAYSTATION", true)
	if _, err := repo.StartServerSeason(ctx, guild, fresh, 100, rules, ranked.DefaultSameVictimCooldownMinutes, true, now); !errors.Is(err, ErrRankedNoActiveSeason) {
		t.Fatalf("reset without a season: expected ErrRankedNoActiveSeason, got %v", err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM ranked_seasons WHERE server_id = ANY($1)`, []int64{inactive, pc, crossGuild, fresh}).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected starts must create no season, got %d %v", n, err)
	}
}

// The same-victim wait is the awarding season's own frozen value: 0 awards every kill, 30 holds
// until exactly thirty minutes, and a reset to another value does not reach back into the archive.
func TestRankedSameVictimWaitIsTheSeasonsOwn(t *testing.T) {
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
	guild, err := NewGuildRepository(db.Pool).UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-wait-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServerRepository(db.Pool).UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "fixture", ProviderServiceID: fmt.Sprintf("ranked-wait-%d", suffix), Game: "dayz", Platform: "XBOX", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	players := NewPlayerRepository(db.Pool)
	attacker, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("wait-attacker-%d", suffix), "Attacker", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	victim, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("wait-victim-%d", suffix), "Victim", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRankedRepository(db.Pool)
	kills := NewKillRepository(db.Pool)
	rules := ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}
	n := 0
	award := func(at time.Time, outcome string) {
		t.Helper()
		n++
		id, err := kills.InsertKillReturning(ctx, KillRecord{GuildID: guild, ServerID: server.ID, SessionID: "ranked-wait", Fingerprint: fmt.Sprintf("ranked-wait-%d-%d", suffix, n), KillerPlayerID: attacker, VictimPlayerID: victim, EventTime: &at})
		if err != nil {
			t.Fatal(err)
		}
		got, err := repo.AwardActiveServerKill(ctx, server.ID, id)
		if err != nil || got.Outcome != outcome {
			t.Fatalf("kill %d at %s = %+v, %v; want %s", n, at.Format(time.TimeOnly), got, err, outcome)
		}
	}
	for _, bad := range []int{-1, 121} {
		if _, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, bad, false, time.Now().UTC()); err == nil {
			t.Fatalf("a wait of %d minutes was accepted", bad)
		}
	}

	// Season 1: no wait. Every kill of the same victim counts, even in the same second.
	start := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	first, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, 0, false, start)
	if err != nil || first.SameVictimCooldownMinutes != 0 {
		t.Fatalf("start with no wait: %+v %v", first, err)
	}
	if active, err := repo.ActiveServerSeason(ctx, guild, server.ID); err != nil || active == nil || active.SameVictimCooldownMinutes != 0 {
		t.Fatalf("active season wait: %+v %v", active, err)
	}
	award(start.Add(time.Minute), "AWARDED")
	award(start.Add(time.Minute), "AWARDED")
	award(start.Add(time.Minute+time.Second), "AWARDED")

	// Season 2: thirty minutes, changed only by the reset.
	second := start.Add(time.Hour)
	next, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, 30, true, second)
	if err != nil || next.SameVictimCooldownMinutes != 30 {
		t.Fatalf("reset to thirty minutes: %+v %v", next, err)
	}
	firstKill := second.Add(time.Minute)
	award(firstKill, "AWARDED")
	award(firstKill.Add(5*time.Minute), "COOLDOWN") // the old fixed rule would have awarded this
	award(firstKill.Add(30*time.Minute-time.Second), "COOLDOWN")
	award(firstKill.Add(30*time.Minute), "AWARDED")

	// Season 3: the default five minutes behaves as before.
	third := second.Add(2 * time.Hour)
	if _, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, ranked.DefaultSameVictimCooldownMinutes, true, third); err != nil {
		t.Fatal(err)
	}
	award(third.Add(time.Minute), "AWARDED")
	award(third.Add(6*time.Minute-time.Second), "COOLDOWN")
	award(third.Add(6*time.Minute), "AWARDED")

	var waits []int32
	if err := db.Pool.QueryRow(ctx, `SELECT array_agg(same_victim_cooldown_minutes ORDER BY starts_at) FROM ranked_seasons WHERE server_id=$1`, server.ID).Scan(&waits); err != nil || len(waits) != 3 || waits[0] != 0 || waits[1] != 30 || waits[2] != 5 {
		t.Fatalf("each season keeps its own wait: %v %v", waits, err)
	}
}
