package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Staff verdicts on test-run findings ("false alarm" or "looks suspicious")
// and the review score per detector. Suspicious Logins can be reviewed by
// anyone who can open the Players tab (PLAYER_LOCATION_VIEW); Base Boosting,
// like its card, by the server owner (UAV_MANAGE). Verdicts never release a
// detector, send an alert or act on a player.

type caseShadowVerdictRequest struct {
	DetectorID  string `json:"detectorId"`
	IncidentKey string `json:"incidentKey"`
	PlayerID    int64  `json:"playerId"`
	Verdict     string `json:"verdict"`
	Note        string `json:"note"`
}

func (a *App) caseVerdictActor(w http.ResponseWriter, r *http.Request, detectorID string) (adminActor, repository.CaseVerdictScope, bool) {
	if !repository.ShadowVerdictDetector(detectorID) {
		writeSaaSError(w, codeInvalidRequest, "detectorId must be CASE-LOGIN-001 or CASE-BASE-001")
		return adminActor{}, repository.CaseVerdictScope{}, false
	}
	capability := permissions.CapPlayerLocationView
	if detectorID == "CASE-BASE-001" {
		capability = permissions.CapUAVManage
	}
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return adminActor{}, repository.CaseVerdictScope{}, false
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return adminActor{}, repository.CaseVerdictScope{}, false
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. review unavailable")
		return adminActor{}, repository.CaseVerdictScope{}, false
	}
	return ac, repository.CaseVerdictScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}, true
}

// handleGetCaseShadowVerdicts is GET .../admin/case/shadow-verdicts?detectorId=…
func (a *App) handleGetCaseShadowVerdicts(w http.ResponseWriter, r *http.Request) {
	_, scope, ok := a.caseVerdictActor(w, r, r.URL.Query().Get("detectorId"))
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	sum, items, err := repository.NewCaseShadowVerdictRepository(a.DB.Pool).Summary(ctx, scope, r.URL.Query().Get("detectorId"), 200)
	if err != nil {
		slog.Warn("component=case", "event", "verdict_summary_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the review")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"summary": sum, "items": items})
}

// handleSaveCaseShadowVerdict is PUT .../admin/case/shadow-verdicts.
func (a *App) handleSaveCaseShadowVerdict(w http.ResponseWriter, r *http.Request) {
	var req caseShadowVerdictRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid verdict")
		return
	}
	ac, scope, ok := a.caseVerdictActor(w, r, req.DetectorID)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var reviewer *int64
	if ac.user != nil {
		reviewer = &ac.user.ID
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	v, err := repository.NewCaseShadowVerdictRepository(a.DB.Pool).Save(ctx, scope, req.DetectorID, req.IncidentKey, req.PlayerID, req.Verdict, req.Note, reviewer)
	if errors.Is(err, repository.ErrInvalidVerdict) {
		writeSaaSError(w, codeInvalidRequest, "invalid verdict")
		return
	}
	if err != nil {
		slog.Warn("component=case", "event", "verdict_save_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save the verdict")
		return
	}
	a.recordAudit(ctx, ac, "CASE_SHADOW_VERDICT_SAVED", "case-detector:"+req.DetectorID, "", "success", nil,
		map[string]any{"verdict": v.Verdict, "playerId": v.PlayerID})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"verdict": v})
}
