package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestCasePipelineHealthDoesNotPromoteHistoricalOrUnscopedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	current, old := "current", "old"
	yes := true
	offset := int64(7)
	base := caseSourceIntegrity{ServerID: 42, GeneratedAt: now, WorkerAvailable: true,
		SourceState: killfeed.ADMHealthy, CollectorConfigured: true, LastPollAt: &now,
		SelectedSourceRef: &current, AcceptedSourceRef: &current, SelectedIsAccepted: &yes,
		LatestEvidenceSourceRef: &current, LatestEvidenceIngestedAt: &now, LatestEvidenceOffset: &offset,
		EvidenceObservationStatus: "RETAINED_EVENTS_OBSERVED"}
	tests := []struct {
		name, want, reason string
		change             func(*caseSourceIntegrity)
	}{
		{"current readback", "CURRENT_SOURCE_OBSERVATIONS", "BOUNDED_NON_ATOMIC_READBACK", func(*caseSourceIntegrity) {}},
		{"worker absent", "DEGRADED", "WORKER_UNAVAILABLE", func(s *caseSourceIntegrity) { s.WorkerAvailable = false }},
		{"stalled poll", "DEGRADED", killfeed.ADMWorkerStalled, func(s *caseSourceIntegrity) { s.SourceState = killfeed.ADMWorkerStalled }},
		{"lagging boot", "DEGRADED", killfeed.ADMSourceLagging, func(s *caseSourceIntegrity) { s.SourceState = killfeed.ADMSourceLagging }},
		{"stale poll despite healthy label", "DEGRADED", "POLL_FRESHNESS_UNVERIFIED", func(s *caseSourceIntegrity) { v := now.Add(-3 * time.Minute); s.LastPollAt = &v }},
		{"future poll", "DEGRADED", "POLL_FRESHNESS_UNVERIFIED", func(s *caseSourceIntegrity) { v := now.Add(time.Minute); s.LastPollAt = &v }},
		{"collector off", "INSUFFICIENT_EVIDENCE", "COLLECTOR_NOT_CONFIGURED", func(s *caseSourceIntegrity) { s.CollectorConfigured = false }},
		{"unaccepted source", "INSUFFICIENT_EVIDENCE", "SELECTED_SOURCE_UNVERIFIED", func(s *caseSourceIntegrity) { s.SelectedIsAccepted = nil }},
		{"historical event", "INSUFFICIENT_EVIDENCE", "HISTORICAL_EVENTS_ONLY", func(s *caseSourceIntegrity) {
			s.LatestEvidenceSourceRef = &old
			s.EvidenceObservationStatus = "HISTORICAL_OR_OTHER_SOURCE_EVENTS"
		}},
		{"mismatched status", "INSUFFICIENT_EVIDENCE", "CURRENT_SOURCE_ADDRESS_UNVERIFIED", func(s *caseSourceIntegrity) { s.LatestEvidenceSourceRef = &old }},
		{"quiet no events", "INSUFFICIENT_EVIDENCE", "NO_RETAINED_CURRENT_SOURCE_EVENTS", func(s *caseSourceIntegrity) {
			s.SourceState = killfeed.ADMQuiet
			s.EvidenceObservationStatus = "NO_RETAINED_EVENTS"
			s.LatestEvidenceIngestedAt = nil
		}},
		{"predates source change", "INSUFFICIENT_EVIDENCE", "CURRENT_SOURCE_ADDRESS_UNVERIFIED", func(s *caseSourceIntegrity) { changed := now.Add(time.Minute); s.LastSourceChangeAt = &changed }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base
			tt.change(&s)
			got := caseAssessPipelineHealth(s)
			if got.State != tt.want || !got.ConclusionsSuspended || len(got.Reasons) != 1 || got.Reasons[0] != tt.reason {
				t.Fatalf("unexpected pipeline health: %+v", got)
			}
		})
	}
}
