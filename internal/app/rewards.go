package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/rewards"
	"github.com/yourname/dayz-killfeed/internal/vip"
)

// Rewards automation (docs/CLIENT_HUB_GROWTH.md): owners set Champion Point rewards for reaching
// a Ranked tier, a weekly activity streak and a stats season top N; Champion pays them through
// the economy ledger, once each.

// rewardsInterval is how often a guild's rules are evaluated. Payouts are idempotent, so the
// interval only bounds the work, never the correctness.
const rewardsInterval = 10 * time.Minute

var rewardsLastRun sync.Map // guildID -> time.Time

func (a *App) registerRewardRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/rewards", a.handleRewards)
	h("PUT "+adminBase+"/rewards/rules", a.handleSaveRewardRule)
	h("DELETE "+adminBase+"/rewards/rules/{ruleID}", a.handleDeleteRewardRule)
}

func (a *App) handleRewards(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapRewardsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Rewards == nil {
		writeSaaSError(w, codeInternalError, "rewards unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rules, err := a.Rewards.ListRules(ctx, ac.scope.GuildID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load reward rules")
		return
	}
	payouts, err := a.Rewards.RecentPayouts(ctx, ac.scope.GuildID, 50)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load reward payouts")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"rules": rules, "payouts": payouts, "tiers": rewards.Tiers})
}

func (a *App) handleSaveRewardRule(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapRewardsManage)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.Economy) {
		return
	}
	req, ok := decodeJSONBody[rewards.Rule](w, r)
	if !ok {
		return
	}
	rule, err := rewards.Normalize(req)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	if a.Rewards == nil {
		writeSaaSError(w, codeInternalError, "rewards unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	saved, err := a.Rewards.UpsertRule(ctx, ac.scope.GuildID, rule, ac.user.DiscordUserID)
	if err != nil {
		slog.Warn("component=saas_api", "event", "reward_rule_save_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save the reward")
		return
	}
	a.recordAudit(ctx, ac, "REWARD_RULE_SAVE", fmt.Sprintf("reward_rule:%d", saved.ID), saved.Kind, "success", nil, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

func (a *App) handleDeleteRewardRule(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapRewardsManage)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "ruleID")
	if !ok {
		return
	}
	if a.Rewards == nil {
		writeSaaSError(w, codeInternalError, "rewards unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	deleted, err := a.Rewards.DeleteRule(ctx, ac.scope.GuildID, id)
	if errors.Is(err, repository.ErrRewardRuleNotFound) {
		writeSaaSError(w, codeNotFound, "reward not found")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not delete the reward")
		return
	}
	a.recordAudit(ctx, ac, "REWARD_RULE_DELETE", fmt.Sprintf("reward_rule:%d", id), deleted.Kind, "success", deleted, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// runRewards evaluates a guild's reward rules at most every rewardsInterval.
func (a *App) runRewards(ctx context.Context, guildID int64, now time.Time) {
	if a.Rewards == nil {
		return
	}
	if last, ok := rewardsLastRun.Load(guildID); ok && now.Sub(last.(time.Time)) < rewardsInterval {
		return
	}
	rewardsLastRun.Store(guildID, now)
	paid, err := a.evaluateRewards(ctx, guildID, now)
	if err != nil {
		slog.Warn("component=rewards", "msg", "reward evaluation failed", "err", err.Error())
	}
	if paid > 0 {
		slog.Info("component=rewards", "msg", "rewards paid", "count", paid)
	}
}

// evaluateRewards pays every reward earned and not yet paid. History from before a rule was
// created (weeks that ended, seasons that ended) is never paid; a Ranked tier pays whoever holds
// it in the current season.
func (a *App) evaluateRewards(ctx context.Context, guildID int64, now time.Time) (int, error) {
	rules, err := a.Rewards.ListRules(ctx, guildID)
	if err != nil {
		return 0, err
	}
	var grants []repository.RewardGrant
	var holders []repository.RankHolder
	holdersLoaded := false
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		switch rule.Kind {
		case rewards.KindRankReached:
			if !holdersLoaded {
				if holders, err = a.Rewards.RankHolders(ctx, guildID); err != nil {
					return 0, err
				}
				holdersLoaded = true
			}
			for _, h := range holders {
				if rewards.AtLeast(h.Tier, ranked.Tier(rule.Tier)) {
					grants = append(grants, repository.RewardGrant{PlayerID: h.PlayerID, Amount: rule.Points, Reference: rewards.RankReference(h.SeasonID, rule.Tier), Description: rewards.Describe(rule, 0)})
				}
			}
		case rewards.KindWeeklyActive:
			start, end, label := rewards.LastFullWeek(now)
			if !end.After(rule.CreatedAt) {
				continue
			}
			players, err := a.Rewards.WeeklyActive(ctx, guildID, start, end, rule.MinHours, rule.MinDays)
			if err != nil {
				return 0, err
			}
			for _, p := range players {
				grants = append(grants, repository.RewardGrant{PlayerID: p, Amount: rule.Points, Reference: rewards.WeeklyReference(label), Description: rewards.Describe(rule, 0)})
			}
		case rewards.KindSeasonTop:
			places, err := a.Rewards.SeasonTop(ctx, guildID, rule.CreatedAt, rule.Places)
			if err != nil {
				return 0, err
			}
			for _, p := range places {
				grants = append(grants, repository.RewardGrant{PlayerID: p.PlayerID, Amount: rule.Points, Reference: rewards.SeasonReference(p.SeasonID), Description: rewards.Describe(rule, p.Place)})
			}
		}
	}
	grants = a.applyRewardMultipliers(ctx, guildID, grants)
	return a.Rewards.Pay(ctx, guildID, grants)
}

// applyRewardMultipliers raises each reward by the player's active supporter tier multiplier
// (vip.go). Without tiers, or if they cannot be read, rewards pay at face value.
func (a *App) applyRewardMultipliers(ctx context.Context, guildID int64, grants []repository.RewardGrant) []repository.RewardGrant {
	if a.VIP == nil || len(grants) == 0 {
		return grants
	}
	multipliers, err := a.VIP.Multipliers(ctx, guildID)
	if err != nil {
		slog.Warn("component=rewards", "msg", "VIP multipliers unavailable; paying face value", "err", err.Error())
		return grants
	}
	for i := range grants {
		if m, ok := multipliers[grants[i].PlayerID]; ok {
			grants[i].Amount = vip.Multiply(grants[i].Amount, m)
		}
	}
	return grants
}
