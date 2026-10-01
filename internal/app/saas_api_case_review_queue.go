package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type caseReviewQueuePage struct {
	Mode             string                         `json:"mode"`
	ServerID         int64                          `json:"serverId"`
	Items            []repository.CaseReviewSummary `json:"items"`
	NextCursor       *string                        `json:"nextCursor"`
	DetectorsEnabled bool                           `json:"detectorsEnabled"`
	AlertsEnabled    bool                           `json:"alertsEnabled"`
	Enforcement      string                         `json:"enforcement"`
}

// This route only reads neutral case summaries. It cannot admit findings,
// modify a review, deliver a Discord message or run a detector.
func (a *App) handleAntiCheatCases(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok { return }
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. review unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) { return }
	var before *int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			writeSaaSError(w, codeInvalidRequest, "invalid case cursor")
			return
		}
		before = &value
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 50 {
			writeSaaSError(w, codeInvalidRequest, "limit must be 1-50")
			return
		}
		limit = value
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	items, err := repository.NewCaseReviewReader(a.DB.Pool).ListAuthorized(ctx,
		repository.CaseReviewScope{GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID,
			InstallationID: ac.scope.InstallationID}, ac.user.ID, before, limit)
	if err != nil {
		slog.Warn("component=case", "event", "case_review_queue_read_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load C.A.S.E. cases")
		return
	}
	out := caseReviewQueuePage{Mode: "NEUTRAL_REVIEW_QUEUE_ONLY", ServerID: *ac.scope.ServerID,
		Items: items, DetectorsEnabled: false, AlertsEnabled: false, Enforcement: "DISABLED"}
	if len(items) == limit {
		cursor := strconv.FormatInt(items[len(items)-1].ID, 10)
		out.NextCursor = &cursor
	}
	a.recordAudit(ctx, ac, "CASE_REVIEW_QUEUE_VIEWED", "", "", "success", nil,
		map[string]any{"count": len(items)})
	writeSaaSJSON(w, http.StatusOK, out)
}
