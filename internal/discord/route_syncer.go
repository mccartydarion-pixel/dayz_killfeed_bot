package discord

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// RouteSyncInterval is how often the syncer re-evaluates routes on its own.
// It matches routing.DefaultTTL: a route changed outside this process is
// visible to the resolver within that TTL, so polling at the same cadence
// bounds out-of-process propagation without adding a cache. In-process route
// writes call Trigger and are picked up immediately.
const RouteSyncInterval = 30 * time.Second

// routePanelVerifyEvery re-checks that routed static panels still exist even
// when the resolved channels have not changed (e.g. a moderator deleted one).
const routePanelVerifyEvery = 10 * time.Minute

// legacyPanelSpec describes one guild-level static panel: its route key, what
// it shows, and the legacy GuildSetup fields it used before routes.
type legacyPanelSpec struct {
	routeKey string
	content  func() PanelContent
	channel  func(*GuildSetup) string
	message  func(*GuildSetup) string
	clear    func(*GuildSetup)
}

var routedStaticPanels = []legacyPanelSpec{
	{
		routeKey: routeKeyLinkGamertag,
		content: func() PanelContent {
			return PanelContent{Embed: LinkUsernameInfoEmbed(), Components: LinkUsernamePanelComponents()}
		},
		channel: func(g *GuildSetup) string { return g.LinkPanelChannelID },
		message: func(g *GuildSetup) string { return g.LinkPanelMessageID },
		clear:   func(g *GuildSetup) { g.LinkPanelMessageID = "" },
	},
	{
		routeKey: routeKeyStatsLeaderboards,
		content: func() PanelContent {
			return PanelContent{Embed: PlayerStatsInfoEmbed(), Components: PlayerStatsPanelComponents()}
		},
		channel: func(g *GuildSetup) string { return g.PlayerStatsChannelID },
		message: func(g *GuildSetup) string { return g.PlayerStatsInfoMessageID },
		clear:   func(g *GuildSetup) { g.PlayerStatsInfoMessageID = "" },
	},
}

type routeSyncState struct {
	known    bool
	sig      string
	verified time.Time
}

// routeSyncKey identifies one guild's state for one route key.
type routeSyncKey struct {
	guildRowID int64
	routeKey   string
}

// RouteSyncer keeps the guild-level, route-driven artifacts in step with the
// installation routes, for every guild that has an active server: the
// LINK_GAMERTAG and STATS_LEADERBOARDS panels, and a refresh of the
// AUTO_LEADERBOARD when its routed channels change. Everything is resolved
// per (guild, server) through the shared resolver - it owns no route cache
// of its own. Only the primary guild has legacy GuildSetup panels to retire
// or restore.
type RouteSyncer struct {
	resolver RouteResolver
	servers  AllGuildServersFunc
	panels   *RoutePanels
	api      messageDeleter
	setup    SetupStore
	// guildID is the primary guild's Discord snowflake (for its legacy
	// GuildSetup) and primaryGuildRowID its internal guilds.id.
	guildID           string
	primaryGuildRowID int64

	leaderboard interface {
		RefreshOnce(ctx context.Context) error
	}
	// restoreLegacy re-creates missing legacy panels (SetupManager.EnsureConfigured)
	// when a guild's routes are removed and it falls back to legacy channels.
	restoreLegacy func()

	trigger chan struct{}

	mu    sync.Mutex
	state map[routeSyncKey]*routeSyncState
	// booted is set after the first full pass. A guild first seen later (a
	// customer connecting a server while the bot runs) was not covered by
	// the scheduler's startup refresh, so its routes count as a change.
	booted bool
}

// NewRouteSyncer builds a syncer. api is used only to retire legacy panel
// messages; setup/guildID locate the legacy GuildSetup of the primary guild,
// whose internal id is primaryGuildRowID.
func NewRouteSyncer(resolver RouteResolver, servers AllGuildServersFunc, panels *RoutePanels, api messageDeleter, setup SetupStore, guildID string, primaryGuildRowID int64) *RouteSyncer {
	return &RouteSyncer{resolver: resolver, servers: servers, panels: panels, api: api, setup: setup, guildID: guildID, primaryGuildRowID: primaryGuildRowID, trigger: make(chan struct{}, 1), state: make(map[routeSyncKey]*routeSyncState)}
}

// SetLeaderboard attaches the scheduler refreshed when AUTO_LEADERBOARD routes change.
func (s *RouteSyncer) SetLeaderboard(l interface {
	RefreshOnce(ctx context.Context) error
}) {
	if s != nil {
		s.leaderboard = l
	}
}

// SetLegacyRestore attaches the callback run when routes are removed.
func (s *RouteSyncer) SetLegacyRestore(fn func()) {
	if s != nil {
		s.restoreLegacy = fn
	}
}

