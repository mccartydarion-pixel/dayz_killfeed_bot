package discord

import (
	"context"
	"log/slog"
	"time"
)

// LeaderboardRefreshInterval is the single source of truth for how often the
// persistent public leaderboard panel is automatically refreshed.
const LeaderboardRefreshInterval = 3 * time.Hour

// LeaderboardScheduler owns the guild's one persistent leaderboard panel and
// refreshes it on a fixed interval, editing the existing message in place.
// Kills never trigger a refresh directly; they only make the next scheduled
// (or manually triggered) refresh reflect the latest data.
type LeaderboardScheduler struct {
	panel       *LeaderboardPanel
	stats       StatsReader
	guildRowID  int64
	cfg         LeaderboardConfig
	onMessageID func(string)
}

// NewLeaderboardScheduler creates a scheduler bound to one guild's panel.
func NewLeaderboardScheduler(panel *LeaderboardPanel, stats StatsReader, guildRowID int64, cfg LeaderboardConfig, onMessageID func(string)) *LeaderboardScheduler {
	return &LeaderboardScheduler{panel: panel, stats: stats, guildRowID: guildRowID, cfg: cfg, onMessageID: onMessageID}
}

// RefreshOnce queries current data and updates the panel if it changed. Used
// by both the automatic ticker and the manual /admin leaderboard-refresh path,
// so there is exactly one leaderboard-refresh code path.
func (s *LeaderboardScheduler) RefreshOnce(ctx context.Context) error {
	if s == nil || s.panel == nil || s.stats == nil {
		return nil
	}
	snapshot, err := s.loadSnapshot(ctx)
	if err != nil {
		return err
	}
	id, changed, err := s.panel.Update(snapshot)
	if err != nil {
		slog.Warn("component=discord", "msg", "leaderboard refresh failed", "err", err.Error())
		return err
	}
	if changed && s.onMessageID != nil && id != "" {
		s.onMessageID(id)
	}
	return nil
}

func (s *LeaderboardScheduler) loadSnapshot(ctx context.Context) (LeaderboardSnapshot, error) {
	kills, err := s.stats.TopByKills(ctx, s.guildRowID, s.cfg.TopKillsLimit)
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	kd, err := s.stats.TopByKD(ctx, s.guildRowID, s.cfg.TopKDLimit, s.cfg.MinKillsForKD)
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	longest, err := s.stats.TopLongestKill(ctx, s.guildRowID, s.cfg.TopLongestLimit)
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	return LeaderboardSnapshot{TopKills: kills, TopKD: kd, TopLongest: longest, GeneratedAt: time.Now()}, nil
}

// Run performs one immediate refresh (so members do not wait 3 hours after
// startup), then refreshes every LeaderboardRefreshInterval until ctx is
// cancelled. Callers must start this exactly once per guild.
func (s *LeaderboardScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	if err := s.RefreshOnce(ctx); err != nil {
		slog.Warn("component=discord", "msg", "initial leaderboard refresh failed", "err", err.Error())
	}
	ticker := time.NewTicker(LeaderboardRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = s.RefreshOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}
