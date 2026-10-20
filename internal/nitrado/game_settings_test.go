package nitrado

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParseGameSettingValue(t *testing.T) {
	for _, on := range []string{"1", "true", "TRUE", "on", "yes", " Yes "} {
		if !ParseGameSettingValue(on) {
			t.Errorf("%q should be on", on)
		}
	}
	for _, off := range []string{"", "0", "false", "off", "no", "2", "enabled"} {
		if ParseGameSettingValue(off) {
			t.Errorf("%q should be off", off)
		}
	}
}

func TestGameSettingsDiscoversKeysAcrossCategories(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/services/svc1/gameservers" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"gameserver":{"settings":{
			"general":{"wipe_on_next_restart":"0","priority":"Alpha","admin-password":"secret","expertMode":"false"},
			"config":{"hostname":"Champions","password":"secret","disableBaseDamage":"1","disableContainerDamage":"true","enablewhitelist":"on","disableRespawnDialog":false,"disableMultiAccountMitigation":1}
		}}}}`))
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL, "token", srv.Client()).GameSettings(context.Background(), "svc1")
	if err != nil {
		t.Fatalf("GameSettings: %v", err)
	}
	if len(got) != len(GameSettingCatalogue) {
		t.Fatalf("expected one item per catalogue entry, got %d", len(got))
	}
	byID := map[string]GameSetting{}
	for i, item := range got {
		if item.ID != GameSettingCatalogue[i].ID {
			t.Fatalf("item %d is %s, want catalogue order (%s)", i, item.ID, GameSettingCatalogue[i].ID)
		}
		byID[item.ID] = item
	}
	want := map[string]GameSetting{
		"wipe_on_next_restart":               {ID: "wipe_on_next_restart", Supported: true, Category: "general", Key: "wipe_on_next_restart", Enabled: false},
		"disable_base_damage":                {ID: "disable_base_damage", Supported: true, Category: "config", Key: "disableBaseDamage", Enabled: true},
		"disable_container_damage":           {ID: "disable_container_damage", Supported: true, Category: "config", Key: "disableContainerDamage", Enabled: true},
		"whitelist":                          {ID: "whitelist", Supported: true, Category: "config", Key: "enablewhitelist", Enabled: true}, // case-insensitive match keeps the service's spelling
		"disable_respawn_dialog":             {ID: "disable_respawn_dialog", Supported: true, Category: "config", Key: "disableRespawnDialog", Enabled: false},
		"disable_multi_account_mitigation":   {ID: "disable_multi_account_mitigation", Supported: true, Category: "config", Key: "disableMultiAccountMitigation", Enabled: true},
		"mouse_and_keyboard":                 {ID: "mouse_and_keyboard"},
		"disable_respawn_in_unconsciousness": {ID: "disable_respawn_in_unconsciousness"},
	}
	for id, w := range want {
		if g := byID[id]; !reflect.DeepEqual(g, w) {
			t.Errorf("%s: got %+v, want %+v", id, g, w)
		}
	}
}

func TestGameSettingsPrefersTheExactSpellingAndTheFirstCandidate(t *testing.T) {
	settings := map[string]map[string]json.RawMessage{
		"general": {"enablewhitelist": json.RawMessage(`"1"`)},
		"config":  {"enableWhitelist": json.RawMessage(`"0"`), "whitelist_enabled": json.RawMessage(`"1"`)},
		"misc":    {"enableMouseAndKeyboard": json.RawMessage(`"1"`), "mouseAndKeyboard": json.RawMessage(`"0"`)},
	}
	got := discoverGameSettings(settings)
	byID := map[string]GameSetting{}
	for _, item := range got {
		byID[item.ID] = item
	}
	// An exact match anywhere beats a case-insensitive match, and the first candidate key beats a later one.
	if w := byID["whitelist"]; w.Category != "config" || w.Key != "enableWhitelist" || w.Enabled {
		t.Fatalf("whitelist should resolve to config.enableWhitelist (off), got %+v", w)
	}
	if m := byID["mouse_and_keyboard"]; m.Category != "misc" || m.Key != "enableMouseAndKeyboard" || !m.Enabled {
		t.Fatalf("mouse & keyboard should resolve to misc.enableMouseAndKeyboard (on), got %+v", m)
	}
}

func TestGameSettingsReportsEverythingUnsupportedWithoutSettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gameserver":{"settings":{"general":{"expertMode":"false"}}}}}`))
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL, "token", srv.Client()).GameSettings(context.Background(), "svc1")
	if err != nil {
		t.Fatalf("GameSettings: %v", err)
	}
	for _, item := range got {
		if item.Supported || item.Enabled || item.Category != "" || item.Key != "" {
			t.Fatalf("%s should be unsupported and zero, got %+v", item.ID, item)
		}
	}
}

func TestSetGameSettingWritesOneAndZero(t *testing.T) {
	var sent []map[string]string
	var method, path, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		sent = append(sent, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "token", srv.Client())
	if err := client.SetGameSetting(context.Background(), "svc1", "config", "disableBaseDamage", true); err != nil {
		t.Fatalf("SetGameSetting(on): %v", err)
	}
	if err := client.SetGameSetting(context.Background(), "svc1", "general", "wipe_on_next_restart", false); err != nil {
		t.Fatalf("SetGameSetting(off): %v", err)
	}
	if method != http.MethodPost || path != "/services/svc1/gameservers/settings" || contentType != "application/json" {
		t.Fatalf("unexpected request: %s %s (%s)", method, path, contentType)
	}
	want := []map[string]string{
		{"category": "config", "key": "disableBaseDamage", "value": "1"},
		{"category": "general", "key": "wipe_on_next_restart", "value": "0"},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

func TestSetGameSettingRefusesBadInput(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	client := NewClient(srv.URL, "token", srv.Client())
	if err := client.SetGameSetting(context.Background(), "", "config", "disableBaseDamage", true); err == nil {
		t.Error("expected an error for an empty service ID")
	}
	if err := client.SetGameSetting(context.Background(), "svc1", "", "disableBaseDamage", true); err == nil {
		t.Error("expected an error for an empty category")
	}
	if err := client.SetGameSetting(context.Background(), "svc1", "general", "admin-password", true); err == nil {
		t.Error("expected an error for a key outside the catalogue")
	}
	if called {
		t.Fatal("a refused write must never reach Nitrado")
	}
}

func TestSetGameSettingSurfacesARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	if err := NewClient(srv.URL, "token", srv.Client()).SetGameSetting(context.Background(), "svc1", "config", "disableBaseDamage", true); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestGameSettingCatalogueIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range GameSettingCatalogue {
		if spec.ID == "" || spec.Label == "" || spec.Help == "" || len(spec.Keys) == 0 {
			t.Errorf("incomplete catalogue entry %+v", spec)
		}
		if seen[spec.ID] {
			t.Errorf("duplicate catalogue id %s", spec.ID)
		}
		seen[spec.ID] = true
		if got, ok := GameSettingSpecByID(spec.ID); !ok || got.ID != spec.ID {
			t.Errorf("GameSettingSpecByID(%s) should find the entry", spec.ID)
		}
	}
	if len(GameSettingCatalogue) != 8 || !GameSettingCatalogue[0].Destructive {
		t.Fatalf("expected 8 switches with the storage wipe first and destructive, got %d", len(GameSettingCatalogue))
	}
	if _, ok := GameSettingSpecByID("nope"); ok {
		t.Fatal("an unknown id must not resolve")
	}
}
