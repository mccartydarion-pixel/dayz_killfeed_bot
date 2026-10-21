package nitrado

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestClientBuildsAuthorizationHeaders(t *testing.T) {
	client := NewClient("https://api.nitrado.net", "test-token", nil)
	if client == nil {
		t.Fatal("expected client instance")
	}
	if client.token != "test-token" {
		t.Fatal("expected token to be preserved")
	}
}

func TestBaseURLDefaultsToDocumentedAPI(t *testing.T) {
	client := NewClient("", "token", nil)
	if client.BaseURL() != "https://api.nitrado.net" {
		t.Fatalf("expected base URL https://api.nitrado.net, got %q", client.BaseURL())
	}
}

func newStatusServer(t *testing.T, status int, wantPath string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantPath != "" && r.URL.Path != wantPath {
			t.Errorf("unexpected request path: got %q want %q", r.URL.Path, wantPath)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("expected Bearer Authorization header on the wire")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"error","message":"nitrado error"}`))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "secret-test-token", srv.Client())
}

func TestAuthenticationCheckUsesDocumentedPath(t *testing.T) {
	client := newStatusServer(t, http.StatusOK, "/services")
	if err := client.AuthenticationCheck(context.Background()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestAuthenticationCheckClassifiesFailures(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantKind ErrorKind
	}{
		{"unauthorized is authentication", http.StatusUnauthorized, KindAuthentication},
		{"forbidden is permission", http.StatusForbidden, KindPermission},
		{"not found is invalid endpoint", http.StatusNotFound, KindInvalidEndpoint},
		{"rate limited is temporary", http.StatusTooManyRequests, KindTemporary},
		{"maintenance is temporary", http.StatusServiceUnavailable, KindTemporary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newStatusServer(t, tc.status, "/services")
			err := client.AuthenticationCheck(context.Background())
			var reqErr *RequestError
			if !errors.As(err, &reqErr) {
				t.Fatalf("expected RequestError, got %T (%v)", err, err)
			}
			if reqErr.Kind != tc.wantKind {
				t.Fatalf("kind=%s, want %s", reqErr.Kind, tc.wantKind)
			}
			if reqErr.StatusCode != tc.status {
				t.Fatalf("status=%d, want %d", reqErr.StatusCode, tc.status)
			}
		})
	}
}

func TestServiceLookup404IsNotAuthentication(t *testing.T) {
	client := newStatusServer(t, http.StatusNotFound, "/services/12345")
	_, err := client.servicePayload(context.Background(), "12345")
	var reqErr *RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if reqErr.Kind != KindNotFound {
		t.Fatalf("service 404 must be kind=not_found, got %s", reqErr.Kind)
	}
	if reqErr.Kind == KindAuthentication {
		t.Fatal("service lookup 404 must never be classified as authentication")
	}
}

func TestRequestLoggingNeverContainsToken(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	// Request logs are DEBUG level; capture them explicitly.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client := newStatusServer(t, http.StatusOK, "/services")
	if err := client.AuthenticationCheck(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "method=GET") || !strings.Contains(out, "path=/services") {
		t.Fatalf("expected method and path in request log, got: %q", out)
	}
	if strings.Contains(out, "secret-test-token") || strings.Contains(out, "Authorization") {
		t.Fatalf("request log leaked credentials: %q", out)
	}
}

// TestClientHasNoFTPTransportFields is section 9F of the noftp API audit: a
// guard against ever silently reintroducing an FTP client/protocol
// dependency into ADM discovery or download. Client's only transport is
// httpClient (*http.Client, used by every discovery/download call via do/
// readDirectURL - see logs.go), so no field name should ever suggest an
// FTP/SFTP connection.
func TestClientHasNoFTPTransportFields(t *testing.T) {
	typ := reflect.TypeOf(Client{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "ftp") {
			t.Fatalf("unexpected FTP-shaped field %q on Client - all ADM discovery/download must go through the Nitrado REST API only", typ.Field(i).Name)
		}
	}
	if reflect.TypeOf((*http.Client)(nil)) != reflect.TypeOf(Client{}.httpClient) {
		t.Fatal("expected Client's transport to be *http.Client")
	}
}

func TestAPIBaseURLOverrideAppliesOnlyToDefaultClients(t *testing.T) {
	t.Cleanup(func() { SetAPIBaseURLOverride("") })
	if got := NewClient(DefaultBaseURL, "t", nil).BaseURL(); got != DefaultBaseURL {
		t.Fatalf("no override: %s", got)
	}
	SetAPIBaseURLOverride("http://fixture:8080/")
	if got := NewClient(DefaultBaseURL, "t", nil).BaseURL(); got != "http://fixture:8080" {
		t.Fatalf("override not applied: %s", got)
	}
	if got := NewClient("", "t", nil).BaseURL(); got != "http://fixture:8080" {
		t.Fatalf("override not applied to empty base: %s", got)
	}
	if got := NewClient("http://explicit", "t", nil).BaseURL(); got != "http://explicit" {
		t.Fatalf("explicit base must win: %s", got)
	}
	SetAPIBaseURLOverride("")
	if got := NewClient(DefaultBaseURL, "t", nil).BaseURL(); got != DefaultBaseURL {
		t.Fatalf("reset: %s", got)
	}
}
