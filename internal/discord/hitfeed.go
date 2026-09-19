package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// routeKeyHitfeed mirrors routing.RouteHitfeed (asserted equal in tests).
const routeKeyHitfeed = "HITFEED"

// HITFEED flood policy. A firefight can log dozens of PLAYER_HIT lines per
// second, so hits are never sent one-per-message:
//
//   - hits are aggregated per (attacker, victim, weapon) encounter over a short
//     fixed window that starts at the encounter's first hit;
//   - closed encounters are sent as compact embeds, batched up to
//     hitfeedEmbedsPerMessage per Discord message;
//   - at most hitfeedMessagesPerTick messages go out per hitfeedTick, so a
//     channel never sees more than one message every 2s however hot the fight;
//   - open encounters and the send backlog are both bounded; overflow is
//     dropped (oldest backlog first) and counted, never queued without limit.
//
// Worst case: 1 message / 2s carrying 10 cards (5 cards/s sustained).
const (
	hitfeedWindow           = 5 * time.Second
	hitfeedTick             = 2 * time.Second
	hitfeedMaxOpen          = 200
	hitfeedMaxReady         = 100
	hitfeedEmbedsPerMessage = 10
	hitfeedMessagesPerTick  = 1
	hitfeedMaxWeaponLen     = 60
	hitfeedMaxNameLen       = 40
)

// HitSender is the Discord surface the hit feed needs; *discordgo.Session
// satisfies it directly and tests use a fake.
type HitSender interface {
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// hitEncounter is one attacker->victim exchange with one weapon inside the
// aggregation window. It only carries fields the ADM parser actually produced.
type hitEncounter struct {
	attacker, victim, weapon string
	firstAt                  time.Time
	hits                     int

	// damage is the sum of parsed damage values; damageHits is how many hits
	// carried one, so a partial sum is never presented as the total.
	damage     float64
	damageHits int

	// Latest values win for per-hit fields.
	ammo, zone string
	distance   *float64
}

func (e *hitEncounter) add(ev *killfeed.Event) {
	e.hits++
	if ev.Damage != nil {
		e.damage += *ev.Damage
		e.damageHits++
	}
	if ev.Ammo != "" {
		e.ammo = ev.Ammo
	}
	if ev.HitZone != "" {
		e.zone = ev.HitZone
	}
	if ev.Distance != nil {
		d := *ev.Distance
		e.distance = &d
	}
}

// HitfeedPublisher publishes PLAYER_HIT events to the server's HITFEED route.
// One publisher belongs to one server worker and resolves through the shared
// routing.Resolver on that server's (guild, server) identity - never by guild
// alone - so two servers of one guild cannot leak hits into each other's channel.
//
// There is no legacy HITFEED channel and no fallback to KILLFEED: no route (or
// a failed lookup) means hits are simply not published. The hot path,
// PublishHit, only touches an in-memory map: all route lookups and Discord I/O
// happen on the Run goroutine, so a slow database or Discord can never stall
// ADM parsing.
type HitfeedPublisher struct {
	sender HitSender
	route  *RouteBinding
	now    func() time.Time

	// active mirrors "a route is currently configured", refreshed each tick.
	// Until a route exists PublishHit returns immediately, so guilds without a
	// HITFEED route pay nothing and hold no memory.
	active atomic.Bool

	mu      sync.Mutex
	channel string // route resolved on the last tick
	open    map[string]*hitEncounter
	ready   []*hitEncounter

	droppedOpen, droppedReady, failedSends int64
	lastDropLog                            time.Time
}

// NewHitfeedPublisher binds a publisher to one server. guildRowID is the
// internal guilds.id and serverID the game_servers.id of the owning worker.
func NewHitfeedPublisher(sender HitSender, resolver RouteResolver, guildRowID, serverID int64) *HitfeedPublisher {
	return &HitfeedPublisher{
		sender: sender,
		route:  NewRouteBinding(resolver, guildRowID, serverID, routeKeyHitfeed),
		now:    time.Now,
		open:   make(map[string]*hitEncounter),
	}
}

// PublishHit implements killfeed.HitPublisher. It never blocks and never fails.
// Only PLAYER_HIT is accepted: kills are the KILLFEED's job and must not be
// republished here.
func (p *HitfeedPublisher) PublishHit(ev *killfeed.Event) {
	if p == nil || ev == nil || ev.Type != killfeed.EventPlayerHit || ev.Attacker == nil || ev.Victim == nil {
		return
	}
	if !p.active.Load() {
		return
	}
	key := hitKey(ev)
	p.mu.Lock()
	defer p.mu.Unlock()
	enc := p.open[key]
	if enc == nil {
		if len(p.open) >= hitfeedMaxOpen {
			p.droppedOpen++
			return
		}
		enc = &hitEncounter{attacker: ev.Attacker.Name, victim: ev.Victim.Name, weapon: ev.Weapon, firstAt: p.now()}
		p.open[key] = enc
	}
	enc.add(ev)
}

// hitKey identifies an encounter by stable player ids (names when an id is
// missing) and weapon; a different weapon is a different card.
func hitKey(ev *killfeed.Event) string {
	who := func(p *killfeed.PlayerRef) string {
		if p.ID != "" {
			return "id:" + p.ID
		}
		return "n:" + p.Name
	}
	return who(ev.Attacker) + ">" + who(ev.Victim) + "|" + ev.Weapon
}

// Run drives the feed until ctx is done, then flushes what is still open once
// (best effort - hits are not durable). Start it as its own goroutine per
// server worker.
func (p *HitfeedPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.safeTick(false)
	ticker := time.NewTicker(hitfeedTick)
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

// Flush refreshes the route, closes every open encounter regardless of age and
// sends what the per-tick budget allows. Run calls it on shutdown; it also lets
// callers and tests drive the feed deterministically. Panic-safe, nil-safe.
func (p *HitfeedPublisher) Flush() {
	if p == nil {
		return
	}
	p.safeTick(true)
}

// safeTick runs one tick and survives any panic, so a bug here can never take
// down the worker (which shares the process with every other server).
func (p *HitfeedPublisher) safeTick(final bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=hitfeed", "msg", "tick panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	p.tick(final)
}

// tick refreshes the route, closes expired encounters and sends one budgeted
// batch. final closes every open encounter regardless of age (shutdown).
func (p *HitfeedPublisher) tick(final bool) {
	channel := p.route.ChannelID() // "" = no route / lookup failed: nothing to publish to
	p.active.Store(channel != "")

	now := p.now()
	p.mu.Lock()
	p.channel = channel
	if channel == "" {
		// No-op: drop anything gathered under an earlier route.
		p.open = make(map[string]*hitEncounter)
		p.ready = nil
		p.mu.Unlock()
		return
	}
	for key, enc := range p.open {
		if final || now.Sub(enc.firstAt) >= hitfeedWindow {
			p.ready = append(p.ready, enc)
			delete(p.open, key)
		}
	}
	sort.SliceStable(p.ready, func(i, j int) bool { return p.ready[i].firstAt.Before(p.ready[j].firstAt) })
	if over := len(p.ready) - hitfeedMaxReady; over > 0 {
		p.droppedReady += int64(over)
		p.ready = p.ready[over:] // keep the newest
	}
	n := hitfeedMessagesPerTick * hitfeedEmbedsPerMessage
	if n > len(p.ready) {
		n = len(p.ready)
	}
	batch := append([]*hitEncounter(nil), p.ready[:n]...)
	p.ready = p.ready[n:]
	dropped := p.takeDropLogLocked(now)
	p.mu.Unlock()

	if dropped != "" {
		slog.Warn("component=hitfeed", "event", "hitfeed_flood_drop", "detail", dropped)
	}
	for start := 0; start < len(batch); start += hitfeedEmbedsPerMessage {
		end := start + hitfeedEmbedsPerMessage
		if end > len(batch) {
			end = len(batch)
		}
		p.send(channel, batch[start:end])
	}
}

// send posts one message of up to hitfeedEmbedsPerMessage cards. A failure is
// logged and the cards are dropped: hits are ephemeral, and retrying would turn
// a Discord outage into an unbounded backlog.
func (p *HitfeedPublisher) send(channel string, encounters []*hitEncounter) {
	if p.sender == nil || len(encounters) == 0 {
		return
	}
	embeds := make([]*discordgo.MessageEmbed, 0, len(encounters))
	for _, enc := range encounters {
		embeds = append(embeds, buildHitEmbed(enc))
	}
	_, err := p.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Embeds:          embeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // player names never ping
	})
	if err != nil {
		p.mu.Lock()
		p.failedSends++
		p.mu.Unlock()
		slog.Warn("component=hitfeed", "event", "hitfeed_send_failed", "channel_id", channel, "cards", len(encounters), "err", err.Error())
	}
}

