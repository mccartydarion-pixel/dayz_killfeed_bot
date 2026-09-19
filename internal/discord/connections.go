package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// routeKeyConnections mirrors routing.RouteConnections (asserted equal in tests).
const routeKeyConnections = "CONNECTIONS"

// CONNECTIONS delivery policy. Connections are far rarer than hits, but a
// server restart makes every player reconnect within seconds, so delivery is
// bounded rather than one message per event:
//
//   - one bounded queue and one goroutine per server worker (never a goroutine
//     per event);
//   - each tick sends at most ONE message, batching up to
//     connectionsLinesPerMessage events into a single embed (a lone event gets
//     the plain single-line card);
//   - every event keeps its own line and player - players are never merged;
//   - the queue holds at most connectionsMaxQueue events; on overflow the
//     OLDEST are dropped and the next message says how many were not shown.
//
// Worst case: 1 message / 2s carrying 20 events (10 events/s sustained).
const (
	connectionsTick             = 2 * time.Second
	connectionsMaxQueue         = 300
	connectionsLinesPerMessage  = 20
	connectionsMessagesPerTick  = 1
	connectionsFinalMessages    = 3
	connectionsMaxNameLen       = 40
	connectionsMinShownSession  = time.Minute
	connectionsBatchTitle       = "🔌 **SERVER CONNECTIONS**"
	connectionsSingleConnected  = "🟢 **CONNECTED**"
	connectionsSingleDisconnect = "🔴 **DISCONNECTED**"
)

// ConnectionsPublisher publishes authoritative connect/disconnect state changes
// to the server's CONNECTIONS route. One publisher belongs to one server worker
// and resolves through the shared routing.Resolver on that server's (guild,
// server) identity - never by guild alone.
//
// There is no legacy CONNECTIONS text channel: no route (or a failed lookup)
// means nothing is published, and it never falls back to KILLFEED or to the
// player-count voice channel. PublishConnection only appends to a bounded
// in-memory queue; route lookups and Discord I/O run on Run's goroutine, so a
// slow database or Discord can never stall ADM parsing or presence tracking.
type ConnectionsPublisher struct {
	sender HitSender // the same narrow Discord surface *discordgo.Session satisfies
	route  *RouteBinding

	// active mirrors "a route is configured", refreshed each tick, so a guild
	// without a route pays nothing and buffers nothing.
	active atomic.Bool

	mu             sync.Mutex
	channel        string
	queue          []killfeed.ConnectionNotice
	dropped        int64 // total events dropped by the queue bound
	notShown       int64 // dropped since the last message, reported in it
	failedSends    int64
	lastDropWarned time.Time
}

// NewConnectionsPublisher binds a publisher to one server. guildRowID is the
// internal guilds.id and serverID the game_servers.id of the owning worker.
func NewConnectionsPublisher(sender HitSender, resolver RouteResolver, guildRowID, serverID int64) *ConnectionsPublisher {
	return &ConnectionsPublisher{
		sender: sender,
		route:  NewRouteBinding(resolver, guildRowID, serverID, routeKeyConnections),
	}
}

// PublishConnection implements killfeed.ConnectionPublisher. It never blocks and
// never fails. Only the two states the ADM log states explicitly are accepted;
// there is no reconnect kind and none is inferred.
func (p *ConnectionsPublisher) PublishConnection(n killfeed.ConnectionNotice) {
	if p == nil || (n.Kind != killfeed.ConnectionConnected && n.Kind != killfeed.ConnectionDisconnected) {
		return
	}
	if !p.active.Load() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) >= connectionsMaxQueue {
		copy(p.queue, p.queue[1:]) // drop the oldest, keep the newest
		p.queue = p.queue[:len(p.queue)-1]
		p.dropped++
		p.notShown++
	}
	p.queue = append(p.queue, n)
}

// Run drives the feed until ctx is done, then flushes what is queued (best
// effort - connection notices are not durable). Start it as its own goroutine
// per server worker.
func (p *ConnectionsPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.safeTick(false)
	ticker := time.NewTicker(connectionsTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.safeTick(true)
			return
		case <-ticker.C:
			p.safeTick(false)
		}
	}
}

// Flush refreshes the route and sends what is queued, up to a small bounded
// number of messages. Run calls it on shutdown; it also lets callers and tests
// drive the feed deterministically. Panic-safe, nil-safe.
func (p *ConnectionsPublisher) Flush() {
	if p == nil {
		return
	}
	p.safeTick(true)
}

func (p *ConnectionsPublisher) safeTick(final bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=connections", "msg", "tick panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	p.tick(final)
}

