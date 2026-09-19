package discord

import (
	"context"
	"log/slog"
	"sync"
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
	mu          sync.Mutex
	dirty       bool

	// refreshMu serialises RefreshOnce: the timer, /admin refresh and the route
	// syncer can all trigger one, and two concurrent refreshes must not race to
	// create the same panel.
	refreshMu sync.Mutex

	// Installation route model (see SetRouting). Unset -> legacy only.
	routes        RouteResolver
	servers       GuildServersFunc
	routePanels   *RoutePanels
	retireLegacy  func()
	legacyRetired bool
}

// GuildServersFunc lists the guild's internal id and the ids of its active
// game servers. The leaderboard is a guild-level message, so it follows the
// union of the AUTO_LEADERBOARD routes of every server in the guild - each
// resolved on its own (guild, server) identity.
type GuildServersFunc func(ctx context.Context) (guildRowID int64, serverIDs []int64, err error)

// SetRouting moves the persistent leaderboard onto AUTO_LEADERBOARD routes.
// Resolution order per refresh: route(s) -> legacy GuildSetup channel. Exactly
// one mode is active, so a guild is never published to both. retireLegacy
// removes the legacy panel message the first time route mode takes over, and
// is optional.
func (s *LeaderboardScheduler) SetRouting(resolver RouteResolver, servers GuildServersFunc, panels *RoutePanels, retireLegacy func()) {
	if s == nil {
		return
	}
	s.routes, s.servers, s.routePanels, s.retireLegacy = resolver, servers, panels, retireLegacy
}

func (s *LeaderboardScheduler) MarkDirty() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
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
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	snapshot, err := s.loadSnapshot(ctx)
	if err != nil {
		return err
	}
	if handled, routeErr := s.refreshRouted(ctx, snapshot); handled {
		if routeErr == nil {
			s.mu.Lock()
			s.dirty = false
			s.mu.Unlock()
		}
		return routeErr
	}
	if s.legacyRetired {
		// Back on the legacy channel after route mode: the legacy message was
		// retired, so post a fresh one instead of editing a deleted id.
		s.panel.Reset()
		s.legacyRetired = false
	}
	id, changed, err := s.panel.Update(snapshot)
	if err != nil {
		slog.Warn("component=discord", "msg", "leaderboard refresh failed", "err", err.Error())
		return err
	}
	if changed && s.onMessageID != nil && id != "" {
		s.onMessageID(id)
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// refreshRouted publishes to AUTO_LEADERBOARD route channels when any server
// of the guild has one. handled=false means "use the legacy panel": no routing
// configured, no route found, or the server/route lookup failed (a failed
// lookup falls back to legacy and never tears down routed panels).
func (s *LeaderboardScheduler) refreshRouted(ctx context.Context, snapshot LeaderboardSnapshot) (handled bool, err error) {
	if s.routes == nil || s.servers == nil || s.routePanels == nil {
		return false, nil
	}
	guildRowID, serverIDs, listErr := s.servers(ctx)
	if listErr != nil {
		slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyAutoLeaderboard, "reason", "lookup_error", "err", listErr.Error())
		return false, nil
	}
	channels, lookupErrs := resolveGuildRouteChannels(ctx, s.routes, guildRowID, serverIDs, routeKeyAutoLeaderboard)
	if len(channels) == 0 {
		if lookupErrs == 0 {
			// Definitively no routes: retire any routed panel left from a
			// route that has since been removed.
			if _, syncErr := s.routePanels.Sync(ctx, guildRowID, routeKeyAutoLeaderboard, nil, PanelContent{}, true, true); syncErr != nil {
				slog.Warn("component=discord", "event", "route_panel_retire_failed", "route_key", routeKeyAutoLeaderboard, "err", syncErr.Error())
			}
		}
		return false, nil
	}
	content := PanelContent{Embed: BuildLeaderboardEmbed(snapshot, s.cfg)}
	res, syncErr := s.routePanels.Sync(ctx, guildRowID, routeKeyAutoLeaderboard, channels, content, true, lookupErrs == 0)
	if syncErr != nil {
		slog.Warn("component=discord", "msg", "routed leaderboard refresh failed", "err", syncErr.Error())
		return true, syncErr
	}
	if res.Errors == 0 && !s.legacyRetired {
		if s.retireLegacy != nil {
			s.retireLegacy()
		}
		s.panel.Reset()
		s.legacyRetired = true
	}
	return true, nil
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
			if err := s.RefreshOnce(ctx); err != nil {
				slog.Warn("component=discord", "msg", "scheduled leaderboard refresh failed", "err", err.Error())
			}
		case <-ctx.Done():
			return
		}
	}
}
