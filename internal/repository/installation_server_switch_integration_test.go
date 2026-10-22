//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// An installation with per-server history (a registered base, a C.A.S.E. setting) can move to
// another game server; the history stays with the server it was recorded for.
func TestInstallationMovesToAnotherServerWithHistory(t *testing.T) {
	db := saasIntegrationDB(t)
	fx := newSaaSFixture(t, db)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$1 WHERE id=$2`, fx.ServerRowID, fx.InstallationID); err != nil {
		t.Fatal(err)
	}
	var owner, base int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,'Owner') RETURNING id`,
		fx.GuildRowID, fmt.Sprintf("switch-%d", time.Now().UnixNano())).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Old Fort',100,100,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO base_raid_alarm_settings(installation_id,guild_id,server_id,enabled,cooldown_seconds) VALUES($1,$2,$3,TRUE,600)`,
		fx.InstallationID, fx.GuildRowID, fx.ServerRowID); err != nil {
		t.Fatal(err)
	}
	next, err := NewServerRepository(db.Pool).UpsertGameServer(ctx, GameServer{GuildID: fx.GuildRowID, Provider: "NITRADO",
		ProviderServiceID: fmt.Sprintf("switch-svc-%d", time.Now().UnixNano()), Game: "DayZ", Platform: "PLAYSTATION", DisplayName: "New Server", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewInstallationRepository(db.Pool).SetGameServer(ctx, fx.OrgID, fx.InstallationID, next.ID); err != nil {
		t.Fatalf("an installation with history must be able to switch servers: %v", err)
	}
	var server int64
	if err := db.Pool.QueryRow(ctx, `SELECT server_id FROM case_registered_bases WHERE id=$1`, base).Scan(&server); err != nil || server != fx.ServerRowID {
		t.Fatalf("the base stays with the server it was registered on: %d %v", server, err)
	}
	// Removing the installation still removes its rows.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM installations WHERE id=$1`, fx.InstallationID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM case_registered_bases WHERE installation_id=$1`, fx.InstallationID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("installation delete cascades: %d %v", left, err)
	}
}
