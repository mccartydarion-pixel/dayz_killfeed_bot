package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Faction Security. Server owner (like base registration): GET shows the
// switch, latest shared alerts, the sale offer and sales; PUT turns it on or
// off; PUT …/offer sells it to players for Champion Points.
// Player: GET/PUT …/security-marketplace/faction-security shows the signed-in
// base owner's faction and lets them choose who in it is told.

type factionSecuritySettingsRequest struct {
	Enabled bool `json:"enabled"`
}

func (a *App) handleGetFactionSecurity(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewFactionSecurityRepository(a.DB.Pool)
	settings, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=faction_security", "event", "get_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load Faction Security")
		return
	}
	recent, err := repo.RecentShares(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, 10)
	if err != nil {
		slog.Warn("component=faction_security", "event", "recent_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load Faction Security")
		return
	}
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	offer, err := sales.GetOffer(ctx, scope, repository.ServiceFactionSecurity)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Faction Security")
		return
	}
	sold, active, err := sales.RecentSales(ctx, scope, repository.ServiceFactionSecurity, 10)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Faction Security")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "recent": recent, "offer": offer, "sales": sold, "activeSubscribers": active})
}

func (a *App) handleSetFactionSecurity(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req factionSecuritySettingsRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid Faction Security settings")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	settings, err := repository.NewFactionSecurityRepository(a.DB.Pool).SetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, req.Enabled, actor)
	if err != nil {
		slog.Warn("component=faction_security", "event", "set_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save Faction Security")
		return
	}
	a.recordAudit(ctx, ac, "FACTION_SECURITY_SAVED", "faction-security", "", "success", nil, map[string]any{"enabled": settings.Enabled})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// handleSetFactionSecurityOffer is PUT .../admin/case/faction-security/offer.
func (a *App) handleSetFactionSecurityOffer(w http.ResponseWriter, r *http.Request) {
	a.setSecurityOffer(w, r, repository.ServiceFactionSecurity, "FACTION_SECURITY_OFFER_SAVED")
}

type playerFactionSecurityResponse struct {
	Available   bool                              `json:"available"`
	Reason      string                            `json:"reason,omitempty"`
	Recipients  string                            `json:"recipients"`
	ActiveUntil *time.Time                        `json:"activeUntil,omitempty"`
	Faction     repository.FactionSecuritySummary `json:"faction"`
}

type playerFactionSecurityRequest struct {
	Recipients string `json:"recipients"`
}

// playerFactionSecurity loads the signed-in player's view. ok is false when a
// response has already been written.
func (a *App) playerFactionSecurity(w http.ResponseWriter, r *http.Request) (playerFactionSecurityResponse, economyRequest, int64, bool) {
	var out playerFactionSecurityResponse
	er, ok := a.scopedContext(w, r, "")
	if !ok {
		return out, er, 0, false
	}
	if er.scope.ServerID <= 0 || a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeConflict, "the installation has no DayZ server selected")
		return out, er, 0, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "verify faction security player", err)
		return out, er, 0, false
	}
	repo := repository.NewFactionSecurityRepository(a.DB.Pool)
	failed := func() (playerFactionSecurityResponse, economyRequest, int64, bool) {
		writeSaaSError(w, codeInternalError, "could not load Faction Security")
		return out, er, 0, false
	}
	settings, err := repo.GetSettings(ctx, er.scope.InstallationID, er.scope.GuildID, er.scope.ServerID)
	if err != nil {
		return failed()
	}
	if out.Recipients, err = repo.GetPreference(ctx, er.scope.InstallationID, acct.AccountID); err != nil {
		return failed()
	}
	if out.Faction, err = repo.Summary(ctx, er.scope.GuildID, acct.AccountID); err != nil {
		return failed()
	}
	if !settings.Enabled {
		out.Reason = serviceReasonOff
		return out, er, acct.AccountID, true
	}
	scope := repository.SecurityScope{InstallationID: er.scope.InstallationID, GuildID: er.scope.GuildID, ServerID: er.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	offer, err := sales.GetOffer(ctx, scope, repository.ServiceFactionSecurity)
	if err != nil {
		return failed()
	}
	if out.ActiveUntil, err = sales.ActiveUntil(ctx, scope.InstallationID, acct.AccountID, repository.ServiceFactionSecurity); err != nil {
		return failed()
	}
	if offer.Enabled && out.ActiveUntil == nil {
		out.Reason = serviceReasonNotPaid
		return out, er, acct.AccountID, true
	}
	out.Available = true
	return out, er, acct.AccountID, true
}

// handleGetPlayerFactionSecurity is GET .../security-marketplace/faction-security.
func (a *App) handleGetPlayerFactionSecurity(w http.ResponseWriter, r *http.Request) {
	out, _, _, ok := a.playerFactionSecurity(w, r)
	if ok {
		writeSaaSJSON(w, http.StatusOK, out)
	}
}

// handleSetPlayerFactionSecurity is PUT .../security-marketplace/faction-security:
// the base owner chooses everyone in their faction, or only its leaders.
func (a *App) handleSetPlayerFactionSecurity(w http.ResponseWriter, r *http.Request) {
	var req playerFactionSecurityRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	if req.Recipients != repository.FactionRecipientsAll && req.Recipients != repository.FactionRecipientsLeaders {
		writeSaaSError(w, codeInvalidRequest, "recipients must be ALL or LEADERS")
		return
	}
	out, er, playerID, ok := a.playerFactionSecurity(w, r)
	if !ok {
		return
	}
	if out.Reason == serviceReasonOff {
		writeSaaSError(w, codeConflict, "Faction Security isn't on for this server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	if err := repository.NewFactionSecurityRepository(a.DB.Pool).SetPreference(ctx, er.scope.InstallationID, playerID, req.Recipients); err != nil {
		slog.Warn("component=faction_security", "event", "set_preference_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save your choice")
		return
	}
	out.Recipients = req.Recipients
	writeSaaSJSON(w, http.StatusOK, out)
}
