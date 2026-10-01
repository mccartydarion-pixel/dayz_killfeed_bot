package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Base Black Box. Server owner (like base registration): GET shows the switch,
// latest history, the sale offer and sales; PUT turns it on or off and sets how
// long history is kept; PUT …/offer sells it to players for Champion Points.
// Player: GET …/security-marketplace/black-box shows the history of the bases
// the signed-in, verified player owns, when they can see it.

type baseBlackBoxSettingsRequest struct {
	Enabled       bool `json:"enabled"`
	RetentionDays *int `json:"retentionDays"`
}

func (a *App) handleGetBaseBlackBox(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewBaseBlackBoxRepository(a.DB.Pool)
	settings, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=base_black_box", "event", "get_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the Base Black Box")
		return
	}
	recent, err := repo.Recent(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, 20)
	if err != nil {
		slog.Warn("component=base_black_box", "event", "recent_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the Base Black Box")
		return
	}
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	offer, err := sales.GetOffer(ctx, scope, repository.ServiceBaseBlackBox)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the Base Black Box")
		return
	}
	sold, active, err := sales.RecentSales(ctx, scope, repository.ServiceBaseBlackBox, 10)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the Base Black Box")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "recent": recent, "offer": offer, "sales": sold, "activeSubscribers": active})
}

func (a *App) handleSetBaseBlackBox(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req baseBlackBoxSettingsRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid Base Black Box settings")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewBaseBlackBoxRepository(a.DB.Pool)
	current, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the Base Black Box")
		return
	}
	retention := current.RetentionDays
	if req.RetentionDays != nil {
		retention = *req.RetentionDays
	}
	if retention < repository.BlackBoxMinRetention || retention > repository.BlackBoxMaxRetention {
		writeSaaSError(w, codeInvalidRequest, "history can be kept for 3 to 30 days")
		return
	}
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	settings, err := repo.SetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, req.Enabled, retention, actor)
	if err != nil {
		slog.Warn("component=base_black_box", "event", "set_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save the Base Black Box")
		return
	}
	a.recordAudit(ctx, ac, "BASE_BLACK_BOX_SAVED", "base-black-box", "", "success", nil,
		map[string]any{"enabled": settings.Enabled, "retentionDays": settings.RetentionDays})
	a.refreshSecurityPanel(repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// handleSetBaseBlackBoxOffer is PUT .../admin/case/black-box/offer.
func (a *App) handleSetBaseBlackBoxOffer(w http.ResponseWriter, r *http.Request) {
	a.setSecurityOffer(w, r, repository.ServiceBaseBlackBox, "BASE_BLACK_BOX_OFFER_SAVED")
}

// Why a player can't use a base service (Black Box history, Faction
// Security) right now.
const (
	serviceReasonOff     = "OFF"      // the server owner hasn't turned it on
	serviceReasonNotPaid = "NOT_PAID" // it's on sale and the player has no paid time
)

type playerBlackBoxResponse struct {
	Available     bool                       `json:"available"`
	Reason        string                     `json:"reason,omitempty"`
	RetentionDays int                        `json:"retentionDays"`
	ActiveUntil   *time.Time                 `json:"activeUntil,omitempty"`
	Bases         []repository.BlackBoxBase  `json:"bases"`
	Events        []repository.BlackBoxEvent `json:"events"`
}

// handlePlayerBaseBlackBox is GET .../security-marketplace/black-box. The
// verified DayZ link decides whose bases are shown; a player never sees
// another player's history.
func (a *App) handlePlayerBaseBlackBox(w http.ResponseWriter, r *http.Request) {
	er, ok := a.scopedContext(w, r, "")
	if !ok {
		return
	}
	if er.scope.ServerID <= 0 || a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeConflict, "the installation has no DayZ server selected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "verify black box player", err)
		return
	}
	repo := repository.NewBaseBlackBoxRepository(a.DB.Pool)
	settings, err := repo.GetSettings(ctx, er.scope.InstallationID, er.scope.GuildID, er.scope.ServerID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load your base history")
		return
	}
	out := playerBlackBoxResponse{RetentionDays: settings.RetentionDays, Bases: []repository.BlackBoxBase{}, Events: []repository.BlackBoxEvent{}}
	if !settings.Enabled {
		out.Reason = serviceReasonOff
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	scope := repository.SecurityScope{InstallationID: er.scope.InstallationID, GuildID: er.scope.GuildID, ServerID: er.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	onSale, err := sales.OnSale(ctx, scope, repository.ServiceBaseBlackBox)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load your base history")
		return
	}
	until, err := sales.ActiveUntil(ctx, scope.InstallationID, acct.AccountID, repository.ServiceBaseBlackBox)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load your base history")
		return
	}
	out.ActiveUntil = until
	if onSale && until == nil {
		out.Reason = serviceReasonNotPaid
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	bases, events, err := repo.OwnerHistory(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, acct.AccountID, 200)
	if err != nil {
		slog.Warn("component=base_black_box", "event", "owner_history_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load your base history")
		return
	}
	out.Available, out.Bases, out.Events = true, bases, events
	writeSaaSJSON(w, http.StatusOK, out)
}