// Trigger requests an immediate sync (non-blocking, coalesced). Called after an
// in-process route write; nil-safe.
func (s *RouteSyncer) Trigger() {
	if s == nil {
		return
	}
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run syncs once, then on every Trigger and every RouteSyncInterval until ctx ends.
func (s *RouteSyncer) Run(ctx context.Context) {
	if s == nil {
		return
	}
	ticker := time.NewTicker(RouteSyncInterval)
	defer ticker.Stop()
	s.SyncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncOnce(ctx)
		case <-s.trigger:
			s.SyncOnce(ctx)
		}
	}
}

// HasRoute reports whether any server of the primary guild currently has a
// route for routeKey. SetupManager uses it so it never creates a legacy panel
// next to a routed one; only the primary guild has legacy panels. A failed
// lookup reports false (legacy), matching the fallback rule.
func (s *RouteSyncer) HasRoute(routeKey string) bool {
	if s == nil || s.resolver == nil || s.servers == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*routeLookupTimeout)
	defer cancel()
	guilds, err := s.servers(ctx)
	if err != nil {
		return false
	}
	serverIDs, ok := serversOfGuild(guilds, s.primaryGuildRowID)
	if !ok {
		return false
	}
	channels, _ := resolveGuildRouteChannels(ctx, s.resolver, s.primaryGuildRowID, serverIDs, routeKey)
	return len(channels) > 0
}

// SyncOnce evaluates every migrated guild-level route of every guild once.
// Failures are logged and never fatal; nothing is torn down while a lookup
// is failing.
func (s *RouteSyncer) SyncOnce(ctx context.Context) {
	if s == nil || s.resolver == nil || s.servers == nil {
		return
	}
	guilds, err := s.servers(ctx)
	if err != nil {
		slog.Warn("component=discord", "event", "route_sync_servers_failed", "err", err.Error())
		return
	}
	s.mu.Lock()
	booted := s.booted
	s.mu.Unlock()
	var changed []pendingLeaderboardSig
	for _, g := range guilds {
		for _, spec := range routedStaticPanels {
			s.syncStaticPanel(ctx, g, spec)
		}
		if st, sig, moved := s.leaderboardRoutesMoved(ctx, g, booted); moved {
			changed = append(changed, pendingLeaderboardSig{st, sig, g.GuildRowID})
		}
	}
	s.mu.Lock()
	s.booted = true
	s.mu.Unlock()
	if len(changed) == 0 {
		return
	}
	// One refresh covers every guild, so the changed guilds share it.
	err = s.leaderboard.RefreshOnce(ctx)
	failed, perGuild := s.leaderboard.(interface{ RefreshFailed(int64) bool })
	if err != nil {
		slog.Warn("component=discord", "event", "route_leaderboard_refresh_failed", "err", err.Error())
		if !perGuild {
			return // keep the old signatures so the next pass retries
		}
	}
	// Record each changed guild whose board went up; a guild that failed keeps its old
	// signature and is retried next pass, without holding the others back.
	for _, c := range changed {
		if err != nil && failed.RefreshFailed(c.guildRowID) {
			continue
		}
		c.state.sig = c.sig
	}
}

// pendingLeaderboardSig is a guild whose AUTO_LEADERBOARD channels changed
// and the signature to record once the refresh succeeded.
type pendingLeaderboardSig struct {
	state      *routeSyncState
	sig        string
	guildRowID int64
}

func (s *RouteSyncer) stateFor(guildRowID int64, routeKey string) *routeSyncState {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := routeSyncKey{guildRowID, routeKey}
	st := s.state[key]
	if st == nil {
		st = &routeSyncState{}
		s.state[key] = st
	}
	return st
}

func (s *RouteSyncer) syncStaticPanel(ctx context.Context, g GuildServers, spec legacyPanelSpec) {
	guildRowID := g.GuildRowID
	channels, lookupErrs := resolveGuildRouteChannels(ctx, s.resolver, guildRowID, g.ServerIDs, spec.routeKey)
	if lookupErrs > 0 && len(channels) == 0 {
		return // unknown state: keep whatever is live
	}
	st := s.stateFor(guildRowID, spec.routeKey)
	sig := strings.Join(channels, ",")
	if st.known && st.sig == sig && time.Since(st.verified) < routePanelVerifyEvery {
		return
	}
	if s.panels == nil {
		return
	}
	res, err := s.panels.Sync(ctx, guildRowID, spec.routeKey, channels, spec.content(), false, lookupErrs == 0)
	if err != nil {
		slog.Warn("component=discord", "event", "route_panel_sync_failed", "route_key", spec.routeKey, "guild_id", guildRowID, "err", err.Error())
		return
	}
	if res.Errors == 0 {
		st.known, st.sig, st.verified = true, sig, time.Now()
	}
	if guildRowID != s.primaryGuildRowID {
		return // only the primary guild has a legacy panel to retire or restore
	}
	switch {
	case len(channels) > 0 && res.Errors == 0:
		// Route mode is live: retire the legacy panel so only one copy exists.
		s.retireLegacyPanel(spec)
	case len(channels) == 0 && lookupErrs == 0 && res.Removed > 0 && s.restoreLegacy != nil:
		// Every route was removed: bring the legacy panel back.
		s.restoreLegacy()
	}
}

