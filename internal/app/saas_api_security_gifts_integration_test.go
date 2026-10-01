//go:build integration

package app

import (
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSecurityGiftOwnerOnly(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "gift-admin")
	w.mapRole(admin, "gift-admin-role", "ADMINISTRATOR")
	player := w.seedPlayer("Lucky")
	path := w.path("/case/security-gifts")
	body := map[string]any{"playerId": player, "serviceId": "BASE_RAID_ALARM", "days": 7, "note": "Event winner", "idempotencyKey": "gift-request-1"}
	if rr := w.call(w.a.handleGiveSecurityGift, http.MethodPost, path, admin, body, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin gift: %d", rr.Code)
	}
	for _, bad := range []map[string]any{
		{"playerId": player, "serviceId": "OFFLINE_PROTECTION", "days": 7, "idempotencyKey": "gift-request-2"},
		{"playerId": player, "serviceId": "BASE_RAID_ALARM", "days": 120, "idempotencyKey": "gift-request-2"},
		{"playerId": player + 999999, "serviceId": "BASE_RAID_ALARM", "days": 7, "idempotencyKey": "gift-request-2"},
		{"playerId": player, "serviceId": "BASE_RAID_ALARM", "days": 7, "idempotencyKey": "x"},
		{"playerId": player, "serviceId": "BASE_RAID_ALARM", "days": 7, "idempotencyKey": "gift-request-2", "price": 5},
	} {
		if rr := w.call(w.a.handleGiveSecurityGift, http.MethodPost, path, w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad gift %v: %d %s", bad, rr.Code, rr.Body.String())
		}
	}
	rr := w.call(w.a.handleGiveSecurityGift, http.MethodPost, path, w.f.OwnerDiscordID, body, nil)
	type res struct {
		Gift      repository.SecurityPurchase `json:"gift"`
		Duplicate bool                        `json:"duplicate"`
	}
	got := decodeBody[res](t, rr)
	if rr.Code != http.StatusCreated || !got.Gift.Gift || got.Gift.PricePoints != 0 || got.Gift.PlayerName != "Lucky" || got.Duplicate {
		t.Fatalf("gift: %d %+v", rr.Code, got)
	}
	again := w.call(w.a.handleGiveSecurityGift, http.MethodPost, path, w.f.OwnerDiscordID, body, nil)
	if again.Code != http.StatusOK || !decodeBody[res](t, again).Duplicate {
		t.Fatalf("replay: %d", again.Code)
	}
	list := decodeBody[struct {
		Gifts []repository.SecurityGift `json:"gifts"`
	}](t, w.call(w.a.handleListSecurityGifts, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if len(list.Gifts) != 1 || list.Gifts[0].Note != "Event winner" {
		t.Fatalf("list: %+v", list)
	}
	var audits int
	if err := w.a.DB.Pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='SECURITY_GIFT_GIVEN'`, w.f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audits (once, not on replay): %d %v", audits, err)
	}
	if _, err := w.a.DB.Pool.Exec(t.Context(), `UPDATE security_service_purchases SET expiry_notified_at=NOW() WHERE installation_id=$1`, w.f.InstallationID); err != nil {
		t.Fatal(err)
	}
}
