//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBaseBlackBoxOwnerSettings(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "black-box-admin")
	w.mapRole(admin, "black-box-admin-role", "ADMINISTRATOR")
	path := w.path("/case/black-box")
	if got := w.call(w.a.handleGetBaseBlackBox, http.MethodGet, path, admin, nil, nil); got.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", got.Code)
	}
	type view struct {
		Settings repository.BaseBlackBoxSettings `json:"settings"`
		Offer    repository.SecurityOffer        `json:"offer"`
	}
	got := decodeBody[view](t, w.call(w.a.handleGetBaseBlackBox, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if got.Settings.Enabled || got.Settings.RetentionDays != 14 || got.Offer.Enabled || got.Offer.ServiceID != repository.ServiceBaseBlackBox {
		t.Fatalf("unsafe default: %+v", got)
	}
	for _, bad := range []map[string]any{{"enabled": true, "retentionDays": 2}, {"enabled": true, "retentionDays": 31}, {"enabled": true, "margin": 9}} {
		if rr := w.call(w.a.handleSetBaseBlackBox, http.MethodPut, path, w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad %v accepted: %d", bad, rr.Code)
		}
	}
	if rr := w.call(w.a.handleSetBaseBlackBox, http.MethodPut, path, admin, map[string]any{"enabled": true}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin turned it on: %d", rr.Code)
	}
	on := w.call(w.a.handleSetBaseBlackBox, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true, "retentionDays": 7}, nil)
	if s := decodeBody[view](t, on).Settings; on.Code != http.StatusOK || !s.Enabled || s.RetentionDays != 7 {
		t.Fatalf("turn on: %d %+v", on.Code, s)
	}
	keep := w.call(w.a.handleSetBaseBlackBox, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": false}, nil)
	if s := decodeBody[view](t, keep).Settings; s.Enabled || s.RetentionDays != 7 {
		t.Fatalf("omitted retention must keep the saved value: %+v", s)
	}
	if rr := w.call(w.a.handleSetBaseBlackBoxOffer, http.MethodPut, path+"/offer", w.f.OwnerDiscordID, map[string]any{"enabled": true, "pricePoints": 400, "durationDays": 14}, nil); rr.Code != http.StatusOK {
		t.Fatalf("set offer: %d %s", rr.Code, rr.Body.String())
	}
	got = decodeBody[view](t, w.call(w.a.handleGetBaseBlackBox, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if !got.Offer.Enabled || got.Offer.PricePoints != 400 {
		t.Fatalf("offer not shown: %+v", got.Offer)
	}
}

func TestBaseBlackBoxPlayerHistory(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	ownerDiscord, otherDiscord := w.players[0], w.players[1]
	owner := w.linkPlayer(w.a1, ownerDiscord, "Base Owner")
	other := w.linkPlayer(w.a1, otherDiscord, "Someone Else")
	guild, server := w.gameContext(w.a1)
	pool := w.a.DB.Pool
	scope := repository.SecurityScope{InstallationID: w.a1.InstallationID, GuildID: guild, ServerID: server}
	path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace/black-box", w.a1.OrgID, w.a1.InstallationID)
	var baseID int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Home',1000,2000,50) RETURNING id`, scope.InstallationID, guild, server, owner).Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO base_black_box_events
 (installation_id,guild_id,server_id,base_id,kind,player_id,player_name,closest_meters) VALUES($1,$2,$3,$4,'VISIT',$5,'Someone Else',80)`,
		scope.InstallationID, guild, server, baseID, other); err != nil {
		t.Fatal(err)
	}
	view := func(who string) map[string]any {
		t.Helper()
		return w.expect(w.do(http.MethodGet, path, who, nil), http.StatusOK, "black box view").JSON(t)
	}
	if v := view(ownerDiscord); v["available"] != false || v["reason"] != "OFF" || len(v["events"].([]any)) != 0 {
		t.Fatalf("history shown while off: %+v", v)
	}
	if _, err := repository.NewBaseBlackBoxRepository(pool).SetSettings(ctx, scope.InstallationID, guild, server, true, 14, nil); err != nil {
		t.Fatal(err)
	}
	v := view(ownerDiscord)
	if v["available"] != true || len(v["bases"].([]any)) != 1 || len(v["events"].([]any)) != 1 {
		t.Fatalf("owner history (free while not on sale): %+v", v)
	}
	if ev := v["events"].([]any)[0].(map[string]any); ev["playerName"] != "Someone Else" || ev["baseName"] != "Home" {
		t.Fatalf("event: %+v", ev)
	}
	if o := view(otherDiscord); o["available"] != true || len(o["events"].([]any)) != 0 || len(o["bases"].([]any)) != 0 {
		t.Fatalf("another player saw this base's history: %+v", o)
	}
	w.expect(w.do(http.MethodGet, path, w.players[2], nil), http.StatusConflict, "unlinked player")

	// On sale: the owner needs paid time to see the history.
	sales := repository.NewSecurityServiceRepository(pool)
	if _, err := sales.SetOffer(ctx, scope, repository.ServiceBaseBlackBox, true, 200, 7, nil); err != nil {
		t.Fatal(err)
	}
	if v := view(ownerDiscord); v["available"] != false || v["reason"] != "NOT_PAID" || len(v["events"].([]any)) != 0 {
		t.Fatalf("unpaid owner saw history while on sale: %+v", v)
	}
	if _, err := repository.NewEconomyRepository(pool).Credit(ctx, repository.LedgerParams{GuildID: guild, PlayerID: owner,
		Type: repository.TxAdminCredit, Amount: 500, ReferenceID: "seed-black-box", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	marketPath := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace", w.a1.OrgID, w.a1.InstallationID)
	bought := w.expect(w.do(http.MethodPost, marketPath+"/purchases", ownerDiscord, map[string]any{"serviceId": "BASE_BLACK_BOX", "idempotencyKey": "black-box-key-1"}),
		http.StatusCreated, "black box purchase").JSON(t)
	if bought["remainingBalance"].(float64) != 300 {
		t.Fatalf("purchase: %+v", bought)
	}
	if v := view(ownerDiscord); v["available"] != true || v["activeUntil"] == nil || len(v["events"].([]any)) != 1 {
		t.Fatalf("paying owner: %+v", v)
	}
}
