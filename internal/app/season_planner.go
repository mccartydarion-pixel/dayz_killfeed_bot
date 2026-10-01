package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Wipe & season planner (docs/CLIENT_HUB_GROWTH.md). An owner schedules a stats season rollover
// or a per-server Ranked reset, sees what will be archived, and Champion runs it at the chosen
// time and announces it. A Ranked reset keeps the server's current RP per kill and thresholds:
// the planner never invents or changes Ranked rules.

const (
	seasonPlanMinLead = time.Minute
	seasonPlanMaxLead = 90 * 24 * time.Hour
)

func (a *App) registerSeasonPlannerRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/seasons/planner", a.handleSeasonPlanner)
	h("POST "+adminBase+"/seasons/planner", a.handleScheduleSeasonAction)
	h("POST "+adminBase+"/seasons/planner/{actionID}/cancel", a.handleCancelSeasonAction)
}

func (a *App) handleSeasonPlanner(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.SeasonPlanner == nil {
		writeSaaSError(w, codeInternalError, "season planner unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	stats, err := a.SeasonPlanner.StatsSeasonPreview(ctx, ac.scope.GuildID)
	if err != nil {
		slog.Warn("component=saas_api", "event", "season_preview_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the stats season")
		return
	}
	var rankedPreview *repository.RankedSeasonPreview
	if ac.scope.ServerID != nil && *ac.scope.ServerID > 0 {
		rankedPreview, err = a.SeasonPlanner.RankedSeasonPreview(ctx, ac.scope.GuildID, *ac.scope.ServerID)
		if err != nil {
			slog.Warn("component=saas_api", "event", "ranked_preview_failed", "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not load the Ranked season")
			return
		}
		if rankedPreview != nil && a.Ranked != nil {
			if standings, err := a.Ranked.ServerStandings(ctx, *ac.scope.ServerID, 3); err == nil {
				for _, s := range standings {
					rankedPreview.Top = append(rankedPreview.Top, repository.PreviewLeader{Name: s.Name, Value: float64(s.RP)})
				}
			}
		}
	}
	items, err := a.SeasonPlanner.List(ctx, ac.scope.GuildID, 20)
	if err != nil {
		slog.Warn("component=saas_api", "event", "season_actions_list_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load scheduled season changes")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"statsSeason": stats, "rankedSeason": rankedPreview, "items": items})
}

type scheduleSeasonRequest struct {
	Kind          string    `json:"kind"`
	RunAt         time.Time `json:"runAt"`
	NewSeasonName string    `json:"newSeasonName"`
	Announce      *bool     `json:"announce"`
	Confirm       string    `json:"confirm"`
}

// seasonActionPhrase is the typed confirmation for scheduling each kind of change.
func seasonActionPhrase(kind string) string {
	if kind == repository.SeasonActionRanked {
		return "SCHEDULE RANKED RESET"
	}
	return "SCHEDULE STATS RESET"
}

func (a *App) handleScheduleSeasonAction(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[scheduleSeasonRequest](w, r)
	if !ok {
		return
	}
	if req.Kind != repository.SeasonActionStats && req.Kind != repository.SeasonActionRanked {
		writeSaaSError(w, codeInvalidRequest, "kind must be STATS_SEASON or RANKED_RESET")
		return
	}
	if !requireConfirmation(w, req.Confirm, seasonActionPhrase(req.Kind)) {
		return
	}
	now := time.Now().UTC()
	if req.RunAt.IsZero() || req.RunAt.Before(now.Add(seasonPlanMinLead)) || req.RunAt.After(now.Add(seasonPlanMaxLead)) {
		writeSaaSError(w, codeInvalidRequest, "pick a time at least a minute from now and at most 90 days ahead")
		return
	}
	action := repository.SeasonAction{GuildID: ac.scope.GuildID, Kind: req.Kind, RunAt: req.RunAt.UTC(), Announce: req.Announce == nil || *req.Announce, CreatedBy: ac.user.DiscordUserID}
	switch req.Kind {
	case repository.SeasonActionStats:
		name := strings.TrimSpace(req.NewSeasonName)
		if name == "" || len([]rune(name)) > 80 {
			writeSaaSError(w, codeInvalidRequest, "name the new stats season (up to 80 characters)")
			return
		}
		action.NewSeasonName = name
	case repository.SeasonActionRanked:
		// Same gate as resetting Ranked by hand.
		if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.RankedSeasons) {
			return
		}
		if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
			writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
			return
		}
		server := *ac.scope.ServerID
		action.ServerID = &server
	}
	if a.SeasonPlanner == nil {
		writeSaaSError(w, codeInternalError, "season planner unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if action.Kind == repository.SeasonActionRanked {
		// Fail now rather than at run time when there is nothing to reset.
		preview, err := a.SeasonPlanner.RankedSeasonPreview(ctx, ac.scope.GuildID, *action.ServerID)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not load the Ranked season")
			return
		}
		if preview == nil {
			writeSaaSError(w, codeInvalidRequest, "this server has no active Ranked season to reset; start one first")
			return
		}
	}
	saved, err := a.SeasonPlanner.Schedule(ctx, action)
	if errors.Is(err, repository.ErrSeasonActionExists) {
		writeSaaSError(w, codeInvalidRequest, "a change like this is already scheduled; cancel it first")
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "event", "season_action_schedule_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not schedule the season change")
		return
	}
	a.recordAudit(ctx, ac, "SEASON_CHANGE_SCHEDULE", fmt.Sprintf("season_action:%d", saved.ID), saved.Kind, "success", nil,
		map[string]any{"kind": saved.Kind, "runAt": saved.RunAt, "newSeasonName": saved.NewSeasonName, "serverId": saved.ServerID, "announce": saved.Announce})
	writeSaaSJSON(w, http.StatusOK, saved)
}

