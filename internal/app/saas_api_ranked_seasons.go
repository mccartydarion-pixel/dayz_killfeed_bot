package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
	RPPerKill  int64             `json:"rpPerKill"`
	Thresholds ranked.Thresholds `json:"thresholds"`
	// SameVictimCooldownMinutes is kept raw so an absent field (an older website) means the
	// default and anything that is not a whole number gets a plain-language 400.
	SameVictimCooldownMinutes json.RawMessage `json:"sameVictimCooldownMinutes,omitempty"`
	Confirm                   string          `json:"confirm"`
}

const sameVictimCooldownMessage = "the wait before the same player counts again must be a whole number of minutes from 0 to 120"

// parseSameVictimCooldownMinutes reads the season's same-victim wait from a start/reset request:
// absent or null is the default (5), otherwise a whole number of minutes from 0 to 120.
func parseSameVictimCooldownMinutes(raw json.RawMessage) (int, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ranked.DefaultSameVictimCooldownMinutes, true
	}
	minutes, err := strconv.Atoi(text)
	if err != nil || !ranked.ValidSameVictimCooldownMinutes(minutes) {
		return 0, false
	}
	return minutes, true
}

func (a *App) handleGetServerRankedSeason(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok {
		return
	}
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	if a.Ranked == nil {
		writeSaaSError(w, codeInternalError, "ranked season system unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	season, err := a.Ranked.ActiveServerSeason(ctx, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Ranked season")
		return
	}
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
	if !ok {
		return
	}
	// Starting or resetting a season is Champion-only; reading the current one stays open.
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.RankedSeasons) {
		return
	}
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	req, ok := decodeJSONBody[serverRankedSeasonRequest](w, r)
	if !ok {
		return
	}
	if reset && !requireConfirmation(w, req.Confirm, "RESET SERVER RANKED") {
		return
	}
	if req.RPPerKill <= 0 || req.Thresholds.Validate() != nil {
		writeSaaSError(w, codeInvalidRequest, "positive RP per kill and seven increasing tier thresholds are required")
		return
	}
	cooldownMinutes, ok := parseSameVictimCooldownMinutes(req.SameVictimCooldownMinutes)
	if !ok {
		writeSaaSError(w, codeInvalidRequest, sameVictimCooldownMessage)
		return
	}
	if a.Ranked == nil {
		writeSaaSError(w, codeInternalError, "ranked season system unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	season, err := a.Ranked.StartServerSeason(ctx, ac.scope.GuildID, *ac.scope.ServerID, req.RPPerKill, req.Thresholds, cooldownMinutes, reset, time.Now().UTC())
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
	if reset {
		action = "SERVER_RANKED_RESET"
	}
	a.recordAudit(ctx, ac, action, "server:"+strconv.FormatInt(*ac.scope.ServerID, 10), "Ranked season", "success", nil, map[string]int64{"seasonId": season.ID})
	for _, board := range a.ServerRanksBoards {
		board.SyncOnce(ctx)
	}
	writeSaaSJSON(w, http.StatusOK, season)
}

// serverRankedSeasonWaitRequest is the body of PATCH .../ranked/server-season. Only the wait can
// change on an active season; the frozen rules are decoded just to refuse an attempt to send them.
type serverRankedSeasonWaitRequest struct {
	SameVictimCooldownMinutes json.RawMessage `json:"sameVictimCooldownMinutes"`
	RPPerKill                 json.RawMessage `json:"rpPerKill"`
	Thresholds                json.RawMessage `json:"thresholds"`
}

// parseSameVictimCooldownChange reads a mid-season wait change. Unlike start/reset there is no
// default: the field is required. rpPerKill and thresholds are refused, not ignored.
func parseSameVictimCooldownChange(req serverRankedSeasonWaitRequest) (int, string) {
	if len(req.RPPerKill) > 0 || len(req.Thresholds) > 0 {
		return 0, "RP per kill and tier thresholds are frozen for the season; only the wait can be changed (reset the season to change the others)"
	}
	text := strings.TrimSpace(string(req.SameVictimCooldownMinutes))
	if text == "" || text == "null" {
		return 0, sameVictimCooldownMessage
	}
	minutes, ok := parseSameVictimCooldownMinutes(req.SameVictimCooldownMinutes)
	if !ok {
		return 0, sameVictimCooldownMessage
	}
	return minutes, ""
}

// handleChangeServerRankedSeasonWait changes the active season's same-victim wait without a reset.
// Same gate as start/reset. It applies to kills from now on (see ChangeActiveSeasonCooldown).
func (a *App) handleChangeServerRankedSeasonWait(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.RankedSeasons) {
		return
	}
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	req, ok := decodeJSONBody[serverRankedSeasonWaitRequest](w, r)
	if !ok {
		return
	}
	minutes, problem := parseSameVictimCooldownChange(req)
	if problem != "" {
		writeSaaSError(w, codeInvalidRequest, problem)
		return
	}
	if a.Ranked == nil {
		writeSaaSError(w, codeInternalError, "ranked season system unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	season, previous, err := a.Ranked.ChangeActiveSeasonCooldown(ctx, ac.scope.GuildID, *ac.scope.ServerID, minutes, ac.user.DiscordUserID, time.Now().UTC())
	if errors.Is(err, repository.ErrRankedNoActiveSeason) {
		writeSaaSError(w, codeRankedNoActiveSeason, "there is no active Ranked season; start one first")
		return
	}
	if err != nil {
		slog.Warn("component=ranked", "event", "season_wait_change_failed", "server_id", *ac.scope.ServerID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not change the Ranked wait time")
		return
	}
	if previous != minutes {
		a.recordAudit(ctx, ac, "SERVER_RANKED_WAIT_CHANGE", "server:"+strconv.FormatInt(*ac.scope.ServerID, 10), "Ranked season wait", "success",
			map[string]int64{"seasonId": season.ID, "sameVictimCooldownMinutes": int64(previous)},
			map[string]int64{"seasonId": season.ID, "sameVictimCooldownMinutes": int64(minutes)})
	}
	writeSaaSJSON(w, http.StatusOK, season)
}
