package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Route keys of the bounty feeds; they mirror routing.RouteBounty and
// routing.RouteBountyTracking (asserted equal in tests).
const (
	routeKeyBounty         = "BOUNTY"
	routeKeyBountyTracking = "BOUNTY_TRACKING"
)

// Neither feed is ever needed for correctness: a bounty is created, claimed and
// expired in the database whether or not any route is configured or Discord is
// reachable. These types only report state that has already been committed.

// --- formatting ------------------------------------------------------------------

// formatAmount renders an amount with thousands separators (250000 -> "250,000").
func formatAmount(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func bountyName(s string) string {
	if s = sanitizeName(s); s == "" {
		return "Unknown"
	}
	return safeTrunc(s, 40)
}

// --- BOUNTY_TRACKING: lifecycle feed ----------------------------------------------

// BountyTracker publishes bounty lifecycle events (placed, increased, claimed,
// expired, cancelled) to each affected server's BOUNTY_TRACKING route. Events
// arrive from the bounty Service strictly AFTER their database change committed,
// and only ever describe that committed state; a Discord failure therefore can
// never undo or repeat a claim.
//
// Routing is per (guild row, server): an event scoped to a server goes to that
// server's route; a claim (or streak placement) goes to the server the kill
// happened on; a guild-wide event with no server goes to every server's route
// (deduplicated by channel). No route, or a failed lookup, is a no-op (with a
// throttled warning) - there is no fallback to any other channel.
//
// Notify only appends to a bounded queue; route lookups and Discord I/O run on
// Run's goroutine, one message (up to 10 cards) per channel per tick.
type BountyTracker struct {
	sender   HitSender
	resolver RouteResolver
	servers  GuildServersFunc

	// Optional custom embed templates for the lifecycle cards (nil = default). The
	// persistent BOUNTY board is NOT customizable: it is a multi-row view, not a
	// single-event card (docs/EMBED_RUNTIME.md).
	custom     EmbedCustomizer
	serverName ServerNameFunc

	mu          sync.Mutex
	queue       []bounties.Event
	dropped     int64
	failedSends int64
	lastWarn    map[string]time.Time
}

const (
	bountyTrackerTick          = 2 * time.Second
	bountyTrackerMaxQueue      = 200
	bountyTrackerEventsPerTick = 20
	bountyTrackerFinalEvents   = 60
	bountyEmbedsPerMessage     = 10
)

func NewBountyTracker(sender HitSender, resolver RouteResolver, servers GuildServersFunc) *BountyTracker {
	return &BountyTracker{sender: sender, resolver: resolver, servers: servers, lastWarn: make(map[string]time.Time)}
}

// Notify implements bounties.Notifier. It never blocks and never fails.
// SetCustomizer enables custom embed templates for BOUNTY_TRACKING cards.
func (t *BountyTracker) SetCustomizer(c EmbedCustomizer, serverName ServerNameFunc) {
	if t == nil {
		return
	}
	t.custom = c
	t.serverName = serverName
}

// card builds the lifecycle card for one event as shown on one server's route.
func (t *BountyTracker) card(e bounties.Event, serverID int64) *discordgo.MessageEmbed {
	def := buildBountyEventEmbed(e)
	return customEmbed(t.custom, e.GuildID, serverID, routeKeyBountyTracking, def, func() map[string]string {
		return bountyVars(e, serverNameOf(t.serverName, serverID))
	})
}

func (t *BountyTracker) Notify(e bounties.Event) {
	if t == nil || e.Kind == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.queue) >= bountyTrackerMaxQueue {
		copy(t.queue, t.queue[1:]) // drop the oldest
		t.queue = t.queue[:len(t.queue)-1]
		t.dropped++
	}
	t.queue = append(t.queue, e)
}

// Run drives the feed until ctx is done, then flushes what is queued.
func (t *BountyTracker) Run(ctx context.Context) {
	if t == nil {
		return
	}
	ticker := time.NewTicker(bountyTrackerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.safeTick(true)
			return
		case <-ticker.C:
			t.safeTick(false)
		}
	}
}

