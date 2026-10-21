package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/progression"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Daily and weekly challenges (docs/PROGRESSION.md "Challenges").

// challengeGrace keeps checking a period this long after it ended, so a kill that reaches the bot
// a few minutes late (Nitrado writes logs in steps) still completes yesterday's challenge.
const challengeGrace = 30 * time.Minute

type challengeDTO struct {
	progression.Challenge
	Index     int  `json:"index"`
	Progress  int  `json:"progress"`
	Done      bool `json:"done"`
	Completed int  `json:"completedBy,omitempty"`
}

type challengeSetDTO struct {
	Period     string         `json:"period"`
	StartsAt   string         `json:"startsAt"`
	EndsAt     string         `json:"endsAt"`
	Points     int64          `json:"points"`
	XP         int            `json:"xp"`
	Challenges []challengeDTO `json:"challenges"`
}

func (a *App) challengeFeatures(ctx context.Context, serverID int64) progression.Features {
	var f progression.Features
	if t, err := a.Territory.Settings(ctx, serverID); err == nil {
		f.Territory = t.Enabled
	}
	var hot bool
	if err := a.DB.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT s.hot_zones_enabled FROM installations i JOIN installation_feature_settings s ON s.installation_id=i.id
WHERE i.game_server_id=$1 LIMIT 1),FALSE)`, serverID).Scan(&hot); err == nil {
		f.HotZones = hot
	}
	return f
}

// challengeXP is the battle pass XP a challenge of the period earns right now (0 without a season).
func (a *App) challengeXP(ctx context.Context, serverID int64, period string, now time.Time) int {
	s, err := a.BattlePass.OpenSeason(ctx, serverID)
	if err != nil || s == nil || !s.Running(now) {
		return 0
	}
	if period == progression.PeriodWeek {
		return s.XPWeeklyChallenge
	}
	return s.XPDailyChallenge
}

func (a *App) handleAdminChallenges(w http.ResponseWriter, r *http.Request) {
	_, serverID, ok := a.progressionAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s, err := a.Challenges.Settings(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load challenges")
		return
	}
	now := time.Now().UTC()
	f := a.challengeFeatures(ctx, serverID)
	sets := []challengeSetDTO{}
	for _, p := range []struct {
		period string
		count  int
		points int64
	}{{progression.PeriodDay, s.DailyCount, s.DailyPoints}, {progression.PeriodWeek, s.WeeklyCount, s.WeeklyPoints}} {
		if p.count == 0 {
			continue
		}
		start := progression.PeriodStart(p.period, now)
		dto := challengeSetDTO{Period: p.period, StartsAt: rfc3339(start), EndsAt: rfc3339(progression.PeriodEnd(p.period, start)), Points: p.points,
			XP: a.challengeXP(ctx, serverID, p.period, now), Challenges: []challengeDTO{}}
		// While challenges are off the owner sees what today would draw, without storing it.
		list := progression.Generate(serverID, p.period, start, p.count, f)
		counts := map[int]int{}
		if s.Enabled {
			if set, err := a.Challenges.FindSet(ctx, serverID, p.period, now); err == nil && set != nil {
				list = set.Challenges
				counts, _ = a.Challenges.CompletionCounts(ctx, set.ID)
			}
		}
		for i, c := range list {
			dto.Challenges = append(dto.Challenges, challengeDTO{Challenge: c, Index: i, Completed: counts[i]})
		}
		sets = append(sets, dto)
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": s, "sets": sets, "features": f})
}

func (a *App) handleSaveChallenges(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.progressionAdmin(w, r, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.ChallengeSettings](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	before, _ := a.Challenges.Settings(ctx, serverID)
	saved, err := a.Challenges.SaveSettings(ctx, serverID, req, ac.user.DiscordUserID, time.Now().UTC())
	if errors.Is(err, repository.ErrChallengeSettingsInvalid) {
		writeSaaSError(w, codeInvalidRequest, "1 to 5 daily and 0 to 4 weekly challenges, paying 0 to 1,000,000 points each")
		return
	}
	if err != nil {
		progressionWarn("challenge settings save", serverID, err)
		writeSaaSError(w, codeInternalError, "could not save challenges")
		return
	}
	a.recordAudit(ctx, ac, "CHALLENGES_SAVE", fmt.Sprintf("server:%d", serverID), "", "success", before, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

// handlePlayerChallenges is GET .../player/servers/{installationID}/challenges: today's and this
// week's challenges with the acting player's progress.
func (a *App) handlePlayerChallenges(w http.ResponseWriter, r *http.Request) {
	pc, ok := a.progressionPlayer(w, r, false)
	if !ok {
		return
	}
	defer pc.cancel()
	ctx, scope := pc.ctx, pc.scope
	s, err := a.Challenges.Settings(ctx, scope.ServerID)
	if err != nil {
		playerFailed(w, "challenges", err)
		return
	}
	resp := map[string]any{"enabled": s.Enabled, "sets": []challengeSetDTO{}}
	if !s.Enabled {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	now := time.Now().UTC()
	f := a.challengeFeatures(ctx, scope.ServerID)
	sets := []challengeSetDTO{}
	for _, p := range []struct {
		period string
		count  int
		points int64
	}{{progression.PeriodDay, s.DailyCount, s.DailyPoints}, {progression.PeriodWeek, s.WeeklyCount, s.WeeklyPoints}} {
		if p.count == 0 {
			continue
		}
		set, err := a.Challenges.EnsureSet(ctx, scope.ServerID, p.period, now, p.count, f)
		if err != nil {
			playerFailed(w, "challenge set", err)
			return
		}
		done := map[int]bool{}
		if list, err := a.Challenges.PlayerCompletions(ctx, []int64{set.ID}, scope.PlayerID); err == nil {
			for _, c := range list {
				done[c.Idx] = true
			}
		}
		dto := challengeSetDTO{Period: p.period, StartsAt: rfc3339(set.StartsAt), EndsAt: rfc3339(set.EndsAt), Points: p.points,
			XP: a.challengeXP(ctx, scope.ServerID, p.period, now), Challenges: []challengeDTO{}}
		for i, c := range set.Challenges {
			progress := 0
			if m, err := a.Challenges.Progress(ctx, scope.GuildID, scope.ServerID, c, set.StartsAt, set.EndsAt, scope.PlayerID, now); err == nil {
				progress = m[scope.PlayerID]
			}
			if progress > c.Target {
				progress = c.Target
			}
			if done[i] {
				progress = c.Target
			}
			dto.Challenges = append(dto.Challenges, challengeDTO{Challenge: c, Index: i, Progress: progress, Done: done[i]})
		}
		sets = append(sets, dto)
	}
	resp["sets"] = sets
	writeSaaSJSON(w, http.StatusOK, resp)
}

// runChallenges draws each enabled server's sets, records who finished what and pays them.
func (a *App) runChallenges(ctx context.Context, guildID int64, now time.Time) {
	servers, err := a.Challenges.EnabledServers(ctx, guildID)
	if err != nil {
		slog.Warn("component=progression", "msg", "challenge servers failed", "err", err.Error())
		return
	}
	for _, cs := range servers {
		if !a.upgradeRuns.due(fmt.Sprintf("challenges:%d", cs.ServerID), challengesEvery, now) {
			continue
		}
		f := progression.Features{HotZones: cs.HotZones, Territory: cs.Territory}
		for _, p := range []struct {
			period string
			count  int
			points int64
		}{{progression.PeriodDay, cs.Settings.DailyCount, cs.Settings.DailyPoints}, {progression.PeriodWeek, cs.Settings.WeeklyCount, cs.Settings.WeeklyPoints}} {
			if p.count == 0 {
				continue
			}
			set, err := a.Challenges.EnsureSet(ctx, cs.ServerID, p.period, now, p.count, f)
			if err != nil {
				progressionWarn("challenge set", cs.ServerID, err)
				continue
			}
			if cs.Settings.Announce {
				if claimed, err := a.Challenges.ClaimAnnouncement(ctx, set.ID, now); err == nil && claimed {
					a.postProgressionCard(ctx, guildID, cs.ServerID, buildChallengesCard(set, p.points, a.serverName(cs.ServerID)))
				}
			}
			a.completeChallenges(ctx, guildID, cs.ServerID, set, p.points, now)
			// The period that just ended, for kills that arrive late.
			if prev, err := a.Challenges.FindSet(ctx, cs.ServerID, p.period, now.Add(-challengeGrace)); err == nil && prev != nil && prev.ID != set.ID {
				a.completeChallenges(ctx, guildID, cs.ServerID, *prev, p.points, now)
			}
		}
	}
	a.payChallenges(ctx, guildID, now)
}

func (a *App) completeChallenges(ctx context.Context, guildID, serverID int64, set repository.ChallengeSet, points int64, now time.Time) {
	for i, c := range set.Challenges {
		progress, err := a.Challenges.Progress(ctx, guildID, serverID, c, set.StartsAt, set.EndsAt, 0, now)
		if err != nil {
			progressionWarn("challenge progress", serverID, err)
			return
		}
		done, err := a.Challenges.Completed(ctx, set.ID, i)
		if err != nil {
			progressionWarn("challenge completions", serverID, err)
			return
		}
		var finished []int64
		for player, n := range progress {
			if n >= c.Target && !done[player] {
				finished = append(finished, player)
			}
		}
		if n, err := a.Challenges.Complete(ctx, guildID, set.ID, i, finished, points, now); err != nil {
			progressionWarn("challenge complete", serverID, err)
		} else if n > 0 {
			slog.Info("component=progression", "event", "challenge_completed", "server_id", serverID, "set_id", set.ID, "index", i, "players", n)
		}
	}
}

// payChallenges credits the guild's unpaid completions and adds their battle pass XP.
func (a *App) payChallenges(ctx context.Context, guildID int64, now time.Time) {
	unpaid, err := a.Challenges.Unpaid(ctx, guildID, 200)
	if err != nil {
		slog.Warn("component=progression", "msg", "unpaid challenges failed", "err", err.Error())
		return
	}
	seasons := map[int64]*repository.BattlePassSeason{}
	for _, c := range unpaid {
		if c.Points > 0 && a.EconomyService != nil {
			if _, err := a.EconomyService.Credit(ctx, economy.Request{GuildID: c.GuildID, ServerID: c.ServerID, PlayerID: c.PlayerID, Amount: c.Points,
				Type: economy.TypeSystemReward, ReferenceID: fmt.Sprintf("challenge:%d:%d", c.SetID, c.Idx), Description: "Challenge: " + c.Title}); err != nil {
				progressionWarn("challenge payment", c.ServerID, err)
				continue
			}
		}
		season, seen := seasons[c.ServerID]
		if !seen {
			season, _ = a.BattlePass.OpenSeason(ctx, c.ServerID)
			seasons[c.ServerID] = season
		}
		if season != nil && season.Running(c.CompletedAt) {
			xp, source := season.XPDailyChallenge, "DAILY"
			if c.Period == progression.PeriodWeek {
				xp, source = season.XPWeeklyChallenge, "WEEKLY"
			}
			if err := a.BattlePass.AddXP(ctx, season.ID, c.PlayerID, source, fmt.Sprintf("%d:%d", c.SetID, c.Idx), xp, c.CompletedAt); err != nil {
				progressionWarn("challenge xp", c.ServerID, err)
				continue
			}
		}
		if err := a.Challenges.MarkPaid(ctx, c.SetID, c.Idx, c.PlayerID, now); err != nil {
			progressionWarn("challenge paid stamp", c.ServerID, err)
		}
	}
}

func buildChallengesCard(set repository.ChallengeSet, points int64, serverName string) *discordgo.MessageEmbed {
	title := "📋 Today's challenges"
	ends := "Resets at 00:00 UTC."
	if set.Period == progression.PeriodWeek {
		title = "🗓️ This week's challenges"
		ends = "Resets Monday 00:00 UTC."
	}
	lines := make([]string, 0, len(set.Challenges))
	for _, c := range set.Challenges {
		lines = append(lines, "• "+c.Title)
	}
	desc := strings.Join(lines, "\n") + "\n\n"
	if points > 0 {
		desc += fmt.Sprintf("Each one pays **%s**. ", pointsText(points))
	}
	desc += ends + " Track your progress in the Player Hub."
	// The title is the event; the server is named in the footer, like on every card.
	return &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Challenges"), Color: presentation.Crimson, Title: title, Description: desc,
		Footer: presentation.Footer(serverName, "")}
}
