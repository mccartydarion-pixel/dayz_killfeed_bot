package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type serverRankedSeasonRequest struct {
	RPPerKill int64 `json:"rpPerKill"`
	Thresholds ranked.Thresholds `json:"thresholds"`
	Confirm string `json:"confirm"`
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
	if errors.Is(err, repository.ErrRankedIneligible) {
		writeSaaSError(w, codeInvalidRequest, "server is unavailable or has no active Ranked season to reset")
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
