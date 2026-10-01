//go:build integration

package app

import (
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestPerimeterWatchOwnerSettings(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "perimeter-admin")
	w.mapRole(admin, "perimeter-admin-role", "ADMINISTRATOR")
	path := w.path("/case/perimeter-watch")
	if got := w.call(w.a.handleGetPerimeterWatch, http.MethodGet, path, admin, nil, nil); got.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", got.Code)
	}
	type view struct {
		Settings repository.PerimeterWatchSettings `json:"settings"`
		Offer    repository.SecurityOffer          `json:"offer"`
	}
	got := decodeBody[view](t, w.call(w.a.handleGetPerimeterWatch, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if got.Settings.Enabled || got.Settings.MarginMeters != 100 || got.Offer.Enabled || got.Offer.ServiceID != repository.ServicePerimeterWatch {
		t.Fatalf("unsafe default: %+v", got)
	}
	for _, bad := range []map[string]any{{"enabled": true, "marginMeters": 5}, {"enabled": true, "cooldownSeconds": 60}, {"enabled": true, "radius": 9}} {
		if rr := w.call(w.a.handleSetPerimeterWatch, http.MethodPut, path, w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad %v accepted: %d", bad, rr.Code)
		}
	}
	on := w.call(w.a.handleSetPerimeterWatch, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true, "marginMeters": 150}, nil)
	if s := decodeBody[view](t, on).Settings; on.Code != http.StatusOK || !s.Enabled || s.MarginMeters != 150 || s.CooldownSeconds != 1800 {
		t.Fatalf("turn on: %d %+v", on.Code, s)
	}
	if rr := w.call(w.a.handleSetPerimeterWatchOffer, http.MethodPut, path+"/offer", admin, map[string]any{"enabled": true, "pricePoints": 300, "durationDays": 7}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin set offer: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetPerimeterWatchOffer, http.MethodPut, path+"/offer", w.f.OwnerDiscordID, map[string]any{"enabled": true, "pricePoints": 300, "durationDays": 7}, nil); rr.Code != http.StatusOK {
		t.Fatalf("set offer: %d %s", rr.Code, rr.Body.String())
	}
	got = decodeBody[view](t, w.call(w.a.handleGetPerimeterWatch, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if !got.Offer.Enabled || got.Offer.PricePoints != 300 {
		t.Fatalf("offer not shown: %+v", got.Offer)
	}
}
