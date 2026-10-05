package caseintel

import (
	"testing"
	"time"
)

func TestDetectorHealthSuspendsAcrossFailureModes(t *testing.T) {
	now := time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)
	healthy := DetectorHealthInput{ModuleID: "CASE-TELEPORT-001", Enabled: true, SourceSupported: true,
		ModuleValidated: true, RequiredTelemetryPresent: true, EvidenceComplete: true,
		PositionRequired: true, Now: now, LatestSourceAt: now.Add(-time.Minute),
		LatestPositionAt: now.Add(-time.Minute), LastSuccessfulEvaluationAt: now.Add(-time.Minute),
		SourceMaxAge: 2 * time.Minute, PositionMaxAge: 2 * time.Minute,
		PollingDelay: time.Second, PollingDelayLimit: 30 * time.Second}
	tests := []struct {
		name, want string
		change     func(*DetectorHealthInput)
	}{
		{"disabled", "DISABLED", func(x *DetectorHealthInput) { x.Enabled = false }},
		{"unsupported source", "UNSUPPORTED", func(x *DetectorHealthInput) { x.SourceSupported = false }},
		{"processing failure", "ERROR", func(x *DetectorHealthInput) { x.ProcessingError = true }},
		{"stale source", "DEGRADED", func(x *DetectorHealthInput) { x.LatestSourceAt = now.Add(-3 * time.Minute) }},
		{"future source", "DEGRADED", func(x *DetectorHealthInput) { x.LatestSourceAt = now.Add(time.Second) }},
		{"stale position", "DEGRADED", func(x *DetectorHealthInput) { x.LatestPositionAt = now.Add(-3 * time.Minute) }},
		{"behind poller", "DEGRADED", func(x *DetectorHealthInput) { x.PollingDelay = time.Minute }},
		{"duplicate", "DEGRADED", func(x *DetectorHealthInput) { x.DuplicateEvents = true }},
		{"telemetry missing", "INSUFFICIENT_EVIDENCE", func(x *DetectorHealthInput) { x.RequiredTelemetryPresent = false }},
		{"no validated detector", "INSUFFICIENT_EVIDENCE", func(x *DetectorHealthInput) { x.ModuleValidated = false }},
	}
	for _, tc := range tests {
		in := healthy
		tc.change(&in)
		got := AssessDetectorHealth(in)
		if got.State != tc.want || !got.ConclusionsSuspended || len(got.Reasons) == 0 {
			t.Fatalf("%s: %+v", tc.name, got)
		}
	}
	got := AssessDetectorHealth(healthy)
	if got.State != "INSUFFICIENT_EVIDENCE" || !got.ConclusionsSuspended || !containsReason(got.Reasons, "MODULE_NOT_RELEASED") {
		t.Fatalf("current catalog became active: %+v", got)
	}
	healthy.ModuleID = "CASE-OTHER"
	if got := AssessDetectorHealth(healthy); got.State != "UNSUPPORTED" || !got.ConclusionsSuspended {
		t.Fatalf("unknown: %+v", got)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
