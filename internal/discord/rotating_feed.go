package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// rotatingFeedAPI is the narrow Discord surface RotatingFeed needs.
// *discordgo.Session satisfies it directly (FeedSession adds the idempotent
// nonce send); tests use a fake.
type rotatingFeedAPI interface {
	ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// singleMessageDeleter is the optional single-message delete used when only
// one card leaves the window (Discord's bulk delete needs 2+) and to recover
// individual cards after a failed bulk delete. *discordgo.Session implements it.
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

// Immediate-mode failure pacing. A card that still fails after deliver's
// bounded in-call retries is re-attempted on a doubling timer (first
// feedRetryBase, capped at feedRetryMax) instead of waiting for the next event
// or interval. A configuration fault (unknown channel, missing access) pauses
// posting to that channel for feedFaultPause - no request per event - until
// the pause ends or the route moves to another channel.
var (
	feedRetryBase  = 2 * time.Second
	feedRetryMax   = 60 * time.Second
	feedFaultPause = 5 * time.Minute
)

// maxCleanupAttempts bounds retries of one failed card deletion.
const maxCleanupAttempts = 5

// feedItem is one queued card with the timestamps the latency ledger needs.
type feedItem struct {
	embed      *discordgo.MessageEmbed
	detectedAt time.Time // when Champion read the ADM line (zero if unknown)
	enqueuedAt time.Time // when the durable write acked and the card was queued
	// nonce identifies this card to Discord for every attempt, so a retry of
	// a create that actually succeeded returns that message, not a duplicate.
	nonce string
}

// postedCard is a card currently shown in the channel (immediate mode).
type postedCard struct {
	id       string
	postedAt time.Time
}

// orphanCard is a card whose deletion failed; it is retried, never forgotten.
type orphanCard struct {
	channelID, id string
	attempts      int
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
//
// Delivery guarantees (both modes): cards are posted in queue order, one at a
// time, from this feed's own goroutine (never the ADM pipeline). A transient
// failure stops the batch and keeps the failed card and everything after it,
// in order, for the next attempt. Each card carries a Discord nonce, so a
// retried create never produces a second card. A card is counted delivered
// only after Discord confirms it. Undelivered cards live in memory: a crash
// (not a graceful shutdown, which flushes) loses them - the events themselves
// are already durable in the database.
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
	orphans       []orphanCard // cards whose deletion failed, retried each attempt
	retryDelay    time.Duration
	retryTimer    *time.Timer
	faultChannel  string
	faultUntil    time.Time

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
	f.pending = append(f.pending, feedItem{embed: embed, detectedAt: detectedAt, enqueuedAt: time.Now(), nonce: newCardNonce()})
	f.mu.Unlock()
	if f.mode == FeedModeImmediate {
		f.poke()
	}
}

func (f *RotatingFeed) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// newCardNonce returns a 20-character random nonce (Discord allows up to 25).
func newCardNonce() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
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
	defer f.stopRetryTimer()
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
	data := &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{it.embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	var msg *discordgo.Message
	var err error
	if ns, ok := f.api.(nonceSender); ok && it.nonce != "" {
		// Every attempt (deliver's in-call retries and later re-queues)
		// reuses the card's nonce: Discord returns the existing message for a
		// create that already succeeded.
		err = deliver(f.route, channelID, func() error {
			m, e := ns.ChannelMessageSendNonce(channelID, data, it.nonce)
			msg = m
			return e
		})
	} else {
		msg, err = deliverMessage(f.api, f.route, channelID, data)
	}
	if err == nil {
		Deliveries.recordLatency(f.route, it.detectedAt, it.enqueuedAt, time.Now())
	}
	return msg, err
}

// deliverItems posts items strictly in order and returns the posted message
// IDs, the items still to deliver (the failed one and everything after it, in
// order) and the failure class that stopped it ("" when all were handled). A
// payload Discord rejects outright (400) is dropped - retrying cannot fix it -
// and recorded in the ledger.
func (f *RotatingFeed) deliverItems(channelID string, items []feedItem) (ids []string, retry []feedItem, stopClass string) {
	for i, it := range items {
		msg, err := f.send(channelID, it)
		if err != nil {
			class := ClassifyDeliveryError(err)
			slog.Warn("component=discord", "msg", "feed publish failed", "route", f.route, "channel_id", channelID, "class", class, "err", err.Error())
			if class == DeliveryRejected {
				continue
			}
			return ids, items[i:], class
		}
		ids = append(ids, msg.ID)
	}
	return ids, nil, ""
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

	f.retryOrphans()
	if len(lastIDs) > 0 && lastChannel != "" {
		cards := make([]postedCard, len(lastIDs))
		for i, id := range lastIDs {
			cards[i] = postedCard{id: id}
		}
		f.deleteCards(lastChannel, cards)
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

	newIDs, retry, _ := f.deliverItems(channelID, items)

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

// postImmediate (immediate mode) posts every pending card, strictly in order,
// and keeps the channel at most maxItems managed cards by removing the oldest
// card right after each new one is confirmed. Unlike the rotating cycle it
// never skips a card because others are waiting: every event is delivered
// (bounded only by maxPendingCards, whose overflow is counted, never silent).
func (f *RotatingFeed) postImmediate() {
	if f.api == nil {
		return
	}
	f.retryOrphans()
	f.mu.Lock()
	if len(f.pending) == 0 {
		f.mu.Unlock()
		return
	}
	channelID := f.channelID()
	if channelID == "" {
		f.boundPendingLocked()
		f.mu.Unlock()
		return // nowhere to post yet: cards wait
	}
	if channelID == f.faultChannel && time.Now().Before(f.faultUntil) {
		// Configuration fault on this channel: no request per event. Cards
		// wait for the channel to be repaired or re-routed.
		f.boundPendingLocked()
		f.mu.Unlock()
		return
	}
	var moved []postedCard
	oldChannel := f.lastChannelID
	if oldChannel != "" && oldChannel != channelID {
		// The route moved: cards in the old channel are no longer this
		// feed's window. Delete them like the rotating cycle would.
		moved, f.window = f.window, nil
	}
	f.lastChannelID = channelID
	f.mu.Unlock()
	f.deleteCards(oldChannel, moved)

	stopClass := ""
	for {
		f.mu.Lock()
		if len(f.pending) == 0 {
			f.mu.Unlock()
			break
		}
		it := f.pending[0]
		f.mu.Unlock()

		msg, err := f.send(channelID, it)
		if err != nil {
			class := ClassifyDeliveryError(err)
			slog.Warn("component=discord", "msg", "feed publish failed", "route", f.route, "channel_id", channelID, "class", class, "err", err.Error())
			if class == DeliveryRejected {
				f.mu.Lock()
				f.pending = f.pending[1:] // cannot ever succeed; recorded in the ledger
				f.mu.Unlock()
				continue
			}
			stopClass = class // keep this card first, in order
			break
		}
		f.mu.Lock()
		f.pending = f.pending[1:]
		f.window = append(f.window, postedCard{id: msg.ID, postedAt: time.Now()})
		var evicted []postedCard
		if over := len(f.window) - f.maxItems; over > 0 {
			evicted = append(evicted, f.window[:over]...)
			f.window = append([]postedCard(nil), f.window[over:]...)
		}
		f.mu.Unlock()
		f.deleteCards(channelID, evicted)
	}

	f.mu.Lock()
	switch {
	case stopClass == DeliveryConfigFault:
		f.faultChannel, f.faultUntil = channelID, time.Now().Add(feedFaultPause)
		f.scheduleRetryLocked(feedFaultPause)
	case stopClass != "":
		// Transient: retry on a doubling timer instead of waiting for the
		// next event or interval.
		if f.retryDelay <= 0 {
			f.retryDelay = feedRetryBase
		} else if f.retryDelay *= 2; f.retryDelay > feedRetryMax {
			f.retryDelay = feedRetryMax
		}
		f.scheduleRetryLocked(f.retryDelay)
	default:
		f.retryDelay = 0
		f.faultChannel, f.faultUntil = "", time.Time{}
	}
	f.boundPendingLocked()
	f.mu.Unlock()
}

// maxPendingCards bounds an immediate-mode backlog (a long Discord outage or
// broken route). Overflow drops the OLDEST cards and is counted in the ledger.
const maxPendingCards = 500

// boundPendingLocked enforces maxPendingCards. Caller holds f.mu.
func (f *RotatingFeed) boundPendingLocked() {
	if over := len(f.pending) - maxPendingCards; over > 0 {
		f.pending = append([]feedItem(nil), f.pending[over:]...)
		Deliveries.recordDropped(f.route, over)
		slog.Error("component=discord", "msg", "feed backlog overflow; oldest undelivered cards dropped", "route", f.route, "dropped", over)
	}
}

// scheduleRetryLocked arms a single wake-up. Caller holds f.mu.
func (f *RotatingFeed) scheduleRetryLocked(d time.Duration) {
	if f.retryTimer != nil {
		f.retryTimer.Stop()
	}
	f.retryTimer = time.AfterFunc(d, f.poke)
}

// scheduleOrphanRetryLocked (immediate mode) retries failed card deletions
// on the feed's retry timer, so a quiet channel does not keep an extra card
// until the next event or interval. Rotating mode retries on its next cycle.
// Caller holds f.mu.
func (f *RotatingFeed) scheduleOrphanRetryLocked() {
	if f.mode != FeedModeImmediate || len(f.orphans) == 0 {
		return
	}
	delay := feedRetryBase << min(f.orphans[0].attempts, 5)
	if delay > feedRetryMax {
		delay = feedRetryMax
	}
	f.scheduleRetryLocked(delay)
}

func (f *RotatingFeed) stopRetryTimer() {
	f.mu.Lock()
	if f.retryTimer != nil {
		f.retryTimer.Stop()
	}
	f.mu.Unlock()
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

// deleteCards removes cards from a channel. Anything that could not be
// deleted is kept as an orphan and retried - never forgotten, which would
// leave the channel above maxItems cards forever.
func (f *RotatingFeed) deleteCards(channelID string, cards []postedCard) {
	if channelID == "" || len(cards) == 0 {
		return
	}
	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = c.id
	}
	failed := f.deleteIDs(channelID, ids)
	if len(failed) == 0 {
		return
	}
	f.mu.Lock()
	for _, id := range failed {
		f.orphans = append(f.orphans, orphanCard{channelID: channelID, id: id, attempts: 1})
	}
	orphaned := len(f.orphans)
	f.scheduleOrphanRetryLocked()
	f.mu.Unlock()
	Deliveries.recordCleanup(f.route, len(failed), orphaned)
}

// deleteIDs deletes ids from channelID and returns the IDs still present.
// A bulk delete that fails falls back to one-by-one deletes; a message (or
// channel) that no longer exists counts as deleted (isUnknownMessage).
func (f *RotatingFeed) deleteIDs(channelID string, ids []string) []string {
	single, canSingle := f.api.(singleMessageDeleter)
	if len(ids) >= 2 || !canSingle {
		err := f.api.ChannelMessagesBulkDelete(channelID, ids)
		if err == nil || isUnknownMessage(err) {
			return nil
		}
		slog.Warn("component=discord", "msg", "feed card cleanup failed", "route", f.route, "channel_id", channelID, "cards", len(ids), "err", err.Error())
		if !canSingle {
			return ids
		}
	}
	var failed []string
	for _, id := range ids {
		if err := single.ChannelMessageDelete(channelID, id); err != nil && !isUnknownMessage(err) {
			slog.Warn("component=discord", "msg", "feed card delete failed", "route", f.route, "channel_id", channelID, "message_id", id, "err", err.Error())
			failed = append(failed, id)
		}
	}
	return failed
}

// retryOrphans re-attempts failed deletions, up to maxCleanupAttempts each.
func (f *RotatingFeed) retryOrphans() {
	f.mu.Lock()
	orphans := f.orphans
	f.orphans = nil
	f.mu.Unlock()
	if len(orphans) == 0 {
		return
	}
	var still []orphanCard
	abandoned := 0
	for _, o := range orphans {
		if len(f.deleteIDs(o.channelID, []string{o.id})) == 0 {
			continue
		}
		o.attempts++
		if o.attempts >= maxCleanupAttempts {
			abandoned++
			slog.Error("component=discord", "msg", "feed card could not be removed; manual cleanup needed", "route", f.route, "channel_id", o.channelID, "message_id", o.id)
			continue
		}
		still = append(still, o)
	}
	f.mu.Lock()
	f.orphans = append(still, f.orphans...)
	orphaned := len(f.orphans)
	f.scheduleOrphanRetryLocked()
	f.mu.Unlock()
	Deliveries.recordCleanupState(f.route, orphaned, abandoned)
}
