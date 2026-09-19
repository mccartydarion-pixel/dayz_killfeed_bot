package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// routeKeyPveFeed mirrors routing.RoutePveFeed (asserted equal in tests).
const routeKeyPveFeed = "PVE_FEED"

// PVE_FEED delivery policy. Provably non-PvP deaths are rare, but they are
// bounded anyway: one queue and one goroutine per server worker, at most one
// message per tick carrying up to pveEmbedsPerMessage cards (one card per death,
// so different players are never merged), and a bounded queue whose overflow
// drops the OLDEST notices and says so in the next message.
const (
	pveTick             = 2 * time.Second
	pveMaxQueue         = 100
	pveEmbedsPerMessage = 10
	pveMessagesPerTick  = 1
	pveFinalMessages    = 3
	pveMaxNameLen       = 40
)

// PveFeedPublisher publishes provably non-PvP deaths to the server's PVE_FEED
// route. One publisher belongs to one server worker and resolves through the
// shared routing.Resolver on that server's (guild, server) identity - never by
// guild alone.
//
// PVE_FEED has no legacy channel and no fallback: no route (or a failed lookup)
// means it publishes nothing and never falls back to KILLFEED. What happens to an
// UNCLAIMED death is not this type's business - PublishPveDeath returns false and
// the engine leaves the death to the pre-existing legacy death feed, exactly as
// before PVE_FEED existed. A death it does claim is published only here.
//
// PublishPveDeath only appends to a bounded in-memory queue; route lookups and
// Discord I/O run on Run's goroutine, so a slow database or Discord can never
// stall persistence, ADM parsing or the other feeds.
type PveFeedPublisher struct {
	sender HitSender // the narrow Discord surface *discordgo.Session satisfies
	route  *RouteBinding

	// Optional custom embed templates (nil = the Champion default, always).
	// Presentation only: which deaths the feed owns is decided elsewhere.
	custom                      EmbedCustomizer
	serverName                  ServerNameFunc
	routeGuildID, routeServerID int64

	// active mirrors "a route is configured", refreshed each tick. It is what
	// decides claiming: no route -> nothing is claimed and nothing is buffered.
	active atomic.Bool

	mu          sync.Mutex
	channel     string
	queue       []killfeed.PveDeathNotice
	dropped     int64
	notShown    int64
	failedSends int64
	lastWarned  time.Time
}

// NewPveFeedPublisher binds a publisher to one server. guildRowID is the internal
// guilds.id and serverID the game_servers.id of the owning worker.
func NewPveFeedPublisher(sender HitSender, resolver RouteResolver, guildRowID, serverID int64) *PveFeedPublisher {
	return &PveFeedPublisher{
		sender: sender,
		route:  NewRouteBinding(resolver, guildRowID, serverID, routeKeyPveFeed),

		routeGuildID: guildRowID, routeServerID: serverID,
	}
}

// SetCustomizer enables custom embed templates for this server's PVE_FEED cards.
func (p *PveFeedPublisher) SetCustomizer(c EmbedCustomizer, serverName ServerNameFunc) {
	if p == nil {
		return
	}
	p.custom = c
	p.serverName = serverName
}

func (p *PveFeedPublisher) card(n killfeed.PveDeathNotice) *discordgo.MessageEmbed {
	def := buildPveEmbed(n)
	return customEmbed(p.custom, p.routeGuildID, p.routeServerID, routeKeyPveFeed, def, func() map[string]string {
		return pveVars(n, serverNameOf(p.serverName, p.routeServerID))
	})
}

// PublishPveDeath implements killfeed.PveDeathPublisher. It never blocks and
// never fails. It claims (returns true) only a death of a supported, proven
// cause while a route is configured; anything else is left unclaimed.
func (p *PveFeedPublisher) PublishPveDeath(n killfeed.PveDeathNotice) bool {
	if p == nil || !pveCauseSupported(n.Cause) || !p.active.Load() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) >= pveMaxQueue {
		copy(p.queue, p.queue[1:]) // drop the oldest, keep the newest
		p.queue = p.queue[:len(p.queue)-1]
		p.dropped++
		p.notShown++
	}
	p.queue = append(p.queue, n)
	return true
}

func pveCauseSupported(c killfeed.DeathCause) bool {
	switch c {
	case killfeed.DeathCauseSuicide, killfeed.DeathCauseInfected, killfeed.DeathCauseAnimal, killfeed.DeathCauseEnvironment:
		return true
	}
	return false
}

