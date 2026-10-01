package app

import (
	"os"
	"strconv"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/featureflags"
)

// C.A.S.E. collection requires BOTH an opt-in toggle and a specific game
// server allowlist. A malformed/empty allowlist always fails closed; one
// service may host several customer installations and must never turn on
// high-volume evidence writes for them implicitly.
//
// The platform owner can override either decision per installation (Owner Hub "Feature
// flags"); caseFlags is the resolver app.go installs, nil in a bare runtime.
var caseFlags *featureflags.Resolver

func caseEvidenceEnabledForServer(serverID int64) bool {
	return caseFlags.EnabledForServer(serverID, featureflags.CaseEvidence, caseEvidenceEnvForServer(serverID))
}

func caseEvidenceEnvForServer(serverID int64) bool {
	if serverID <= 0 || !strings.EqualFold(strings.TrimSpace(os.Getenv("CASE_EVIDENCE_ENABLED")), "true") {
		return false
	}
	for _, raw := range strings.Split(os.Getenv("CASE_EVIDENCE_SERVER_IDS"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err == nil && id == serverID {
			return true
		}
	}
	return false
}

// Build actions have their own explicit switch and per-server allowlist.
// Both must be set even when the older C.A.S.E. collector is already enabled.
func caseBuildEvidenceEnabledForServer(serverID int64) bool {
	if !caseEvidenceEnabledForServer(serverID) {
		return false
	}
	return caseFlags.EnabledForServer(serverID, featureflags.CaseBuildEvidence, caseBuildEvidenceEnvForServer(serverID))
}

func caseBuildEvidenceEnvForServer(serverID int64) bool {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("CASE_BUILD_EVIDENCE_ENABLED")), "true") {
		return false
	}
	for _, raw := range strings.Split(os.Getenv("CASE_BUILD_EVIDENCE_SERVER_IDS"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err == nil && id == serverID { return true }
	}
	return false
}
