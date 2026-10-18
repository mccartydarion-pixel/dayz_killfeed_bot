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

// CONNECTIONS delivery policy. Every connect and disconnect is its own card
// (the customizable single-event card, docs/DISCORD_DESIGN.md row 5): Nitrado
// hands the log over in chunks of several minutes, so events that happened
// minutes apart reach the bot together, and merging them would turn almost
// every event into a summary line. Delivery is still bounded, because a
// server restart makes every player reconnect within seconds:
//
//   - one bounded queue and one goroutine per server worker (never a goroutine
//     per event);
//   - each tick sends at most connectionsCardsPerTick cards, one event each;
//   - a flood (more than connectionsSummaryThreshold events queued, or events
//     dropped) switches that tick to ONE summary message of up to
//     connectionsLinesPerMessage events; every event keeps its own line and
//     player - players are never merged;
//   - the queue holds at most connectionsMaxQueue events; on overflow the
//     OLDEST are dropped and the next message says how many were not shown.
//
// Worst case: connectionsCardsPerTick messages / 2s while cards are sent, or
// 1 message / 2s carrying 20 events (10 events/s sustained) under a flood.
const (
	connectionsTick             = 2 * time.Second
	connectionsMaxQueue         = 300
	connectionsLinesPerMessage  = 20
	connectionsCardsPerTick     = 5
	connectionsFinalCards       = 5
	connectionsSummaryThreshold = 12
	connectionsMaxNameLen       = 40
	connectionsMinShownSession  = time.Minute
	connectionsBatchTitle       = "🔌 **Server connections**"
	connectionsSingleConnected  = "🟢 Connection"
	connectionsSingleDisconnect = "🔴 Disconnection"
	connectionsAuthor           = "Connections notification"
	// connectionsEarningsTimeout bounds the economy lookup per disconnect card; up to
	// connectionsCardsPerTick cards share one 2s tick, so a slow database must not stall it.
	connectionsEarningsTimeout = 400 * time.Millisecond
)

// SessionEarningsFunc reports how much a player earned (credits to their Champion Points
// wallet) since they connected: the amount and whether the server keeps an economy at all. A
// server without one reports false and the card shows no "Cash earned" row. It runs on the
// publisher's own goroutine, never on the parser's.
type SessionEarningsFunc func(ctx context.Context, playerID string, since time.Time) (amount int64, ok bool)

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

	// Optional custom embed templates (nil = the Champion default, always). Only a
	// single-event card is customizable; a batched summary keeps the default.
	custom                      EmbedCustomizer
	serverName                  ServerNameFunc
	earnings                    SessionEarningsFunc
	routeGuildID, routeServerID int64

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

		routeGuildID: guildRowID, routeServerID: serverID,
	}
}

// SetCustomizer enables custom embed templates for this server's CONNECTIONS cards.
func (p *ConnectionsPublisher) SetCustomizer(c EmbedCustomizer, serverName ServerNameFunc) {
	if p == nil {
		return
	}
	p.custom = c
	p.serverName = serverName
}

// SetEarnings enables the "Cash earned" row of a disconnect card.
func (p *ConnectionsPublisher) SetEarnings(f SessionEarningsFunc) {
	if p == nil {
		return
	}
	p.earnings = f
}

// card renders one message: a single connect/disconnect (the normal case) may use the
// custom template; a flood batch (several events, or "earlier events not shown") is a
// summary list, which a single-event template cannot describe, so it always keeps the default.
func (p *ConnectionsPublisher) card(batch []killfeed.ConnectionNotice, omitted int64) *discordgo.MessageEmbed {
	if len(batch) != 1 || omitted != 0 {
		return buildConnectionsEmbed(batch, omitted)
	}
	server := serverNameOf(p.serverName, p.routeServerID)
	n := batch[0]
	cash, cashKnown := p.sessionEarnings(n)
	def := buildConnectionEmbed(n, server, cash, cashKnown)
	return customEmbed(p.custom, p.routeGuildID, p.routeServerID, routeKeyConnections, def, func() map[string]string {
		m := connectionVars(n, server)
		if cashKnown {
			m["cash_earned"] = formatAmount(cash)
		}
		return m
	})
}

// sessionEarnings asks the earnings hook, bounded by connectionsEarningsTimeout, for what a
// disconnecting player earned since they connected. Unknown (no hook, no economy, no observed
// connect, lookup failed) is reported as not known and the row is left off the card.
func (p *ConnectionsPublisher) sessionEarnings(n killfeed.ConnectionNotice) (int64, bool) {
	if p.earnings == nil || n.Kind != killfeed.ConnectionDisconnected || n.PlayerID == "" || n.Session <= 0 {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectionsEarningsTimeout)
	defer cancel()
	since := connectionTime(n).Add(-n.Session)
	return p.earnings(ctx, n.PlayerID, since)
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
	messages := connectionsCardsPerTick
	if final {
		messages = connectionsFinalCards
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
		if len(batch) > 1 || omitted > 0 {
			return // a flood tick: one summary message, the rest waits for the next tick
		}
	}
}

