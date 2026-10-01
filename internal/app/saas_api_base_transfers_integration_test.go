//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBaseTransferPlayerRoutes(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	giverDiscord, mateDiscord := w.players[0], w.players[1]
	giver := w.linkPlayer(w.a1, giverDiscord, "Giver")
	mate := w.linkPlayer(w.a1, mateDiscord, "Mate")
	guild, server := w.gameContext(w.a1)
	var faction, hall int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'Xfer','XFR',$2) RETURNING id`, guild, giver).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int64{giver, mate} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,'MEMBER')`, guild, faction, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Hall',100,100,50) RETURNING id`, w.a1.InstallationID, guild, server, giver).Scan(&hall); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace/base-transfers", w.a1.OrgID, w.a1.InstallationID)
	v := w.expect(w.do(http.MethodGet, path, giverDiscord, nil), http.StatusOK, "options").JSON(t)
	if len(v["bases"].([]any)) != 1 || len(v["mates"].([]any)) != 1 || len(v["transfers"].([]any)) != 0 {
		t.Fatalf("options: %+v", v)
	}
	w.expect(w.do(http.MethodPost, path, giverDiscord, map[string]any{"baseId": hall, "toPlayerId": mate, "extra": 1}), http.StatusBadRequest, "unknown field")
	w.expect(w.do(http.MethodPost, path, mateDiscord, map[string]any{"baseId": hall, "toPlayerId": giver}), http.StatusConflict, "not their base")
	created := w.expect(w.do(http.MethodPost, path, giverDiscord, map[string]any{"baseId": hall, "toPlayerId": mate}), http.StatusCreated, "create").JSON(t)
	id := int64(created["transfer"].(map[string]any)["id"].(float64))
	w.expect(w.do(http.MethodPost, path, giverDiscord, map[string]any{"baseId": hall, "toPlayerId": mate}), http.StatusConflict, "already waiting")
	if got := w.expect(w.do(http.MethodGet, path, mateDiscord, nil), http.StatusOK, "mate view").JSON(t)["transfers"].([]any); len(got) != 1 {
		t.Fatalf("mate sees the transfer: %+v", got)
	}
	w.expect(w.do(http.MethodPost, fmt.Sprintf("%s/%d/cancel", path, id), mateDiscord, nil), http.StatusNotFound, "mate can't cancel")
	w.expect(w.do(http.MethodPost, fmt.Sprintf("%s/%d/cancel", path, id), giverDiscord, nil), http.StatusOK, "cancel")
	w.expect(w.do(http.MethodGet, path, w.players[2], nil), http.StatusConflict, "unlinked player")
}

func TestBaseTransferOwnerRoutes(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	admin := zoneActor(t, w, "xfer-admin")
	w.mapRole(admin, "xfer-admin-role", "ADMINISTRATOR")
	giver, mate := w.seedPlayer("Owner Giver"), w.seedPlayer("Owner Mate")
	var faction, hall int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'OXfer','OXF',$2) RETURNING id`, w.guildID, giver).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for i, p := range []int64{giver, mate} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,'MEMBER')`, w.guildID, faction, p); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`,
			w.guildID, p, fmt.Sprintf("oxfer-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Owner Hall',100,100,50) RETURNING id`, w.f.InstallationID, w.guildID, w.serverID, giver).Scan(&hall); err != nil {
		t.Fatal(err)
	}
	s := repository.BaseRequestScope{InstallationID: w.f.InstallationID, GuildID: w.guildID, ServerID: w.serverID}
	repo := repository.NewBaseTransferRepository(pool)
	first, err := repo.Create(ctx, s, giver, hall, mate)
	if err != nil {
		t.Fatal(err)
	}
	pv := func(id int64) map[string]string {
		m := w.pathValues()
		m["transferID"] = fmt.Sprint(id)
		return m
	}
	if rr := w.call(w.a.handleGetBaseTransfers, http.MethodGet, w.path("/case/base-transfers"), admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin list: %d", rr.Code)
	}
	list := decodeBody[struct {
		Transfers []repository.BaseTransfer `json:"transfers"`
	}](t, w.call(w.a.handleGetBaseTransfers, http.MethodGet, w.path("/case/base-transfers"), w.f.OwnerDiscordID, nil, nil))
	if len(list.Transfers) != 1 || list.Transfers[0].Status != repository.BaseTransferPending {
		t.Fatalf("list: %+v", list)
	}
	decline := w.path(fmt.Sprintf("/case/base-transfers/%d/decline", first.ID))
	if rr := w.call(w.a.handleDeclineBaseTransfer, http.MethodPost, decline, w.f.OwnerDiscordID, map[string]any{"reason": "Talk to staff first"}, pv(first.ID)); rr.Code != http.StatusOK {
		t.Fatalf("decline: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.call(w.a.handleApproveBaseTransfer, http.MethodPost, w.path(fmt.Sprintf("/case/base-transfers/%d/approve", first.ID)), w.f.OwnerDiscordID, nil, pv(first.ID)); rr.Code != http.StatusConflict {
		t.Fatalf("approve after decline: %d", rr.Code)
	}
	second, err := repo.Create(ctx, s, giver, hall, mate)
	if err != nil {
		t.Fatal(err)
	}
	approve := w.path(fmt.Sprintf("/case/base-transfers/%d/approve", second.ID))
	if rr := w.call(w.a.handleApproveBaseTransfer, http.MethodPost, approve, admin, nil, pv(second.ID)); rr.Code != http.StatusForbidden {
		t.Fatalf("admin approve: %d", rr.Code)
	}
	if rr := w.call(w.a.handleApproveBaseTransfer, http.MethodPost, approve, w.f.OwnerDiscordID, nil, pv(second.ID)); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
	var owner int64
	if err := pool.QueryRow(ctx, `SELECT owner_player_id FROM case_registered_bases WHERE id=$1`, hall).Scan(&owner); err != nil || owner != mate {
		t.Fatalf("owner changed: %d %v", owner, err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action IN ('BASE_TRANSFER_APPROVED','BASE_TRANSFER_DECLINED')`, w.f.InstallationID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audits: %d %v", audits, err)
	}
}