// Run drives the feed until ctx is done, then flushes what is queued (best
// effort). Start it as its own goroutine per server worker.
func (p *PveFeedPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.safeTick(false)
	ticker := time.NewTicker(pveTick)
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
// number of messages. Panic-safe, nil-safe; lets callers and tests drive the
// feed deterministically.
func (p *PveFeedPublisher) Flush() {
	if p == nil {
		return
	}
	p.safeTick(true)
}

func (p *PveFeedPublisher) safeTick(final bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=pvefeed", "msg", "tick panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	p.tick(final)
}

func (p *PveFeedPublisher) tick(final bool) {
	channel := p.route.ChannelID() // "" = no route / lookup failed
	p.active.Store(channel != "")

	p.mu.Lock()
	p.channel = channel
	if channel == "" {
		p.queue = nil // route removed: nothing gathered under it is sent
		p.notShown = 0
		p.mu.Unlock()
		return
	}
	messages := pveMessagesPerTick
	if final {
		messages = pveFinalMessages
	}
	warn := ""
	if p.dropped > 0 && (p.lastWarned.IsZero() || time.Since(p.lastWarned) >= time.Minute) {
		p.lastWarned = time.Now()
		warn = fmt.Sprintf("dropped_events=%d", p.dropped)
	}
	p.mu.Unlock()

	if warn != "" {
		slog.Warn("component=pvefeed", "event", "pvefeed_flood_drop", "detail", warn)
	}
	for i := 0; i < messages; i++ {
		batch, omitted := p.takeBatch()
		if len(batch) == 0 {
			return
		}
		p.send(channel, batch, omitted)
	}
}

func (p *PveFeedPublisher) takeBatch() ([]killfeed.PveDeathNotice, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return nil, 0
	}
	n := pveEmbedsPerMessage
	if n > len(p.queue) {
		n = len(p.queue)
	}
	batch := append([]killfeed.PveDeathNotice(nil), p.queue[:n]...)
	p.queue = p.queue[n:]
	omitted := p.notShown
	p.notShown = 0
	return batch, omitted
}

// send posts one message. A failure is logged and the notices are dropped (no
// retry backlog): it cannot reach persistence, ADM parsing or any other feed.
func (p *PveFeedPublisher) send(channel string, batch []killfeed.PveDeathNotice, omitted int64) {
	if p.sender == nil {
		return
	}
	embeds := make([]*discordgo.MessageEmbed, 0, len(batch))
	for _, n := range batch {
		embeds = append(embeds, p.card(n))
	}
	if omitted > 0 {
		embeds[0].Description += fmt.Sprintf("\n… %d earlier PvE deaths were not shown", omitted)
	}
	_, err := p.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Embeds:          embeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // player names never ping
	})
	if err != nil {
		p.mu.Lock()
		p.failedSends++
		p.mu.Unlock()
		slog.Warn("component=pvefeed", "event", "pvefeed_send_failed", "channel_id", channel, "cards", len(batch), "err", err.Error())
	}
}

// buildPveEmbed renders one compact card. The wording only ever states the cause
// the parser proved; there is no generic "unknown cause" card because an
// unproven death is never claimed in the first place.
//
//	💀 SUICIDE        ☣️ PVE DEATH        🐺 PVE DEATH        ⚠️ PVE DEATH
//	Name died by      Name was killed     Name was killed     Name died to the
//	suicide.          by an infected.     by an animal.       environment.
//
// Only the sanitised display name is shown - no id, coordinates or weapon (a
// suicide's "weapon" is just the item held, not a cause).
func buildPveEmbed(n killfeed.PveDeathNotice) *discordgo.MessageEmbed {
	name := sanitizeName(n.Name)
	if name == "" {
		name = "Unknown"
	}
	name = safeTrunc(name, pveMaxNameLen)

	var text string
	switch n.Cause {
	case killfeed.DeathCauseSuicide:
		text = fmt.Sprintf("💀 **SUICIDE**\n%s died by suicide.", name)
	case killfeed.DeathCauseInfected:
		text = fmt.Sprintf("☣️ **PVE DEATH**\n%s was killed by an infected.", name)
	case killfeed.DeathCauseAnimal:
		text = fmt.Sprintf("🐺 **PVE DEATH**\n%s was killed by an animal.", name)
	default: // DeathCauseEnvironment (the only other supported value)
		text = fmt.Sprintf("⚠️ **PVE DEATH**\n%s died to the environment.", name)
	}
	return &discordgo.MessageEmbed{Description: text, Color: presentation.WarningAmber}
}