// leaderboardRoutesMoved reports whether a guild's AUTO_LEADERBOARD channels
// differ from the ones the leaderboard was last refreshed for. booted says
// the syncer's first pass is over, so a guild observed for the first time
// now is new since startup.
func (s *RouteSyncer) leaderboardRoutesMoved(ctx context.Context, g GuildServers, booted bool) (st *routeSyncState, sig string, moved bool) {
	if s.leaderboard == nil {
		return nil, "", false
	}
	channels, lookupErrs := resolveGuildRouteChannels(ctx, s.resolver, g.GuildRowID, g.ServerIDs, routeKeyAutoLeaderboard)
	if lookupErrs > 0 && len(channels) == 0 {
		return nil, "", false
	}
	st = s.stateFor(g.GuildRowID, routeKeyAutoLeaderboard)
	sig = strings.Join(channels, ",")
	if !st.known {
		st.known = true
		if booted && sig != "" {
			// A guild first seen after startup: the scheduler's startup
			// refresh did not cover it, so publish now. Its signature is
			// recorded once that succeeds, so a failure is retried.
			return st, sig, true
		}
		// First observation at startup: the scheduler's own startup refresh
		// already published for the current routes; just record them.
		st.sig = sig
		return st, sig, false
	}
	return st, sig, st.sig != sig
}

// retireLegacyPanel deletes the legacy panel message (best-effort) and clears
// its stored id, so the routed panel is the only live copy.
func (s *RouteSyncer) retireLegacyPanel(spec legacyPanelSpec) {
	retireLegacyPanel(s.api, s.setup, s.guildID, spec.channel, spec.message, spec.clear)
}

// retireLegacyPanel deletes a legacy panel message and clears its GuildSetup id.
func retireLegacyPanel(api messageDeleter, store SetupStore, guildID string, channel, message func(*GuildSetup) string, clear func(*GuildSetup)) {
	if store == nil || guildID == "" {
		return
	}
	setup, err := store.Get(guildID)
	if err != nil || setup == nil {
		return
	}
	messageID := message(setup)
	if messageID == "" {
		return
	}
	if channelID := channel(setup); channelID != "" && api != nil {
		_ = api.ChannelMessageDelete(channelID, messageID) // best-effort: it may already be gone
	}
	before := *setup
	clear(setup)
	if err := store.Save(*setup); err != nil {
		slog.Warn("component=discord", "event", "legacy_panel_retire_save_failed", "err", err.Error())
	}
	// Save cannot clear a message id on the Postgres store (its upsert keeps
	// existing ids when a value is empty), so a retired id used to survive and
	// resurface after every restart. Clear the retired columns explicitly.
	if c, ok := store.(messageIDClearer); ok {
		if cols := clearedMessageColumns(before, *setup); len(cols) > 0 {
			if err := c.ClearMessageIDs(guildID, cols...); err != nil {
				slog.Warn("component=discord", "event", "legacy_panel_retire_clear_failed", "err", err.Error())
			}
		}
	}
}

// messageIDClearer is implemented by setup stores whose Save cannot clear ids.
type messageIDClearer interface {
	ClearMessageIDs(guildID string, columns ...string) error
}

// clearedMessageColumns lists the panel message-id columns that went from set
// to empty between before and after.
func clearedMessageColumns(before, after GuildSetup) []string {
	var cols []string
	add := func(b, a, col string) {
		if b != "" && a == "" {
			cols = append(cols, col)
		}
	}
	add(before.LeaderboardMessageID, after.LeaderboardMessageID, repository.GuildColumnLeaderboardMessage)
	add(before.PlayerStatsInfoMessageID, after.PlayerStatsInfoMessageID, repository.GuildColumnPlayerStatsInfoMessage)
	add(before.LinkPanelMessageID, after.LinkPanelMessageID, repository.GuildColumnLinkPanelMessage)
	add(before.ServerStatusMessageID, after.ServerStatusMessageID, repository.GuildColumnServerStatusMessage)
	add(before.OnlinePlayersMessageID, after.OnlinePlayersMessageID, repository.GuildColumnOnlinePlayersMessage)
	return cols
}

// NewLegacyLeaderboardRetirer returns the callback the LeaderboardScheduler
// runs when route mode takes over the leaderboard.
func NewLegacyLeaderboardRetirer(api messageDeleter, store SetupStore, guildID string) func() {
	return func() {
		retireLegacyPanel(api, store, guildID,
			func(g *GuildSetup) string { return g.LeaderboardsChannelID },
			func(g *GuildSetup) string { return g.LeaderboardMessageID },
			func(g *GuildSetup) { g.LeaderboardMessageID = "" })
	}
}
