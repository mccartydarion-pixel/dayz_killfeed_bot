//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// linkHomePlayer creates a player in the fixture's guild and links it to discordID with status.
func linkHomePlayer(t *testing.T, repo *PlayerServerRepository, fx saasFixture, discordID, status string) int64 {
	t.Helper()
	ctx := context.Background()
	var player int64
	if err := repo.pool.QueryRow(ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'Home Player') RETURNING id`,
		fx.GuildRowID, fmt.Sprintf("home-%d", time.Now().UnixNano())).Scan(&player); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,$4)`, fx.GuildRowID, player, discordID, status); err != nil {
		t.Fatal(err)
	}
	return player
}

func TestPlayerHomeNeedsOnlyAVerifiedLink(t *testing.T) {
	db := saasIntegrationDB(t)
	fx := newSaaSFixture(t, db)
	repo := NewPlayerServerRepository(db.Pool)
	ctx := context.Background()
	discordID := fmt.Sprintf("home-user-%d", time.Now().UnixNano())

	if _, found, err := repo.HomeForDiscordUser(ctx, discordID); err != nil || found {
		t.Fatalf("no link: found=%v err=%v", found, err)
	}
	player := linkHomePlayer(t, repo, fx, discordID, "PENDING")
	if _, found, err := repo.HomeForDiscordUser(ctx, discordID); err != nil || found {
		t.Fatalf("a PENDING link must not give a home: found=%v err=%v", found, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE player_links SET status='VERIFIED' WHERE player_id=$1`, player); err != nil {
		t.Fatal(err)
	}

	// Verified, never observed: no ListForDiscordUser row, but a home.
	if rows, err := repo.ListForDiscordUser(ctx, discordID); err != nil || len(rows) != 0 {
		t.Fatalf("ListForDiscordUser must still require activity: %v %v", rows, err)
	}
	home, found, err := repo.HomeForDiscordUser(ctx, discordID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if home.InstallationID != fx.InstallationID || home.OrganizationID != fx.OrgID || home.OrganizationName != "Fixture Org" ||
		home.ServerName != "Fixture Server" || home.Platform != "PLAYSTATION" || home.DiscordGuildName != "Fixture Guild" ||
		home.DiscordGuildID == "" || home.ServerStatus == "" || home.LinkStatus != "VERIFIED" || home.Observed {
		t.Fatalf("%+v", home)
	}

	if _, err := db.Pool.Exec(ctx, `INSERT INTO player_server_activity(guild_id, server_id, player_id) VALUES($1,$2,$3)`, fx.GuildRowID, fx.ServerRowID, player); err != nil {
		t.Fatal(err)
	}
	if home, found, err = repo.HomeForDiscordUser(ctx, discordID); err != nil || !found || !home.Observed {
		t.Fatalf("expected observed after activity: %+v found=%v err=%v", home, found, err)
	}
}

// Links in two guilds: the server the player was actually seen on is the home, even though the
// other installation has the lower id.
func TestPlayerHomeOrdersObservedServersFirst(t *testing.T) {
	db := saasIntegrationDB(t)
	first := newSaaSFixture(t, db)
	second := newSaaSFixture(t, db)
	repo := NewPlayerServerRepository(db.Pool)
	ctx := context.Background()
	discordID := fmt.Sprintf("home-two-%d", time.Now().UnixNano())
	linkHomePlayer(t, repo, first, discordID, "VERIFIED")
	onSecond := linkHomePlayer(t, repo, second, discordID, "VERIFIED")

	home, found, err := repo.HomeForDiscordUser(ctx, discordID)
	if err != nil || !found || home.InstallationID != first.InstallationID {
		t.Fatalf("no activity: expected the lower installation id, got %+v found=%v err=%v", home, found, err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO player_server_activity(guild_id, server_id, player_id) VALUES($1,$2,$3)`, second.GuildRowID, second.ServerRowID, onSecond); err != nil {
		t.Fatal(err)
	}
	home, found, err = repo.HomeForDiscordUser(ctx, discordID)
	if err != nil || !found || home.InstallationID != second.InstallationID || !home.Observed {
		t.Fatalf("expected the observed server, got %+v found=%v err=%v", home, found, err)
	}
}

func TestPlayerHomesListsEveryLinkedServerInDefaultOrder(t *testing.T) {
	db := saasIntegrationDB(t)
	first := newSaaSFixture(t, db)
	second := newSaaSFixture(t, db)
	foreign := newSaaSFixture(t, db) // the user holds no link here
	repo := NewPlayerServerRepository(db.Pool)
	ctx := context.Background()
	discordID := fmt.Sprintf("homes-%d", time.Now().UnixNano())

	if homes, err := repo.HomesForDiscordUser(ctx, discordID); err != nil || len(homes) != 0 {
		t.Fatalf("no link: %v %v", homes, err)
	}
	linkHomePlayer(t, repo, first, discordID, "VERIFIED")
	homes, err := repo.HomesForDiscordUser(ctx, discordID)
	if err != nil || len(homes) != 1 || homes[0].InstallationID != first.InstallationID {
		t.Fatalf("single server: %+v %v", homes, err)
	}
	onSecond := linkHomePlayer(t, repo, second, discordID, "VERIFIED")
	if _, err := db.Pool.Exec(ctx, `INSERT INTO player_server_activity(guild_id, server_id, player_id) VALUES($1,$2,$3)`, second.GuildRowID, second.ServerRowID, onSecond); err != nil {
		t.Fatal(err)
	}
	homes, err = repo.HomesForDiscordUser(ctx, discordID)
	if err != nil || len(homes) != 2 || homes[0].InstallationID != second.InstallationID || !homes[0].Observed ||
		homes[1].InstallationID != first.InstallationID || homes[1].Observed {
		t.Fatalf("observed first, then the rest: %+v %v", homes, err)
	}
	for _, home := range homes {
		if home.InstallationID == foreign.InstallationID {
			t.Fatalf("a server without a link was listed: %+v", homes)
		}
	}
	home, found, err := repo.HomeForDiscordUser(ctx, discordID)
	if err != nil || !found || home != homes[0] {
		t.Fatalf("the home is the first of the list: %+v found=%v err=%v", home, found, err)
	}
}
