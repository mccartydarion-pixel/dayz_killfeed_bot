package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestWhitelistAddAndRemoveUseDocumentedEndpoint(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "token", srv.Client())

	if err := client.WhitelistAdd(context.Background(), "svc1", "player-identifier-1"); err != nil {
		t.Fatalf("WhitelistAdd: %v", err)
	}
	if err := client.WhitelistRemove(context.Background(), "svc1", "player-identifier-1"); err != nil {
		t.Fatalf("WhitelistRemove: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "POST /services/svc1/gameservers/games/whitelist?identifier=player-identifier-1") {
		t.Errorf("unexpected add call: %s", calls[0])
	}
	if !strings.HasPrefix(calls[1], "DELETE /services/svc1/gameservers/games/whitelist?identifier=player-identifier-1") {
		t.Errorf("unexpected remove call: %s", calls[1])
	}
}

func TestBanlistAddAndRemoveUseDocumentedEndpoint(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "token", srv.Client())

	if err := client.BanlistAdd(context.Background(), "svc1", "player-identifier-2"); err != nil {
		t.Fatalf("BanlistAdd: %v", err)
	}
	if err := client.BanlistRemove(context.Background(), "svc1", "player-identifier-2"); err != nil {
		t.Fatalf("BanlistRemove: %v", err)
	}
	if calls[0] != "POST /services/svc1/gameservers/games/banlist" || calls[1] != "DELETE /services/svc1/gameservers/games/banlist" {
		t.Errorf("unexpected calls: %v", calls)
	}
}

func TestAccessListActionRequiresIdentifier(t *testing.T) {
	client := NewClient("http://unused.invalid", "token", nil)
	if err := client.WhitelistAdd(context.Background(), "svc1", ""); err == nil {
		t.Fatal("expected an error for an empty identifier")
	}
}
