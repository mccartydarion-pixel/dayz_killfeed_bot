//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBaseRequestsPlayerSide(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.Locations = repository.NewLocationRepository(pool)
	playerDiscord := w.players[0]
	player := w.linkPlayer(w.a1, playerDiscord, "Builder")
	guild, server := w.gameContext(w.a1)
	market := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace/base-requests", w.a1.OrgID, w.a1.InstallationID)

	view := w.expect(w.do(http.MethodGet, market, playerDiscord, nil), http.StatusOK, "no position yet").JSON(t)
	if view["position"] != nil || len(view["requests"].([]any)) != 0 || view["maxBases"].(float64) != 3 {
		t.Fatalf("empty view: %+v", view)
	}
	body := map[string]any{"name": "Hilltop", "radius": 50, "note": "by the tower"}
	w.expect(w.do(http.MethodPost, market, playerDiscord, body), http.StatusConflict, "no logged position")

	// A stale position is refused; a fresh one is used, and typed coordinates are rejected outright.
	if _, err := pool.Exec(ctx, `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at)
 VALUES($1,$2,$3,'Builder',4000,6000,'PLAYER_LIST',NOW()-INTERVAL '2 hours')`, guild, server, player); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, market, playerDiscord, body), http.StatusConflict, "stale position")
	if _, err := pool.Exec(ctx, `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at)
 VALUES($1,$2,$3,'Builder',1234.5,6789.25,'PLAYER_LIST',NOW()-INTERVAL '1 minute')`, guild, server, player); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, market, playerDiscord, map[string]any{"name": "Cheat", "radius": 50, "centerX": 1, "centerZ": 1}), http.StatusBadRequest, "typed coordinates")
	w.expect(w.do(http.MethodPost, market, playerDiscord, map[string]any{"name": "Huge", "radius": 500}), http.StatusBadRequest, "oversized base")
	created := w.expect(w.do(http.MethodPost, market, playerDiscord, body), http.StatusCreated, "request").JSON(t)["request"].(map[string]any)
	if created["centerX"].(float64) != 1234.5 || created["centerZ"].(float64) != 6789.25 || created["status"] != "PENDING" {
		t.Fatalf("request uses the logged position: %+v", created)
	}
	w.expect(w.do(http.MethodPost, market, playerDiscord, body), http.StatusConflict, "second pending")
	w.expect(w.do(http.MethodGet, market, w.players[2], nil), http.StatusConflict, "unlinked player")
	id := int64(created["id"].(float64))
	w.expect(w.do(http.MethodPost, fmt.Sprintf("%s/%d/cancel", market, id), w.players[1], nil), http.StatusConflict, "unlinked player can't cancel")
	w.expect(w.do(http.MethodPost, fmt.Sprintf("%s/%d/cancel", market, id), playerDiscord, nil), http.StatusOK, "cancel")
	w.expect(w.do(http.MethodPost, fmt.Sprintf("%s/%d/cancel", market, id), playerDiscord, nil), http.StatusNotFound, "cancel twice")
	v := w.expect(w.do(http.MethodGet, market, playerDiscord, nil), http.StatusOK, "after cancel").JSON(t)
	if v["position"].(map[string]any)["fresh"] != true || v["requests"].([]any)[0].(map[string]any)["status"] != "CANCELLED" {
		t.Fatalf("player view: %+v", v)
	}
}

func TestBaseRequestsOwnerSide(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	admin := zoneActor(t, w, "base-request-admin")
	w.mapRole(admin, "base-request-admin-role", "ADMINISTRATOR")
	player := w.seedPlayer("Builder")
	repo := repository.NewCaseBaseRequestRepository(w.a.DB.Pool)
	s := repository.BaseRequestScope{InstallationID: w.f.InstallationID, GuildID: w.guildID, ServerID: w.serverID}
	newRequest := func(name string) int64 {
		t.Helper()
		q, err := repo.Create(ctx, s, repository.BaseRequestInput{PlayerID: player, Name: name, CenterX: 1000, CenterZ: 2000, Radius: 50, PositionSeenAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return q.ID
	}
	path := w.path("/case/base-requests")
	if rr := w.call(w.a.handleGetBaseRequests, http.MethodGet, path, admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin list: %d", rr.Code)
	}
	id := newRequest("Hilltop")
	list := decodeBody[struct {
		Requests   []repository.CaseBaseRequest `json:"requests"`
		MapKeyHint string                       `json:"mapKeyHint"`
	}](t, w.call(w.a.handleGetBaseRequests, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if len(list.Requests) != 1 || list.Requests[0].ID != id {
		t.Fatalf("owner list: %+v", list)
	}
	pv := map[string]string{"requestID": strconv.FormatInt(id, 10)}
	if rr := w.call(w.a.handleApproveBaseRequest, http.MethodPost, path+"/x/approve", admin, map[string]any{"mapKey": "chernarusplus"}, pv); rr.Code != http.StatusForbidden {
		t.Fatalf("admin approve: %d", rr.Code)
	}
	if rr := w.call(w.a.handleApproveBaseRequest, http.MethodPost, path+"/x/approve", w.f.OwnerDiscordID, map[string]any{"mapKey": ""}, pv); rr.Code != http.StatusBadRequest {
		t.Fatalf("approve without a map: %d", rr.Code)
	}
	if rr := w.call(w.a.handleApproveBaseRequest, http.MethodPost, path+"/x/approve", w.f.OwnerDiscordID, map[string]any{"mapKey": "chernarusplus", "x": 1}, pv); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", rr.Code)
	}
	rr := w.call(w.a.handleApproveBaseRequest, http.MethodPost, path+"/x/approve", w.f.OwnerDiscordID, map[string]any{"mapKey": "chernarusplus", "radius": 60}, pv)
	approved := decodeBody[struct {
		Request repository.CaseBaseRequest `json:"request"`
	}](t, rr).Request
	if rr.Code != http.StatusOK || approved.Status != repository.BaseRequestApproved || approved.BaseID == nil || approved.Radius != 60 {
		t.Fatalf("approve: %d %+v", rr.Code, approved)
	}
	if rr := w.call(w.a.handleDeclineBaseRequest, http.MethodPost, path+"/x/decline", w.f.OwnerDiscordID, map[string]any{}, pv); rr.Code != http.StatusConflict {
		t.Fatalf("decline after approval: %d", rr.Code)
	}
	declineID := newRequest("Shack")
	rr = w.call(w.a.handleDeclineBaseRequest, http.MethodPost, path+"/x/decline", w.f.OwnerDiscordID, map[string]any{"reason": "Inside the safe zone"},
		map[string]string{"requestID": strconv.FormatInt(declineID, 10)})
	if declined := decodeBody[struct {
		Request repository.CaseBaseRequest `json:"request"`
	}](t, rr).Request; rr.Code != http.StatusOK || declined.Status != repository.BaseRequestDeclined || declined.DeclineReason != "Inside the safe zone" {
		t.Fatalf("decline: %d %+v", rr.Code, declined)
	}
	var audits int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action IN ('CASE_BASE_REQUEST_APPROVED','CASE_BASE_REQUEST_DECLINED')`,
		w.f.InstallationID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audits: %d %v", audits, err)
	}
}
