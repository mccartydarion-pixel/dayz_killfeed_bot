package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// handleCaseBaseShadow is GET .../admin/case/bases/shadow (server owner only):
// a read-only Base Boosting shadow run over the newest retained build lines
// and this server's registered bases. It records nothing, cannot notify and
// cannot enforce.
func (a *App) handleCaseBaseShadow(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	serverID := *ac.scope.ServerID
	builds, truncated, err := repo.ListBaseShadowBuilds(ctx, ac.scope.GuildID, serverID, 500)
	if err != nil {
		slog.Warn("component=case", "event", "base_shadow_builds_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not run the Base Boosting test")
		return
	}
	bases, err := repo.ListBaseShadowBases(ctx, ac.scope.InstallationID, ac.scope.GuildID, serverID)
	if err != nil {
		slog.Warn("component=case", "event", "base_shadow_bases_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not run the Base Boosting test")
		return
	}
	offset, err := repository.NewCaseEvidenceRepository(a.DB.Pool).CaseServerUTCOffset(ctx, ac.scope.GuildID, serverID)
	if err != nil {
		offset = nil // fail closed: no trusted time, no conclusions
	}
	report := caseintel.ShadowBaseBoost(caseintel.ShadowBaseInput{
		Scope:  caseintel.Core8Scope{GuildID: ac.scope.GuildID, InstallationID: ac.scope.InstallationID, ServerID: serverID},
		Builds: builds, Bases: bases, UTCOffsetMinutes: offset,
		Telemetry:          a.caseADMTelemetry(serverID, time.Now().UTC()),
		BuildActionsLogged: caseBuildEvidenceEnabledForServer(serverID), WindowTruncated: truncated,
	})
	a.recordAudit(ctx, ac, "CASE_BASE_SHADOW_VIEWED", "", "", "success", nil,
		map[string]any{"buildActions": report.BuildActions, "findings": len(report.Result.Findings)})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"baseShadow": report})
}
