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

// TestClearPanelMessageIDsActuallyClears guards the leaderboard regression:
// UpsertGuild COALESCEs message ids, so saving an empty id never cleared a
// retired legacy board and it resurfaced after every restart.
func TestClearPanelMessageIDsActuallyClears(t *testing.T) {
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
	gid := fmt.Sprintf("integration-panel-clear-%d", time.Now().UnixNano())
	if _, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: gid, LeaderboardsChannelID: "lb", LeaderboardMessageID: "old-board", LinkPanelMessageID: "link"}); err != nil {
		t.Fatal(err)
	}
	// The pre-fix retire path: saving an empty id keeps the old one.
	if _, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: gid, LeaderboardsChannelID: "lb"}); err != nil {
		t.Fatal(err)
	}
	rec, _, err := guilds.GetGuild(ctx, gid)
	if err != nil || rec.LeaderboardMessageID != "old-board" {
		t.Fatalf("precondition: upsert preserves ids, got %+v %v", rec, err)
	}
	if err := guilds.ClearPanelMessageIDs(ctx, gid, GuildColumnLeaderboardMessage); err != nil {
		t.Fatal(err)
	}
	rec, _, err = guilds.GetGuild(ctx, gid)
	if err != nil || rec.LeaderboardMessageID != "" || rec.LinkPanelMessageID != "link" || rec.LeaderboardsChannelID != "lb" {
		t.Fatalf("only the leaderboard message id may be cleared, got %+v %v", rec, err)
	}
	if err := guilds.ClearPanelMessageIDs(ctx, gid, "leaderboards_channel_id"); err == nil {
		t.Fatal("non message-id columns must be rejected")
	}
}
