//go:build integration

package app

import (
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseStaffAlertSettingsOwnerOnlyAndOffByDefault(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "case-alerts-admin")
	w.mapRole(admin, "case-alerts-admin-role", "ADMINISTRATOR")
	path := w.path("/case/alerts/settings")
	type view struct {
		Settings          repository.CaseAlertSettings `json:"settings"`
		ReleasedDetectors []string                     `json:"releasedDetectors"`
		Recent            []repository.CaseAlertRecent `json:"recent"`
		Enforcement       string                       `json:"enforcement"`
	}

	if got := w.call(w.a.handleGetCaseStaffAlerts, http.MethodGet, path, admin, nil, nil); got.Code != http.StatusForbidden {
		t.Fatalf("admin read staff alerts: %d", got.Code)
	}
	got := w.call(w.a.handleGetCaseStaffAlerts, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", got.Code, got.Body.String())
	}
	initial := decodeBody[view](t, got)
	if initial.Settings.Enabled || len(initial.ReleasedDetectors) != 0 || len(initial.Recent) != 0 || initial.Enforcement != "DISABLED" {
		t.Fatalf("unsafe default: %+v", initial)
	}
	if denied := w.call(w.a.handleSetCaseStaffAlerts, http.MethodPut, path, admin, map[string]any{"enabled": true}, nil); denied.Code != http.StatusForbidden {
		t.Fatalf("admin turned staff alerts on: %d", denied.Code)
	}
	if bad := w.call(w.a.handleSetCaseStaffAlerts, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true, "detector": "CASE-LOGIN-001"}, nil); bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", bad.Code)
	}
	on := w.call(w.a.handleSetCaseStaffAlerts, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true}, nil)
	if s := decodeBody[view](t, on); on.Code != http.StatusOK || !s.Settings.Enabled || len(s.ReleasedDetectors) != 0 {
		t.Fatalf("turn on: %d %+v", on.Code, s)
	}
	off := w.call(w.a.handleSetCaseStaffAlerts, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": false}, nil)
	if s := decodeBody[view](t, off); off.Code != http.StatusOK || s.Settings.Enabled {
		t.Fatalf("turn off: %d %+v", off.Code, s)
	}
}
