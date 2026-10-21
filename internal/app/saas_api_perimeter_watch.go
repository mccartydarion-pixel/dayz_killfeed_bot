package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Perimeter Watch owner settings (server owner only, like base registration):
// GET shows the switch, latest alerts, the sale offer and sales; PUT turns it
// on or off; PUT …/offer sells it to players for Champion Points.

type perimeterWatchSettingsRequest struct {
	Enabled         bool `json:"enabled"`
	MarginMeters    *int `json:"marginMeters"`
	CooldownSeconds *int `json:"cooldownSeconds"`
}

func (a *App) handleGetPerimeterWatch(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewPerimeterWatchRepository(a.DB.Pool)
	settings, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=perimeter_watch", "event", "get_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load Perimeter Watch")
		return
	}
	recent, err := repo.RecentAlerts(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, 10)
	if err != nil {
		slog.Warn("component=perimeter_watch", "event", "recent_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load Perimeter Watch")
		return
	}
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	offer, err := sales.GetOffer(ctx, scope, repository.ServicePerimeterWatch)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Perimeter Watch")
		return
	}
	sold, active, err := sales.RecentSales(ctx, scope, repository.ServicePerimeterWatch, 10)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Perimeter Watch")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "recent": recent, "offer": offer, "sales": sold, "activeSubscribers": active})
}

func (a *App) handleSetPerimeterWatch(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req perimeterWatchSettingsRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid Perimeter Watch settings")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewPerimeterWatchRepository(a.DB.Pool)
	current, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Perimeter Watch")
		return
	}
	margin, cooldown := current.MarginMeters, current.CooldownSeconds
	if req.MarginMeters != nil {
		margin = *req.MarginMeters
	}
	if req.CooldownSeconds != nil {
		cooldown = *req.CooldownSeconds
	}
	if margin < repository.PerimeterMinMargin || margin > repository.PerimeterMaxMargin {
		writeSaaSError(w, codeInvalidRequest, "the watched area must reach 25 to 300 m past the base")
		return
	}
	if cooldown < repository.PerimeterMinCooldown || cooldown > repository.PerimeterMaxCooldown {
		writeSaaSError(w, codeInvalidRequest, "the cooldown must be between 5 minutes and 2 hours")
		return
	}
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	settings, err := repo.SetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, req.Enabled, margin, cooldown, actor)
	if err != nil {
		slog.Warn("component=perimeter_watch", "event", "set_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save Perimeter Watch")
		return
	}
	a.recordAudit(ctx, ac, "PERIMETER_WATCH_SAVED", "perimeter-watch", "", "success", nil,
		map[string]any{"enabled": settings.Enabled, "marginMeters": settings.MarginMeters, "cooldownSeconds": settings.CooldownSeconds})
	a.refreshSecurityPanel(repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// handleSetPerimeterWatchOffer is PUT .../admin/case/perimeter-watch/offer.
func (a *App) handleSetPerimeterWatchOffer(w http.ResponseWriter, r *http.Request) {
	a.setSecurityOffer(w, r, repository.ServicePerimeterWatch, "PERIMETER_WATCH_OFFER_SAVED")
}
