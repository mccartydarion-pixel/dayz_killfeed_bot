package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// KillfeedPublisher sends authoritative PLAYER_KILL events to the configured
// killfeed channel. Publish failures are logged and never stop log processing.
// The channel resolves from the stored guild setup first, then the env fallback.
type KillfeedPublisher struct {
	client   *Client
	store    SetupStore
	guildID  string
	fallback string // legacy KILLFEED_CHANNEL_ID env fallback
	feed     *RotatingFeed

	// Installation route model: when routes is set, the KILLFEED route for
	// the installation owning (routeGuildID, routeServerID) wins over every
	// legacy source. See RouteChannelID.
	routes                      RouteResolver
	routeGuildID, routeServerID int64
	logMu                       sync.Mutex
	lastRouteState              string
	lastRouteErrLog             time.Time

	// Optional custom embed templates (nil = the Champion default, always).
	custom     EmbedCustomizer
	serverName string
}

// SetCustomizer enables custom embed templates for this server's KILLFEED cards.
// serverName feeds {{server_name}}. Presentation only.
func (p *KillfeedPublisher) SetCustomizer(c EmbedCustomizer, serverName string) {
	if p == nil {
		return
	}
	p.custom = c
	p.serverName = serverName
}

// killCard is the embed for one kill: the Champion default, or - when the installation
// saved an enabled KILLFEED template that renders - the custom card. Exactly one card
// either way, so nothing is ever published twice.
func (p *KillfeedPublisher) killCard(ev *killfeed.Event) *discordgo.MessageEmbed {
	def := BuildKillEmbed(ev)
	return customEmbed(p.custom, p.routeGuildID, p.routeServerID, routeKeyKillfeed, def, func() map[string]string {
		return killfeedVars(ev, p.serverName)
	})
}

// RouteResolver resolves a feature route key to the Discord channel an
// installation configured for it (implemented by routing.Resolver).
type RouteResolver interface {
	Resolve(ctx context.Context, guildRowID, serverID int64, routeKey string) (channelID string, found bool, err error)
}

// routeKeyKillfeed mirrors routing.RouteKillfeed (this package cannot import
// routing without a needless dependency; the value is asserted equal in
// tests).
const routeKeyKillfeed = "KILLFEED"

// SetRouting attaches the installation route resolver for the server this
// publisher belongs to. guildRowID is the internal guilds.id and serverID
// the game_servers.id of the worker's server - never resolved by guild
// alone, since one guild can host several servers with different routes.
func (p *KillfeedPublisher) SetRouting(resolver RouteResolver, guildRowID, serverID int64) {
	if p == nil {
		return
	}
	p.routes = resolver
	p.routeGuildID = guildRowID
	p.routeServerID = serverID
}

// RouteChannelID returns the KILLFEED route channel for this publisher's
// server, or "" when there is none (no route configured, no resolver, or
// the lookup failed) - in which case callers use the legacy channel. It
// never returns an error: a lookup failure is logged and treated as "no
// route" so kill processing and persistence are never affected.
func (p *KillfeedPublisher) RouteChannelID() string {
	if p == nil || p.routes == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	channelID, found, err := p.routes.Resolve(ctx, p.routeGuildID, p.routeServerID, routeKeyKillfeed)
	if err != nil {
		p.logRouteFallback("lookup_error", err)
		return ""
	}
	if !found || channelID == "" {
		p.logRouteFallback("no_route", nil)
		return ""
	}
	p.logMu.Lock()
	p.lastRouteState = "route"
	p.logMu.Unlock()
	return channelID
}

