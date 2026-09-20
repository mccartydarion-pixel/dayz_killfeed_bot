package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// routeKeyEconomy mirrors routing.RouteEconomy (asserted equal in tests).
const routeKeyEconomy = "ECONOMY"

// EconomyFeed publishes committed economy transactions to each affected server's
// ECONOMY route. Events arrive from the economy Service (and from the bounty
// claim) strictly AFTER their database transaction committed, and only ever
// describe that committed state - so a Discord failure can never undo or repeat a
// transaction. Economy correctness never depends on this feed or on any route.
//
// Routing is per (guild row, server): an event attributed to a server goes to that
// server's ECONOMY route (a bounty payout: the server the kill happened on); a
// guild-wide event (an admin adjustment) goes to every server's route, deduplicated
// by channel. No route, or a failed lookup, is a no-op (throttled warning) - there
// is no fallback to KILLFEED or any other channel.
//
// Notify only appends to a bounded queue; route lookups and Discord I/O run on
// Run's goroutine, at most economyFeedEventsPerTick events per tick, in messages of
// up to 10 cards.
//
// Privacy: a card shows the display name, the type, the amount, and (for rewards
// only) the resulting balance. It never shows internal ids, Discord ids, the acting
// admin or an adjustment's reason.
type EconomyFeed struct {
	sender   HitSender
	resolver RouteResolver
	servers  GuildServersFunc

	// Optional custom embed templates (nil = the Champion default, always).
	custom     EmbedCustomizer
	serverName ServerNameFunc

	mu          sync.Mutex
	queue       []economy.Event
	dropped     int64
	failedSends int64
	lastWarn    map[string]time.Time
}

const (
	economyFeedTick           = 2 * time.Second
	economyFeedMaxQueue       = 200
	economyFeedEventsPerTick  = 20
	economyFeedFinalEvents    = 60
	economyFeedEmbedsPerFrame = 10
)

func NewEconomyFeed(sender HitSender, resolver RouteResolver, servers GuildServersFunc) *EconomyFeed {
	return &EconomyFeed{sender: sender, resolver: resolver, servers: servers, lastWarn: make(map[string]time.Time)}
}

// SetCustomizer enables custom embed templates for ECONOMY cards.
func (f *EconomyFeed) SetCustomizer(c EmbedCustomizer, serverName ServerNameFunc) {
	if f == nil {
		return
	}
	f.custom = c
	f.serverName = serverName
}

func (f *EconomyFeed) card(e economy.Event, serverID int64) *discordgo.MessageEmbed {
	def := buildEconomyEmbed(e)
	return customEmbed(f.custom, e.GuildID, serverID, routeKeyEconomy, def, func() map[string]string {
		return economyVars(e, serverNameOf(f.serverName, serverID))
	})
}

// Notify implements economy.Notifier. It never blocks and never fails.
func (f *EconomyFeed) Notify(e economy.Event) {
	if f == nil || e.Type == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) >= economyFeedMaxQueue {
		copy(f.queue, f.queue[1:]) // drop the oldest
		f.queue = f.queue[:len(f.queue)-1]
		f.dropped++
	}
	f.queue = append(f.queue, e)
}

// Run drives the feed until ctx is done, then flushes what is queued.
func (f *EconomyFeed) Run(ctx context.Context) {
	if f == nil {
		return
	}
	ticker := time.NewTicker(economyFeedTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			f.safeTick(true)
			return
		case <-ticker.C:
			f.safeTick(false)
		}
	}
}

// Flush sends what is queued (a bounded amount). Panic-safe, nil-safe.
func (f *EconomyFeed) Flush() {
	if f == nil {
		return
	}
	f.safeTick(true)
}

func (f *EconomyFeed) safeTick(final bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=economy_feed", "msg", "tick panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	f.tick(final)
}

func (f *EconomyFeed) tick(final bool) {
	n := economyFeedEventsPerTick
	if final {
		n = economyFeedFinalEvents
	}
	f.mu.Lock()
	if n > len(f.queue) {
		n = len(f.queue)
	}
	batch := append([]economy.Event(nil), f.queue[:n]...)
	f.queue = f.queue[n:]
	dropped := f.dropped
	f.dropped = 0
	f.mu.Unlock()
	if dropped > 0 {
		slog.Warn("component=economy_feed", "event", "economy_feed_flood_drop", "dropped_events", dropped)
	}
	if len(batch) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	perChannel := make(map[string][]*discordgo.MessageEmbed)
	var order []string
	for _, e := range batch {
		for _, target := range f.targetsFor(ctx, e) {
			ch := target.channel
			if _, seen := perChannel[ch]; !seen {
				order = append(order, ch)
			}
			perChannel[ch] = append(perChannel[ch], f.card(e, target.serverID))
		}
	}
	for _, ch := range order {
		embeds := perChannel[ch]
		for start := 0; start < len(embeds); start += economyFeedEmbedsPerFrame {
			end := start + economyFeedEmbedsPerFrame
			if end > len(embeds) {
				end = len(embeds)
			}
			f.send(ch, embeds[start:end])
		}
	}
}