// Flush sends what is queued (a bounded amount). Panic-safe, nil-safe.
func (t *BountyTracker) Flush() {
	if t == nil {
		return
	}
	t.safeTick(true)
}

func (t *BountyTracker) safeTick(final bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=bounty_tracking", "msg", "tick panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	t.tick(final)
}

func (t *BountyTracker) tick(final bool) {
	n := bountyTrackerEventsPerTick
	if final {
		n = bountyTrackerFinalEvents
	}
	t.mu.Lock()
	if n > len(t.queue) {
		n = len(t.queue)
	}
	batch := append([]bounties.Event(nil), t.queue[:n]...)
	t.queue = t.queue[n:]
	dropped := t.dropped
	t.dropped = 0
	t.mu.Unlock()
	if dropped > 0 {
		slog.Warn("component=bounty_tracking", "event", "bounty_tracking_flood_drop", "dropped_events", dropped)
	}
	if len(batch) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	perChannel := make(map[string][]*discordgo.MessageEmbed)
	var order []string
	for _, e := range batch {
		for _, target := range t.targetsFor(ctx, e) {
			ch := target.channel
			if _, seen := perChannel[ch]; !seen {
				order = append(order, ch)
			}
			perChannel[ch] = append(perChannel[ch], t.card(e, target.serverID))
		}
	}
	for _, ch := range order {
		embeds := perChannel[ch]
		for start := 0; start < len(embeds); start += bountyEmbedsPerMessage {
			end := start + bountyEmbedsPerMessage
			if end > len(embeds) {
				end = len(embeds)
			}
			t.send(ch, embeds[start:end])
		}
	}
}

// channelsFor resolves the BOUNTY_TRACKING channels an event goes to. A missing
// route or a lookup error contributes nothing.
// channelTarget is one destination channel and the server whose route it is (the
// server decides which installation's template applies).
type channelTarget struct {
	channel  string
	serverID int64
}

func (t *BountyTracker) targetsFor(ctx context.Context, e bounties.Event) []channelTarget {
	if t.resolver == nil {
		return nil
	}
	var serverIDs []int64
	switch {
	case e.ServerID > 0:
		serverIDs = []int64{e.ServerID}
	case e.KillServerID > 0:
		serverIDs = []int64{e.KillServerID}
	default:
		if t.servers == nil {
			return nil
		}
		guildRowID, ids, err := t.servers(ctx)
		if err != nil || guildRowID != e.GuildID {
			return nil
		}
		serverIDs = ids
	}
	seen := map[string]bool{}
	var out []channelTarget
	for _, id := range serverIDs {
		ch, found, err := t.resolver.Resolve(ctx, e.GuildID, id, routeKeyBountyTracking)
		if err != nil {
			t.warnLookup(e.GuildID, id, err)
			continue
		}
		if !found || ch == "" || seen[ch] {
			continue
		}
		seen[ch] = true
		out = append(out, channelTarget{channel: ch, serverID: id})
	}
	return out
}

func (t *BountyTracker) warnLookup(guildRowID, serverID int64, err error) {
	key := fmt.Sprintf("%d|%d", guildRowID, serverID)
	t.mu.Lock()
	last := t.lastWarn[key]
	if time.Since(last) < time.Minute {
		t.mu.Unlock()
		return
	}
	t.lastWarn[key] = time.Now()
	t.mu.Unlock()
	slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyBountyTracking, "guild_id", guildRowID, "server_id", serverID, "reason", "lookup_error", "err", err.Error())
}

// send posts one message. A failure is logged and dropped (no retry backlog): the
// bounty change it describes is already committed and must never be redone.
func (t *BountyTracker) send(channel string, embeds []*discordgo.MessageEmbed) {
	if t.sender == nil || len(embeds) == 0 {
		return
	}
	_, err := t.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Embeds:          embeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // names never ping
	})
	if err != nil {
		t.mu.Lock()
		t.failedSends++
		t.mu.Unlock()
		slog.Warn("component=bounty_tracking", "event", "bounty_tracking_send_failed", "channel_id", channel, "cards", len(embeds), "err", err.Error())
	}
}

