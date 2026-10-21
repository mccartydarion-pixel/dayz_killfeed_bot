package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 21, daily play reward: champ credits for each UTC day a player is seen on the server
// (a real connection, not backfilled history), plus a bonus for each day in a row up to seven.
// Each player is paid once a day: the payment's reference is the day, so a repeat is a no-op.

const (
	dailyRewardEvery = 10 * time.Minute
	dailyStreakCap   = 7
)

// playStreak is how many days in a row up to and including days[0] (newest first, one per day).
func playStreak(days []time.Time) int {
	if len(days) == 0 {
		return 0
	}
	n := 1
	for i := 1; i < len(days) && n < dailyStreakCap; i++ {
		if days[i-1].Sub(days[i]) != 24*time.Hour {
			break
		}
		n++
	}
	return n
}

// dailyReward is what a day pays: the base plus the bonus for each day of streak after the first.
func dailyReward(base, bonus int64, streak int) int64 {
	if streak < 1 {
		return 0
	}
	return base + bonus*int64(streak-1)
}

func (a *App) runDailyPlayReward(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	if s.Settings.DailyLoginCredits <= 0 || a.EconomyService == nil || !a.upgradeRuns.due(fmt.Sprintf("daily:%d", s.ServerID), dailyRewardEvery, now) {
		return
	}
	day := now.UTC().Truncate(24 * time.Hour)
	ref := day.Format("2006-01-02")
	players, err := a.Upgrades.PlayDays(ctx, s.ServerID, day)
	if err != nil {
		slog.Warn("component=upgrades", "msg", "daily reward list failed", "server_id", s.ServerID, "err", err.Error())
		return
	}
	paid := 0
	for playerID, days := range players {
		claimed, err := a.Upgrades.ClaimNotice(ctx, "DAILY_PLAY", s.ServerID, playerID, ref, now)
		if err != nil || !claimed {
			continue
		}
		streak := playStreak(days)
		amount := dailyReward(s.Settings.DailyLoginCredits, s.Settings.DailyLoginStreakBonus, streak)
		desc := "Daily play reward"
		if streak > 1 {
			desc = fmt.Sprintf("Daily play reward (%d days in a row)", streak)
		}
		if _, err := a.EconomyService.Credit(ctx, economy.Request{GuildID: guildID, ServerID: s.ServerID, PlayerID: playerID, Amount: amount,
			Type: economy.TypeSystemReward, ReferenceID: fmt.Sprintf("reward:daily:%d:%s", s.ServerID, ref), Description: desc, Actor: "SYSTEM"}); err != nil {
			slog.Warn("component=upgrades", "msg", "daily reward failed", "player_id", playerID, "err", err.Error())
			continue
		}
		paid++
	}
	if paid > 0 {
		slog.Info("component=upgrades", "event", "daily_rewards_paid", "server_id", s.ServerID, "players", paid)
	}
}
