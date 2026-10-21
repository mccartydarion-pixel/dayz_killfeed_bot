package killfeed

import (
	"log/slog"
	"sync"
	"time"
)

// BatchTracker follows one download batch (one ADM read) from the moment its bytes arrived to
// the moment Discord accepted its last card, and writes one log line per batch:
//
//	component=killfeed event=batch_processed server_id=… events=… cards=…
//	read_to_last_card_queued_ms=… cards_posted_ms=… complete=…
//
// events is how many kills, deaths, connects and disconnects of the batch were persisted;
// cards is how many kill and death cards the feeds queued for it; read_to_last_card_queued_ms
// is the time from the download to the last of those cards entering its feed's queue (the
// database work of the whole batch, section 23 of docs/PERFORMANCE.md); cards_posted_ms is the
// time from the download to Discord accepting the last card (the queue wait and Discord's rate
// limit on top). A batch without cards is logged as soon as its events are persisted, with
// cards_posted_ms=0. A batch whose cards never all post (a dropped card, a route without a
// channel) is logged with complete=false when it is older than batchLogTimeout or when the
// server has more than maxOpenBatches batches waiting, so no batch is lost from the log.
//
// Measurement only: it reads the timestamps the pipeline already carries (FeedTiming.ReadAt
// identifies the batch) and never delays anything. Nothing about players or messages is kept.
type BatchTracker struct {
	mu   sync.Mutex
	open map[int64][]*batchProgress // per server, oldest first
	now  func() time.Time
	// sink receives every batch instead of the log when set (tests).
	sink func(BatchReport)
}

// BatchReport is one settled batch, as logged.
type BatchReport struct {
	ServerID               int64
	Events, Cards, Posted  int
	ReadToLastCardQueuedMs int64
	CardsPostedMs          int64
	Complete               bool
}

type batchProgress struct {
	readAt       time.Time
	events       int
	finished     bool
	cardsQueued  int
	cardsPosted  int
	lastQueuedAt time.Time
	lastPostedAt time.Time
}

var (
	batchLogTimeout = 15 * time.Minute
	maxOpenBatches  = 32
)

// Batches is the process-wide tracker the engine, the persistence queue and the feeds report to.
var Batches = NewBatchTracker()

// NewBatchTracker returns an empty tracker.
func NewBatchTracker() *BatchTracker {
	return &BatchTracker{open: map[int64][]*batchProgress{}, now: time.Now}
}

// find returns the batch for (serverID, readAt), creating it when create is set.
func (t *BatchTracker) find(serverID int64, readAt time.Time, create bool) *batchProgress {
	for _, b := range t.open[serverID] {
		if b.readAt.Equal(readAt) {
			return b
		}
	}
	if !create {
		return nil
	}
	b := &batchProgress{readAt: readAt}
	t.open[serverID] = append(t.open[serverID], b)
	return b
}

// CardQueued records that a feed queued a card of the batch timing belongs to.
func (t *BatchTracker) CardQueued(timing FeedTiming, at time.Time) {
	if t == nil || timing.ServerID == 0 || timing.ReadAt.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.find(timing.ServerID, timing.ReadAt, true)
	b.cardsQueued++
	if at.After(b.lastQueuedAt) {
		b.lastQueuedAt = at
	}
}

// CardPosted records that Discord accepted a card of the batch timing belongs to.
func (t *BatchTracker) CardPosted(timing FeedTiming, at time.Time) {
	if t == nil || timing.ServerID == 0 || timing.ReadAt.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.find(timing.ServerID, timing.ReadAt, true)
	b.cardsPosted++
	if at.After(b.lastPostedAt) {
		b.lastPostedAt = at
	}
	t.settleLocked(timing.ServerID)
}

// BatchFinished records that the engine has handed every event of the batch read at readAt to
// persistence and all of them are durable (and their cards queued). events is their number.
func (t *BatchTracker) BatchFinished(serverID int64, readAt time.Time, events int) {
	if t == nil || serverID == 0 || readAt.IsZero() || events <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.find(serverID, readAt, true)
	b.events = events
	b.finished = true
	t.settleLocked(serverID)
}

// settleLocked logs and forgets every batch of the server that is complete, too old, or beyond
// the per-server bound. Caller holds t.mu.
func (t *BatchTracker) settleLocked(serverID int64) {
	now := t.now()
	open := t.open[serverID]
	keep := open[:0]
	for i, b := range open {
		complete := b.finished && b.cardsPosted >= b.cardsQueued
		expired := now.Sub(b.readAt) > batchLogTimeout
		crowded := len(open)-i > maxOpenBatches
		if !complete && !expired && !crowded {
			keep = append(keep, b)
			continue
		}
		if !b.finished {
			// Only cards reported so far and no batch end: the engine never handed this batch
			// over (its read was interrupted); nothing to measure.
			continue
		}
		t.log(serverID, b, complete)
	}
	if len(keep) == 0 {
		delete(t.open, serverID)
		return
	}
	t.open[serverID] = keep
}

func (t *BatchTracker) log(serverID int64, b *batchProgress, complete bool) {
	queuedMs, postedMs := int64(0), int64(0)
	if b.cardsQueued > 0 {
		queuedMs = b.lastQueuedAt.Sub(b.readAt).Milliseconds()
	}
	if b.cardsPosted > 0 {
		postedMs = b.lastPostedAt.Sub(b.readAt).Milliseconds()
	}
	if t.sink != nil {
		t.sink(BatchReport{ServerID: serverID, Events: b.events, Cards: b.cardsQueued, Posted: b.cardsPosted, ReadToLastCardQueuedMs: queuedMs, CardsPostedMs: postedMs, Complete: complete})
		return
	}
	slog.Info("component=killfeed", "event", "batch_processed", "server_id", serverID, "events", b.events, "cards", b.cardsQueued,
		"cards_posted", b.cardsPosted, "read_to_last_card_queued_ms", queuedMs, "cards_posted_ms", postedMs, "complete", complete)
}

// OpenBatches reports how many batches of the server are still waiting for their cards. Tests only.
func (t *BatchTracker) OpenBatches(serverID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.open[serverID])
}