// takeDropLogLocked reports drop counters at most once a minute, so a flood
// produces one warning rather than one per dropped hit.
func (p *HitfeedPublisher) takeDropLogLocked(now time.Time) string {
	if p.droppedOpen == 0 && p.droppedReady == 0 {
		return ""
	}
	if !p.lastDropLog.IsZero() && now.Sub(p.lastDropLog) < time.Minute {
		return ""
	}
	p.lastDropLog = now
	return fmt.Sprintf("dropped_new_encounters=%d dropped_backlog_cards=%d", p.droppedOpen, p.droppedReady)
}

// buildHitEmbed renders one compact card from real parsed fields only:
//
//	🎯 HIT  PlayerA  ➜  PlayerB
//	M4-A1 · 556x45 · 42m
//	Torso · 3 hits · 84 dmg
//
// Missing weapon/ammo/distance/zone/damage are omitted, never shown as
// "unknown". HP and coordinates are parsed but deliberately not shown.
func buildHitEmbed(e *hitEncounter) *discordgo.MessageEmbed {
	name := func(s string) string {
		if s = sanitizeName(s); s == "" {
			return "Unknown"
		}
		return safeTrunc(s, hitfeedMaxNameLen)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🎯 **HIT**  %s  ➜  %s", name(e.attacker), name(e.victim))

	var gear []string
	if e.weapon != "" {
		gear = append(gear, safeTrunc(sanitizeName(e.weapon), hitfeedMaxWeaponLen))
	}
	if ammo := strings.TrimPrefix(e.ammo, "Bullet_"); ammo != "" {
		gear = append(gear, sanitizeName(ammo))
	}
	if e.distance != nil {
		gear = append(gear, fmt.Sprintf("%.0fm", *e.distance))
	}
	if len(gear) > 0 {
		b.WriteString("\n" + strings.Join(gear, " · "))
	}

	var detail []string
	if e.zone != "" {
		detail = append(detail, sanitizeName(e.zone))
	}
	if e.hits == 1 {
		detail = append(detail, "1 hit")
	} else {
		detail = append(detail, fmt.Sprintf("%d hits", e.hits))
	}
	if e.damageHits > 0 && e.damageHits == e.hits {
		detail = append(detail, fmt.Sprintf("%.0f dmg", e.damage))
	}
	b.WriteString("\n" + strings.Join(detail, " · "))

	return &discordgo.MessageEmbed{Description: b.String(), Color: presentation.InfoSteel}
}
