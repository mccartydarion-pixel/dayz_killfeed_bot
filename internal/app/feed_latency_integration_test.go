//go:build integration

package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The worker's server clock reads the UTC offset Live Sync stored (live_sync_server_clock): unknown
// for a server that has none, the stored value for one that has.
func TestServerUTCOffsetSourceReadsLearnedClock(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	guildID, err := repository.NewGuildRepository(db.Pool).UpsertGuild(ctx, repository.GuildRecord{DiscordGuildID: fmt.Sprintf("clock-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(label string) repository.GameServer {
		server, err := repository.NewServerRepository(db.Pool).UpsertGameServer(ctx, repository.GameServer{GuildID: guildID, Provider: "fixture",
			ProviderServiceID: fmt.Sprintf("clock-%s-%d", label, suffix), Game: "dayz", Platform: "PLAYSTATION", Status: "CONNECTED", Active: true})
		if err != nil {
			t.Fatal(err)
		}
		return *server
	}
	learned, unlearned := newServer("learned"), newServer("unlearned")
	if err = repository.NewLiveSyncRepository(db.Pool).SetServerUTCOffset(ctx, guildID, learned.ID, -240, "feed-latency-integration"); err != nil {
		t.Fatal(err)
	}

	a := &App{DB: db}
	known := a.serverUTCOffsetSource(ctx, learned)
	unknown := a.serverUTCOffsetSource(ctx, unlearned)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if minutes, ok := known(); ok {
			if minutes != -240 {
				t.Fatalf("offset = %d, want -240", minutes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stored offset was never read")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the other source's first read has had time to finish
	if minutes, ok := unknown(); ok {
		t.Fatalf("a server without a learned clock reported offset %d", minutes)
	}
}
