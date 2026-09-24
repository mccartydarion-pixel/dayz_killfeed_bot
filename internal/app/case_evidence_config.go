package app

import (
	"os"
	"strconv"
	"strings"
)

// C.A.S.E. collection requires BOTH an opt-in toggle and a specific game
// server allowlist. A malformed/empty allowlist always fails closed; one
// service may host several customer installations and must never turn on
// high-volume evidence writes for them implicitly.
func caseEvidenceEnabledForServer(serverID int64) bool {
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
