package app

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

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
	return caseSelfServeOptedIn(serverID)
}

// Self-serve: when CASE_EVIDENCE_SELF_SERVE=true (default off) a server
// owner's own switch (case_evidence_optins) counts like the allowlist. The
// choices are read once at startup, like the allowlist, because the collector
// attaches when a server's worker starts; a change applies after the next
// restart. caseCollectorRunning records which servers actually attached.
var (
	caseOptinMu          sync.RWMutex
	caseOptinServers     = map[int64]bool{}
	caseCollectorRunning sync.Map // serverID -> true
)

func caseEvidenceSelfServe() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CASE_EVIDENCE_SELF_SERVE")), "true") &&
		strings.EqualFold(strings.TrimSpace(os.Getenv("CASE_EVIDENCE_ENABLED")), "true")
}

func caseSelfServeOptedIn(serverID int64) bool {
	if !caseEvidenceSelfServe() {
		return false
	}
	caseOptinMu.RLock()
	defer caseOptinMu.RUnlock()
	return caseOptinServers[serverID]
}

type caseOptinLister interface {
	EnabledServerIDs(ctx context.Context) ([]int64, error)
}

// loadCaseEvidenceOptins snapshots owners' choices at startup. Failure keeps
// self-serve off (fail closed).
func loadCaseEvidenceOptins(ctx context.Context, repo caseOptinLister) {
	if !caseEvidenceSelfServe() || repo == nil {
		return
	}
	ids, err := repo.EnabledServerIDs(ctx)
	if err != nil {
		slog.Warn("component=case", "event", "evidence_optins_load_failed", "err", err.Error())
		return
	}
	next := make(map[int64]bool, len(ids))
	for _, id := range ids {
		next[id] = true
	}
	caseOptinMu.Lock()
	caseOptinServers = next
	caseOptinMu.Unlock()
	slog.Info("component=case", "event", "evidence_optins_loaded", "servers", len(ids))
}

func caseCollectorAttached(serverID int64) bool {
	_, ok := caseCollectorRunning.Load(serverID)
	return ok
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
