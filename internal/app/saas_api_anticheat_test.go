package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCaseObservationContractIsEvidenceOnly(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 30, 0, 0, time.UTC)
	response := newCaseObservationResponse(42, now)
	if response.ServerID != 42 || response.Mode != "OBSERVATION_ONLY" || response.Status != "AWAITING_EVENTS" {
		t.Fatalf("unexpected response scope or state: %+v", response)
	}
	if response.DetectorsEnabled || response.Enforcement != "DISABLED" {
		t.Fatal("detectors or enforcement must not be marked active in observation-only phase")
	}
	if response.Telemetry.HitEvents24h != nil {
		t.Fatal("missing durable hit telemetry must be null, never zero")
	}
	if response.Telemetry.Coverage.ContinuousGPS || response.Telemetry.Coverage.Shots != "NOT_AVAILABLE" || response.Telemetry.Coverage.ControllerInputs != "NOT_AVAILABLE" {
		t.Fatal("unsupported console telemetry must not be advertised as available")
	}
	if response.RecentEvents == nil || response.Cases == nil || response.Alerts == nil || response.Watchlist == nil {
		t.Fatal("list fields must be explicit empty arrays")
	}
	if len(response.Cases) != 0 || len(response.Alerts) != 0 || len(response.Watchlist) != 0 {
		t.Fatal("no manufactured detection evidence")
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hitEvents24h":null`) || !strings.Contains(string(raw), `"cases":[]`) {
		t.Fatalf("contract must preserve null vs empty: %s", raw)
	}
}

func TestCaseISOStringOptional(t *testing.T) {
	if caseISO(nil) != nil {
		t.Fatal("missing time must remain missing")
	}
	at := time.Date(2026, 9, 23, 12, 30, 0, 0, time.FixedZone("test", 3600))
	got := caseISO(&at)
	if got == nil || *got != "2026-09-23T11:30:00Z" {
		t.Fatalf("expected UTC source timestamp, got %v", got)
	}
}
