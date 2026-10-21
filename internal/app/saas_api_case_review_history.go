package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type caseReviewHistoryPage struct {
	Mode             string                             `json:"mode"`
	ServerID         int64                              `json:"serverId"`
	CaseID           int64                              `json:"caseId"`
	Items            []repository.SyntheticAuditSummary `json:"items"`
	NextCursor       *string                            `json:"nextCursor"`
	DetectorsEnabled bool                               `json:"detectorsEnabled"`
	AlertsEnabled    bool                               `json:"alertsEnabled"`
	Enforcement      string                             `json:"enforcement"`
}

// The history route exposes only immutable neutral transitions for one case.
// It cannot return notes/evidence/player data or mutate review state.
func (a *App) handleAntiCheatCaseHistory(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok {
		return
	}
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. review unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	caseID, err := strconv.ParseInt(r.PathValue("caseID"), 10, 64)
	if err != nil || caseID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "invalid case ID")
		return
	}
	var before *int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || value <= 0 {
			writeSaaSError(w, codeInvalidRequest, "invalid history cursor")
			return
		}
		before = &value
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < 1 || value > 50 {
			writeSaaSError(w, codeInvalidRequest, "limit must be 1-50")
			return
		}
		limit = value
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	items, err := repository.NewCaseReviewReader(a.DB.Pool).ListAuthorizedAudit(ctx,
		repository.CaseReviewScope{GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID,
			InstallationID: ac.scope.InstallationID}, caseID, ac.user.ID, before, limit)
	if err != nil {
		slog.Warn("component=case", "event", "case_review_history_read_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load C.A.S.E. history")
		return
	}
	out := caseReviewHistoryPage{Mode: "NEUTRAL_REVIEW_HISTORY_ONLY", ServerID: *ac.scope.ServerID,
		CaseID: caseID, Items: items, DetectorsEnabled: false, AlertsEnabled: false,
		Enforcement: "DISABLED"}
	if len(items) == limit {
		cursor := strconv.FormatInt(items[len(items)-1].ID, 10)
		out.NextCursor = &cursor
	}
	a.recordAudit(ctx, ac, "CASE_REVIEW_HISTORY_VIEWED", "", "", "success", nil,
		map[string]any{"caseId": caseID, "count": len(items)})
	writeSaaSJSON(w, http.StatusOK, out)
}
