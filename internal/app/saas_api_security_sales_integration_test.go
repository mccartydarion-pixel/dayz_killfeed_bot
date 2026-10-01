//go:build integration

package app

import (
	"net/http"
	"testing"
)

func TestSecuritySalesOwnerOnly(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "sales-admin")
	w.mapRole(admin, "sales-admin-role", "ADMINISTRATOR")
	path := w.path("/case/security-sales")
	if rr := w.call(w.a.handleGetSecuritySales, http.MethodGet, path, admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", rr.Code)
	}
	if rr := w.call(w.a.handleGetSecuritySales, http.MethodGet, path+"?days=12", w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad window accepted: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetSentinelProOffer, http.MethodPut, w.path("/case/sentinel-pro/offer"), w.f.OwnerDiscordID,
		map[string]any{"enabled": true, "pricePoints": 1500, "durationDays": 30}, nil); rr.Code != http.StatusOK {
		t.Fatalf("set offer: %d", rr.Code)
	}
	type view struct {
		Days     int `json:"days"`
		Services []struct {
			ServiceID   string `json:"serviceId"`
			Label       string `json:"label"`
			OnSale      bool   `json:"onSale"`
			SwitchedOn  bool   `json:"switchedOn"`
			PricePoints int64  `json:"pricePoints"`
			Sales       int    `json:"sales"`
		} `json:"services"`
		TotalSales int   `json:"totalSales"`
		Recent     []any `json:"recent"`
	}
	got := decodeBody[view](t, w.call(w.a.handleGetSecuritySales, http.MethodGet, path+"?days=7", w.f.OwnerDiscordID, nil, nil))
	if got.Days != 7 || len(got.Services) != 5 || got.TotalSales != 0 || got.Recent == nil {
		t.Fatalf("summary: %+v", got)
	}
	bundle := got.Services[4]
	if bundle.ServiceID != "SENTINEL_PRO" || bundle.Label != "Sentinel Pro" || !bundle.OnSale || bundle.SwitchedOn || bundle.PricePoints != 1500 {
		t.Fatalf("bundle row: %+v", bundle)
	}
	if got.Services[0].Label != "Base Raid Alarm" || got.Services[0].OnSale {
		t.Fatalf("raid row: %+v", got.Services[0])
	}
}
