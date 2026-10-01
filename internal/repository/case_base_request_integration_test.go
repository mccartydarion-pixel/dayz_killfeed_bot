//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCaseBaseRequestsLifecycle(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	repo := NewCaseBaseRequestRepository(pool)
	s := BaseRequestScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	player, neighbour := seedPlayer("Builder"), seedPlayer("Neighbour")
	if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,'br-builder','VERIFIED',NOW())`, fx.GuildRowID, player); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Next Door',1080,2000,50)`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, neighbour); err != nil {
		t.Fatal(err)
	}
	seen := time.Now().Add(-time.Minute)
	in := BaseRequestInput{PlayerID: player, Name: "  Hilltop  ", Note: "near the tower", CenterX: 1000, CenterZ: 2000, Radius: 50, PositionSeenAt: seen}

	for _, bad := range []BaseRequestInput{
		{PlayerID: player, Name: "", CenterX: 1, CenterZ: 1, Radius: 50, PositionSeenAt: seen},
		{PlayerID: player, Name: "x", CenterX: 1, CenterZ: 1, Radius: 151, PositionSeenAt: seen},
		{PlayerID: player, Name: "x", CenterX: 1, CenterZ: 1, Radius: 50},
	} {
		if _, err := repo.Create(ctx, s, bad); !errors.Is(err, ErrInvalidBaseRequest) {
			t.Fatalf("bad request %+v: %v", bad, err)
		}
	}
	first, err := repo.Create(ctx, s, in)
	if err != nil || first.Name != "Hilltop" || first.Status != BaseRequestPending {
		t.Fatalf("create: %+v %v", first, err)
	}
	if _, err := repo.Create(ctx, s, in); !errors.Is(err, ErrBaseRequestOpen) {
		t.Fatalf("second pending request: %v", err)
	}
	owner, err := repo.ForOwner(ctx, s, 10)
	if err != nil || len(owner) != 1 || owner[0].PlayerName != "Builder" || len(owner[0].Overlaps) != 1 || owner[0].Overlaps[0] != "Next Door" {
		t.Fatalf("owner list with overlap: %+v %v", owner, err)
	}
	if hint, err := repo.MapKeyHint(ctx, s); err != nil || hint != "chernarusplus" {
		t.Fatalf("map hint: %q %v", hint, err)
	}

	// Cancel: only the player's own pending request.
	if err := repo.Cancel(ctx, s, neighbour, first.ID); !errors.Is(err, ErrBaseRequestNotFound) {
		t.Fatalf("another player cancelled it: %v", err)
	}
	if err := repo.Cancel(ctx, s, player, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Approve(ctx, s, first.ID, "chernarusplus", "", 0, &fx.OwnerUserID); !errors.Is(err, ErrBaseRequestDecided) {
		t.Fatalf("approved a cancelled request: %v", err)
	}

	// Decline.
	second, err := repo.Create(ctx, s, in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := repo.Decline(ctx, s, second.ID, "  Too close to the trader  ", &fx.OwnerUserID)
	if err != nil || d.Request.Status != BaseRequestDeclined || d.Request.DeclineReason != "Too close to the trader" || d.DiscordUserID != "br-builder" {
		t.Fatalf("decline: %+v %v", d, err)
	}
	if _, err := repo.Decline(ctx, s, second.ID, "", nil); !errors.Is(err, ErrBaseRequestDecided) {
		t.Fatalf("declined twice: %v", err)
	}

	// Approve registers the base with the owner's adjustments.
	third, err := repo.Create(ctx, s, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Approve(ctx, s, third.ID, "", "", 0, nil); !errors.Is(err, ErrInvalidBaseRequest) {
		t.Fatalf("approved without a map: %v", err)
	}
	if _, err := repo.Approve(ctx, s, third.ID, "chernarusplus", "", 500, nil); !errors.Is(err, ErrInvalidBaseRequest) {
		t.Fatalf("approved an oversized base: %v", err)
	}
	other := BaseRequestScope{InstallationID: s.InstallationID, GuildID: s.GuildID + 999, ServerID: s.ServerID}
	if _, err := repo.Approve(ctx, other, third.ID, "chernarusplus", "", 0, nil); !errors.Is(err, ErrBaseRequestNotFound) {
		t.Fatalf("approved from another scope: %v", err)
	}
	ok, err := repo.Approve(ctx, s, third.ID, "chernarusplus", "Hilltop Fort", 40, &fx.OwnerUserID)
	if err != nil || ok.Request.Status != BaseRequestApproved || ok.Request.BaseID == nil || ok.Request.Name != "Hilltop Fort" || ok.DiscordUserID != "br-builder" {
		t.Fatalf("approve: %+v %v", ok, err)
	}
	var owned int64
	var name, state string
	var cx, radius float64
	if err := pool.QueryRow(ctx, `SELECT owner_player_id,name,state,center_x,radius FROM case_registered_bases WHERE id=$1`, *ok.Request.BaseID).
		Scan(&owned, &name, &state, &cx, &radius); err != nil || owned != player || name != "Hilltop Fort" || state != "DRAFT" || cx != 1000 || radius != 40 {
		t.Fatalf("registered base: %d %q %q %v %v %v", owned, name, state, cx, radius, err)
	}
	bases, err := repo.PlayerBases(ctx, s, player)
	if err != nil || len(bases) != 1 || bases[0].Name != "Hilltop Fort" {
		t.Fatalf("player bases: %+v %v", bases, err)
	}
	mine, err := repo.Mine(ctx, s, player, 10)
	if err != nil || len(mine) != 3 || mine[0].Status != BaseRequestApproved {
		t.Fatalf("mine: %+v %v", mine, err)
	}

	// Limit: bases plus pending requests.
	if _, err := repo.Create(ctx, s, in); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Second',5000,5000,30)`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, player); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_base_requests SET status='CANCELLED',decided_at=NOW() WHERE player_id=$1 AND status='PENDING'`, player); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, s, in); err != nil {
		t.Fatalf("third slot: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_base_requests SET status='CANCELLED',decided_at=NOW() WHERE player_id=$1 AND status='PENDING'`, player); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Third',7000,7000,30)`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, player); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, s, in); !errors.Is(err, ErrBaseRequestLimit) {
		t.Fatalf("over the base limit: %v", err)
	}
}
