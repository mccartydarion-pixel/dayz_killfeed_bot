package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// FeedJournal is the durable record of immediate-mode cards (migration 0057,
// repository.FeedCardRepository). With it, a restart - graceful or a crash -
// replays cards that were queued but never confirmed, and takes back the
// previous process's cards still shown in the channel (so the window stays
// the newest maxItems instead of growing by up to maxItems per restart).
type FeedJournal interface {
	Record(ctx context.Context, feedKey string, cards []repository.FeedCard) error
	MarkPosted(ctx context.Context, feedKey, nonce, channelID, messageID string, at time.Time) error
	MarkRemoved(ctx context.Context, feedKey string, messageIDs []string, at time.Time) error
	MarkDropped(ctx context.Context, feedKey string, nonces []string, reason string, at time.Time) error
	Open(ctx context.Context, feedKey string) ([]repository.FeedCard, error)
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}

// Journal tunables. A queued card older than feedReplayWindow at restart is
// not posted (it would be misleading as a "live" kill); it is marked dropped,
// counted in the ledger and logged - never silently discarded.
var (
	feedReplayWindow   = time.Hour
	feedJournalTimeout = 5 * time.Second
	feedJournalRetain  = 24 * time.Hour
)

// Drop reasons stored in discord_feed_cards.drop_reason.
const (
	dropStaleAfterRestart = "STALE_AFTER_RESTART"
	dropUnreadable        = "UNREADABLE"
	dropRejected          = "REJECTED"
	dropBacklogOverflow   = "BACKLOG_OVERFLOW"
	dropHandedToRotating  = "HANDED_TO_ROTATING"
)

// SetJournal attaches the feed journal under feedKey ("<route>:<server id>").
// Must be called before Run. Only immediate mode writes to it; a rotating
// feed only drains what an earlier immediate process left (rollback).
func (f *RotatingFeed) SetJournal(j FeedJournal, feedKey string) {
	if f == nil || j == nil || feedKey == "" {
		return
	}
	f.journal = j
	f.journalKey = feedKey
}

func (f *RotatingFeed) journaling() bool {
	return f.journal != nil && f.mode == FeedModeImmediate
}

func (f *RotatingFeed) journalFailed(op string, err error) {
	Deliveries.recordJournalFailure(f.route)
	slog.Error("component=discord", "msg", "feed journal operation failed; delivery continues from memory", "route", f.route, "feed", f.journalKey, "op", op, "err", err.Error())
}

// journalPending records queued cards not yet journaled. A failure leaves
// them unjournaled (retried next pass) and never blocks delivery.
func (f *RotatingFeed) journalPending() {
	if !f.journaling() {
		return
	}
	f.mu.Lock()
	var cards []repository.FeedCard
	for _, it := range f.pending {
		if it.journaled {
			continue
		}
		raw, err := json.Marshal(it.embed)
		if err != nil {
			continue
		}
		cards = append(cards, repository.FeedCard{Nonce: it.nonce, Embed: raw, DetectedAt: it.detectedAt, EnqueuedAt: it.enqueuedAt})
	}
	f.mu.Unlock()
	if len(cards) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
	defer cancel()
	if err := f.journal.Record(ctx, f.journalKey, cards); err != nil {
		f.journalFailed("record", err)
		return
	}
	done := make(map[string]bool, len(cards))
	for _, c := range cards {
		done[c.Nonce] = true
	}
	f.mu.Lock()
	for i := range f.pending {
		if done[f.pending[i].nonce] {
			f.pending[i].journaled = true
		}
	}
	f.mu.Unlock()
}

func (f *RotatingFeed) journalPosted(it feedItem, channelID, messageID string, at time.Time) {
	if !f.journaling() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
	defer cancel()
	if err := f.journal.MarkPosted(ctx, f.journalKey, it.nonce, channelID, messageID, at); err != nil {
		f.journalFailed("mark_posted", err)
	}
}

func (f *RotatingFeed) journalRemoved(ids []string) {
	if !f.journaling() || len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
	defer cancel()
	if err := f.journal.MarkRemoved(ctx, f.journalKey, ids, time.Now()); err != nil {
		f.journalFailed("mark_removed", err)
	}
}

func (f *RotatingFeed) journalDropped(items []feedItem, reason string) {
	if f.journal == nil || len(items) == 0 {
		return
	}
	nonces := make([]string, len(items))
	for i, it := range items {
		nonces[i] = it.nonce
	}
	f.journalDroppedNonces(nonces, reason)
}