// takeBatch removes the next message's worth of events from the queue: one
// event (its own card) normally, up to connectionsLinesPerMessage events (a
// summary) when the queue is a flood or earlier events were dropped. It also
// returns how many earlier events were dropped since the last message.
func (p *ConnectionsPublisher) takeBatch() ([]killfeed.ConnectionNotice, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil, 0
	}
	n := 1
	if len(p.queue) > connectionsSummaryThreshold || p.notShown > 0 {
		n = connectionsLinesPerMessage
	}
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
	_, err := deliverMessage(p.sender, "CONNECTIONS", channel, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{p.card(batch, omitted)},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // player names never ping
	})
	if err != nil {
		p.mu.Lock()
		p.failedSends++
		p.mu.Unlock()
		slog.Warn("component=connections", "event", "connections_send_failed", "channel_id", channel, "events", len(batch), "err", err.Error())
	}
}

// connectionTime is when the notice happened: the line's own time when the engine derived it,
// else when the line was read.
func connectionTime(n killfeed.ConnectionNotice) time.Time {
	if !n.At.IsZero() {
		return n.At
	}
	return time.Now()
}

// buildConnectionEmbed renders a lone connect or disconnect:
//
//	🟢 Connection                              (title)
//	**Connection – 5:42:53 PM**                (the line's own time, in the reader's zone)
//	`Ceiyxe` is connected
//
//	🔴 Disconnection
//	**Disconnection – 5:48:58 PM**
//	`titanlaxman` has disconnected
//	Session time   00H 33M 26S                 (when Champion saw the connect)
//	Location       9047.2;11890.0;227.7        (when the line carries a position)
//	Cash earned    3,500                       (servers with an economy)
//
// The display name is shown in code so it reads as data (no markdown, no pings); the ADM id
// is never shown. The footer names the server; the embed timestamp is the event's time.
func buildConnectionEmbed(n killfeed.ConnectionNotice, server string, cash int64, cashKnown bool) *discordgo.MessageEmbed {
	at := connectionTime(n)
	name := connectionName(n.Name)
	e := &discordgo.MessageEmbed{Author: presentation.BrandAuthor(connectionsAuthor), Footer: presentation.Footer(server, "")}
	if n.Kind == killfeed.ConnectionConnected {
		e.Title, e.Color = connectionsSingleConnected, presentation.SuccessGreen
		e.Description = "**Connection – " + presentation.Timestamp(at, 'T') + "**\n`" + name + "` is connected"
	} else {
		e.Title, e.Color = connectionsSingleDisconnect, presentation.Red
		e.Description = "**Disconnection – " + presentation.Timestamp(at, 'T') + "**\n`" + name + "` has disconnected"
		if n.Session > 0 {
			presentation.AppendFields(e, presentation.StatusField("Session time", formatSessionClock(n.Session), false))
		}
		if n.Position != nil {
			presentation.AppendFields(e, presentation.StatusField("Location", formatLocation(*n.Position), false))
		}
		if cashKnown {
			presentation.AppendFields(e, presentation.StatusField("Cash earned", formatAmount(cash), false))
		}
	}
	presentation.StampEmbed(e, at)
	return e
}

// formatSessionClock renders a session as "00H 33M 26S".
func formatSessionClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	sec := int(d/time.Second) % 60
	return fmt.Sprintf("%02dH %02dM %02dS", h, m, sec)
}

// formatLocation renders a position as the ADM prints it, "x;y;z" to one decimal.
func formatLocation(p killfeed.Position) string {
	return fmt.Sprintf("%.1f;%.1f;%.1f", p.X, p.Y, p.Z)
}

// buildConnectionsEmbed renders one message.
//
//	single:  buildConnectionEmbed (the card above, without the economy row).
//	batch:   🔌 SERVER CONNECTIONS, one line per event, one player per line.
//
// Only the display name and (for a disconnect, when Champion observed the
// connect) the session length are shown in a batch - never the ADM id.
func buildConnectionsEmbed(batch []killfeed.ConnectionNotice, omitted int64) *discordgo.MessageEmbed {
	if len(batch) == 1 && omitted == 0 {
		return buildConnectionEmbed(batch[0], "", 0, false)
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
			b.WriteString(" • " + s)
		}
	}
	if omitted > 0 {
		b.WriteString("\n… " + presentation.Plural(omitted, "earlier connection event was", "earlier connection events were") + " not shown")
	}
	return &discordgo.MessageEmbed{Description: b.String(), Color: presentation.InfoSteel}
}

// connectionName sanitises a display name: mention/channel characters and
// control characters are removed and the length is bounded.
func connectionName(s string) string {
	return presentation.SafeName(s, connectionsMaxNameLen)
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
