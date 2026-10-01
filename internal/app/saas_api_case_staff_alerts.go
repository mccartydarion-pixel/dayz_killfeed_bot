package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Owner switch for real C.A.S.E. staff alerts (case_staff_alert_worker.go).
// GET shows the switch, which detectors are released and the latest alerts;
// PUT turns alerts on or off. Server owner only.

type caseStaffAlertSettingsRequest struct {
	Enabled bool `json:"enabled"`
}

func (a *App) caseStaffAlertActor(w http.ResponseWriter, r *http.Request) (adminActor, *repository.CaseStaffAlertRepository, bool) {
	ac, ok := a.requireCapability(w, r, permissions.CapUAVManage)
	if !ok {
		return adminActor{}, nil, false
	}
	if ac.level != permissions.LevelOwner {
		writeSaaSError(w, codeAdminForbidden, "server owner required")
		return adminActor{}, nil, false
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return adminActor{}, nil, false
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. alerts unavailable")
		return adminActor{}, nil, false
	}
	return ac, repository.NewCaseStaffAlertRepository(a.DB.Pool), true
}

// releasedCaseModules lists detectors that may send staff alerts.
func releasedCaseModules() []string {
	out := make([]string, 0)
	for _, d := range caseintel.ClientCatalog() {
		if caseintel.ModuleReleased(d.ID) {
			out = append(out, d.ID)
		}
	}
	return out
}

func (a *App) handleGetCaseStaffAlerts(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseStaffAlertActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	settings, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=case_alerts", "event", "get_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load C.A.S.E. alerts")
		return
	}
	recent, err := repo.Recent(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, 10)
	if err != nil {
		slog.Warn("component=case_alerts", "event", "recent_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load C.A.S.E. alerts")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "releasedDetectors": releasedCaseModules(),
		"recent": recent, "enforcement": "DISABLED"})
}

func (a *App) handleSetCaseStaffAlerts(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseStaffAlertActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req caseStaffAlertSettingsRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid C.A.S.E. alert settings")
		return
	}
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	settings, err := repo.SetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, req.Enabled, actor)
	if err != nil {
		slog.Warn("component=case_alerts", "event", "set_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save C.A.S.E. alerts")
		return
	}
	a.recordAudit(ctx, ac, "CASE_STAFF_ALERTS_SAVED", "case-alerts", "", "success", nil, map[string]any{"enabled": settings.Enabled})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "releasedDetectors": releasedCaseModules()})
}
