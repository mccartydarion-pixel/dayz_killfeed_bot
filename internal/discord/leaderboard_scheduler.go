package discord

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
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
	stats       AutoLeaderboardReader
	ranks       RankReader
	serverIDs   GuildServersFunc
	serverNames ServerNameFunc
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

// AutoLeaderboardReader is the read surface of the Auto Leaderboard V3: one
// bounded aggregate query per ALL-TIME category (implemented by
// *repository.StatsRepository).
type AutoLeaderboardReader interface {
	TopByKills(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
	TopByBestStreak(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
	TopByDeaths(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
	TopLongestKill(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
}

// RankReader is the authoritative CURRENT player-rank source for the
// "Current Top 15 Ranks" board, ordered by that rank system's own ordering.
// The selected public server's active Ranked season supplies this board.
// Without an active season the Ranks embed remains inactive.
type RankReader interface {
	TopCurrentRanks(ctx context.Context, guildID int64, limit int) ([]RankEntry, error)
}

// SetRankSource activates the Current Ranks board with an authoritative
// source. nil keeps it inactive.
func (s *LeaderboardScheduler) SetRankSource(r RankReader) {
	if s != nil {
		s.ranks = r
	}
}

// SetServerNames lets the header show the guild's real server display
// name(s). Unset, the header simply omits the name line.
func (s *LeaderboardScheduler) SetServerNames(servers GuildServersFunc, names ServerNameFunc) {
	if s != nil {
		s.serverIDs, s.serverNames = servers, names
	}
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
func NewLeaderboardScheduler(panel *LeaderboardPanel, stats AutoLeaderboardReader, guildRowID int64, cfg LeaderboardConfig, onMessageID func(string)) *LeaderboardScheduler {
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
	// One message, every embed: the whole package is sent/edited in one call.
	content := PanelContent{Embeds: BuildAutoLeaderboardEmbeds(snapshot, s.cfg)}
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

// loadSnapshot loads EVERY category before anything is rendered, so the
// message is only ever edited with a complete snapshot: any failed ranking
// query fails the whole refresh and the last good board stays untouched
// (logged by the caller, retried next refresh). Four bounded aggregate
// queries, plus one for ranks when a rank source is wired.
func (s *LeaderboardScheduler) loadSnapshot(ctx context.Context) (LeaderboardSnapshot, error) {
	kills, err := s.stats.TopByKills(ctx, s.guildRowID, boardLimit(s.cfg.TopKillsLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	streaks, err := s.stats.TopByBestStreak(ctx, s.guildRowID, boardLimit(s.cfg.TopStreaksLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	deaths, err := s.stats.TopByDeaths(ctx, s.guildRowID, boardLimit(s.cfg.TopDeathsLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	longest, err := s.stats.TopLongestKill(ctx, s.guildRowID, boardLimit(s.cfg.TopLongestLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	snap := LeaderboardSnapshot{TopKills: kills, TopStreaks: streaks, TopDeaths: deaths, TopLongest: longest}
	if s.ranks != nil {
		ranks, rankErr := s.ranks.TopCurrentRanks(ctx, s.guildRowID, boardLimit(s.cfg.TopRanksLimit))
		if rankErr != nil && !errors.Is(rankErr, repository.ErrRankedIneligible) {
			return LeaderboardSnapshot{}, rankErr
		}
		if rankErr == nil {
			snap.CurrentRanks, snap.RanksEnabled = ranks, true
		}
	}
	snap.ServerName = s.serverName(ctx)
	snap.GeneratedAt = time.Now()
	return snap, nil
}

// boardLimit is the per-category query limit: the configured Top-N, capped at
// (and defaulting to) presentation.MaxBoardEntries.
func boardLimit(n int) int {
	if n <= 0 || n > presentation.MaxBoardEntries {
		return presentation.MaxBoardEntries
	}
	return n
}

// serverName is the header's display name: the guild's active server
// name(s), de-duplicated in server order. A lookup failure only drops the
// name line - it never fails the refresh.
func (s *LeaderboardScheduler) serverName(ctx context.Context) string {
	if s.serverIDs == nil || s.serverNames == nil {
		return ""
	}
	_, ids, err := s.serverIDs(ctx)
	if err != nil {
		return ""
	}
	seen := map[string]bool{}
	var names []string
	for _, id := range ids {
		n := strings.TrimSpace(s.serverNames(id))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	return strings.Join(names, " • ")
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
