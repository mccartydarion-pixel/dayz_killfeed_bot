package discord

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Route keys of the publishers migrated onto the shared resolver. They mirror
// routing.Route* (this package does not import routing; tests assert equality).
const (
	routeKeyLinkGamertag      = "LINK_GAMERTAG"
	routeKeyStatsLeaderboards = "STATS_LEADERBOARDS"
	routeKeyAutoLeaderboard   = "AUTO_LEADERBOARD"
	routeKeyAdminLogs         = "ADMIN_LOGS"
)

// routeLookupTimeout bounds one resolver call so a slow database can never
// stall a publisher; a timeout is just another lookup error -> legacy fallback.
const routeLookupTimeout = 2 * time.Second

// RouteBinding resolves one route key for one server, with the same
// never-fatal semantics as KillfeedPublisher.RouteChannelID: "" means "use the
// legacy channel" (no resolver, no route, or the lookup failed). It reuses the
// shared routing.Resolver and its cache - a binding holds no cache of its own,
// only throttled-logging state. Safe for concurrent use; a nil *RouteBinding
// resolves nothing.
type RouteBinding struct {
	resolver             RouteResolver
	guildRowID, serverID int64
	routeKey             string

	mu         sync.Mutex
	lastState  string
	lastErrLog time.Time
}

// NewRouteBinding binds a resolver to one (guild, server, route key). guildRowID
// is the internal guilds.id and serverID the game_servers.id of the owning
// worker - never the guild alone.
func NewRouteBinding(resolver RouteResolver, guildRowID, serverID int64, routeKey string) *RouteBinding {
	return &RouteBinding{resolver: resolver, guildRowID: guildRowID, serverID: serverID, routeKey: routeKey}
}

// ChannelID returns the routed channel, or "" to fall back to legacy.
func (b *RouteBinding) ChannelID() string {
	if b == nil || b.resolver == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeLookupTimeout)
	defer cancel()
	channelID, found, err := b.resolver.Resolve(ctx, b.guildRowID, b.serverID, b.routeKey)
	if err != nil {
		b.logFallback("lookup_error", err)
		return ""
	}
	if !found || channelID == "" {
		b.logFallback("no_route", nil)
		return ""
	}
	b.mu.Lock()
	b.lastState = "route"
	b.mu.Unlock()
	return channelID
}

// logFallback mirrors the KILLFEED diagnostic: state changes only for
// "no_route", at most once a minute for lookup errors; internal ids only.
func (b *RouteBinding) logFallback(reason string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if reason == "lookup_error" {
		if time.Since(b.lastErrLog) < time.Minute {
			return
		}
		b.lastErrLog = time.Now()
		slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", b.routeKey,
			"guild_id", b.guildRowID, "server_id", b.serverID, "reason", reason, "err", err.Error())
		return
	}
	if b.lastState == reason {
		return
	}
	b.lastState = reason
	slog.Info("component=discord", "event", "channel_route_fallback", "route_key", b.routeKey,
		"guild_id", b.guildRowID, "server_id", b.serverID, "reason", reason)
}

// resolveGuildRouteChannels resolves routeKey for every server of one guild and
// returns the distinct routed channels in server order. Guild-level artifacts
// (link/stats panels, the leaderboard) serve the whole guild, so they follow
// the union of its servers' routes - each server resolved on its own (guild,
// server) identity, so cross-organization isolation is the resolver's.
// lookupErrs counts servers whose lookup failed: callers must not tear anything
// down when it is non-zero, because a failed lookup says nothing about routes.
func resolveGuildRouteChannels(ctx context.Context, resolver RouteResolver, guildRowID int64, serverIDs []int64, routeKey string) (channels []string, lookupErrs int) {
	if resolver == nil {
		return nil, 0
	}
	seen := make(map[string]bool)
	for _, serverID := range serverIDs {
		channelID, found, err := resolver.Resolve(ctx, guildRowID, serverID, routeKey)
		if err != nil {
			lookupErrs++
			slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKey,
				"guild_id", guildRowID, "server_id", serverID, "reason", "lookup_error", "err", err.Error())
			continue
		}
		if !found || channelID == "" || seen[channelID] {
			continue
		}
		seen[channelID] = true
		channels = append(channels, channelID)
	}
	return channels, lookupErrs
}

// isUnknownMessage reports whether err means the message (or its channel) does
// not exist - the one failure on which a persistent panel may be recreated.
// Transient failures (rate limits, 5xx, network) must never trigger a resend,
// or a hiccup would duplicate the panel.
func isUnknownMessage(err error) bool {
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) {
		return false
	}
	if rest.Message != nil && (rest.Message.Code == discordgo.ErrCodeUnknownMessage || rest.Message.Code == discordgo.ErrCodeUnknownChannel) {
		return true
	}
	return rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound
}

// messageDeleter is the optional delete capability used to retire a superseded
// panel message. Best-effort everywhere: a failed delete never blocks publishing.
type messageDeleter interface {
	ChannelMessageDelete(channelID, messageID string) error
}
