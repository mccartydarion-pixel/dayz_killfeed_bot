package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
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

// Immediate-mode grouping. When more than one card is waiting, they go out
// together in one Discord message, newest last, within Discord's limits of 10
// embeds per message and 6000 characters across all of a message's embeds
// (feedMaxMessageChars keeps a margin under that). A single waiting card is
// posted alone at once; only when a burst is already in progress (two or more
// cards waiting, fewer than a full message) does the feed wait feedCoalesceWindow
// for the cards right behind them, so a download of hundreds of kills becomes
// tens of messages instead of hundreds of rate-limited posts.
const (
	feedMaxEmbedsPerMessage = 10
	feedMaxMessageChars     = 5500
)

var feedCoalesceWindow = 150 * time.Millisecond

// feedItem is one queued card with the timestamps the latency ledger needs.
type feedItem struct {
	embed      *discordgo.MessageEmbed
	detectedAt time.Time // when Champion read the ADM line (zero if unknown)
	enqueuedAt time.Time // when the durable write acked and the card was queued
	// nonce identifies this card to Discord for every attempt, so a retry of
	// a create that actually succeeded returns that message, not a duplicate.
	nonce string
	// journaled is set once the card is in the feed journal (immediate mode).
	journaled bool
	// solo keeps the card out of grouped messages: set when a grouped message
	// was rejected, so each of its cards is tried on its own and only a card
	// Discord rejects by itself is dropped.
	solo bool
	// timing is the card's stage timestamps for the latency statistics
	// (killfeed.FeedLatency). Measurement only.
	timing killfeed.FeedTiming
}

// postedCard is a message currently shown in the channel (immediate mode):
// one card, or a group of cards posted together. cards is how many it holds
// (0 is read as 1, for cards restored from older journal rows).
type postedCard struct {
	id       string
	postedAt time.Time
	cards    int
}

func (c postedCard) count() int {
	if c.cards <= 0 {
		return 1
	}
	return c.cards
}

