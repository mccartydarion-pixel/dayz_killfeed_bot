package discord

import (
	"context"
	"errors"
	"fmt"
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

// LeaderboardScheduler owns the persistent leaderboard panels and refreshes
// them on a fixed interval, editing the existing messages in place. In route
// mode every guild with an active server gets its own panel (its own stats,
// its own routed channels); the legacy GuildSetup panel exists only for the
// primary guild. Kills never trigger a refresh directly; they only make the
// next scheduled (or manually triggered) refresh reflect the latest data.
type LeaderboardScheduler struct {
	panel       *LeaderboardPanel
	stats       AutoLeaderboardReader
	ranks       RankReader
	movement    BoardMovementStore
	serverIDs   AllGuildServersFunc
	serverNames ServerNameFunc
	// guildRowID is the primary guild (DISCORD_GUILD_ID): the only guild that
	// has a legacy panel to fall back to or retire.
	guildRowID  int64
	cfg         LeaderboardConfig
	onMessageID func(string)
	mu          sync.Mutex
	dirty       bool

	// refreshMu serialises RefreshOnce: the timer, /admin refresh and the route
	// syncer can all trigger one, and two concurrent refreshes must not race to
	// create the same panel.
	refreshMu sync.Mutex
	// failed holds the guilds the last RefreshOnce could not publish (see RefreshFailed); written
	// under refreshMu.
	failed map[int64]bool

	// Installation route model (see SetRouting). Unset -> legacy only.
	routes        RouteResolver
	servers       AllGuildServersFunc
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

// WeeklyKillsReader is the optional reader of this week's kills board.
type WeeklyKillsReader interface {
	TopByKillsSince(ctx context.Context, guildID int64, since time.Time, limit int) ([]repository.LeaderboardEntry, error)
}

// BoardMovementStore keeps each board's positions per day, so the board can show who moved.
type BoardMovementStore interface {
	// PreviousPositions is the board's most recent recorded positions from before day.
	PreviousPositions(ctx context.Context, guildID int64, board string, day time.Time) (map[string]int, error)
	// RecordPositions stores the board's positions for day (the first record of a day is kept).
	RecordPositions(ctx context.Context, guildID int64, board string, day time.Time, positions map[string]int) error
}

// SetMovementStore makes the kills and longest-kill boards show movement since the day before.
func (s *LeaderboardScheduler) SetMovementStore(m BoardMovementStore) {
	if s != nil {
		s.movement = m
	}
}

// boardMoves compares positions now with before: "▲n", "▼n" or "🆕" by display name.
func boardMoves(entries []repository.LeaderboardEntry, before map[string]int) (moves map[string]string, now map[string]int) {
	moves, now = map[string]string{}, map[string]int{}
	for i, e := range entries {
		now[e.DisplayName] = i + 1
		if len(before) == 0 {
			continue
		}
		prev, ok := before[e.DisplayName]
		switch {
		case !ok:
			moves[e.DisplayName] = "🆕"
		case prev > i+1:
			moves[e.DisplayName] = fmt.Sprintf("▲%d", prev-(i+1))
		case prev < i+1:
			moves[e.DisplayName] = fmt.Sprintf("▼%d", (i+1)-prev)
		}
	}
	return moves, now
}

func (s *LeaderboardScheduler) applyMovement(ctx context.Context, guildRowID int64, board string, entries []repository.LeaderboardEntry, now time.Time) map[string]string {
	if s.movement == nil {
		return nil
	}
	day := now.UTC().Truncate(24 * time.Hour)
	before, err := s.movement.PreviousPositions(ctx, guildRowID, board, day)
	if err != nil {
		return nil
	}
	moves, positions := boardMoves(entries, before)
	_ = s.movement.RecordPositions(ctx, guildRowID, board, day, positions)
	return moves
}

// SetRankSource activates the Current Ranks board with an authoritative
// source. nil keeps it inactive.
func (s *LeaderboardScheduler) SetRankSource(r RankReader) {
	if s != nil {
		s.ranks = r
	}
}

// SetServerNames lets each guild's header show its real server display
// name(s). Unset, the header simply omits the name line.
func (s *LeaderboardScheduler) SetServerNames(servers AllGuildServersFunc, names ServerNameFunc) {
	if s != nil {
		s.serverIDs, s.serverNames = servers, names
	}
}

// GuildServersFunc lists one guild's internal id and the ids of its active
// game servers. The leaderboard is a guild-level message, so it follows the
// union of the AUTO_LEADERBOARD routes of every server in the guild - each
// resolved on its own (guild, server) identity.
type GuildServersFunc func(ctx context.Context) (guildRowID int64, serverIDs []int64, err error)

// GuildServers is one Discord guild (internal guilds.id) and its active game servers.
type GuildServers struct {
	GuildRowID int64
	ServerIDs  []int64
}

// AllGuildServersFunc lists every guild that has an active game server, with
// those servers. The panels and feeds are multi-tenant: each guild is served
// on its own (guild, server) identities, never through the primary guild's.
type AllGuildServersFunc func(ctx context.Context) ([]GuildServers, error)

// SingleGuild adapts a one-guild provider into an AllGuildServersFunc (tests
// and the legacy single-guild path). A nil provider stays nil.
func SingleGuild(f GuildServersFunc) AllGuildServersFunc {
	if f == nil {
		return nil
	}
	return func(ctx context.Context) ([]GuildServers, error) {
		guildRowID, serverIDs, err := f(ctx)
		if err != nil {
			return nil, err
		}
		return []GuildServers{{GuildRowID: guildRowID, ServerIDs: serverIDs}}, nil
	}
}

// serversOfGuild is guildRowID's entry in a listing; ok=false when it has no
// active server (and so nothing to route).
func serversOfGuild(guilds []GuildServers, guildRowID int64) (serverIDs []int64, ok bool) {
	for _, g := range guilds {
		if g.GuildRowID == guildRowID {
			return g.ServerIDs, true
		}
	}
	return nil, false
}

// SetRouting moves the persistent leaderboard onto AUTO_LEADERBOARD routes,
// for every guild the provider lists. Resolution order per refresh for the
// primary guild: route(s) -> legacy GuildSetup channel; exactly one mode is
// active, so it is never published to both. Another guild has no legacy
// channel: without a route it simply has no leaderboard. retireLegacy removes
// the legacy panel message the first time route mode takes over, and is
// optional.
func (s *LeaderboardScheduler) SetRouting(resolver RouteResolver, servers AllGuildServersFunc, panels *RoutePanels, retireLegacy func()) {
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

// NewLeaderboardScheduler creates a scheduler; guildRowID is the primary
// guild, whose legacy panel is the fallback when it has no route.
func NewLeaderboardScheduler(panel *LeaderboardPanel, stats AutoLeaderboardReader, guildRowID int64, cfg LeaderboardConfig, onMessageID func(string)) *LeaderboardScheduler {
	return &LeaderboardScheduler{panel: panel, stats: stats, guildRowID: guildRowID, cfg: cfg, onMessageID: onMessageID}
}

// RefreshOnce queries current data and updates the panels if they changed.
// Used by both the automatic ticker and the manual /admin leaderboard-refresh
// path, so there is exactly one leaderboard-refresh code path.
func (s *LeaderboardScheduler) RefreshOnce(ctx context.Context) error {
	if s == nil || s.panel == nil || s.stats == nil {
		return nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.failed = map[int64]bool{}
	guilds, listErr := s.listGuilds(ctx)
	primaryServers, _ := serversOfGuild(guilds, s.guildRowID)
	snapshot, err := s.loadSnapshot(ctx, s.guildRowID, primaryServers)
	if err != nil {
		// The primary guild's board cannot be built; every other guild's still is.
		s.failed[s.guildRowID] = true
		_, _ = s.refreshRouted(ctx, guilds, listErr, nil)
		return err
	}
	if handled, routeErr := s.refreshRouted(ctx, guilds, listErr, &snapshot); handled {
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
		s.failed[s.guildRowID] = true
		return err
	}
	if id != "" {
		s.sweep([]string{s.panel.ChannelID()}, map[string]bool{id: true})
	}
	if changed && s.onMessageID != nil && id != "" {
		s.onMessageID(id)
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// listGuilds lists every guild and its servers for one refresh: the routing
// provider, or the names provider while routing is off. A failure only drops
// the header names and route mode (which falls back to legacy, logged by
// refreshRouted).
func (s *LeaderboardScheduler) listGuilds(ctx context.Context) ([]GuildServers, error) {
	list := s.servers
	if list == nil {
		list = s.serverIDs
	}
	if list == nil {
		return nil, nil
	}
	return list(ctx)
}

// refreshRouted publishes every listed guild's board to its AUTO_LEADERBOARD
// route channels. handled reports the primary guild only: false means "use
// its legacy panel" (no routing configured, no route found, or the listing
// failed - a failed lookup falls back to legacy and never tears down routed
// panels). Another guild's failure is logged and retried next refresh; it
// never decides the primary guild's mode.
//
// primary is the primary guild's snapshot, nil when it could not be loaded: that guild is then
// skipped (RefreshOnce reports its error) and the others are still refreshed.
func (s *LeaderboardScheduler) refreshRouted(ctx context.Context, guilds []GuildServers, listErr error, primary *LeaderboardSnapshot) (handled bool, err error) {
	if s.routes == nil || s.servers == nil || s.routePanels == nil {
		return false, nil
	}
	if listErr != nil {
		slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyAutoLeaderboard, "reason", "lookup_error", "err", listErr.Error())
		return false, nil
	}
	var firstErr error
	for _, g := range guilds {
		var snapshot LeaderboardSnapshot
		if g.GuildRowID == s.guildRowID {
			if primary == nil {
				continue
			}
			snapshot = *primary
		} else {
			loaded, loadErr := s.loadSnapshot(ctx, g.GuildRowID, g.ServerIDs)
			if loadErr != nil {
				slog.Warn("component=discord", "msg", "routed leaderboard load failed", "guild_id", g.GuildRowID, "err", loadErr.Error())
				s.failed[g.GuildRowID] = true
				if firstErr == nil {
					firstErr = loadErr
				}
				continue
			}
			snapshot = loaded
		}
		routed, syncErr := s.refreshGuild(ctx, g, snapshot)
		if g.GuildRowID == s.guildRowID {
			handled = routed
		}
		if syncErr != nil {
			s.failed[g.GuildRowID] = true
			if firstErr == nil {
				firstErr = syncErr
			}
		}
	}
	if !handled {
		// The primary guild has no route (its legacy board, if any, is the caller's); another
		// guild's failure is still a failure, so a guild first seen after boot is retried.
		return false, firstErr
	}
	return true, firstErr
}

// refreshGuild publishes one guild's board to its route channels. handled is
// false when the guild has no route (its routed panels, if any, are retired).
func (s *LeaderboardScheduler) refreshGuild(ctx context.Context, g GuildServers, snapshot LeaderboardSnapshot) (handled bool, err error) {
	guildRowID := g.GuildRowID
	primary := guildRowID == s.guildRowID
	channels, lookupErrs := resolveGuildRouteChannels(ctx, s.routes, guildRowID, g.ServerIDs, routeKeyAutoLeaderboard)
	if len(channels) == 0 {
		if lookupErrs == 0 {
			// Definitively no routes: retire any routed panel left from a
			// route that has since been removed.
			if _, syncErr := s.routePanels.Sync(ctx, guildRowID, routeKeyAutoLeaderboard, nil, PanelContent{}, true, true); syncErr != nil {
				slog.Warn("component=discord", "event", "route_panel_retire_failed", "route_key", routeKeyAutoLeaderboard, "guild_id", guildRowID, "err", syncErr.Error())
			}
		}
		return false, nil
	}
	// One message, every embed: the whole package is sent/edited in one call.
	content := PanelContent{Embeds: BuildAutoLeaderboardEmbeds(snapshot, s.cfg)}
	res, syncErr := s.routePanels.Sync(ctx, guildRowID, routeKeyAutoLeaderboard, channels, content, true, lookupErrs == 0)
	if syncErr != nil {
		slog.Warn("component=discord", "msg", "routed leaderboard refresh failed", "guild_id", guildRowID, "err", syncErr.Error())
		return true, syncErr
	}
	if primary && res.Errors == 0 && !s.legacyRetired {
		if s.retireLegacy != nil {
			s.retireLegacy()
		}
		s.panel.Reset()
		s.legacyRetired = true
	}
	if res.Errors == 0 {
		// Every routed channel now holds its recorded V3 board: remove any
		// other copy the bot left behind (an old single-embed season board, a
		// legacy board whose retirement delete failed, an orphaned placeholder).
		if recorded, recErr := s.routePanels.Recorded(ctx, guildRowID, routeKeyAutoLeaderboard); recErr == nil {
			keep := make(map[string]bool, len(recorded))
			for _, r := range recorded {
				keep[r.MessageID] = true
			}
			sweepChannels := append([]string(nil), channels...)
			if primary {
				sweepChannels = append(sweepChannels, s.panel.ChannelID())
			}
			s.sweep(sweepChannels, keep)
		}
	}
	return true, nil
}

// sweep removes the bot's obsolete leaderboard boards from channels, keeping
// the current board message ids. No-op when the Discord API cannot list
// channel history.
func (s *LeaderboardScheduler) sweep(channels []string, keep map[string]bool) {
	if s.panel == nil {
		return
	}
	history, ok := s.panel.editor.(ChannelHistoryAPI)
	if !ok {
		return
	}
	_, gone := sweepObsoleteLeaderboards(history, channels, keep)
	for _, channelID := range gone {
		// The legacy leaderboard channel from guild_settings no longer exists:
		// stop including it in every sweep. Routed channels are re-resolved
		// from the routes on the next refresh and are not touched here.
		if channelID == s.panel.ChannelID() {
			s.panel.ForgetChannel()
		}
	}
}

// loadSnapshot loads EVERY category of one guild before anything is
// rendered, so a message is only ever edited with a complete snapshot: any
// failed ranking query fails that guild's refresh and its last good board
// stays untouched (logged by the caller, retried next refresh). Four bounded
// aggregate queries, plus one for ranks when a rank source is wired.
func (s *LeaderboardScheduler) loadSnapshot(ctx context.Context, guildRowID int64, serverIDs []int64) (LeaderboardSnapshot, error) {
	kills, err := s.stats.TopByKills(ctx, guildRowID, boardLimit(s.cfg.TopKillsLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	streaks, err := s.stats.TopByBestStreak(ctx, guildRowID, boardLimit(s.cfg.TopStreaksLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	deaths, err := s.stats.TopByDeaths(ctx, guildRowID, boardLimit(s.cfg.TopDeathsLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	longest, err := s.stats.TopLongestKill(ctx, guildRowID, boardLimit(s.cfg.TopLongestLimit))
	if err != nil {
		return LeaderboardSnapshot{}, err
	}
	snap := LeaderboardSnapshot{TopKills: kills, TopStreaks: streaks, TopDeaths: deaths, TopLongest: longest}
	if s.ranks != nil {
		ranks, rankErr := s.ranks.TopCurrentRanks(ctx, guildRowID, boardLimit(s.cfg.TopRanksLimit))
		if rankErr != nil && !errors.Is(rankErr, repository.ErrRankedIneligible) {
			return LeaderboardSnapshot{}, rankErr
		}
		if rankErr == nil {
			snap.CurrentRanks, snap.RanksEnabled = ranks, true
		}
	}
	snap.ServerName = s.serverName(serverIDs)
	snap.GeneratedAt = time.Now()
	if weekly, ok := s.stats.(WeeklyKillsReader); ok {
		week, err := weekly.TopByKillsSince(ctx, guildRowID, weekStartUTC(snap.GeneratedAt), boardLimit(s.cfg.TopKillsLimit))
		if err != nil {
			return LeaderboardSnapshot{}, err
		}
		snap.TopKillsWeek, snap.WeekEnabled = week, true
	}
	snap.KillMoves = s.applyMovement(ctx, guildRowID, "KILLS", snap.TopKills, snap.GeneratedAt)
	snap.LongestMoves = s.applyMovement(ctx, guildRowID, "LONGEST", snap.TopLongest, snap.GeneratedAt)
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
// name(s), de-duplicated in server order. Without a name source the name
// line is simply omitted - it never fails the refresh.
func (s *LeaderboardScheduler) serverName(serverIDs []int64) string {
	if s.serverNames == nil {
		return ""
	}
	seen := map[string]bool{}
	var names []string
	for _, id := range serverIDs {
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
// cancelled. Callers must start this exactly once per process.
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

// weekStartUTC is the Monday 00:00 UTC that starts t's week.
func weekStartUTC(t time.Time) time.Time {
	day := t.UTC().Truncate(24 * time.Hour)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

// RefreshFailed reports whether the last RefreshOnce failed to publish guildRowID's board. The
// route syncer records a changed guild's routes once its board is up, so one guild failing on
// every pass does not make every other guild's board refresh again each time.
func (s *LeaderboardScheduler) RefreshFailed(guildRowID int64) bool {
	if s == nil {
		return false
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.failed[guildRowID]
}
