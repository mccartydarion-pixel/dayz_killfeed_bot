//go:build integration

package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// fakeNitradoGameSettings is a Nitrado whose gameserver read carries switches in two categories:
// the gameserver read returns them, the settings write replaces one. It never carries a mouse &
// keyboard key, so that switch is unsupported.
type fakeNitradoGameSettings struct {
	mu       sync.Mutex
	settings map[string]map[string]string
	writes   []map[string]string
}

func withFakeNitradoGameSettings(t *testing.T, a *App) *fakeNitradoGameSettings {
	t.Helper()
	fake := &fakeNitradoGameSettings{settings: map[string]map[string]string{
		"general": {"wipe_on_next_restart": "0", "priority": "Alpha", "admin-password": "secret", "expertMode": "false"},
		"config": {
			"hostname": "Champions", "password": "secret",
			"disableBaseDamage": "1", "disableContainerDamage": "1", "enableWhitelist": "0",
			"disableRespawnDialog": "0", "disableRespawnInUnconsciousness": "0", "disableMultiAccountMitigation": "1",
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/services":
			_, _ = w.Write([]byte(psServiceJSON))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gameservers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"gameserver": map[string]any{"settings": fake.settings}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gameservers/settings"):
			body, _ := io.ReadAll(r.Body)
			var sent map[string]string
			_ = json.Unmarshal(body, &sent)
			fake.writes = append(fake.writes, sent)
			if cat, ok := fake.settings[sent["category"]]; ok {
				cat[sent["key"]] = sent["value"]
			}
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a.saasNitradoClientFactory = func(token string) *nitrado.Client { return nitrado.NewClient(srv.URL, token, nil) }
	return fake
}

func gameSettingByID(t *testing.T, items []gameSettingDTO, id string) gameSettingDTO {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("no item %s in %+v", id, items)
	return gameSettingDTO{}
}

func TestGameSettingsListDiscoversKeysAcrossCategories(t *testing.T) {
	w := newClientAdminWorld(t)
	withFakeNitradoGameSettings(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	rr := w.call(w.a.handleListGameSettings, http.MethodGet, w.path("/game-settings"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list game settings failed: %d %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[gameSettingsDTO](t, rr)
	if len(body.Items) != len(nitrado.GameSettingCatalogue) {
		t.Fatalf("expected all %d switches, got %d", len(nitrado.GameSettingCatalogue), len(body.Items))
	}
	for i, spec := range nitrado.GameSettingCatalogue {
		if body.Items[i].ID != spec.ID || body.Items[i].Label != spec.Label || body.Items[i].Help != spec.Help || body.Items[i].AppliesOn != "restart" {
			t.Fatalf("item %d should be %s in catalogue order with its label and help, got %+v", i, spec.ID, body.Items[i])
		}
	}
	base := gameSettingByID(t, body.Items, "disable_base_damage")
	if !base.Supported || !base.Enabled || base.Category != "config" || base.Key != "disableBaseDamage" || base.Destructive {
		t.Fatalf("unexpected base damage item: %+v", base)
	}
	wipe := gameSettingByID(t, body.Items, "wipe_on_next_restart")
	if !wipe.Supported || wipe.Enabled || wipe.Category != "general" || wipe.Key != "wipe_on_next_restart" || !wipe.Destructive {
		t.Fatalf("unexpected wipe item: %+v", wipe)
	}
	whitelist := gameSettingByID(t, body.Items, "whitelist")
	if !whitelist.Supported || whitelist.Enabled || whitelist.Key != "enableWhitelist" {
		t.Fatalf("unexpected whitelist item: %+v", whitelist)
	}
	mk := gameSettingByID(t, body.Items, "mouse_and_keyboard")
	if mk.Supported || mk.Enabled || mk.Category != "" || mk.Key != "" {
		t.Fatalf("a switch the service lacks must be unsupported and empty, got %+v", mk)
	}
	if s := rr.Body.String(); strings.Contains(s, "secret") || strings.Contains(s, "password") {
		t.Fatalf("the response must never carry other settings: %s", s)
	}
}

func TestGameSettingsSetWritesTheDiscoveredKey(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoGameSettings(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	offRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/disable_base_damage"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: false, Reason: "raid weekend"}, map[string]string{"id": "disable_base_damage"})
	if offRR.Code != http.StatusOK {
		t.Fatalf("set game setting failed: %d %s", offRR.Code, offRR.Body.String())
	}
	got := decodeBody[gameSettingDTO](t, offRR)
	if got.ID != "disable_base_damage" || got.Enabled || !got.Supported || got.Category != "config" || got.Key != "disableBaseDamage" || got.Label != "Disable base damage" {
		t.Fatalf("unexpected item after the write: %+v", got)
	}
	if len(fake.writes) != 1 || fake.writes[0]["category"] != "config" || fake.writes[0]["key"] != "disableBaseDamage" || fake.writes[0]["value"] != "0" {
		t.Fatalf("unexpected Nitrado write: %v", fake.writes)
	}

	onRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/whitelist"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: true}, map[string]string{"id": "whitelist"})
	if onRR.Code != http.StatusOK {
		t.Fatalf("set whitelist failed: %d %s", onRR.Code, onRR.Body.String())
	}
	if got := decodeBody[gameSettingDTO](t, onRR); !got.Enabled {
		t.Fatalf("the answer must carry the refreshed value, got %+v", got)
	}
	if len(fake.writes) != 2 || fake.writes[1]["category"] != "config" || fake.writes[1]["key"] != "enableWhitelist" || fake.writes[1]["value"] != "1" {
		t.Fatalf("unexpected Nitrado write: %v", fake.writes)
	}

	auditRR := w.call(w.a.handleListAuditLog, http.MethodGet, w.path("/audit-log"), w.f.OwnerDiscordID, nil, nil)
	audit := auditRR.Body.String()
	if !strings.Contains(audit, "GAME_SETTING_SET") || !strings.Contains(audit, "disable_base_damage") || !strings.Contains(audit, "raid weekend") {
		t.Fatalf("the change must be audited with its target and reason, got %s", audit)
	}
}

func TestGameSettingsRejectsUnknownAndUnsupportedIDs(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoGameSettings(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	unknownRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/nope"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: true}, map[string]string{"id": "nope"})
	if unknownRR.Code != http.StatusNotFound {
		t.Fatalf("an unknown id must be 404, got %d %s", unknownRR.Code, unknownRR.Body.String())
	}
	unsupportedRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/mouse_and_keyboard"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: true}, map[string]string{"id": "mouse_and_keyboard"})
	if unsupportedRR.Code != http.StatusBadRequest || !strings.Contains(unsupportedRR.Body.String(), "Nitrado does not offer this setting on this server") {
		t.Fatalf("a switch the service lacks must be refused, got %d %s", unsupportedRR.Code, unsupportedRR.Body.String())
	}
	if len(fake.writes) != 0 {
		t.Fatalf("nothing may be written for a refused id: %v", fake.writes)
	}
}

func TestGameSettingsWipeNeedsOwnerAndTypedConfirmation(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoGameSettings(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	// Without the typed word the wipe is not enabled.
	for _, confirm := range []string{"", "wipe", "WIPE ME"} {
		rr := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/wipe_on_next_restart"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: true, Confirm: confirm}, map[string]string{"id": "wipe_on_next_restart"})
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "type WIPE to confirm") {
			t.Fatalf("confirm %q must be refused, got %d %s", confirm, rr.Code, rr.Body.String())
		}
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a refused wipe must never reach Nitrado: %v", fake.writes)
	}

	onRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/wipe_on_next_restart"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: true, Confirm: "WIPE", Reason: "season reset"}, map[string]string{"id": "wipe_on_next_restart"})
	if onRR.Code != http.StatusOK {
		t.Fatalf("the owner's confirmed wipe failed: %d %s", onRR.Code, onRR.Body.String())
	}
	if got := decodeBody[gameSettingDTO](t, onRR); !got.Enabled || !got.Destructive {
		t.Fatalf("unexpected wipe item: %+v", got)
	}
	if len(fake.writes) != 1 || fake.writes[0]["category"] != "general" || fake.writes[0]["key"] != "wipe_on_next_restart" || fake.writes[0]["value"] != "1" {
		t.Fatalf("unexpected Nitrado write: %v", fake.writes)
	}

	// Turning it off again needs no confirmation.
	offRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/wipe_on_next_restart"), w.f.OwnerDiscordID, setGameSettingRequest{Enabled: false}, map[string]string{"id": "wipe_on_next_restart"})
	if offRR.Code != http.StatusOK {
		t.Fatalf("turning the wipe off failed: %d %s", offRR.Code, offRR.Body.String())
	}
	if len(fake.writes) != 2 || fake.writes[1]["value"] != "0" {
		t.Fatalf("unexpected Nitrado write: %v", fake.writes)
	}

	auditRR := w.call(w.a.handleListAuditLog, http.MethodGet, w.path("/audit-log"), w.f.OwnerDiscordID, nil, nil)
	if audit := auditRR.Body.String(); !strings.Contains(audit, "GAME_SETTING_SET") || !strings.Contains(audit, "wipe_on_next_restart") || !strings.Contains(audit, "season reset") {
		t.Fatalf("the wipe must be audited, got %s", audit)
	}
}

func TestGameSettingsAdministratorCannotWipe(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoGameSettings(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	admin := syncUser(t, w.a, fmt.Sprintf("ca-gs-admin-%d", time.Now().UnixNano()), "Admin")
	w.mapRole(admin.DiscordUserID, "discord-role-game-settings-admin", "ADMINISTRATOR")

	// An Administrator manages every other switch...
	listRR := w.call(w.a.handleListGameSettings, http.MethodGet, w.path("/game-settings"), admin.DiscordUserID, nil, nil)
	if listRR.Code != http.StatusOK {
		t.Fatalf("an Administrator must list the switches, got %d %s", listRR.Code, listRR.Body.String())
	}
	setRR := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/disable_container_damage"), admin.DiscordUserID, setGameSettingRequest{Enabled: false}, map[string]string{"id": "disable_container_damage"})
	if setRR.Code != http.StatusOK {
		t.Fatalf("an Administrator must flip an ordinary switch, got %d %s", setRR.Code, setRR.Body.String())
	}
	// ...but never the storage wipe, in either direction, even with the typed word.
	for _, req := range []setGameSettingRequest{{Enabled: true, Confirm: "WIPE"}, {Enabled: false}} {
		rr := w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/wipe_on_next_restart"), admin.DiscordUserID, req, map[string]string{"id": "wipe_on_next_restart"})
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "SERVER_WIPE") {
			t.Fatalf("an Administrator must not wipe, got %d %s", rr.Code, rr.Body.String())
		}
	}
	if len(fake.writes) != 1 || fake.writes[0]["key"] != "disableContainerDamage" {
		t.Fatalf("only the container damage write may reach Nitrado: %v", fake.writes)
	}
}

func TestGameSettingsNeedAdministrator(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoGameSettings(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	mod := syncUser(t, w.a, fmt.Sprintf("ca-gs-mod-%d", time.Now().UnixNano()), "Mod")
	w.mapRole(mod.DiscordUserID, "discord-role-game-settings-moderator", "MODERATOR")

	for name, rr := range map[string]*httptest.ResponseRecorder{
		"list": w.call(w.a.handleListGameSettings, http.MethodGet, w.path("/game-settings"), mod.DiscordUserID, nil, nil),
		"set":  w.call(w.a.handleSetGameSetting, http.MethodPut, w.path("/game-settings/disable_base_damage"), mod.DiscordUserID, setGameSettingRequest{Enabled: false}, map[string]string{"id": "disable_base_damage"}),
	} {
		if rr.Code != http.StatusForbidden {
			t.Fatalf("a Moderator must not %s the game settings, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a forbidden request must never reach Nitrado: %v", fake.writes)
	}
}
