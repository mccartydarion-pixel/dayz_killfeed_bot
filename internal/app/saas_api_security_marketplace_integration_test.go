//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSecurityMarketplaceCatalogUsesVerifiedPlayerAndInstallationScope(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	w.linkPlayer(w.a1, player, "Marketplace Player")
	path := func(f installationFixture, suffix string) string {
		return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace%s", f.OrgID, f.InstallationID, suffix)
	}
	var before, after int
	if err := w.a.DB.Pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM point_transactions").Scan(&before); err != nil {
		t.Fatal(err)
	}
	response := w.expect(w.do(http.MethodGet, path(w.a1, "/catalog"), player, nil), http.StatusOK, "verified player catalog").JSON(t)
	if int64(response["installationId"].(float64)) != w.a1.InstallationID || response["gameServerId"] == nil {
		t.Fatalf("incorrect installation scope: %+v", response)
	}
	items := response["items"].([]any)
	if len(items) != 7 {
		t.Fatalf("unexpected security-service count: %d", len(items))
	}
	for _, entry := range items {
		item := entry.(map[string]any)
		if item["purchasable"] != false || item["status"] != "UNSUPPORTED" {
			t.Fatalf("unverified purchase exposed: %+v", item)
		}
	}
	w.expect(w.do(http.MethodGet, path(w.a1, "/admin/catalog"), w.admin, nil), http.StatusOK, "owner/admin catalog")
	w.expect(w.do(http.MethodGet, path(w.a1, "/admin/catalog"), w.member, nil), http.StatusForbidden, "member cannot administer")
	w.expect(w.do(http.MethodGet, path(w.b1, "/catalog"), player, nil), http.StatusConflict, "foreign guild has no verified player link")
	w.expect(w.do(http.MethodGet, path(w.a1, "/catalog"), w.players[1], nil), http.StatusConflict, "unlinked player")
	if err := w.a.DB.Pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM point_transactions").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("read-only catalog wrote to credit ledger: %d -> %d", before, after)
	}
}

func TestSecurityMarketplaceRaidAlarmPurchase(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	buyerDiscord := w.players[0]
	buyer := w.linkPlayer(w.a1, buyerDiscord, "Alarm Buyer")
	guild, server := w.gameContext(w.a1)
	scope := repository.SecurityScope{InstallationID: w.a1.InstallationID, GuildID: guild, ServerID: server}
	path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace", w.a1.OrgID, w.a1.InstallationID)
	buy := func(key string) map[string]any {
		return map[string]any{"serviceId": "BASE_RAID_ALARM", "idempotencyKey": key}
	}
	raidItem := func() map[string]any {
		t.Helper()
		items := w.expect(w.do(http.MethodGet, path+"/catalog", buyerDiscord, nil), http.StatusOK, "catalog").JSON(t)["items"].([]any)
		for _, e := range items {
			if m := e.(map[string]any); m["service"].(map[string]any)["id"] == "BASE_RAID_ALARM" {
				return m
			}
		}
		t.Fatal("no raid alarm in catalog")
		return nil
	}

	if item := raidItem(); item["purchasable"] != false {
		t.Fatalf("raid alarm purchasable before the owner sells it: %+v", item)
	}
	w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy("first-key-1")), http.StatusConflict, "not for sale yet")

	if _, err := repository.NewSecurityServiceRepository(w.a.DB.Pool).SetOffer(ctx, scope, repository.ServiceBaseRaidAlarm, true, 750, 14, nil); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy("first-key-1")), http.StatusConflict, "alarm switched off: never sold")
	if _, err := repository.NewBaseRaidAlarmRepository(w.a.DB.Pool).SetSettings(ctx, scope.InstallationID, guild, server, true, 600, nil); err != nil {
		t.Fatal(err)
	}
	if item := raidItem(); item["purchasable"] != true || item["pricePoints"].(float64) != 750 || item["durationDays"].(float64) != 14 || item["activeUntil"] != nil {
		t.Fatalf("raid alarm offer not shown: %+v", item)
	}
	w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy("first-key-1")), http.StatusConflict, "no points yet")
	if _, err := repository.NewEconomyRepository(w.a.DB.Pool).Credit(ctx, repository.LedgerParams{GuildID: guild, PlayerID: buyer,
		Type: repository.TxAdminCredit, Amount: 1000, ReferenceID: "seed-alarm-buyer", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	got := w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy("first-key-1")), http.StatusCreated, "purchase").JSON(t)
	if got["remainingBalance"].(float64) != 250 || got["duplicate"] != false {
		t.Fatalf("purchase: %+v", got)
	}
	again := w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy("first-key-1")), http.StatusOK, "replay").JSON(t)
	if again["duplicate"] != true || again["remainingBalance"].(float64) != 250 {
		t.Fatalf("replay charged again: %+v", again)
	}
	if item := raidItem(); item["activeUntil"] == nil {
		t.Fatalf("paid time not shown: %+v", item)
	}
	w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, map[string]any{"serviceId": "OFFLINE_PROTECTION", "idempotencyKey": "other-key-1"}), http.StatusBadRequest, "other services can't be bought")
	w.expect(w.do(http.MethodPost, path+"/purchases", w.players[1], buy("unlinked-1")), http.StatusConflict, "unlinked player")
	var tx int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='SECURITY_PURCHASE'`, guild, buyer).Scan(&tx); err != nil || tx != 1 {
		t.Fatalf("ledger rows: %d %v", tx, err)
	}

	// Perimeter Watch is sold the same way, and needs its own switch on.
	if _, err := repository.NewSecurityServiceRepository(w.a.DB.Pool).SetOffer(ctx, scope, repository.ServicePerimeterWatch, true, 100, 7, nil); err != nil {
		t.Fatal(err)
	}
	pw := map[string]any{"serviceId": "PERIMETER_MONITORING", "idempotencyKey": "perimeter-key-1"}
	w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, pw), http.StatusConflict, "perimeter switched off: never sold")
	if _, err := repository.NewPerimeterWatchRepository(w.a.DB.Pool).SetSettings(ctx, scope.InstallationID, guild, server, true, 100, 1800, nil); err != nil {
		t.Fatal(err)
	}
	bought := w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, pw), http.StatusCreated, "perimeter purchase").JSON(t)
	if bought["remainingBalance"].(float64) != 150 {
		t.Fatalf("perimeter purchase: %+v", bought)
	}
}
