package caseintel

import "testing"

func TestInvestigationModesCannotOverrideMissingTelemetryOrValidation(t *testing.T) {
	in := InvestigationInput{ModuleID: "CASE-TELEPORT-001", Mode: SensitivityStrict,
		Thresholds:    &ValidatedThresholds{Approved: true, MinimumEvidence: 1, Strict: 1, Balanced: 2, Relaxed: 3},
		SourceCurrent: true, PollingCaughtUp: true, RequiredTelemetryPresent: true,
		ModuleValidated: true, EvidenceProvenanceVerified: true, ExclusionsChecked: true,
		DuplicateFree: true, IndependentObservations: 10}
	for _, mode := range []Sensitivity{SensitivityRelaxed, SensitivityBalanced, SensitivityStrict} {
		in.Mode = mode
		for _, tc := range []struct {
			name   string
			change func(*InvestigationInput)
		}{
			{"stale", func(x *InvestigationInput) { x.SourceCurrent = false }},
			{"behind", func(x *InvestigationInput) { x.PollingCaughtUp = false }},
			{"missing position", func(x *InvestigationInput) { x.RequiredTelemetryPresent = false }},
		} {
			bad := in
			tc.change(&bad)
			got := AssessInvestigation(bad)
			if got.Status != "SUSPENDED" || got.Health != "DEGRADED" || got.CanNotify || got.ViolationEstablished {
				t.Fatalf("%s %s: %+v", mode, tc.name, got)
			}
		}
		bad := in
		bad.ModuleValidated = false
		got := AssessInvestigation(bad)
		if got.Status == "REVIEW_CANDIDATE" || got.CanNotify || got.ViolationEstablished {
			t.Fatalf("%s validated missing: %+v", mode, got)
		}
	}
}

func TestInvestigationSensitivityOnlyChangesCorroboration(t *testing.T) {
	in := InvestigationInput{ModuleID: "CASE-LOGIN-001", Mode: SensitivityBalanced,
		Thresholds:    &ValidatedThresholds{Approved: true, MinimumEvidence: 1, Strict: 1, Balanced: 2, Relaxed: 3},
		SourceCurrent: true, PollingCaughtUp: true, RequiredTelemetryPresent: true,
		ModuleValidated: true, EvidenceProvenanceVerified: true, ExclusionsChecked: true,
		DuplicateFree: true, IndependentObservations: 1}
	strict := in
	strict.Mode = SensitivityStrict
	if got := AssessInvestigation(strict); got.Status != "REVIEW_CANDIDATE" || got.CanNotify || got.ViolationEstablished {
		t.Fatalf("strict: %+v", got)
	}
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" || got.RequiredObservations != 2 {
		t.Fatalf("balanced: %+v", got)
	}
	in.IndependentObservations = 2
	if got := AssessInvestigation(in); got.Status != "REVIEW_CANDIDATE" || got.CanNotify {
		t.Fatalf("balanced threshold: %+v", got)
	}
	in.Mode = SensitivityRelaxed
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" || got.RequiredObservations != 3 {
		t.Fatalf("relaxed: %+v", got)
	}
	in.IndependentObservations = 3
	in.DuplicateFree = false
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" {
		t.Fatalf("duplicate: %+v", got)
	}
	in.DuplicateFree = true
	in.ExclusionsChecked = false
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" {
		t.Fatalf("exclusion: %+v", got)
	}
	in.ExclusionsChecked = true
	in.ModuleID = "CASE-UNKNOWN"
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" {
		t.Fatalf("unknown: %+v", got)
	}
	in.ModuleID = "CASE-LOGIN-001"
	in.Thresholds = nil
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" || !containsReason(got.Reasons, "THRESHOLD_NOT_VALIDATED") {
		t.Fatalf("unvalidated thresholds: %+v", got)
	}
	in.Thresholds = &ValidatedThresholds{Approved: true, MinimumEvidence: 2, Strict: 1, Balanced: 2, Relaxed: 3}
	if got := AssessInvestigation(in); got.Status == "REVIEW_CANDIDATE" {
		t.Fatalf("strict below evidence floor: %+v", got)
	}
}