// logRouteFallback records that KILLFEED fell back to the legacy channel.
// Lookups happen per kill, so a "no_route" fallback is logged only when the
// state changes (not once per kill), and lookup errors at most once a
// minute. Only internal IDs are logged - never tokens or channel contents.
func (p *KillfeedPublisher) logRouteFallback(reason string, err error) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	now := time.Now()
	if reason == "lookup_error" {
		if now.Sub(p.lastRouteErrLog) < time.Minute {
			return
		}
		p.lastRouteErrLog = now
		slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyKillfeed,
			"guild_id", p.routeGuildID, "server_id", p.routeServerID, "reason", reason, "err", err.Error())
		return
	}
	if p.lastRouteState == reason {
		return
	}
	p.lastRouteState = reason
	slog.Info("component=discord", "event", "channel_route_fallback", "route_key", routeKeyKillfeed,
		"guild_id", p.routeGuildID, "server_id", p.routeServerID, "reason", reason)
}

// SetFeed attaches the rotating batch/cycle feed. When set, PublishKill
// enqueues into it instead of sending immediately. Optional: unset falls
// back to sending immediately, same as before the rotating feed existed.
func (p *KillfeedPublisher) SetFeed(feed *RotatingFeed) {
	if p == nil {
		return
	}
	p.feed = feed
}

// NewKillfeedPublisher creates a publisher. channelID is the legacy env fallback;
// the stored GuildSetup.KillfeedChannelID takes priority when present.
func NewKillfeedPublisher(client *Client, channelID string) *KillfeedPublisher {
	return &KillfeedPublisher{client: client, fallback: channelID}
}

// BindStore attaches the setup store and guild so the configured channel wins.
func (p *KillfeedPublisher) BindStore(store SetupStore, guildID string) {
	if p == nil {
		return
	}
	p.store = store
	p.guildID = guildID
}

// channelID resolves the active killfeed channel: the installation's
// KILLFEED route first, then the legacy stored setup, then env. Exactly one
// channel is chosen, so a guild with both a route and a legacy channel
// publishes only to the route - never twice.
func (p *KillfeedPublisher) channelID() string {
	if id := p.RouteChannelID(); id != "" {
		return id
	}
	return p.legacyChannelID()
}

// legacyChannelID is the pre-route resolution: stored setup first, then env.
func (p *KillfeedPublisher) legacyChannelID() string {
	if p.store != nil && p.guildID != "" {
		if setup, err := p.store.Get(p.guildID); err == nil && setup != nil && setup.KillfeedChannelID != "" {
			return setup.KillfeedChannelID
		}
	}
	return p.fallback
}

// PublishKill sends a compact competitive embed for an authoritative PLAYER_KILL.
// Player IDs, coordinates, and raw ADM lines are never included. Only fields that
// are actually present are shown. Returns nil even on failure (errors are logged).
func (p *KillfeedPublisher) PublishKill(ev *killfeed.Event) error {
	if p == nil || p.client == nil || p.client.Session() == nil {
		return fmt.Errorf("discord client not ready")
	}
	channelID := p.channelID()
	if channelID == "" {
		return fmt.Errorf("killfeed channel not configured")
	}
	if ev == nil || ev.Type != killfeed.EventPlayerKill {
		return nil // only authoritative kills are published in Phase 3.0
	}

	// Build the style-aware Champion embed (one kill = one embed).
	embed := p.killCard(ev)

	victim, killer := "", ""
	if ev.Victim != nil {
		victim = ev.Victim.Name
	}
	if ev.Killer != nil {
		killer = ev.Killer.Name
	}

	// The rotating feed batches embeds and posts them on its own cycle; see
	// RotatingFeed. Without one configured, fall back to an immediate send.
	if p.feed != nil {
		p.feed.Enqueue(embed)
		slog.Debug("component=discord", "msg", "killfeed queued", "victim", victim, "killer", killer)
		return nil
	}

	// Suppress all mentions: no @everyone/@here/role/user pings from player names.
	send := &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{}, // parse nothing
		},
	}
	if _, err := p.client.Session().ChannelMessageSendComplex(channelID, send); err != nil {
		slog.Error("component=discord", "msg", "killfeed publish failed", "err", err.Error())
		return nil // never propagate; log processing must continue
	}
	slog.Debug("component=discord", "msg", "killfeed published", "victim", victim, "killer", killer)
	return nil
}
