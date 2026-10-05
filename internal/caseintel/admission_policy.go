package caseintel

import (
	"strings"
	"time"
)

// CaseAdmissionCandidate is an offline observation for the future reviewed-case
// gate. Values are NOT trusted authentication, DB provenance or source health.
// No production caller uses this contract.
type CaseAdmissionCandidate struct {
	GuildID, InstallationID, ServerID                    int64
	DetectorID, DetectorVersion                          string
	EvaluationStatus                                     string
	SelectedSource, AcceptedSource, LatestRetainedSource string
	LatestRetainedAt, SourceChangedAt, CapturedAt        time.Time
	Evidence                                             []AdmissibilitySample
	WindowTruncated                                      bool
}

// CaseAdmissionDecision never creates a finding, case, delivery, or sanction.
// Every proposed input must be re-established from the authenticated selected
// installation and a scoped database snapshot before a future real admission.
type CaseAdmissionDecision struct {
	Status              string
	Blockers            []string
	EvidenceFingerprint string
}

// AssessCaseAdmission is an OFFLINE fail-closed policy using the real immutable
// detector registry. The registry currently contains only blocked CASE-MOV-001,
// so no current console observation can produce ADMISSION_CANDIDATE. A fixture
// cannot make its own detector validated by setting a boolean.
func AssessCaseAdmission(in CaseAdmissionCandidate) CaseAdmissionDecision {
	out := CaseAdmissionDecision{Status: "BLOCKED", Blockers: make([]string, 0, 12)}
	add := func(s string) { out.Blockers = append(out.Blockers, s) }
	if in.GuildID <= 0 || in.InstallationID <= 0 || in.ServerID <= 0 {
		add("INVALID_INSTALLATION_SCOPE")
	}
	registered := false
	for _, def := range Registry() {
		if def.ID == in.DetectorID && def.Version == in.DetectorVersion {
			registered = true
			if def.Mode != "VALIDATED_SHADOW" {
				add("DETECTOR_NOT_VALIDATED")
			}
			break
		}
	}
	if !registered {
		add("DETECTOR_VERSION_NOT_REGISTERED")
	}
	if in.EvaluationStatus != "ELIGIBLE_SHADOW" {
		add("SHADOW_EVALUATION_NOT_ELIGIBLE")
	}
	if in.SelectedSource == "" || in.SelectedSource != in.AcceptedSource ||
		in.SelectedSource != in.LatestRetainedSource {
		add("CURRENT_SOURCE_NOT_VERIFIED")
	}
	if in.CapturedAt.IsZero() || in.LatestRetainedAt.IsZero() ||
		in.LatestRetainedAt.After(in.CapturedAt) ||
		(!in.SourceChangedAt.IsZero() && in.LatestRetainedAt.Before(in.SourceChangedAt)) {
		add("CURRENT_EVIDENCE_TIME_NOT_VERIFIED")
	}
	if len(in.Evidence) == 0 || len(in.Evidence) > 50 || in.WindowTruncated {
		add("EVIDENCE_WINDOW_INCOMPLETE")
	} else {
		seenIDs := make(map[int64]bool, len(in.Evidence))
		seenAddresses := make(map[int64]string, len(in.Evidence))
		for _, ev := range in.Evidence {
			if ev.EvidenceID <= 0 || seenIDs[ev.EvidenceID] {
				add("INVALID_OR_DUPLICATE_EVIDENCE_ID")
				break
			}
			seenIDs[ev.EvidenceID] = true
			if ev.SourceID != in.SelectedSource || ev.SourceEndOffset < 0 ||
				!sha256Hex.MatchString(ev.LineSHA256) || strings.TrimSpace(ev.EventType) == "" {
				add("EVIDENCE_SOURCE_ADDRESS_UNVERIFIED")
				break
			}
			hash := strings.ToLower(ev.LineSHA256)
			if old, found := seenAddresses[ev.SourceEndOffset]; found {
				if old != hash {
					add("EVIDENCE_SOURCE_OFFSET_HASH_COLLISION")
				} else {
					add("REPEATED_SOURCE_ADDRESS")
				}
				break
			}
			seenAddresses[ev.SourceEndOffset] = hash
		}
	}
	// This contract cannot grant a real case: a future admission transaction
	// must independently recheck source/evidence scope, registry release mode,
	// reviewer eligibility and idempotency against the persisted rows.
	if len(out.Blockers) == 0 {
		ids := make([]int64, 0, len(in.Evidence))
		for _, ev := range in.Evidence {
			ids = append(ids, ev.EvidenceID)
		}
		fp, err := EvidenceFingerprint(in.GuildID, in.ServerID, in.DetectorID, in.DetectorVersion, ids)
		if err != nil {
			add("EVIDENCE_FINGERPRINT_UNAVAILABLE")
		} else {
			out.Status = "ADMISSION_CANDIDATE"
			out.EvidenceFingerprint = fp
		}
	}
	return out
}