// tick refreshes the route and sends the budgeted number of messages.
func (p *ConnectionsPublisher) tick(final bool) {
	channel := p.route.ChannelID() // "" = no route / lookup failed: nothing to publish to
	p.active.Store(channel != "")

	p.mu.Lock()
	p.channel = channel
	if channel == "" {
		p.queue = nil // no-op: nothing gathered under an earlier route is sent
		p.notShown = 0
		p.mu.Unlock()
		return
	}
	messages := connectionsMessagesPerTick
	if final {
		messages = connectionsFinalMessages
	}
	dropWarn := ""
	if p.dropped > 0 && (p.lastDropWarned.IsZero() || time.Since(p.lastDropWarned) >= time.Minute) {
		p.lastDropWarned = time.Now()
		dropWarn = fmt.Sprintf("dropped_events=%d", p.dropped)
	}
	p.mu.Unlock()

	if dropWarn != "" {
		slog.Warn("component=connections", "event", "connections_flood_drop", "detail", dropWarn)
	}
	for i := 0; i < messages; i++ {
		batch, omitted := p.takeBatch()
		if len(batch) == 0 {
			return
		}
		p.send(channel, batch, omitted)
	}
}

// takeBatch removes up to one message's worth of events from the queue and
// returns how many earlier events were dropped since the last message.
func (p *ConnectionsPublisher) takeBatch() ([]killfeed.ConnectionNotice, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil, 0
	}
	n := connectionsLinesPerMessage
	if n > len(p.queue) {
		n = len(p.queue)
	}
	batch := append([]killfeed.ConnectionNotice(nil), p.queue[:n]...)
	p.queue = p.queue[n:]
	omitted := p.notShown
	p.notShown = 0
	return batch, omitted
}

// send posts one message. A failure is logged and the events are dropped:
// connection notices are ephemeral, and retrying would turn a Discord outage
// into an unbounded backlog.
func (p *ConnectionsPublisher) send(channel string, batch []killfeed.ConnectionNotice, omitted int64) {
	if p.sender == nil {
		return
	}
	_, err := p.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{buildConnectionsEmbed(batch, omitted)},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // player names never ping
	})
	if err != nil {
		p.mu.Lock()
		p.failedSends++
		p.mu.Unlock()
		slog.Warn("component=connections", "event", "connections_send_failed", "channel_id", channel, "events", len(batch), "err", err.Error())
	}
}

// buildConnectionsEmbed renders one message.
//
//	single:  🟢 CONNECTED / PlayerName joined the server.
//	         🔴 DISCONNECTED / PlayerName left the server. / Session: 42m
//	batch:   🔌 SERVER CONNECTIONS, one line per event, one player per line.
//
// Only the display name and (for a disconnect, when Champion observed the
// connect) the session length are ever shown - never the ADM id, coordinates or
// any Discord link state. A session under a minute is omitted.
func buildConnectionsEmbed(batch []killfeed.ConnectionNotice, omitted int64) *discordgo.MessageEmbed {
	if len(batch) == 1 && omitted == 0 {
		n := batch[0]
		name := connectionName(n.Name)
		if n.Kind == killfeed.ConnectionConnected {
			return &discordgo.MessageEmbed{Description: fmt.Sprintf("%s\n%s joined the server.", connectionsSingleConnected, name), Color: presentation.SuccessGreen}
		}
		desc := fmt.Sprintf("%s\n%s left the server.", connectionsSingleDisconnect, name)
		if s := formatSession(n.Session); s != "" {
			desc += "\nSession: " + s
		}
		return &discordgo.MessageEmbed{Description: desc, Color: presentation.ErrorRed}
	}

	var b strings.Builder
	b.WriteString(connectionsBatchTitle)
	for _, n := range batch {
		name := connectionName(n.Name)
		if n.Kind == killfeed.ConnectionConnected {
			fmt.Fprintf(&b, "\n🟢 %s connected", name)
			continue
		}
		fmt.Fprintf(&b, "\n🔴 %s disconnected", name)
		if s := formatSession(n.Session); s != "" {
			b.WriteString(" · " + s)
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n… %d earlier connection events were not shown", omitted)
	}
	return &discordgo.MessageEmbed{Description: b.String(), Color: presentation.InfoSteel}
}

// connectionName sanitises a display name: mention/channel characters and
// control characters are removed and the length is bounded.
func connectionName(s string) string {
	if s = sanitizeName(s); s == "" {
		return "Unknown"
	}
	return safeTrunc(s, connectionsMaxNameLen)
}

// formatSession renders an observed session as "42m" or "1h 5m"; anything under
// a minute (or unknown) yields "" so it is omitted.
func formatSession(d time.Duration) string {
	if d < connectionsMinShownSession {
		return ""
	}
	mins := int(d / time.Minute)
	if mins >= 60 {
		if rem := mins % 60; rem > 0 {
			return fmt.Sprintf("%dh %dm", mins/60, rem)
		}
		return fmt.Sprintf("%dh", mins/60)
	}
	return fmt.Sprintf("%dm", mins)
}
