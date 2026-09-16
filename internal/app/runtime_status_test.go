package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
)

func TestValidBearerToken(t *testing.T) {
	if validBearerToken("Bearer secret", "secret") != true {
		t.Fatal("expected matching bearer token to be valid")
	}
	if validBearerToken("Bearer wrong", "secret") {
		t.Fatal("expected mismatched token to be invalid")
	}
	if validBearerToken("secret", "secret") {
		t.Fatal("expected a header missing the Bearer prefix to be invalid")
	}
	if validBearerToken("Bearer secret", "") {
		t.Fatal("expected an empty configured secret to reject everything")
	}
	if validBearerToken("", "secret") {
		t.Fatal("expected an empty header to be invalid")
	}
}

func TestNullableTimeAndString(t *testing.T) {
	if nullableTime(time.Time{}) != nil {
		t.Fatal("expected zero time to be nil")
	}
	now := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	got := nullableTime(now)
	if got == nil || *got != "2026-09-16T08:00:00Z" {
		t.Fatalf("expected RFC3339 UTC timestamp, got %v", got)
	}
	if nullableString("") != nil {
		t.Fatal("expected empty string to be nil")
	}
	if s := nullableString("x"); s == nil || *s != "x" {
		t.Fatalf("expected pointer to x, got %v", s)
	}
}

func TestRuntimeStatusHandlerRejectsWrongMethod(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: "secret"}}
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/status", nil)
	rr := httptest.NewRecorder()

	a.runtimeStatusHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestRuntimeStatusHandlerRequiresConfiguredSecret(t *testing.T) {
	a := &App{Config: &config.Config{}}
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/status", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rr := httptest.NewRecorder()

	a.runtimeStatusHandler(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when WEBSITE_API_SECRET is unset, got %d", rr.Code)
	}
}

func TestRuntimeStatusHandlerRejectsWrongSecret(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: "correct-secret"}}
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/status", nil)
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rr := httptest.NewRecorder()

	a.runtimeStatusHandler(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a wrong bearer token, got %d", rr.Code)
	}
}

func TestRuntimeStatusHandlerRejectsUnknownGuild(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: "secret", DiscordGuildID: "111"}}
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/status?discord_guild_id=222", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()

	a.runtimeStatusHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a guild this process does not own, got %d", rr.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if payload["ok"] != false {
		t.Fatalf("expected ok=false, got %v", payload["ok"])
	}
}

func TestRuntimeStatusHandlerReturns500WithoutDatabase(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: "secret", DiscordGuildID: "111"}}
	req := httptest.NewRequest(http.MethodGet, "/api/runtime/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()

	a.runtimeStatusHandler(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the database/guild repository is unavailable, got %d", rr.Code)
	}
}
