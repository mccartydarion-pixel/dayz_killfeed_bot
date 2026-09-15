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

// TestBountyUpgradeIsGuildScoped is a cross-guild-leakage regression test:
// Upgrade must only ever affect a bounty that belongs to the guild passed in,
// even when given another guild's valid bounty ID.
func TestBountyUpgradeIsGuildScoped(t *testing.T) {
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
	t.Log("PostgreSQL integration tests: RUNNING")
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
	bounties := NewBountyRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildAID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-guildA-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	guildBID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-guildB-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	playerA, err := players.UpsertPlayer(ctx, guildAID, fmt.Sprintf("dayz-a-%d", suffix), "PlayerA", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	starts := time.Now().UTC()
	bountyA, err := bounties.Create(ctx, Bounty{GuildID: guildAID, TargetPlayerID: playerA, CreatedByType: BountyAutomatic, RewardPoints: 50, StartsAt: &starts}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	// Attempt to upgrade guild A's bounty while scoped to guild B: must be a no-op.
	if err := bounties.Upgrade(ctx, guildBID, bountyA.ID, 999); err != nil {
		t.Fatal(err)
	}
	unchanged, err := bounties.GetActive(ctx, guildAID, playerA)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.RewardPoints != 50 {
		t.Fatalf("cross-guild Upgrade must not modify another guild's bounty, got reward=%d", unchanged.RewardPoints)
	}

	// The same call scoped to the correct guild must take effect.
	if err := bounties.Upgrade(ctx, guildAID, bountyA.ID, 999); err != nil {
		t.Fatal(err)
	}
	changed, err := bounties.GetActive(ctx, guildAID, playerA)
	if err != nil {
		t.Fatal(err)
	}
	if changed.RewardPoints != 999 {
		t.Fatalf("expected same-guild Upgrade to take effect, got reward=%d", changed.RewardPoints)
	}
}
