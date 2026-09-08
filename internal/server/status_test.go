package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusHandlerReturnsSanitizedState(t *testing.T) {
	state := NewState()
	state.SetNitrado(true, true, "DayZ", "gameserver", "active")
	state.SetDiscord(true, "killfeed-bot", true, true, nil)
	state.SetLogSource("DayZServer_x64.ADM", "/logs/DayZServer_x64.ADM", 2048, time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	state.SetPollStats(time.Now(), time.Now(), 2*time.Second, 1120, 14)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	rr := httptest.NewRecorder()

	state.StatusHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rr.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("status response is not valid JSON: %v", err)
	}

	if payload["status"] != "online" {
		t.Fatalf("expected status=online, got %v", payload["status"])
	}
	if payload["service_verified"] != true {
		t.Fatalf("expected service_verified=true, got %v", payload["service_verified"])
	}
	if payload["log_source_found"] != true {
		t.Fatalf("expected log_source_found=true, got %v", payload["log_source_found"])
	}
	if payload["poll_interval"] != "2s" {
		t.Fatalf("expected poll_interval=2s, got %v", payload["poll_interval"])
	}

	for key := range payload {
		if key == "token" || key == "authorization" || key == "password" {
			t.Fatalf("status payload must not expose secret field %q", key)
		}
	}
}

func TestStatusHandlerRejectsNonGet(t *testing.T) {
	state := NewState()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/status", nil)
	rr := httptest.NewRecorder()

	state.StatusHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}