// buildBountyEventEmbed renders one lifecycle card from committed state only.
// Weapon and distance appear on a claim only when the authoritative kill had
// them; no internal id is ever shown. Amounts are Champion Points.
func buildBountyEventEmbed(e bounties.Event) *discordgo.MessageEmbed {
	target := bountyName(e.Target)
	value := formatAmount(e.Amount) + " pts"
	var b strings.Builder
	color := presentation.EventGold
	switch e.Kind {
	case bounties.EventPlaced:
		b.WriteString("🎯 **BOUNTY PLACED**")
		fmt.Fprintf(&b, "\nTarget: %s\nValue: %s", target, value)
		if e.Automatic {
			b.WriteString("\nSource: kill streak")
		}
	case bounties.EventIncreased:
		b.WriteString("📈 **BOUNTY INCREASED**")
		fmt.Fprintf(&b, "\nTarget: %s\nValue: %s", target, value)
		if e.Automatic {
			b.WriteString("\nSource: kill streak")
		}
	case bounties.EventClaimed:
		color = presentation.SuccessGreen
		b.WriteString("💰 **BOUNTY CLAIMED**")
		fmt.Fprintf(&b, "\nHunter: %s\nTarget: %s\nValue: %s", bountyName(e.Hunter), target, value)
		if e.Count > 1 {
			fmt.Fprintf(&b, "\nBounties: %d", e.Count)
		}
		if w := strings.TrimSpace(e.Weapon); w != "" {
			fmt.Fprintf(&b, "\nWeapon: %s", safeTrunc(sanitizeName(w), maxWeaponLen))
		}
		if e.Distance != nil {
			fmt.Fprintf(&b, "\nDistance: %.0fm", *e.Distance)
		}
	case bounties.EventExpired:
		color = presentation.WarningAmber
		b.WriteString("⌛ **BOUNTY EXPIRED**")
		fmt.Fprintf(&b, "\nTarget: %s\nValue: %s", target, value)
	case bounties.EventCancelled:
		color = presentation.WarningAmber
		b.WriteString("🚫 **BOUNTY CANCELLED**")
		fmt.Fprintf(&b, "\nTarget: %s\nValue: %s", target, value)
	default:
		b.WriteString("🎯 **BOUNTY**")
		fmt.Fprintf(&b, "\nTarget: %s", target)
	}
	return &discordgo.MessageEmbed{Description: b.String(), Color: color}
}

// --- BOUNTY: the public board ---------------------------------------------------------

// BoardLister supplies a channel's board entries (*repository.BountyRepository).
type BoardLister interface {
	ListBoard(ctx context.Context, guildRowID int64, serverIDs []int64, limit int) ([]repository.BoardEntry, error)
}

// BountyBoard keeps ONE persistent board message per routed BOUNTY channel,
// reconciled from the database. The message id is recorded durably in
// guild_route_panels, so a restart, a route change, or any create/claim/expire/
// cancel edits or moves the same board instead of posting a new one.
//
// Like every other guild-level routed panel it resolves BOUNTY per (guild row,
// server) for each active server of the guild and keeps one board per distinct
// channel; a channel shared by several servers shows the union of their bounties
// (plus guild-wide ones), a server's own channel shows only its own. A bounty
// scoped to another server never appears. No route -> no board (a previously
// routed board is retired); a failed lookup leaves existing boards alone. The
// database is the source of truth throughout - the board is a view of it.
type BountyBoard struct {
	resolver RouteResolver
	servers  GuildServersFunc
	panels   *RoutePanels
	lister   BoardLister
	trigger  chan struct{}
}

const (
	bountyBoardInterval = 30 * time.Second // also picks up expiries and out-of-process changes
	bountyBoardLimit    = 10
	bountyBoardFooter   = "CHAMPION KILLFEED • BOUNTY BOARD"
)

func NewBountyBoard(resolver RouteResolver, servers GuildServersFunc, panels *RoutePanels, lister BoardLister) *BountyBoard {
	return &BountyBoard{resolver: resolver, servers: servers, panels: panels, lister: lister, trigger: make(chan struct{}, 1)}
}

