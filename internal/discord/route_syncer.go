package discord

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
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
		content:  func() PanelContent { return PanelContent{Embed: LinkUsernameInfoEmbed(), Components: LinkUsernamePanelComponents()} },
		channel:  func(g *GuildSetup) string { return g.LinkPanelChannelID },
		message:  func(g *GuildSetup) string { return g.LinkPanelMessageID },
		clear:    func(g *GuildSetup) { g.LinkPanelMessageID = "" },
	},
	{
		routeKey: routeKeyStatsLeaderboards,
		content:  func() PanelContent { return PanelContent{Embed: PlayerStatsInfoEmbed(), Components: PlayerStatsPanelComponents()} },
		channel:  func(g *GuildSetup) string { return g.PlayerStatsChannelID },
		message:  func(g *GuildSetup) string { return g.PlayerStatsInfoMessageID },
		clear:    func(g *GuildSetup) { g.PlayerStatsInfoMessageID = "" },
	},
}

type routeSyncState struct {
	known    bool
	sig      string
	verified time.Time
}

// RouteSyncer keeps the guild-level, route-driven artifacts in step with the
// installation routes: the LINK_GAMERTAG and STATS_LEADERBOARDS panels, and a
// refresh of the AUTO_LEADERBOARD when its routed channels change. Everything
// is resolved per (guild, server) through the shared resolver - it owns no
// route cache of its own.
type RouteSyncer struct {
	resolver RouteResolver
	servers  GuildServersFunc
	panels   *RoutePanels
	api      messageDeleter
	setup    SetupStore
	guildID  string // Discord guild snowflake, for the legacy GuildSetup

	leaderboard interface {
		RefreshOnce(ctx context.Context) error
	}
	// restoreLegacy re-creates missing legacy panels (SetupManager.EnsureConfigured)
	// when a guild's routes are removed and it falls back to legacy channels.
	restoreLegacy func()

	trigger chan struct{}

	mu    sync.Mutex
	state map[string]*routeSyncState
}

// NewRouteSyncer builds a syncer. api is used only to retire legacy panel
// messages; setup/guildID locate the legacy GuildSetup.
func NewRouteSyncer(resolver RouteResolver, servers GuildServersFunc, panels *RoutePanels, api messageDeleter, setup SetupStore, guildID string) *RouteSyncer {
	return &RouteSyncer{resolver: resolver, servers: servers, panels: panels, api: api, setup: setup, guildID: guildID, trigger: make(chan struct{}, 1), state: make(map[string]*routeSyncState)}
}

// SetLeaderboard attaches the scheduler refreshed when AUTO_LEADERBOARD routes change.
func (s *RouteSyncer) SetLeaderboard(l interface{ RefreshOnce(ctx context.Context) error }) {
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

// HasRoute reports whether any server of the guild currently has a route for
// routeKey. SetupManager uses it so it never creates a legacy panel next to a
// routed one. A failed lookup reports false (legacy), matching the fallback rule.
func (s *RouteSyncer) HasRoute(routeKey string) bool {
	if s == nil || s.resolver == nil || s.servers == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*routeLookupTimeout)
	defer cancel()
	guildRowID, serverIDs, err := s.servers(ctx)
	if err != nil {
		return false
	}
	channels, _ := resolveGuildRouteChannels(ctx, s.resolver, guildRowID, serverIDs, routeKey)
	return len(channels) > 0
}

// SyncOnce evaluates every migrated guild-level route once. Failures are
// logged and never fatal; nothing is torn down while a lookup is failing.
func (s *RouteSyncer) SyncOnce(ctx context.Context) {
	if s == nil || s.resolver == nil || s.servers == nil {
		return
	}
	guildRowID, serverIDs, err := s.servers(ctx)
	if err != nil {
		slog.Warn("component=discord", "event", "route_sync_servers_failed", "err", err.Error())
		return
	}
	for _, spec := range routedStaticPanels {
		s.syncStaticPanel(ctx, guildRowID, serverIDs, spec)
	}
	s.syncLeaderboard(ctx, guildRowID, serverIDs)
}

func (s *RouteSyncer) stateFor(key string) *routeSyncState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[key]
	if st == nil {
		st = &routeSyncState{}
		s.state[key] = st
	}
	return st
}

func (s *RouteSyncer) syncStaticPanel(ctx context.Context, guildRowID int64, serverIDs []int64, spec legacyPanelSpec) {
	channels, lookupErrs := resolveGuildRouteChannels(ctx, s.resolver, guildRowID, serverIDs, spec.routeKey)
	if lookupErrs > 0 && len(channels) == 0 {
		return // unknown state: keep whatever is live
	}
	st := s.stateFor(spec.routeKey)
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
	switch {
	case len(channels) > 0 && res.Errors == 0:
		// Route mode is live: retire the legacy panel so only one copy exists.
		s.retireLegacyPanel(spec)
	case len(channels) == 0 && lookupErrs == 0 && res.Removed > 0 && s.restoreLegacy != nil:
		// Every route was removed: bring the legacy panel back.
		s.restoreLegacy()
	}
}

func (s *RouteSyncer) syncLeaderboard(ctx context.Context, guildRowID int64, serverIDs []int64) {
	if s.leaderboard == nil {
		return
	}
	channels, lookupErrs := resolveGuildRouteChannels(ctx, s.resolver, guildRowID, serverIDs, routeKeyAutoLeaderboard)
	if lookupErrs > 0 && len(channels) == 0 {
		return
	}
	st := s.stateFor(routeKeyAutoLeaderboard)
	sig := strings.Join(channels, ",")
	if !st.known {
		// First observation: the scheduler's own startup refresh already
		// published for the current routes; just record them.
		st.known, st.sig = true, sig
		return
	}
	if st.sig == sig {
		return
	}
	if err := s.leaderboard.RefreshOnce(ctx); err != nil {
		slog.Warn("component=discord", "event", "route_leaderboard_refresh_failed", "err", err.Error())
		return // keep the old signature so the next pass retries
	}
	st.sig = sig
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
	clear(setup)
	if err := store.Save(*setup); err != nil {
		slog.Warn("component=discord", "event", "legacy_panel_retire_save_failed", "err", err.Error())
	}
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
