//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSentinelProOwnerView(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "sentinel-admin")
	w.mapRole(admin, "sentinel-admin-role", "ADMINISTRATOR")
	path := w.path("/case/sentinel-pro")
	if rr := w.call(w.a.handleGetSentinelPro, http.MethodGet, path, admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", rr.Code)
	}
	type view struct {
		Offer    repository.SecurityOffer `json:"offer"`
		Includes []struct {
			ID string `json:"id"`
			On bool   `json:"on"`
		} `json:"includes"`
	}
	got := decodeBody[view](t, w.call(w.a.handleGetSentinelPro, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if got.Offer.Enabled || len(got.Includes) != 4 || got.Includes[0].ID != "BASE_RAID_ALARM" || got.Includes[0].On {
		t.Fatalf("default: %+v", got)
	}
	if _, err := repository.NewPerimeterWatchRepository(w.a.DB.Pool).SetSettings(context.Background(), w.f.InstallationID, w.guildID, w.serverID, true, 100, 1800, nil); err != nil {
		t.Fatal(err)
	}
	if rr := w.call(w.a.handleSetSentinelProOffer, http.MethodPut, path+"/offer", w.f.OwnerDiscordID, map[string]any{"enabled": true, "pricePoints": 2000, "durationDays": 30}, nil); rr.Code != http.StatusOK {
		t.Fatalf("set offer: %d %s", rr.Code, rr.Body.String())
	}
	got = decodeBody[view](t, w.call(w.a.handleGetSentinelPro, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if !got.Offer.Enabled || got.Offer.PricePoints != 2000 || !got.Includes[1].On || got.Includes[0].On {
		t.Fatalf("after offer: %+v", got)
	}
}

func TestSentinelProPurchase(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	buyerDiscord := w.players[0]
	buyer := w.linkPlayer(w.a1, buyerDiscord, "Bundle Buyer")
	guild, server := w.gameContext(w.a1)
	scope := repository.SecurityScope{InstallationID: w.a1.InstallationID, GuildID: guild, ServerID: server}
	path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace", w.a1.OrgID, w.a1.InstallationID)
	item := func() map[string]any {
		t.Helper()
		for _, e := range w.expect(w.do(http.MethodGet, path+"/catalog", buyerDiscord, nil), http.StatusOK, "catalog").JSON(t)["items"].([]any) {
			if m := e.(map[string]any); m["service"].(map[string]any)["id"] == "SENTINEL_PRO" {
				return m
			}
		}
		t.Fatal("no Sentinel Pro in catalog")
		return nil
	}
	if _, err := repository.NewSecurityServiceRepository(w.a.DB.Pool).SetOffer(ctx, scope, repository.ServiceSentinelPro, true, 900, 30, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.NewEconomyRepository(w.a.DB.Pool).Credit(ctx, repository.LedgerParams{GuildID: guild, PlayerID: buyer,
		Type: repository.TxAdminCredit, Amount: 1000, ReferenceID: "seed-bundle", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	buy := map[string]any{"serviceId": "SENTINEL_PRO", "idempotencyKey": "bundle-key-1"}
	if it := item(); it["purchasable"] != false {
		t.Fatalf("bundle on sale with every service off: %+v", it)
	}
	w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy), http.StatusConflict, "every included service off")
	for _, on := range []func() error{
		func() error {
			_, err := repository.NewBaseRaidAlarmRepository(w.a.DB.Pool).SetSettings(ctx, scope.InstallationID, guild, server, true, 600, nil)
			return err
		},
		func() error {
			_, err := repository.NewBaseBlackBoxRepository(w.a.DB.Pool).SetSettings(ctx, scope.InstallationID, guild, server, true, 14, nil)
			return err
		},
	} {
		if err := on(); err != nil {
			t.Fatal(err)
		}
	}
	it := item()
	inc, _ := it["includes"].([]any)
	if it["purchasable"] != true || it["pricePoints"].(float64) != 900 || len(inc) != 2 || inc[0] != "BASE_RAID_ALARM" || inc[1] != "BASE_BLACK_BOX" {
		t.Fatalf("bundle offer: %+v", it)
	}
	if got := w.expect(w.do(http.MethodPost, path+"/purchases", buyerDiscord, buy), http.StatusCreated, "bundle purchase").JSON(t); got["remainingBalance"].(float64) != 100 {
		t.Fatalf("purchase: %+v", got)
	}
	// The bundle's paid time shows on every covered service.
	for _, e := range w.expect(w.do(http.MethodGet, path+"/catalog", buyerDiscord, nil), http.StatusOK, "catalog").JSON(t)["items"].([]any) {
		m := e.(map[string]any)
		switch m["service"].(map[string]any)["id"] {
		case "BASE_RAID_ALARM", "BASE_BLACK_BOX", "SENTINEL_PRO":
			if m["activeUntil"] == nil {
				t.Fatalf("no paid time shown for %+v", m)
			}
		}
	}
}