// Trigger asks for an immediate reconcile (non-blocking, coalesced); nil-safe.
func (b *BountyBoard) Trigger() {
	if b == nil {
		return
	}
	select {
	case b.trigger <- struct{}{}:
	default:
	}
}

// Notify makes the board usable as (part of) a bounties.Notifier: any lifecycle
// event triggers a reconcile.
func (b *BountyBoard) Notify(bounties.Event) { b.Trigger() }

// Run reconciles at startup, on every Trigger, and every bountyBoardInterval.
func (b *BountyBoard) Run(ctx context.Context) {
	if b == nil {
		return
	}
	ticker := time.NewTicker(bountyBoardInterval)
	defer ticker.Stop()
	b.SyncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.SyncOnce(ctx)
		case <-b.trigger:
			b.SyncOnce(ctx)
		}
	}
}

// SyncOnce reconciles every board once. Never fatal; panic-safe.
func (b *BountyBoard) SyncOnce(ctx context.Context) {
	if b == nil || b.resolver == nil || b.servers == nil || b.panels == nil || b.lister == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=bounty_board", "msg", "sync panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	guildRowID, serverIDs, err := b.servers(ctx)
	if err != nil {
		slog.Warn("component=bounty_board", "event", "bounty_board_servers_failed", "err", err.Error())
		return
	}
	byChannel := map[string][]int64{}
	var order []string
	lookupErrs := 0
	for _, serverID := range serverIDs {
		ch, found, rerr := b.resolver.Resolve(ctx, guildRowID, serverID, routeKeyBounty)
		if rerr != nil {
			lookupErrs++
			slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyBounty, "guild_id", guildRowID, "server_id", serverID, "reason", "lookup_error", "err", rerr.Error())
			continue
		}
		if !found || ch == "" {
			continue
		}
		if _, seen := byChannel[ch]; !seen {
			order = append(order, ch)
		}
		byChannel[ch] = append(byChannel[ch], serverID)
	}
	if len(order) == 0 && lookupErrs > 0 {
		return // unknown state: leave whatever is live
	}
	contentFor := func(channelID string) (PanelContent, error) {
		entries, listErr := b.lister.ListBoard(ctx, guildRowID, byChannel[channelID], bountyBoardLimit)
		if listErr != nil {
			return PanelContent{}, listErr
		}
		return PanelContent{Embed: buildBountyBoardEmbed(entries)}, nil
	}
	if _, err := b.panels.SyncEach(ctx, guildRowID, routeKeyBounty, order, contentFor, lookupErrs == 0); err != nil {
		slog.Warn("component=bounty_board", "event", "bounty_board_sync_failed", "err", err.Error())
	}
}

// buildBountyBoardEmbed renders the board. Names only - no internal ids.
func buildBountyBoardEmbed(entries []repository.BoardEntry) *discordgo.MessageEmbed {
	var b strings.Builder
	b.WriteString("🎯 **ACTIVE BOUNTIES**\n")
	if len(entries) == 0 {
		b.WriteString("\n_No active bounties._")
	}
	for i, e := range entries {
		fmt.Fprintf(&b, "\n%d. %s — %s pts", i+1, bountyName(e.TargetName), formatAmount(e.Total))
		if e.Count > 1 {
			fmt.Fprintf(&b, " (%d bounties)", e.Count)
		}
	}
	return &discordgo.MessageEmbed{
		Description: b.String(),
		Color:       presentation.EventGold,
		Footer:      &discordgo.MessageEmbedFooter{Text: bountyBoardFooter},
	}
}

// BountyEvents fans one lifecycle event out to the tracker and the board. Either
// may be nil (route system unavailable); the database is unaffected.
type BountyEvents struct {
	Tracker *BountyTracker
	Board   *BountyBoard
}

// Notify implements bounties.Notifier.
func (e BountyEvents) Notify(ev bounties.Event) {
	e.Tracker.Notify(ev)
	e.Board.Trigger()
}