// windowCards is the number of cards the window's messages hold.
func windowCards(window []postedCard) int {
	n := 0
	for _, c := range window {
		n += c.count()
	}
	return n
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
// and removes the oldest cards when the window is full, so a kill reaches
// Discord in seconds instead of waiting up to a whole interval. Cards that are
// waiting together are posted together, in one message (up to
// feedMaxEmbedsPerMessage of them, feedMaxMessageChars in all), still in
// order; a single waiting card is posted alone at once. The window counts
// cards, not messages, and removes whole messages: with maxItems cards shown
// and a group of n arriving, the oldest messages holding at least n cards are
// deleted first.
//
// Delivery guarantees (both modes): cards are posted in queue order, from
// this feed's own goroutine (never the ADM pipeline). A transient failure
// stops the batch and keeps the failed card and everything after it, in
// order, for the next attempt. Each message carries a Discord nonce (the first
// card's), so a retried create never produces a second message - and a
// retried group returns the message with the cards it already holds, which
// the feed then marks posted without re-sending. A card is counted delivered
// only after Discord confirms it. In immediate mode with a journal
// (SetJournal) every card is also recorded in the database before it is
// posted, so a crash does not lose it: the next start replays it (see
// restore). Without a journal - and in rotating mode, which never journals -
// undelivered cards live in memory only and a crash (not a graceful shutdown,
// which flushes) loses them; the events themselves are durable either way.
type RotatingFeed struct {
	api         rotatingFeedAPI
	store       SetupStore
	guildID     string
	channelIDFn func(*GuildSetup) string
	interval    time.Duration
	maxItems    int
	route       string // delivery-ledger route name (KILLFEED, DEATH_FEED)
	mode        string
	// maxCardsPerMessage caps an immediate-mode grouped message (default
	// feedMaxEmbedsPerMessage; 1 posts every card on its own).
	maxCardsPerMessage int

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

	journal    FeedJournal // optional; see SetJournal
	journalKey string
	// latencyServerID names the server in the latency statistics for cards that
	// do not carry one (cards restored from the journal after a restart).
	latencyServerID int64

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

// SetLatencyServerID names the game server this feed belongs to in the latency
// statistics. Optional; measurement only.
func (f *RotatingFeed) SetLatencyServerID(serverID int64) {
	if f != nil {
		f.latencyServerID = serverID
	}
}

// SetRoute names this feed in the delivery ledger (default KILLFEED).
func (f *RotatingFeed) SetRoute(route string) {
	if f == nil || route == "" {
		return
	}
	f.route = route
}

// SetMaxCardsPerMessage caps how many waiting cards an immediate-mode message
// may carry (1..feedMaxEmbedsPerMessage; 1 disables grouping). Must be called
// before Run.
func (f *RotatingFeed) SetMaxCardsPerMessage(n int) {
	if f == nil {
		return
	}
	if n < 1 {
		n = 1
	}
	if n > feedMaxEmbedsPerMessage {
		n = feedMaxEmbedsPerMessage
	}
	f.maxCardsPerMessage = n
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
		route: "KILLFEED", mode: FeedModeRotating, maxCardsPerMessage: feedMaxEmbedsPerMessage, wake: make(chan struct{}, 1), done: make(chan struct{})}
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
	f.EnqueueTimed(embed, killfeed.FeedTiming{ParsedAt: detectedAt})
}

// EnqueueTimed is EnqueueDetected with every stage timestamp the engine knows
// for the card's event (killfeed.TimingOf), so the latency statistics can
// split the trip into its stages. Like Enqueue it never blocks.
func (f *RotatingFeed) EnqueueTimed(embed *discordgo.MessageEmbed, timing killfeed.FeedTiming) {
	if f == nil || embed == nil {
		return
	}
	now := time.Now()
	f.mu.Lock()
	f.pending = append(f.pending, feedItem{embed: embed, detectedAt: timing.ParsedAt, enqueuedAt: now, nonce: newCardNonce(), timing: timing})
	f.mu.Unlock()
	killfeed.Batches.CardQueued(timing, now)
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
	f.restore()
	if f.mode == FeedModeImmediate {
		f.poke() // replayed cards go out now, not on the next event
	}
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
				f.purgeJournal()
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
	return f.sendGroup(channelID, []feedItem{it})
}

// sendGroup posts items together as one message, under the first item's
// nonce, and records every item as delivered when Discord accepts it. The
// caller checks the returned message's embed count: a message that already
// existed (nonce) holds the cards of the attempt that created it.
func (f *RotatingFeed) sendGroup(channelID string, items []feedItem) (*discordgo.Message, error) {
	embeds := make([]*discordgo.MessageEmbed, len(items))
	for i, it := range items {
		embeds[i] = it.embed
	}
	data := &discordgo.MessageSend{
		Embeds: embeds,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	nonce := items[0].nonce
	var msg *discordgo.Message
	var err error
	callStart := time.Now()
	if ns, ok := f.api.(nonceSender); ok && nonce != "" {
		// Every attempt (deliver's in-call retries and later re-queues)
		// reuses the card's nonce: Discord returns the existing message for a
		// create that already succeeded.
		err = deliver(f.route, channelID, func() error {
			m, e := ns.ChannelMessageSendNonce(channelID, data, nonce)
			msg = m
			return e
		})
	} else {
		msg, err = deliverMessage(f.api, f.route, channelID, data)
	}
	if err != nil {
		return msg, err
	}
	now := time.Now()
	oldest := items[0].enqueuedAt
	for i, it := range items {
		if i > 0 {
			// deliver counted the request; the ledger counts cards delivered.
			Deliveries.recordSuccess(f.route, channelID)
		}
		Deliveries.recordLatency(f.route, it.detectedAt, it.enqueuedAt, now)
		timing := it.timing
		if timing.ParsedAt.IsZero() {
			timing.ParsedAt = it.detectedAt
		}
		if timing.ServerID == 0 {
			timing.ServerID = f.latencyServerID
		}
		killfeed.FeedLatency.Record(f.route, timing, it.enqueuedAt, now)
		killfeed.Batches.CardPosted(timing, now)
		if it.enqueuedAt.Before(oldest) {
			oldest = it.enqueuedAt
		}
	}
	if f.mode == FeedModeImmediate {
		// One line per message so a staging run can compute p50/p95/p99 per
		// stage from the logs (the ledger keeps only avg/max/last).
		detectMs := int64(-1)
		if !items[0].detectedAt.IsZero() {
			detectMs = now.Sub(items[0].detectedAt).Milliseconds()
		}
		slog.Info("component=discord", "msg", "feed card delivered", "route", f.route, "cards", len(items),
			"queue_ms", callStart.Sub(oldest).Milliseconds(), "discord_ms", now.Sub(callStart).Milliseconds(),
			"detect_to_publish_ms", detectMs)
	}
	return msg, nil
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
// grouping the cards that are waiting together into one message (nextGroup),
// and keeps the channel at most maxItems managed cards by deleting the oldest
// messages before posting the next one. If deletion fails, the pending cards
// stay in order (and in the journal) until cleanup succeeds. Unlike the
// rotating cycle it never skips a card because others are waiting: every
// event is delivered (bounded only by maxPendingCards, whose overflow is
// counted, never silent).
func (f *RotatingFeed) postImmediate() {
	if f.api == nil {
		return
	}
	f.retryOrphans()
	f.journalPending()
	f.mu.Lock()
	if len(f.pending) == 0 {
		f.mu.Unlock()
		return
	}
	channelID := f.channelID()
	if channelID == "" {
		dropped := f.boundPendingLocked()
		f.mu.Unlock()
		f.journalDropped(dropped, dropBacklogOverflow)
		return // nowhere to post yet: cards wait
	}
	if channelID == f.faultChannel && time.Now().Before(f.faultUntil) {
		// Configuration fault on this channel: no request per event. Cards
		// wait for the channel to be repaired or re-routed.
		dropped := f.boundPendingLocked()
		f.mu.Unlock()
		f.journalDropped(dropped, dropBacklogOverflow)
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
	coalesced := false
	for {
		// A burst in progress (more than one card waiting, fewer than a full
		// message): give the cards right behind them a moment to arrive, so
		// they share the message - once per pass. A single card never waits.
		f.mu.Lock()
		waiting := len(f.pending)
		f.mu.Unlock()
		if !coalesced && waiting > 1 && waiting < f.maxCardsPerMessage && feedCoalesceWindow > 0 {
			coalesced = true
			time.Sleep(feedCoalesceWindow)
		}
		// A card enqueued while this loop runs must be journaled before it
		// is posted, or a restart would replay it (no-op when all are).
		f.journalPending()
		f.mu.Lock()
		if len(f.pending) == 0 {
			f.mu.Unlock()
			break
		}
		group := f.nextGroupLocked()
		// An earlier expiration or route cleanup may have failed. Such a
		// card is still visible even if no longer in window; do not create
		// another card on this channel until the orphan is actually gone.
		blocked := false
		for _, o := range f.orphans {
			if o.channelID == channelID {
				blocked = true
				break
			}
		}
		// Room for the group: the oldest whole messages whose cards make up
		// the overflow.
		var evict []postedCard
		if f.maxItems > 0 {
			over := windowCards(f.window) + len(group) - f.maxItems
			for _, c := range f.window {
				if over <= 0 {
					break
				}
				evict = append(evict, c)
				over -= c.count()
			}
		}
		f.mu.Unlock()
		if blocked {
			stopClass = "CLEANUP_PENDING"
			break
		}
		if len(evict) > 0 {
			// Delete first, *then* publish. A failed deletion must neither
			// remove the message from our window nor mark its journal rows
			// removed. The pending cards stay queued, preserving both order
			// and crash replay. This is a hard per-channel capacity gate.
			ids := make([]string, len(evict))
			for i, c := range evict {
				ids[i] = c.id
			}
			failed := f.deleteIDs(channelID, ids)
			gone := without(ids, failed)
			f.journalRemoved(gone)
			f.mu.Lock()
			f.window = withoutCards(f.window, gone)
			f.mu.Unlock()
			if len(failed) != 0 {
				Deliveries.recordCleanup(f.route, len(failed), len(failed))
				stopClass = "CLEANUP_FAILED"
				break
			}
		}

		msg, err := f.sendGroup(channelID, group)
		if err != nil {
			class := ClassifyDeliveryError(err)
			slog.Warn("component=discord", "msg", "feed publish failed", "route", f.route, "channel_id", channelID, "class", class, "cards", len(group), "err", err.Error())
			if class == DeliveryRejected {
				if len(group) > 1 {
					// The message as a whole was refused: try its cards one
					// by one, so only a card refused on its own is lost.
					f.mu.Lock()
					for i := range f.pending[:len(group)] {
						f.pending[i].solo = true
					}
					f.mu.Unlock()
					continue
				}
				f.mu.Lock()
				f.pending = f.pending[1:] // cannot ever succeed; recorded in the ledger
				f.mu.Unlock()
				f.journalDropped(group, dropRejected)
				continue
			}
			stopClass = class // keep these cards first, in order
			break
		}
		// A message that already existed (the nonce of a create that did
		// succeed before a timeout or a restart) holds the cards of that
		// earlier attempt: the first len(msg.Embeds) waiting cards, since a
		// group always starts at the head of the queue.
		posted := len(group)
		if n := len(msg.Embeds); n > 0 && n != posted {
			posted = n
		}
		postedAt := time.Now()
		f.mu.Lock()
		if posted > len(f.pending) {
			posted = len(f.pending)
		}
		done := append([]feedItem(nil), f.pending[:posted]...)
		f.pending = f.pending[posted:]
		f.window = append(f.window, postedCard{id: msg.ID, postedAt: postedAt, cards: posted})
		f.mu.Unlock()
		for _, it := range done {
			f.journalPosted(it, channelID, msg.ID, postedAt)
		}
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
	dropped := f.boundPendingLocked()
	f.mu.Unlock()
	f.journalDropped(dropped, dropBacklogOverflow)
}

// nextGroupLocked returns the cards at the head of the queue that go out in
// the next message: as many as fit under maxCardsPerMessage, maxItems (a
// message never holds more than the window) and feedMaxMessageChars, in
// order; always at least the first card. A card marked solo goes alone.
// Caller holds f.mu.
func (f *RotatingFeed) nextGroupLocked() []feedItem {
	limit := f.maxCardsPerMessage
	if limit < 1 {
		limit = 1
	}
	if f.maxItems > 0 && f.maxItems < limit {
		limit = f.maxItems
	}
	n, chars := 0, 0
	for n < len(f.pending) && n < limit {
		it := f.pending[n]
		if it.solo && n > 0 {
			break
		}
		c := embedChars(it.embed)
		if n > 0 && chars+c > feedMaxMessageChars {
			break
		}
		chars += c
		n++
		if it.solo {
			break
		}
	}
	return append([]feedItem(nil), f.pending[:n]...)
}

// embedChars counts the characters Discord counts towards a message's 6000:
// title, description, every field's name and value, the footer and the
// author name.
func embedChars(e *discordgo.MessageEmbed) int {
	if e == nil {
		return 0
	}
	n := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, fld := range e.Fields {
		if fld != nil {
			n += utf8.RuneCountInString(fld.Name) + utf8.RuneCountInString(fld.Value)
		}
	}
	if e.Footer != nil {
		n += utf8.RuneCountInString(e.Footer.Text)
	}
	if e.Author != nil {
		n += utf8.RuneCountInString(e.Author.Name)
	}
	return n
}

// withoutCards returns window minus the messages with the given ids, in order.
func withoutCards(window []postedCard, ids []string) []postedCard {
	if len(ids) == 0 {
		return window
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	keep := window[:0]
	for _, c := range window {
		if !drop[c.id] {
			keep = append(keep, c)
		}
	}
	return keep
}

// maxPendingCards bounds an immediate-mode backlog (a long Discord outage or
// broken route). Overflow drops the OLDEST cards and is counted in the ledger.
const maxPendingCards = 500

// boundPendingLocked enforces maxPendingCards and returns the cards it
// dropped (the caller journals them after releasing f.mu). Caller holds f.mu.
func (f *RotatingFeed) boundPendingLocked() []feedItem {
	over := len(f.pending) - maxPendingCards
	if over <= 0 {
		return nil
	}
	dropped := append([]feedItem(nil), f.pending[:over]...)
	f.pending = append([]feedItem(nil), f.pending[over:]...)
	Deliveries.recordDropped(f.route, over)
	slog.Error("component=discord", "msg", "feed backlog overflow; oldest undelivered cards dropped", "route", f.route, "dropped", over)
	return dropped
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
	f.journalRemoved(without(ids, failed))
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
	var closed []string // deleted, or abandoned: no longer this feed's card
	abandoned := 0
	for _, o := range orphans {
		if len(f.deleteIDs(o.channelID, []string{o.id})) == 0 {
			closed = append(closed, o.id)
			continue
		}
		o.attempts++
		if o.attempts >= maxCleanupAttempts {
			if f.mode == FeedModeImmediate {
				// A visible orphan must remain a capacity blocker. Giving
				// up would allow the next send to exceed maxItems.
				o.attempts = maxCleanupAttempts
				if o.attempts == maxCleanupAttempts {
					slog.Error("component=discord", "msg", "feed cleanup still failing; immediate publication paused on this channel", "route", f.route, "channel_id", o.channelID, "message_id", o.id)
				}
				still = append(still, o)
				continue
			}
			abandoned++
			closed = append(closed, o.id)
			slog.Error("component=discord", "msg", "feed card could not be removed; manual cleanup needed", "route", f.route, "channel_id", o.channelID, "message_id", o.id)
			continue
		}
		still = append(still, o)
	}
	f.journalRemoved(closed)
	f.mu.Lock()
	f.orphans = append(still, f.orphans...)
	orphaned := len(f.orphans)
	f.scheduleOrphanRetryLocked()
	f.mu.Unlock()
	Deliveries.recordCleanupState(f.route, orphaned, abandoned)
}

// without returns ids minus drop, in order.
func without(ids, drop []string) []string {
	if len(drop) == 0 {
		return ids
	}
	skip := make(map[string]bool, len(drop))
	for _, id := range drop {
		skip[id] = true
	}
	var out []string
	for _, id := range ids {
		if !skip[id] {
			out = append(out, id)
		}
	}
	return out
}
