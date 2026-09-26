package discord

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// rotatingFeedAPI is the narrow Discord surface RotatingFeed needs.
// *discordgo.Session satisfies it directly; tests use a fake.
type rotatingFeedAPI interface {
	ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// singleMessageDeleter is the optional single-message delete immediate mode
// uses when only one card leaves the window (Discord's bulk delete needs 2+).
// *discordgo.Session implements it.
type singleMessageDeleter interface {
	ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error
}

// Feed delivery modes (KILLFEED_DELIVERY_MODE). FeedModeRotating is the
// production default and is unchanged; FeedModeImmediate must be enabled
// explicitly (docs/incidents/2026-09-26-P0-reliability.md, killfeed latency).
const (
	FeedModeRotating  = "rotating"
	FeedModeImmediate = "immediate"
)

// feedItem is one queued card with the timestamps the latency ledger needs.
type feedItem struct {
	embed      *discordgo.MessageEmbed
	detectedAt time.Time // when Champion read the ADM line (zero if unknown)
	enqueuedAt time.Time // when the durable write acked and the card was queued
}

// postedCard is a card currently shown in the channel (immediate mode).
type postedCard struct {
	id       string
	postedAt time.Time
}

// RotatingFeed batches embeds and flushes them to a channel on a fixed
// interval: delete the previous cycle's messages, then post up to maxItems
// of the most recently enqueued embeds. A quiet cycle (nothing enqueued)
// still deletes the previous batch on schedule and posts nothing - the
// channel can sit empty until the next item arrives, a deliberate
// consequence of a fixed-interval cycle rather than a "post once N
// accumulate" one.
//
// Immediate mode (SetMode(FeedModeImmediate)) keeps the same presentation -
// at most maxItems cards, none older than about one interval, the channel
// empty after a quiet interval - but posts each card as soon as it is queued
// and removes the oldest card when the window is full, so a kill reaches
// Discord in seconds instead of waiting up to a whole interval.
type RotatingFeed struct {
	api         rotatingFeedAPI
	store       SetupStore
	guildID     string
	channelIDFn func(*GuildSetup) string
	interval    time.Duration
	maxItems    int
	route       string // delivery-ledger route name (KILLFEED, DEATH_FEED)
	mode        string

	mu            sync.Mutex
	pending       []feedItem
	lastMsgIDs    []string
	lastChannelID string
	window        []postedCard // immediate mode: cards currently shown, oldest first

	// routeChannelFn, when set, is consulted first each flush (the
	// installation route model - see KillfeedPublisher.RouteChannelID); an
	// empty result falls back to the legacy GuildSetup field below, so
	// guilds without a configured route behave exactly as before.
	routeChannelFn func() string

	wake chan struct{}
	done chan struct{}
}

// SetRouteChannelResolver attaches the installation-route lookup that takes
// priority over the legacy GuildSetup channel. Optional.
func (f *RotatingFeed) SetRouteChannelResolver(fn func() string) {
	if f == nil {
		return
	}
	f.routeChannelFn = fn
}

// SetRoute names this feed in the delivery ledger (default KILLFEED).
func (f *RotatingFeed) SetRoute(route string) {
	if f == nil || route == "" {
		return
	}
	f.route = route
}

// SetMode selects FeedModeRotating (default) or FeedModeImmediate. Must be
// called before Run.
func (f *RotatingFeed) SetMode(mode string) {
	if f == nil {
		return
	}
	if mode == FeedModeImmediate {
		f.mode = FeedModeImmediate
		return
	}
	f.mode = FeedModeRotating
}

// Mode returns the active delivery mode.
func (f *RotatingFeed) Mode() string {
	if f == nil {
		return ""
	}
	return f.mode
}

// NewRotatingFeed creates a feed. channelIDFn extracts the relevant channel
// field from GuildSetup (e.g. KillfeedChannelID or DeathChannelID) so the
// same type serves both channels.
func NewRotatingFeed(api rotatingFeedAPI, store SetupStore, guildID string, channelIDFn func(*GuildSetup) string, interval time.Duration, maxItems int) *RotatingFeed {
	return &RotatingFeed{api: api, store: store, guildID: guildID, channelIDFn: channelIDFn, interval: interval, maxItems: maxItems,
		route: "KILLFEED", mode: FeedModeRotating, wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// Enqueue appends an embed to the current cycle's pending batch. Safe to
// call concurrently with Run's ticker goroutine.
func (f *RotatingFeed) Enqueue(embed *discordgo.MessageEmbed) {
	f.EnqueueDetected(embed, time.Time{})
}

// EnqueueDetected is Enqueue with the time Champion read the source ADM line,
// so the ledger can report detect->deliver latency. It never blocks: in
// immediate mode it only wakes Run's goroutine.
func (f *RotatingFeed) EnqueueDetected(embed *discordgo.MessageEmbed, detectedAt time.Time) {
	if f == nil || embed == nil {
		return
	}
	f.mu.Lock()
	f.pending = append(f.pending, feedItem{embed: embed, detectedAt: detectedAt, enqueuedAt: time.Now()})
	f.mu.Unlock()
	if f.mode == FeedModeImmediate {
		select {
		case f.wake <- struct{}{}:
		default:
		}
	}
}

// Run ticks flush() on the configured interval until ctx is done, then does
// one final flush before returning so a graceful shutdown/redeploy never
// silently drops whatever was enqueued since the last cycle (up to
// interval's worth of kills/deaths would otherwise vanish - never posted,
// even though they were already persisted to the database). Intended to be
// started as its own goroutine for the lifetime of a server worker.
func (f *RotatingFeed) Run(ctx context.Context) {
	if f == nil {
		return
	}
	defer close(f.done)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if f.mode == FeedModeImmediate {
				f.postImmediate()
			} else {
				f.flush()
			}
			return
		case <-f.wake:
			f.postImmediate()
		case <-ticker.C:
			if f.mode == FeedModeImmediate {
				f.expireImmediate(time.Now())
				f.postImmediate() // anything a failed post left pending
			} else {
				f.flush()
			}
		}
	}
}

// WaitDone blocks until Run has returned, including its final on-shutdown
// flush. Callers must only call this after Run's ctx is cancelled (or about
// to be) - see App.shutdown, which mirrors the same pattern already used to
// drain PersistenceQueue before the process exits.
func (f *RotatingFeed) WaitDone() {
	if f == nil {
		return
	}
	<-f.done
}

func (f *RotatingFeed) channelID() string {
	if f.routeChannelFn != nil {
		if id := f.routeChannelFn(); id != "" {
			return id
		}
	}
	if f.store == nil || f.guildID == "" {
		return ""
	}
	setup, err := f.store.Get(f.guildID)
	if err != nil || setup == nil {
		return ""
	}
	return f.channelIDFn(setup)
}

func (f *RotatingFeed) send(channelID string, it feedItem) (*discordgo.Message, error) {
	msg, err := deliverMessage(f.api, f.route, channelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{it.embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	})
	if err == nil {
		Deliveries.recordLatency(f.route, it.detectedAt, it.enqueuedAt, time.Now())
	}
	return msg, err
}

// deliverItems posts items in order and returns the posted message IDs and
// the items to retry on the next attempt (transient failures, and everything
// from a configuration fault onward; a payload Discord rejects is dropped but
// recorded in the ledger).
func (f *RotatingFeed) deliverItems(channelID string, items []feedItem) (ids []string, retry []feedItem) {
	for i, it := range items {
		msg, err := f.send(channelID, it)
		if err != nil {
			class := ClassifyDeliveryError(err)
			slog.Warn("component=discord", "msg", "rotating feed publish failed", "route", f.route, "channel_id", channelID, "class", class, "err", err.Error())
			if class == DeliveryConfigFault {
				// The channel is unusable: stop instead of failing every
				// remaining item against it, and keep them for a repaired route.
				return ids, append(retry, items[i:]...)
			}
			if class != DeliveryRejected {
				retry = append(retry, it)
			}
			continue
		}
		ids = append(ids, msg.ID)
	}
	return ids, retry
}

// flush deletes the previous batch, then posts up to maxItems of the most
// recently enqueued embeds as the new batch.
func (f *RotatingFeed) flush() {
	if f.api == nil {
		return
	}
	f.mu.Lock()
	items := f.pending
	f.pending = nil
	lastIDs := f.lastMsgIDs
	lastChannel := f.lastChannelID
	f.mu.Unlock()

	if len(lastIDs) > 0 && lastChannel != "" {
		if err := f.api.ChannelMessagesBulkDelete(lastChannel, lastIDs); err != nil {
			slog.Warn("component=discord", "msg", "rotating feed cleanup failed", "channel_id", lastChannel, "err", err.Error())
		}
	}

	channelID := f.channelID()
	if channelID == "" {
		f.mu.Lock()
		f.lastMsgIDs = nil
		f.lastChannelID = ""
		f.mu.Unlock()
		return
	}

	if len(items) > f.maxItems {
		items = items[len(items)-f.maxItems:]
	}

	newIDs, retry := f.deliverItems(channelID, items)

	f.mu.Lock()
	f.lastMsgIDs = newIDs
	f.lastChannelID = channelID
	if len(retry) > 0 {
		// Undelivered items go ahead of anything enqueued meanwhile; the
		// next flush still trims to the newest maxItems.
		f.pending = append(retry, f.pending...)
	}
	f.mu.Unlock()
}

// postImmediate (immediate mode) posts every pending card now, then trims the
// shown window to the newest maxItems cards.
func (f *RotatingFeed) postImmediate() {
	if f.api == nil {
		return
	}
	f.mu.Lock()
	items := f.pending
	f.pending = nil
	f.mu.Unlock()
	if len(items) == 0 {
		return
	}
	channelID := f.channelID()
	if channelID == "" {
		return // nowhere to post: nothing is shown, like the rotating mode
	}
	if len(items) > f.maxItems {
		items = items[len(items)-f.maxItems:]
	}
	f.mu.Lock()
	if f.lastChannelID != "" && f.lastChannelID != channelID {
		// The route moved: cards in the old channel are no longer this
		// feed's window. Delete them like the rotating cycle would.
		old, oldChannel := f.window, f.lastChannelID
		f.window = nil
		f.mu.Unlock()
		f.deleteCards(oldChannel, old)
		f.mu.Lock()
	}
	f.mu.Unlock()

	ids, retry := f.deliverItems(channelID, items)

	now := time.Now()
	f.mu.Lock()
	f.lastChannelID = channelID
	for _, id := range ids {
		f.window = append(f.window, postedCard{id: id, postedAt: now})
	}
	var evicted []postedCard
	if over := len(f.window) - f.maxItems; over > 0 {
		evicted = append(evicted, f.window[:over]...)
		f.window = append([]postedCard(nil), f.window[over:]...)
	}
	if len(retry) > 0 {
		f.pending = append(retry, f.pending...)
	}
	f.mu.Unlock()
	f.deleteCards(channelID, evicted)
}

// expireImmediate (immediate mode) removes cards shown for longer than one
// interval, so a quiet period empties the channel exactly as the rotating
// cycle does.
func (f *RotatingFeed) expireImmediate(now time.Time) {
	f.mu.Lock()
	channelID := f.lastChannelID
	var expired []postedCard
	keep := f.window[:0]
	for _, c := range f.window {
		if now.Sub(c.postedAt) >= f.interval {
			expired = append(expired, c)
		} else {
			keep = append(keep, c)
		}
	}
	f.window = keep
	f.mu.Unlock()
	f.deleteCards(channelID, expired)
}

func (f *RotatingFeed) deleteCards(channelID string, cards []postedCard) {
	if channelID == "" || len(cards) == 0 {
		return
	}
	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = c.id
	}
	var err error
	if len(ids) == 1 {
		if d, ok := f.api.(singleMessageDeleter); ok {
			err = d.ChannelMessageDelete(channelID, ids[0])
		} else {
			err = f.api.ChannelMessagesBulkDelete(channelID, ids)
		}
	} else {
		err = f.api.ChannelMessagesBulkDelete(channelID, ids)
	}
	if err != nil {
		slog.Warn("component=discord", "msg", "feed card cleanup failed", "route", f.route, "channel_id", channelID, "cards", len(ids), "err", err.Error())
	}
}
