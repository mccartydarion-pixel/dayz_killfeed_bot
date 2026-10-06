package nitrado

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRestartPostsToDocumentedEndpointWithMessage(t *testing.T) {
	var sawPath, sawMethod, sawMessage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawMethod = r.Method
		sawMessage = r.URL.Query().Get("message")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	if err := client.Restart(context.Background(), "12345", "scheduled maintenance"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if sawMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", sawMethod)
	}
	if sawPath != "/services/12345/gameservers/restart" {
		t.Errorf("unexpected path: %s", sawPath)
	}
	if sawMessage != "scheduled maintenance" {
		t.Errorf("expected message param to be forwarded, got %q", sawMessage)
	}
}

func TestStopPostsToDocumentedEndpoint(t *testing.T) {
	var sawPath, sawMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	if err := client.Stop(context.Background(), "12345", ""); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sawMethod != http.MethodPost || sawPath != "/services/12345/gameservers/stop" {
		t.Errorf("unexpected request: method=%s path=%s", sawMethod, sawPath)
	}
}

func TestRestartNonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "token", srv.Client())
	err := client.Restart(context.Background(), "12345", "")
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestRestartRequiresServiceID(t *testing.T) {
	client := NewClient("http://unused.invalid", "token", nil)
	if err := client.Restart(context.Background(), "", ""); err == nil {
		t.Fatal("expected an error for an empty service ID")
	}
}

// accessListServer is a Nitrado stand-in holding settings.general: it serves the gameserver
// details and applies settings writes, recording each written key and value.
func accessListServer(t *testing.T, general map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/services/svc1/gameservers":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"gameserver": map[string]any{"settings": map[string]any{"general": general}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/services/svc1/gameservers/settings":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["category"] != "general" {
				t.Errorf("category: %q", body["category"])
			}
			general[body["key"]] = body["value"]
			writes = append(writes, body["key"]+"="+body["value"])
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

func TestBanlistAddAndRemoveRewriteTheBansSetting(t *testing.T) {
	general := map[string]any{"bans": "OldCheater\r\nAnother One", "whitelist": "", "admin_password": "secret"}
	srv, writes := accessListServer(t, general)
	client := NewClient(srv.URL, "token", srv.Client())
	ctx := context.Background()

	if err := client.BanlistAdd(ctx, "svc1", " Ch66ats "); err != nil {
		t.Fatalf("BanlistAdd: %v", err)
	}
	if got := general["bans"]; got != "OldCheater\r\nAnother One\r\nCh66ats" {
		t.Fatalf("bans after add: %q", got)
	}
	// Already banned (any letter case): nothing is written again.
	if err := client.BanlistAdd(ctx, "svc1", "ch66ATS"); err != nil {
		t.Fatalf("BanlistAdd again: %v", err)
	}
	if err := client.BanlistRemove(ctx, "svc1", "oldcheater"); err != nil {
		t.Fatalf("BanlistRemove: %v", err)
	}
	if got := general["bans"]; got != "Another One\r\nCh66ats" {
		t.Fatalf("bans after remove: %q", got)
	}
	// Not on the list: nothing is written.
	if err := client.BanlistRemove(ctx, "svc1", "Nobody"); err != nil {
		t.Fatalf("BanlistRemove of an absent name: %v", err)
	}
	if len(*writes) != 2 {
		t.Fatalf("expected 2 writes, got %v", *writes)
	}
	if general["whitelist"] != "" || general["admin_password"] != "secret" {
		t.Fatalf("other settings were touched: %v", general)
	}
}

func TestWhitelistAddAndRemoveRewriteTheWhitelistSetting(t *testing.T) {
	general := map[string]any{"bans": "X", "whitelist": ""}
	srv, writes := accessListServer(t, general)
	client := NewClient(srv.URL, "token", srv.Client())
	if err := client.WhitelistAdd(context.Background(), "svc1", "Friend One"); err != nil {
		t.Fatalf("WhitelistAdd: %v", err)
	}
	if err := client.WhitelistRemove(context.Background(), "svc1", "Friend One"); err != nil {
		t.Fatalf("WhitelistRemove: %v", err)
	}
	if want := []string{"whitelist=Friend One", "whitelist="}; len(*writes) != 2 || (*writes)[0] != want[0] || (*writes)[1] != want[1] {
		t.Fatalf("writes: %v", *writes)
	}
	if general["bans"] != "X" {
		t.Fatalf("bans touched: %v", general["bans"])
	}
}

func TestAccessListIsNotWrittenWhenTheSettingIsMissing(t *testing.T) {
	srv, writes := accessListServer(t, map[string]any{"whitelist": ""})
	client := NewClient(srv.URL, "token", srv.Client())
	if err := client.BanlistAdd(context.Background(), "svc1", "Ch66ats"); !errors.Is(err, ErrAccessListUnsupported) {
		t.Fatalf("expected ErrAccessListUnsupported, got %v", err)
	}
	if len(*writes) != 0 {
		t.Fatalf("nothing may be written: %v", *writes)
	}
	if err := client.BanlistAdd(context.Background(), "svc1", "two\nlines"); err == nil {
		t.Fatal("a name with a line break must be refused")
	}
}

func TestAccessListActionRequiresIdentifier(t *testing.T) {
	client := NewClient("http://unused.invalid", "token", nil)
	if err := client.WhitelistAdd(context.Background(), "svc1", ""); err == nil {
		t.Fatal("expected an error for an empty identifier")
	}
}

func TestDeleteFileSendsDeleteWithThePath(t *testing.T) {
	var sawPath, sawMethod, sawFile string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawMethod, sawFile = r.URL.Path, r.Method, r.URL.Query().Get("path")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "token", srv.Client())
	const file = "/games/ni1_1/noftp/dayzps/mpmissions/dayzOffline.chernarusplus/storage_1/players.db"
	if err := client.DeleteFile(context.Background(), "12345", file); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if sawMethod != http.MethodDelete || sawPath != "/services/12345/gameservers/file_server/delete" || sawFile != file {
		t.Fatalf("unexpected request: %s %s path=%q", sawMethod, sawPath, sawFile)
	}
	status = http.StatusForbidden
	if err := client.DeleteFile(context.Background(), "12345", file); err == nil {
		t.Fatal("a refused delete must be an error")
	}
	if client.DeleteFile(context.Background(), "", file) == nil || client.DeleteFile(context.Background(), "12345", "") == nil {
		t.Fatal("a service and a path are required")
	}
}
