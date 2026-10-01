package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Ranked season error codes: specific and safe to show the owner (no ids,
// tokens or player data).
const (
	codeRankedServerIneligible = "RANKED_SERVER_INELIGIBLE"
	codeRankedNoActiveSeason   = "RANKED_NO_ACTIVE_SEASON"
)

func init() {
	httpStatusForCode[codeRankedServerIneligible] = http.StatusBadRequest
	httpStatusForCode[codeRankedNoActiveSeason] = http.StatusConflict
}

type serverRankedSeasonRequest struct {
	RPPerKill int64 `json:"rpPerKill"`
	Thresholds ranked.Thresholds `json:"thresholds"`
	Confirm string `json:"confirm"`
}

func (a *App) handleGetServerRankedSeason(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok { return }
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	if a.Ranked == nil { writeSaaSError(w, codeInternalError, "ranked season system unavailable"); return }
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	season, err := a.Ranked.ActiveServerSeason(ctx, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil { writeSaaSError(w, codeInternalError, "could not load Ranked season"); return }
	writeSaaSJSON(w, http.StatusOK, map[string]any{"season": season})
}

func (a *App) handleStartServerRankedSeason(w http.ResponseWriter, r *http.Request) {
	a.changeServerRankedSeason(w, r, false)
}

func (a *App) handleResetServerRankedSeason(w http.ResponseWriter, r *http.Request) {
	a.changeServerRankedSeason(w, r, true)
}

func (a *App) changeServerRankedSeason(w http.ResponseWriter, r *http.Request, reset bool) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok { return }
	// Starting or resetting a season is Champion-only; reading the current one stays open.
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.RankedSeasons) { return }
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	req, ok := decodeJSONBody[serverRankedSeasonRequest](w, r)
	if !ok { return }
	if reset && !requireConfirmation(w, req.Confirm, "RESET SERVER RANKED") { return }
	if req.RPPerKill <= 0 || req.Thresholds.Validate() != nil {
		writeSaaSError(w, codeInvalidRequest, "positive RP per kill and seven increasing tier thresholds are required")
		return
	}
	if a.Ranked == nil {
		writeSaaSError(w, codeInternalError, "ranked season system unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) { return }
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	season, err := a.Ranked.StartServerSeason(ctx, ac.scope.GuildID, *ac.scope.ServerID, req.RPPerKill, req.Thresholds, reset, time.Now().UTC())
	if errors.Is(err, repository.ErrRankedSeasonConflict) {
		writeSaaSError(w, codeAdminConfirmationNeeded, "an active Ranked season exists; use the reset endpoint to archive it")
		return
	}
	if errors.Is(err, repository.ErrRankedServerIneligible) {
		slog.Warn("component=ranked", "event", "season_change_rejected", "reason", "server_ineligible", "server_id", *ac.scope.ServerID)
		writeSaaSError(w, codeRankedServerIneligible, "the selected DayZ server is not an active PlayStation or Xbox server for this Discord; reconnect or reselect it in Champion setup")
		return
	}
	if errors.Is(err, repository.ErrRankedNoActiveSeason) {
		writeSaaSError(w, codeRankedNoActiveSeason, "there is no active Ranked season to reset; start one instead")
		return
	}
	if err != nil {
		slog.Warn("component=ranked", "event", "season_change_failed", "server_id", *ac.scope.ServerID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not change Ranked season")
		return
	}
	action := "SERVER_RANKED_START"
	if reset { action = "SERVER_RANKED_RESET" }
	a.recordAudit(ctx, ac, action, "server:"+strconv.FormatInt(*ac.scope.ServerID, 10), "Ranked season", "success", nil, map[string]int64{"seasonId": season.ID})
	for _, board := range a.ServerRanksBoards { board.SyncOnce(ctx) }
	writeSaaSJSON(w, http.StatusOK, season)
}
