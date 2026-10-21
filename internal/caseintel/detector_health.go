package caseintel

import "time"

// DetectorHealthInput is an offline view over one exact-installation module.
// A future runtime caller must derive these fields from protected source and
// scoped configuration reads; caller-provided flags alone are not proof.
type DetectorHealthInput struct {
	ModuleID                   string
	Enabled                    bool
	SourceSupported            bool
	ModuleValidated            bool
	RequiredTelemetryPresent   bool
	EvidenceComplete           bool
	ProcessingError            bool
	DuplicateEvents            bool
	PositionRequired           bool
	Now                        time.Time
	LatestSourceAt             time.Time
	LatestPositionAt           time.Time
	LastSuccessfulEvaluationAt time.Time
	SourceMaxAge               time.Duration
	PositionMaxAge             time.Duration
	PollingDelay               time.Duration
	PollingDelayLimit          time.Duration
}

type DetectorHealth struct {
	ModuleID                   string    `json:"moduleId"`
	State                      string    `json:"state"`
	Reasons                    []string  `json:"reasons"`
	LastSuccessfulEvaluationAt time.Time `json:"lastSuccessfulEvaluationAt,omitempty"`
	ConclusionsSuspended       bool      `json:"conclusionsSuspended"`
}

func clientModuleMode(id string) string {
	for _, d := range ClientCatalog() {
		if d.ID == id {
			return d.Mode
		}
	}
	return ""
}

// AssessDetectorHealth is read-only and cannot run a detector or emit an alert.
// Unknown modules and missing clock/freshness limits fail closed. A future
// runtime must also verify eligibility against the real detector registry.
func AssessDetectorHealth(in DetectorHealthInput) DetectorHealth {
	out := DetectorHealth{ModuleID: in.ModuleID, State: "INSUFFICIENT_EVIDENCE", Reasons: []string{}, LastSuccessfulEvaluationAt: in.LastSuccessfulEvaluationAt, ConclusionsSuspended: true}
	add := func(s string) { out.Reasons = append(out.Reasons, s) }
	mode := clientModuleMode(in.ModuleID)
	if mode == "" {
		out.State = "UNSUPPORTED"
		add("UNKNOWN_MODULE")
		return out
	}
	if !in.Enabled {
		out.State = "DISABLED"
		add("OWNER_DISABLED")
		return out
	}
	if !in.SourceSupported {
		out.State = "UNSUPPORTED"
		add("SOURCE_UNSUPPORTED")
		return out
	}
	if in.ProcessingError {
		out.State = "ERROR"
		add("PROCESSING_ERROR")
		return out
	}
	if in.Now.IsZero() || in.SourceMaxAge <= 0 || in.PollingDelayLimit <= 0 ||
		(in.PositionRequired && in.PositionMaxAge <= 0) {
		out.State = "DEGRADED"
		add("HEALTH_CLOCK_UNVERIFIED")
		return out
	}
	stale := func(t time.Time, max time.Duration) bool { return t.IsZero() || t.After(in.Now) || in.Now.Sub(t) > max }
	if stale(in.LatestSourceAt, in.SourceMaxAge) {
		add("SOURCE_STALE")
	}
	if in.PositionRequired && stale(in.LatestPositionAt, in.PositionMaxAge) {
		add("POSITION_STALE")
	}
	if in.PollingDelay < 0 || in.PollingDelay > in.PollingDelayLimit {
		add("POLLING_BEHIND")
	}
	if in.DuplicateEvents {
		add("DUPLICATE_EVENTS")
	}
	if len(out.Reasons) > 0 {
		out.State = "DEGRADED"
		return out
	}
	if !in.RequiredTelemetryPresent {
		add("REQUIRED_TELEMETRY_MISSING")
	}
	if !in.ModuleValidated {
		add("MODULE_NOT_VALIDATED")
	}
	if mode != "VALIDATED_SHADOW" {
		add("MODULE_NOT_RELEASED")
	}
	if !in.EvidenceComplete {
		add("EVIDENCE_INCOMPLETE")
	}
	if in.LastSuccessfulEvaluationAt.IsZero() || in.LastSuccessfulEvaluationAt.After(in.Now) {
		add("NO_VERIFIED_EVALUATION")
	}
	if len(out.Reasons) > 0 {
		return out
	}
	out.State = "ACTIVE"
	out.ConclusionsSuspended = false
	return out
}
