package app

import (
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// This describes the protected, exact-installation observation pipeline.
// It does not report detector readiness, ADM completeness, or gameplay time.
type casePipelineHealth struct {
	State                string   `json:"state"`
	Reasons              []string `json:"reasons"`
	ConclusionsSuspended bool     `json:"conclusionsSuspended"`
}

func caseAssessPipelineHealth(s caseSourceIntegrity) casePipelineHealth {
	out := casePipelineHealth{State: "INSUFFICIENT_EVIDENCE", Reasons: []string{}, ConclusionsSuspended: true}
	add := func(reason string) { out.Reasons = append(out.Reasons, reason) }
	if !s.WorkerAvailable {
		out.State = "DEGRADED"
		add("WORKER_UNAVAILABLE")
		return out
	}
	switch s.SourceState {
	case killfeed.ADMWorkerStalled, killfeed.ADMTransportError, killfeed.ADMSourceLagging:
		out.State = "DEGRADED"
		add(s.SourceState)
		return out
	case killfeed.ADMHealthy, killfeed.ADMQuiet:
		// A healthy poll is only a transport observation, not evidence coverage.
	default:
		out.State = "DEGRADED"
		add("SOURCE_HEALTH_UNVERIFIED")
		return out
	}
	if s.GeneratedAt.IsZero() || s.LastPollAt == nil || s.LastPollAt.After(s.GeneratedAt) ||
		s.GeneratedAt.Sub(*s.LastPollAt) > 2*time.Minute {
		out.State = "DEGRADED"
		add("POLL_FRESHNESS_UNVERIFIED")
		return out
	}
	if !s.CollectorConfigured {
		add("COLLECTOR_NOT_CONFIGURED")
		return out
	}
	if s.SelectedSourceRef == nil || s.AcceptedSourceRef == nil || s.SelectedIsAccepted == nil ||
		!*s.SelectedIsAccepted || *s.SelectedSourceRef != *s.AcceptedSourceRef {
		add("SELECTED_SOURCE_UNVERIFIED")
		return out
	}
	switch s.EvidenceObservationStatus {
	case "RETAINED_EVENTS_OBSERVED":
		if s.LatestEvidenceSourceRef == nil || *s.LatestEvidenceSourceRef != *s.SelectedSourceRef ||
			s.LatestEvidenceIngestedAt == nil || s.LatestEvidenceOffset == nil || *s.LatestEvidenceOffset < 0 ||
			s.GeneratedAt.IsZero() || s.LatestEvidenceIngestedAt.After(s.GeneratedAt) ||
			(s.LastSourceChangeAt != nil && s.LatestEvidenceIngestedAt.Before(*s.LastSourceChangeAt)) {
			add("CURRENT_SOURCE_ADDRESS_UNVERIFIED")
			return out
		}
		out.State = "CURRENT_SOURCE_OBSERVATIONS"
		add("BOUNDED_NON_ATOMIC_READBACK")
	case "NO_RETAINED_EVENTS":
		add("NO_RETAINED_CURRENT_SOURCE_EVENTS")
	case "HISTORICAL_OR_OTHER_SOURCE_EVENTS":
		add("HISTORICAL_EVENTS_ONLY")
	default:
		add("CURRENT_SOURCE_OBSERVATION_UNVERIFIED")
	}
	return out
}