func (f *EconomyFeed) targetsFor(ctx context.Context, e economy.Event) []channelTarget {
	if f.resolver == nil {
		return nil
	}
	var serverIDs []int64
	if e.ServerID > 0 {
		serverIDs = []int64{e.ServerID}
	} else {
		if f.servers == nil {
			return nil
		}
		guildRowID, ids, err := f.servers(ctx)
		if err != nil || guildRowID != e.GuildID {
			return nil
		}
		serverIDs = ids
	}
	seen := map[string]bool{}
	var out []channelTarget
	for _, id := range serverIDs {
		ch, found, err := f.resolver.Resolve(ctx, e.GuildID, id, routeKeyEconomy)
		if err != nil {
			f.warnLookup(e.GuildID, id, err)
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

func (f *EconomyFeed) warnLookup(guildRowID, serverID int64, err error) {
	key := fmt.Sprintf("%d|%d", guildRowID, serverID)
	f.mu.Lock()
	last := f.lastWarn[key]
	if time.Since(last) < time.Minute {
		f.mu.Unlock()
		return
	}
	f.lastWarn[key] = time.Now()
	f.mu.Unlock()
	slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyEconomy, "guild_id", guildRowID, "server_id", serverID, "reason", "lookup_error", "err", err.Error())
}

// send posts one message. A failure is logged and dropped (no retry): the
// transaction it describes is committed and must never be redone.
func (f *EconomyFeed) send(channel string, embeds []*discordgo.MessageEmbed) {
	if f.sender == nil || len(embeds) == 0 {
		return
	}
	_, err := f.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Embeds:          embeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // names never ping
	})
	if err != nil {
		f.mu.Lock()
		f.failedSends++
		f.mu.Unlock()
		slog.Warn("component=economy_feed", "event", "economy_feed_send_failed", "channel_id", channel, "cards", len(embeds), "err", err.Error())
	}
}

// buildEconomyEmbed renders one card:
//
//	💰 BOUNTY REWARD            ➕ ADMIN CREDIT             ➖ ADMIN DEBIT
//	PlayerA earned 125,000 pts  PlayerA received 50,000 pts PlayerA lost 25,000 pts
//	Balance: 340,000 pts
//
// Amounts are Champion Points. Only rewards show the resulting balance.
func buildEconomyEmbed(e economy.Event) *discordgo.MessageEmbed {
	name := bountyName(e.PlayerName) // the shared display-name sanitiser
	amount := formatAmount(e.Amount) + " pts"
	var b strings.Builder
	color := presentation.EventGold
	switch e.Type {
	case economy.TypeBountyClaim:
		b.WriteString("💰 **BOUNTY REWARD**")
		fmt.Fprintf(&b, "\n%s earned %s\nBalance: %s pts", name, amount, formatAmount(e.BalanceAfter))
		color = presentation.SuccessGreen
	case economy.TypeSystemReward:
		b.WriteString("🎁 **REWARD**")
		fmt.Fprintf(&b, "\n%s earned %s\nBalance: %s pts", name, amount, formatAmount(e.BalanceAfter))
		color = presentation.SuccessGreen
	case economy.TypeAdminCredit:
		b.WriteString("➕ **ADMIN CREDIT**")
		fmt.Fprintf(&b, "\n%s received %s", name, amount)
	case economy.TypeAdminDebit:
		b.WriteString("➖ **ADMIN DEBIT**")
		fmt.Fprintf(&b, "\n%s lost %s", name, amount)
		color = presentation.WarningAmber
	case economy.TypeShopPurchase:
		b.WriteString("🛒 **SHOP PURCHASE**")
		if strings.TrimSpace(e.Item) != "" {
			fmt.Fprintf(&b, "\n%s bought %s for %s", name, bountyName(e.Item), amount)
		} else {
			fmt.Fprintf(&b, "\n%s spent %s", name, amount)
		}
	case economy.TypeShopRefund:
		b.WriteString("↩️ **SHOP REFUND**")
		fmt.Fprintf(&b, "\n%s was refunded %s", name, amount)
		color = presentation.SuccessGreen
	default:
		verb := "received"
		if !e.Credit {
			verb = "lost"
			color = presentation.WarningAmber
		}
		b.WriteString("💠 **ECONOMY**")
		fmt.Fprintf(&b, "\n%s %s %s", name, verb, amount)
	}
	return &discordgo.MessageEmbed{Description: b.String(), Color: color}
}
