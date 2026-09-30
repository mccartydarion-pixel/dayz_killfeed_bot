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
		season, err := repo.StartServerSeason(ctx, guild, id, 100, rules, false, now)
		if err != nil || season.RPPerKill != 100 || season.Thresholds != rules || season.Platform != "PLAYSTATION" {
			t.Fatalf("status %s: start must succeed with the owner's rules, got %+v %v", status, season, err)
		}
	}
	xbox := newServer("xbox", "ONLINE", "XBOX", true)
	if _, err := repo.StartServerSeason(ctx, guild, xbox, 100, rules, false, now); err != nil {
		t.Fatalf("xbox: %v", err)
	}

	inactive := newServer("inactive", "DISCONNECTED", "PLAYSTATION", false)
	pc := newServer("pc", "ONLINE", "PC", true)
	for name, id := range map[string]int64{"inactive": inactive, "pc": pc} {
		if _, err := repo.StartServerSeason(ctx, guild, id, 100, rules, false, now); !errors.Is(err, ErrRankedServerIneligible) {
			t.Fatalf("%s: expected ErrRankedServerIneligible, got %v", name, err)
		}
	}
	// Tenant scope: a real, active server is never startable from another guild.
	crossGuild := newServer("tenant", "CONNECTED", "PLAYSTATION", true)
	if _, err := repo.StartServerSeason(ctx, other, crossGuild, 100, rules, false, now); !errors.Is(err, ErrRankedServerIneligible) {
		t.Fatalf("cross-guild start must be rejected, got %v", err)
	}
	// Reset with nothing active is its own, explicit error.
	fresh := newServer("fresh", "CONNECTED", "PLAYSTATION", true)
	if _, err := repo.StartServerSeason(ctx, guild, fresh, 100, rules, true, now); !errors.Is(err, ErrRankedNoActiveSeason) {
		t.Fatalf("reset without a season: expected ErrRankedNoActiveSeason, got %v", err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM ranked_seasons WHERE server_id = ANY($1)`, []int64{inactive, pc, crossGuild, fresh}).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected starts must create no season, got %d %v", n, err)
	}
}