func (a *App) handleCancelSeasonAction(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatsReset)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "actionID")
	if !ok {
		return
	}
	if a.SeasonPlanner == nil {
		writeSaaSError(w, codeInternalError, "season planner unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	cancelled, err := a.SeasonPlanner.Cancel(ctx, ac.scope.GuildID, id)
	if errors.Is(err, repository.ErrSeasonActionNotFound) {
		writeSaaSError(w, codeNotFound, "that season change is not pending")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not cancel the season change")
		return
	}
	a.recordAudit(ctx, ac, "SEASON_CHANGE_CANCEL", fmt.Sprintf("season_action:%d", id), cancelled.Kind, "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, cancelled)
}

// runSeasonPlanner posts "scheduled" notices and runs due season changes for one guild. Called
// from the competitive scheduler tick; ClaimDue guarantees each change runs at most once.
func (a *App) runSeasonPlanner(ctx context.Context, guildID int64, now time.Time) {
	if a.SeasonPlanner == nil {
		return
	}
	if a.CompletionPublisher != nil {
		if notices, err := a.SeasonPlanner.PendingNotices(ctx, guildID, 10); err == nil {
			for _, n := range notices {
				if err := a.CompletionPublisher.Announce(ctx, discord.BuildSeasonPlanEmbed(seasonPlanCard(n, "SCHEDULED"))); err != nil {
					slog.Warn("component=season_planner", "msg", "notice failed", "action_id", n.ID, "err", err.Error())
					continue
				}
				_ = a.SeasonPlanner.MarkNoticeSent(ctx, guildID, n.ID, now)
			}
		}
	}
	due, err := a.SeasonPlanner.ClaimDue(ctx, guildID, now, 5)
	if err != nil {
		slog.Warn("component=season_planner", "msg", "claim failed", "err", err.Error())
		return
	}
	for _, action := range due {
		detail, runErr := a.executeSeasonAction(ctx, action, now)
		status := "DONE"
		if runErr != nil {
			status, detail = "FAILED", runErr.Error()
			slog.Warn("component=season_planner", "msg", "season change failed", "action_id", action.ID, "kind", action.Kind, "err", runErr.Error())
		} else {
			slog.Info("component=season_planner", "msg", "season change applied", "action_id", action.ID, "kind", action.Kind)
		}
		if err := a.SeasonPlanner.Finish(ctx, guildID, action.ID, status, detail, now); err != nil {
			slog.Warn("component=season_planner", "msg", "finish stamp failed", "action_id", action.ID, "err", err.Error())
		}
		if runErr == nil && action.Announce && a.CompletionPublisher != nil {
			if err := a.CompletionPublisher.Announce(ctx, discord.BuildSeasonPlanEmbed(seasonPlanCard(action, "DONE"))); err != nil {
				slog.Warn("component=season_planner", "msg", "done card failed", "action_id", action.ID, "err", err.Error())
			}
		}
	}
}

// executeSeasonAction applies one change and returns a short owner-facing result line.
func (a *App) executeSeasonAction(ctx context.Context, action repository.SeasonAction, now time.Time) (string, error) {
	switch action.Kind {
	case repository.SeasonActionStats:
		if a.Seasons == nil {
			return "", errors.New("stats season system unavailable")
		}
		// Archive exactly as /season end does (results row + completion card on the next
		// tick's recovery), then open the named new season.
		archived := ""
		if active, err := a.Seasons.GetActiveSeason(ctx, action.GuildID); err == nil && active != nil {
			if _, err := a.Seasons.FinalizeSeason(ctx, action.GuildID, active.ID, now); err != nil {
				return "", fmt.Errorf("could not archive %q", active.Name)
			}
			archived = active.Name
		}
		season, err := a.Seasons.Start(ctx, action.GuildID, action.NewSeasonName, now)
		if err != nil {
			return "", fmt.Errorf("archived %q but could not start %q", archived, action.NewSeasonName)
		}
		if archived == "" {
			return fmt.Sprintf("Started %q.", season.Name), nil
		}
		return fmt.Sprintf("Archived %q and started %q.", archived, season.Name), nil
	case repository.SeasonActionRanked:
		if a.Ranked == nil || action.ServerID == nil {
			return "", errors.New("ranked season system unavailable")
		}
		current, err := a.Ranked.ActiveServerSeason(ctx, action.GuildID, *action.ServerID)
		if err != nil {
			return "", errors.New("could not load the Ranked season")
		}
		if current == nil {
			return "", errors.New("no active Ranked season to reset")
		}
		next, err := a.Ranked.StartServerSeason(ctx, action.GuildID, *action.ServerID, current.RPPerKill, current.Thresholds, true, now)
		if err != nil {
			return "", fmt.Errorf("could not reset Ranked: %s", err.Error())
		}
		return fmt.Sprintf("Archived Ranked season %d and started season %d with the same rules.", current.ID, next.ID), nil
	}
	return "", fmt.Errorf("unknown season change %q", action.Kind)
}

func seasonPlanCard(action repository.SeasonAction, stage string) discord.SeasonPlanCard {
	return discord.SeasonPlanCard{Stage: stage, Kind: action.Kind, RunAt: action.RunAt, NewSeasonName: action.NewSeasonName, ServerName: action.ServerName}
}
