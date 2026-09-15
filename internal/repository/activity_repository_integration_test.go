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

// TestActivityGetReturnsNilNotErrorWhenNoRow is the ACTIVITY_LOOKUP_FAILED
// regression test: a player who has never connected to a server has no
// player_server_activity row yet. That must resolve as "zero observed
// playtime", never as a lookup failure (pgx.ErrNoRows must not leak out as a
// generic error).
func TestActivityGetReturnsNilNotErrorWhenNoRow(t *testing.T) {
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
	servers := NewServerRepository(db.Pool)
	activity := NewActivityRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-activity-guild-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	playerID, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-activity-%d", suffix), "ActivityPlayer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	server, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guildID, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("svc-%d", suffix), Game: "DayZ", DisplayName: "Test Server", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}

	// No player_server_activity row exists yet for this player/server pair.
	got, err := activity.Get(ctx, guildID, server.ID, playerID)
	if err != nil {
		t.Fatalf("expected no error for a missing activity row, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil activity for a player never observed on this server, got %+v", got)
	}

	playtime, err := activity.GetObservedPlaytime(ctx, guildID, server.ID, playerID, time.Now())
	if err != nil {
		t.Fatalf("expected zero playtime, not an error, for an unobserved player, got %v", err)
	}
	if playtime != 0 {
		t.Fatalf("expected zero observed playtime, got %s", playtime)
	}

	// After a connect event, Get must return the real row.
	if err := activity.Connect(ctx, guildID, server.ID, playerID, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = activity.Get(ctx, guildID, server.ID, playerID)
	if err != nil {
		t.Fatalf("expected no error after connect, got %v", err)
	}
	if got == nil {
		t.Fatal("expected an activity row to exist after connect")
	}
}
