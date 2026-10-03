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

// TestFeaturesChannelRoundTrip covers the /features setting: unknown guild reads empty, a set id
// reads back, and "" forgets it without touching the guild's other channels.
func TestFeaturesChannelRoundTrip(t *testing.T) {
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
	gid := fmt.Sprintf("integration-features-%d", time.Now().UnixNano())

	if got, err := guilds.FeaturesChannel(ctx, gid); err != nil || got != "" {
		t.Fatalf("unknown guild: %q %v", got, err)
	}
	if _, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: gid, KillfeedChannelID: "kf"}); err != nil {
		t.Fatal(err)
	}
	if got, err := guilds.FeaturesChannel(ctx, gid); err != nil || got != "" {
		t.Fatalf("fresh guild: %q %v", got, err)
	}
	if err := guilds.SetFeaturesChannel(ctx, gid, "123"); err != nil {
		t.Fatal(err)
	}
	if got, err := guilds.FeaturesChannel(ctx, gid); err != nil || got != "123" {
		t.Fatalf("after set: %q %v", got, err)
	}
	if err := guilds.SetFeaturesChannel(ctx, gid, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := guilds.FeaturesChannel(ctx, gid); err != nil || got != "" {
		t.Fatalf("after clear: %q %v", got, err)
	}
	rec, _, err := guilds.GetGuild(ctx, gid)
	if err != nil || rec.KillfeedChannelID != "kf" {
		t.Fatalf("other channels must be untouched, got %+v %v", rec, err)
	}
}
