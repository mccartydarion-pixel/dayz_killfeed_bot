//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestFactionSecurityOwnerSettings(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "faction-security-admin")
	w.mapRole(admin, "faction-security-admin-role", "ADMINISTRATOR")
	path := w.path("/case/faction-security")
	if got := w.call(w.a.handleGetFactionSecurity, http.MethodGet, path, admin, nil, nil); got.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", got.Code)
	}
	type view struct {
		Settings repository.FactionSecuritySettings `json:"settings"`
		Offer    repository.SecurityOffer           `json:"offer"`
	}
	got := decodeBody[view](t, w.call(w.a.handleGetFactionSecurity, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if got.Settings.Enabled || got.Offer.Enabled || got.Offer.ServiceID != repository.ServiceFactionSecurity {
		t.Fatalf("unsafe default: %+v", got)
	}
	if rr := w.call(w.a.handleSetFactionSecurity, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true, "everyone": true}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetFactionSecurity, http.MethodPut, path, admin, map[string]any{"enabled": true}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin turned it on: %d", rr.Code)
	}
	on := w.call(w.a.handleSetFactionSecurity, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true}, nil)
	if s := decodeBody[view](t, on).Settings; on.Code != http.StatusOK || !s.Enabled {
		t.Fatalf("turn on: %d %+v", on.Code, s)
	}
	if rr := w.call(w.a.handleSetFactionSecurityOffer, http.MethodPut, path+"/offer", w.f.OwnerDiscordID, map[string]any{"enabled": true, "pricePoints": 250, "durationDays": 30}, nil); rr.Code != http.StatusOK {
		t.Fatalf("set offer: %d %s", rr.Code, rr.Body.String())
	}
	got = decodeBody[view](t, w.call(w.a.handleGetFactionSecurity, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if !got.Offer.Enabled || got.Offer.PricePoints != 250 || got.Offer.DurationDays != 30 {
		t.Fatalf("offer not shown: %+v", got.Offer)
	}
}

func TestFactionSecurityPlayerChoice(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	ownerDiscord := w.players[0]
	owner := w.linkPlayer(w.a1, ownerDiscord, "Base Owner")
	mate := w.linkPlayer(w.a1, w.players[1], "Faction Mate")
	guild, server := w.gameContext(w.a1)
	pool := w.a.DB.Pool
	scope := repository.SecurityScope{InstallationID: w.a1.InstallationID, GuildID: guild, ServerID: server}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'Bears','BRS',$2) RETURNING id`, guild, owner).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for p, role := range map[int64]string{owner: "OWNER", mate: "MEMBER"} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, guild, faction, p, role); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace/faction-security", w.a1.OrgID, w.a1.InstallationID)
	get := func() map[string]any {
		t.Helper()
		return w.expect(w.do(http.MethodGet, path, ownerDiscord, nil), http.StatusOK, "faction security view").JSON(t)
	}
	v := get()
	if v["available"] != false || v["reason"] != "OFF" || v["recipients"] != "ALL" {
		t.Fatalf("off: %+v", v)
	}
	if f := v["faction"].(map[string]any); f["inFaction"] != true || f["factionName"] != "Bears" || f["linkedMembers"].(float64) != 1 || f["linkedLeaders"].(float64) != 0 {
		t.Fatalf("faction summary: %+v", f)
	}
	w.expect(w.do(http.MethodPut, path, ownerDiscord, map[string]any{"recipients": "LEADERS"}), http.StatusConflict, "can't choose while off")
	if _, err := repository.NewFactionSecurityRepository(pool).SetSettings(ctx, scope.InstallationID, guild, server, true, nil); err != nil {
		t.Fatal(err)
	}
	if v := get(); v["available"] != true || v["reason"] != nil {
		t.Fatalf("free while not on sale: %+v", v)
	}
	w.expect(w.do(http.MethodPut, path, ownerDiscord, map[string]any{"recipients": "SOME"}), http.StatusBadRequest, "bad choice")
	saved := w.expect(w.do(http.MethodPut, path, ownerDiscord, map[string]any{"recipients": "LEADERS"}), http.StatusOK, "choose leaders").JSON(t)
	if saved["recipients"] != "LEADERS" || get()["recipients"] != "LEADERS" {
		t.Fatalf("choice not saved: %+v", saved)
	}
	w.expect(w.do(http.MethodGet, path, w.players[2], nil), http.StatusConflict, "unlinked player")
	if _, err := repository.NewSecurityServiceRepository(pool).SetOffer(ctx, scope, repository.ServiceFactionSecurity, true, 100, 7, nil); err != nil {
		t.Fatal(err)
	}
	if v := get(); v["available"] != false || v["reason"] != "NOT_PAID" {
		t.Fatalf("on sale, unpaid: %+v", v)
	}
	if _, err := repository.NewEconomyRepository(pool).Credit(ctx, repository.LedgerParams{GuildID: guild, PlayerID: owner,
		Type: repository.TxAdminCredit, Amount: 100, ReferenceID: "seed-faction-security", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	market := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace", w.a1.OrgID, w.a1.InstallationID)
	w.expect(w.do(http.MethodPost, market+"/purchases", ownerDiscord, map[string]any{"serviceId": "FACTION_SECURITY", "idempotencyKey": "faction-key-1"}), http.StatusCreated, "purchase")
	if v := get(); v["available"] != true || v["activeUntil"] == nil {
		t.Fatalf("paid: %+v", v)
	}
}
