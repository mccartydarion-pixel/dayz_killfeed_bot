//go:build integration

package app

import (
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBaseRaidAlarmSettingsOwnerOnlyAndOffByDefault(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "raid-alarm-admin")
	w.mapRole(admin, "raid-alarm-admin-role", "ADMINISTRATOR")
	path := w.path("/case/raid-alarm")
	type view struct {
		Settings repository.BaseRaidAlarmSettings `json:"settings"`
		Recent   []repository.BaseRaidAlert       `json:"recent"`
	}

	if got := w.call(w.a.handleGetBaseRaidAlarm, http.MethodGet, path, admin, nil, nil); got.Code != http.StatusForbidden {
		t.Fatalf("admin read raid alarm: %d", got.Code)
	}
	got := w.call(w.a.handleGetBaseRaidAlarm, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", got.Code, got.Body.String())
	}
	initial := decodeBody[view](t, got)
	if initial.Settings.Enabled || initial.Settings.CooldownSeconds != repository.BaseRaidDefaultCooldownSeconds || len(initial.Recent) != 0 {
		t.Fatalf("unsafe default: %+v", initial)
	}

	if denied := w.call(w.a.handleSetBaseRaidAlarm, http.MethodPut, path, admin, map[string]any{"enabled": true}, nil); denied.Code != http.StatusForbidden {
		t.Fatalf("admin turned alarm on: %d", denied.Code)
	}
	for _, bad := range []map[string]any{{"enabled": true, "cooldownSeconds": 10}, {"enabled": true, "price": 100}} {
		if res := w.call(w.a.handleSetBaseRaidAlarm, http.MethodPut, path, w.f.OwnerDiscordID, bad, nil); res.Code != http.StatusBadRequest {
			t.Fatalf("bad body %v accepted: %d", bad, res.Code)
		}
	}
	saved := w.call(w.a.handleSetBaseRaidAlarm, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true, "cooldownSeconds": 900}, nil)
	if saved.Code != http.StatusOK {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	if s := decodeBody[view](t, saved).Settings; !s.Enabled || s.CooldownSeconds != 900 {
		t.Fatalf("not saved: %+v", s)
	}
	off := w.call(w.a.handleSetBaseRaidAlarm, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": false}, nil)
	if s := decodeBody[view](t, off).Settings; off.Code != http.StatusOK || s.Enabled || s.CooldownSeconds != 900 {
		t.Fatalf("turn off: %d %+v", off.Code, s)
	}
}
