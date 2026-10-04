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
	"github.com/yourname/dayz-killfeed/internal/progression"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The battle pass (docs/PROGRESSION.md "Battle pass").

const (
	battlePassColor       = 0xA855F7
	battlePassGrantsBatch = 200
	// battlePassXPLookback is how far back each pass looks for kills and playtime to turn into XP;
	// the references make every piece count once, so overlapping passes are harmless.
	battlePassXPLookback = 48 * time.Hour
)

func battlePassAuthor() *discordgo.MessageEmbedAuthor {
	return &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® BATTLE PASS"}
}

func battlePassMessage(err error) string {
	switch {
	case errors.Is(err, repository.ErrBattlePassRewardsEmpty):
		return "add at least one reward"
	case errors.Is(err, repository.ErrBattlePassInvalid):
		return "check the season: a name, an end after the start (at most a year), 5 to 100 levels, 100 to 100,000 XP a level, and every reward on a level the season has, at most one per level and track"
	case errors.Is(err, repository.ErrBattlePassOpen):
		return "end the current season before starting a new one"
	case errors.Is(err, repository.ErrBattlePassNotFound):
		return "that season is not running"
	}
	return ""
}

func (a *App) handleAdminBattlePass(w http.ResponseWriter, r *http.Request) {
	_, serverID, ok := a.progressionAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	season, err := a.BattlePass.LatestSeason(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the battle pass")
		return
	}
	resp := map[string]any{"season": season, "defaults": repository.DefaultBattlePassSeason(now), "stats": nil, "leaderboard": []repository.BattlePassProgress{}}
	if season != nil {
		if st, err := a.BattlePass.Stats(ctx, *season); err == nil {
			resp["stats"] = st
		}
		if lb, err := a.BattlePass.Leaderboard(ctx, *season, 10); err == nil {
			resp["leaderboard"] = lb
		}
		resp["running"] = season.Running(now)
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

func (a *App) handleCreateBattlePass(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.progressionAdmin(w, r, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.BattlePassSeason](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	if req.StartsAt.IsZero() || req.StartsAt.Before(now) {
		req.StartsAt = now
	}
	if req.Rewards == nil {
		req.Rewards = progression.DefaultRewards(req.Levels)
	}
	req.ServerID, req.GuildID, req.InstallationID = serverID, ac.scope.GuildID, ac.scope.InstallationID
	saved, err := a.BattlePass.CreateSeason(ctx, req, ac.user.DiscordUserID)
	if msg := battlePassMessage(err); msg != "" {
		code := codeInvalidRequest
		if errors.Is(err, repository.ErrBattlePassOpen) {
			code = codeConflict
		}
		writeSaaSError(w, code, msg)
		return
	}
	if err != nil {
		progressionWarn("battle pass create", serverID, err)
		writeSaaSError(w, codeInternalError, "could not start the season")
		return
	}
	a.recordAudit(ctx, ac, "BATTLE_PASS_START", fmt.Sprintf("server:%d", serverID), "", "success", nil, map[string]any{"id": saved.ID, "name": saved.Name, "levels": saved.Levels, "endsAt": saved.EndsAt})
	writeSaaSJSON(w, http.StatusOK, saved)
}

func (a *App) handleUpdateBattlePass(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.progressionAdmin(w, r, true)
	if !ok {
		return
	}
	seasonID, ok := pathInt64(w, r, "seasonID")
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.BattlePassSeason](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	current, err := a.BattlePass.Season(ctx, serverID, seasonID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the season")
		return
	}
	if current == nil || current.EndedAt != nil {
		writeSaaSError(w, codeNotFound, "that season is not running")
		return
	}
	// A season that has started keeps its start.
	if !current.StartsAt.After(time.Now().UTC()) || req.StartsAt.IsZero() {
		req.StartsAt = current.StartsAt
	}
	req.ID, req.ServerID = seasonID, serverID
	saved, err := a.BattlePass.UpdateSeason(ctx, req)
	if msg := battlePassMessage(err); msg != "" {
		code := codeInvalidRequest
		if errors.Is(err, repository.ErrBattlePassNotFound) {
			code = codeNotFound
		}
		writeSaaSError(w, code, msg)
		return
	}
	if err != nil {
		progressionWarn("battle pass update", serverID, err)
		writeSaaSError(w, codeInternalError, "could not save the season")
		return
	}
	a.recordAudit(ctx, ac, "BATTLE_PASS_UPDATE", fmt.Sprintf("server:%d", serverID), "", "success", current, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

func (a *App) handleEndBattlePass(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.progressionAdmin(w, r, true)
	if !ok {
		return
	}
	seasonID, ok := pathInt64(w, r, "seasonID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	err := a.BattlePass.EndSeason(ctx, serverID, seasonID, time.Now().UTC())
	if errors.Is(err, repository.ErrBattlePassNotFound) {
		writeSaaSError(w, codeNotFound, "that season is not running")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not end the season")
		return
	}
	a.recordAudit(ctx, ac, "BATTLE_PASS_END", fmt.Sprintf("server:%d", serverID), "", "success", nil, map[string]any{"id": seasonID})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"ended": true})
}

type battlePassRewardDTO struct {
	progression.Reward
	Reached bool `json:"reached"`
	Granted bool `json:"granted"`
}

// handlePlayerBattlePass is GET .../player/servers/{installationID}/battle-pass: the season, the
// player's level, XP and rewards, the leaderboard and the titles and badges they own.
func (a *App) handlePlayerBattlePass(w http.ResponseWriter, r *http.Request) {
	pc, ok := a.progressionPlayer(w, r, false)
	if !ok {
		return
	}
	defer pc.cancel()
	ctx, scope := pc.ctx, pc.scope
	now := time.Now().UTC()
	cosmetics, choice, err := a.BattlePass.Cosmetics(ctx, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "cosmetics", err)
		return
	}
	resp := map[string]any{"season": nil, "cosmetics": cosmetics, "choice": choice}
	season, err := a.BattlePass.LatestSeason(ctx, scope.ServerID)
	if err != nil {
		playerFailed(w, "battle pass", err)
		return
	}
	// A season that ended more than two weeks ago is history, not something to show.
	if season == nil || (season.EndedAt != nil && now.Sub(*season.EndedAt) > 14*24*time.Hour) {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	me, err := a.BattlePass.PlayerProgress(ctx, *season, scope.PlayerID)
	if err != nil {
		playerFailed(w, "battle pass progress", err)
		return
	}
	granted := map[[2]any]bool{}
	if list, err := a.BattlePass.PlayerGrants(ctx, season.ID, scope.PlayerID); err == nil {
		for _, g := range list {
			granted[[2]any{g.Level, g.Track}] = true
		}
	}
	rewards := make([]battlePassRewardDTO, 0, len(season.Rewards))
	for _, rw := range season.Rewards {
		rewards = append(rewards, battlePassRewardDTO{Reward: rw, Reached: rw.Level <= me.Level, Granted: granted[[2]any{rw.Level, rw.Track}]})
	}
	board, _ := a.BattlePass.Leaderboard(ctx, *season, 10)
	resp["season"] = season
	resp["running"] = season.Running(now)
	resp["me"] = me
	resp["rewards"] = rewards
	resp["leaderboard"] = board
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handleBuyBattlePassPremium is POST .../battle-pass/premium: the player unlocks the premium track
// of the running season with Champion Points. Rewards of levels already reached follow on the next
// scheduler pass.
func (a *App) handleBuyBattlePassPremium(w http.ResponseWriter, r *http.Request) {
	pc, ok := a.progressionPlayer(w, r, true)
	if !ok {
		return
	}
	defer pc.cancel()
	ctx, scope := pc.ctx, pc.scope
	season, err := a.BattlePass.OpenSeason(ctx, scope.ServerID)
	if err != nil {
		playerFailed(w, "battle pass", err)
		return
	}
	if season == nil {
		writeSaaSError(w, codeNotFound, "there is no battle pass season running")
		return
	}
	now := time.Now().UTC()
	_, balance, err := a.BattlePass.BuyPremium(ctx, repository.PerkScope{GuildID: scope.GuildID, InstallationID: scope.InstallationID, ServerID: scope.ServerID},
		season.ID, scope.PlayerID, now)
	switch {
	case errors.Is(err, repository.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "you don't have enough Champion Points")
		return
	case errors.Is(err, repository.ErrBattlePassHasPremium), errors.Is(err, repository.ErrBattlePassNoPremium), errors.Is(err, repository.ErrBattlePassClosed):
		writeSaaSError(w, codeConflict, err.Error())
		return
	case errors.Is(err, repository.ErrBattlePassNotFound):
		writeSaaSError(w, codeNotFound, "there is no battle pass season running")
		return
	case err != nil:
		playerFailed(w, "battle pass premium", err)
		return
	}
	// Grant the premium rewards of levels already reached now, not on the next pass.
	if _, err := a.BattlePass.GrantLevels(ctx, season.ID, now); err == nil {
		a.deliverBattlePassGrants(ctx, *season, now)
	}
	slog.Info("component=progression", "event", "battle_pass_premium_bought", "season_id", season.ID, "player_id", scope.PlayerID)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"premium": true, "balance": balance})
}

// handleSetCosmetics is PUT .../player/servers/{installationID}/cosmetics: the title and badge the
// player shows ("" for none).
func (a *App) handleSetCosmetics(w http.ResponseWriter, r *http.Request) {
	pc, ok := a.progressionPlayer(w, r, true)
	if !ok {
		return
	}
	defer pc.cancel()
	req, ok := decodeJSONBody[repository.CosmeticChoice](w, r)
	if !ok {
		return
	}
	err := a.BattlePass.SetCosmetics(pc.ctx, pc.scope.GuildID, pc.scope.ServerID, pc.scope.PlayerID, req, time.Now().UTC())
	if errors.Is(err, repository.ErrCosmeticNotOwned) {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	if err != nil {
		playerFailed(w, "cosmetics", err)
		return
	}
	_, choice, err := a.BattlePass.Cosmetics(pc.ctx, pc.scope.ServerID, pc.scope.PlayerID)
	if err != nil {
		playerFailed(w, "cosmetics", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, choice)
}

// runBattlePass turns kills and playtime into XP, grants reached rewards and posts the season's
// start and end cards.
func (a *App) runBattlePass(ctx context.Context, guildID int64, now time.Time) {
	seasons, err := a.BattlePass.GuildSeasons(ctx, guildID, now)
	if err != nil {
		slog.Warn("component=progression", "msg", "battle pass seasons failed", "err", err.Error())
		return
	}
	for _, s := range seasons {
		if !a.upgradeRuns.due(fmt.Sprintf("battle-pass:%d", s.ID), battlePassEvery, now) {
			continue
		}
		if err := a.BattlePass.IngestXP(ctx, s, now.Add(-battlePassXPLookback), now); err != nil {
			progressionWarn("battle pass xp", s.ServerID, err)
			continue
		}
		if _, err := a.BattlePass.GrantLevels(ctx, s.ID, now); err != nil {
			progressionWarn("battle pass grants", s.ServerID, err)
			continue
		}
		a.deliverBattlePassGrants(ctx, s, now)
		serverName := a.serverName(s.ServerID)
		if s.EndedAt == nil && s.Running(now) {
			if claimed, err := a.BattlePass.ClaimAnnouncement(ctx, s.ID, false, now); err == nil && claimed {
				a.postProgressionCard(ctx, guildID, s.ServerID, buildBattlePassStartCard(s, serverName))
			}
		}
		if s.EndedAt != nil {
			if claimed, err := a.BattlePass.ClaimAnnouncement(ctx, s.ID, true, now); err == nil && claimed {
				if board, err := a.BattlePass.Leaderboard(ctx, s, 3); err == nil && len(board) > 0 {
					a.postProgressionCard(ctx, guildID, s.ServerID, buildBattlePassEndCard(s, board, serverName))
				}
			}
		}
	}
}

// deliverBattlePassGrants pays the season's undelivered rewards: points through the ledger (one
// reference per level and track, so a retry never pays twice), titles and badges to the player.
func (a *App) deliverBattlePassGrants(ctx context.Context, s repository.BattlePassSeason, now time.Time) {
	grants, err := a.BattlePass.UnpaidGrants(ctx, s.ID, battlePassGrantsBatch)
	if err != nil {
		progressionWarn("battle pass unpaid grants", s.ServerID, err)
		return
	}
	for _, g := range grants {
		if g.Kind == progression.RewardPoints && g.Amount > 0 && a.EconomyService != nil {
			if _, err := a.EconomyService.Credit(ctx, economy.Request{GuildID: g.GuildID, ServerID: g.ServerID, PlayerID: g.PlayerID, Amount: g.Amount,
				Type: economy.TypeSystemReward, ReferenceID: fmt.Sprintf("bp:%d:%d:%s", g.SeasonID, g.Level, g.Track),
				Description: fmt.Sprintf("Battle pass %s · level %d", g.SeasonName, g.Level)}); err != nil {
				progressionWarn("battle pass payment", g.ServerID, err)
				continue
			}
		}
		if err := a.BattlePass.DeliverGrant(ctx, g, now); err != nil {
			progressionWarn("battle pass delivery", g.ServerID, err)
		}
	}
}

func buildBattlePassStartCard(s repository.BattlePassSeason, serverName string) *discordgo.MessageEmbed {
	where := ""
	if strings.TrimSpace(serverName) != "" {
		where = " on " + serverName
	}
	ways := []string{}
	if s.XPKill > 0 {
		ways = append(ways, fmt.Sprintf("%d XP a kill", s.XPKill))
	}
	if s.XPHour > 0 {
		ways = append(ways, fmt.Sprintf("%d XP an hour played", s.XPHour))
	}
	if s.XPDailyChallenge > 0 || s.XPWeeklyChallenge > 0 {
		ways = append(ways, "XP for every challenge")
	}
	desc := fmt.Sprintf("**%s** is live%s until %s: %d levels of rewards.", s.Name, where, s.EndsAt.UTC().Format("Jan 2"), s.Levels)
	if len(ways) > 0 {
		desc += "\nEarn " + strings.Join(ways, ", ") + "."
	}
	if s.PremiumPrice > 0 {
		desc += fmt.Sprintf("\nUnlock the premium track for **%s** in the Player Hub.", pointsText(s.PremiumPrice))
	}
	return &discordgo.MessageEmbed{Author: battlePassAuthor(), Color: battlePassColor, Title: "🎟️ New battle pass season", Description: desc}
}

func buildBattlePassEndCard(s repository.BattlePassSeason, board []repository.BattlePassProgress, serverName string) *discordgo.MessageEmbed {
	medals := []string{"🥇", "🥈", "🥉"}
	lines := []string{}
	for i, p := range board {
		if i >= len(medals) {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s · level %d (%s XP)", medals[i], orUnknown(p.Name), p.Level, commaInt(p.XP)))
	}
	title := "🏁 " + s.Name + " has ended"
	if strings.TrimSpace(serverName) != "" {
		title += " · " + serverName
	}
	return &discordgo.MessageEmbed{Author: battlePassAuthor(), Color: battlePassColor, Title: title,
		Description: "Thanks for playing! Top of the season:\n" + strings.Join(lines, "\n")}
}