func (f *RotatingFeed) journalDroppedNonces(nonces []string, reason string) {
	if f.journal == nil || len(nonces) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
	defer cancel()
	if err := f.journal.MarkDropped(ctx, f.journalKey, nonces, reason, time.Now()); err != nil {
		f.journalFailed("mark_dropped", err)
	}
}

// purgeJournal deletes closed journal rows past retention.
func (f *RotatingFeed) purgeJournal() {
	if f.journal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
	defer cancel()
	if _, err := f.journal.Purge(ctx, time.Now().Add(-feedJournalRetain)); err != nil {
		f.journalFailed("purge", err)
	}
}

// restore runs once at the start of Run. It reads the cards an earlier
// process left open for this feed:
//
//   - queued, never confirmed: replayed in their original order with their
//     original nonce (so a create that did succeed just before a crash is
//     returned by Discord, not posted twice), unless older than
//     feedReplayWindow - then dropped, counted and logged;
//   - still shown: immediate mode takes them back as its window (the newest
//     maxItems within one interval on the current channel; the rest are
//     deleted), so a restart never leaves extra cards behind. A rotating feed
//     (rollback) deletes them all and hands the queued cards to its cycle.
func (f *RotatingFeed) restore() {
	if f.journal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
	cards, err := f.journal.Open(ctx, f.journalKey)
	cancel()
	if err != nil {
		f.journalFailed("open", err)
		return
	}
	f.purgeJournal()
	if len(cards) == 0 {
		return
	}
	now := time.Now()
	var queued []feedItem
	var stale, unreadable []string
	var shown []repository.FeedCard
	for _, c := range cards {
		if c.MessageID != "" {
			shown = append(shown, c)
			continue
		}
		if now.Sub(c.EnqueuedAt) > feedReplayWindow {
			stale = append(stale, c.Nonce)
			continue
		}
		var e discordgo.MessageEmbed
		if err := json.Unmarshal(c.Embed, &e); err != nil {
			unreadable = append(unreadable, c.Nonce)
			continue
		}
		queued = append(queued, feedItem{embed: &e, detectedAt: c.DetectedAt, enqueuedAt: c.EnqueuedAt, nonce: c.Nonce, journaled: true})
	}
	if n := len(stale) + len(unreadable); n > 0 {
		f.journalDroppedNonces(stale, dropStaleAfterRestart)
		f.journalDroppedNonces(unreadable, dropUnreadable)
		Deliveries.recordDropped(f.route, n)
		slog.Error("component=discord", "msg", "feed cards queued before restart were not replayed", "route", f.route, "feed", f.journalKey, "too_old", len(stale), "unreadable", len(unreadable), "replay_window", feedReplayWindow.String())
	}

	// Cards still shown, grouped for deletion by channel.
	remove := map[string][]postedCard{}
	var window []postedCard
	lastChannel := ""
	if len(shown) > 0 {
		lastChannel = shown[len(shown)-1].ChannelID
	}
	for _, c := range shown {
		card := postedCard{id: c.MessageID, postedAt: c.PostedAt}
		if f.mode != FeedModeImmediate || c.ChannelID != lastChannel || now.Sub(c.PostedAt) >= f.interval {
			remove[c.ChannelID] = append(remove[c.ChannelID], card)
			continue
		}
		window = append(window, card)
	}
	if over := len(window) - f.maxItems; over > 0 {
		remove[lastChannel] = append(remove[lastChannel], window[:over]...)
		window = window[over:]
	}

	f.mu.Lock()
	if f.mode == FeedModeImmediate {
		f.window = window
		if len(window) > 0 {
			f.lastChannelID = lastChannel
		}
	}
	f.pending = append(queued, f.pending...)
	f.mu.Unlock()

	for ch, cs := range remove {
		f.deleteCards(ch, cs)
		if f.mode != FeedModeImmediate {
			// A rotating feed does not journal; close these rows now (a
			// failed delete is still retried in memory as an orphan).
			ids := make([]string, len(cs))
			for i, c := range cs {
				ids[i] = c.id
			}
			ctx, cancel := context.WithTimeout(context.Background(), feedJournalTimeout)
			if err := f.journal.MarkRemoved(ctx, f.journalKey, ids, time.Now()); err != nil {
				f.journalFailed("mark_removed", err)
			}
			cancel()
		}
	}
	if f.mode != FeedModeImmediate {
		f.journalDropped(queued, dropHandedToRotating)
	}
	if len(queued) > 0 {
		Deliveries.recordReplayed(f.route, len(queued))
	}
	slog.Info("component=discord", "msg", "feed journal restored", "route", f.route, "feed", f.journalKey, "mode", f.mode,
		"replayed", len(queued), "window", len(window), "removed_shown", len(shown)-len(window))
}
