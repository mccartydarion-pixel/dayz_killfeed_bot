package killfeed

import (
	"testing"
	"time"
)

func newSinkTracker(now *time.Time) (*BatchTracker, *[]BatchReport) {
	t := NewBatchTracker()
	var reports []BatchReport
	t.sink = func(r BatchReport) { reports = append(reports, r) }
	t.now = func() time.Time { return *now }
	return t, &reports
}

// One line per download batch, written when Discord accepted the batch's last card, with the
// read-to-last-card-queued and read-to-last-card-posted times.
func TestBatchTrackerLogsOncePerBatchWhenItsCardsArePosted(t *testing.T) {
	read := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := read
	tr, reports := newSinkTracker(&now)
	timing := FeedTiming{ServerID: 3, ReadAt: read}

	tr.CardQueued(timing, read.Add(200*time.Millisecond))
	tr.CardQueued(timing, read.Add(900*time.Millisecond))
	tr.CardPosted(timing, read.Add(1500*time.Millisecond))
	tr.BatchFinished(3, read, 5) // 5 events persisted, 2 of them cards
	if len(*reports) != 0 {
		t.Fatalf("a batch with a card still queued must not be logged yet: %+v", *reports)
	}
	tr.CardPosted(timing, read.Add(2400*time.Millisecond))
	if len(*reports) != 1 {
		t.Fatalf("expected one report, got %+v", *reports)
	}
	got := (*reports)[0]
	want := BatchReport{ServerID: 3, Events: 5, Cards: 2, Posted: 2, ReadToLastCardQueuedMs: 900, CardsPostedMs: 2400, Complete: true}
	if got != want {
		t.Fatalf("report %+v, want %+v", got, want)
	}
	if tr.OpenBatches(3) != 0 {
		t.Fatal("a logged batch must be forgotten")
	}

	// A batch without cards is logged as soon as its events are persisted.
	tr.BatchFinished(3, read.Add(time.Minute), 2)
	if len(*reports) != 2 || (*reports)[1].Cards != 0 || (*reports)[1].CardsPostedMs != 0 || !(*reports)[1].Complete {
		t.Fatalf("card-less batch: %+v", *reports)
	}
	// Cards restored from the journal (no read time) and a zero batch are ignored.
	tr.CardPosted(FeedTiming{ServerID: 3}, now)
	tr.BatchFinished(3, read, 0)
	if tr.OpenBatches(3) != 0 || len(*reports) != 2 {
		t.Fatalf("unexpected tracking: open=%d reports=%+v", tr.OpenBatches(3), *reports)
	}
}

// A batch whose cards never all post is still logged, as incomplete, once it is old - never lost.
func TestBatchTrackerLogsStaleBatchesAsIncomplete(t *testing.T) {
	read := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := read
	tr, reports := newSinkTracker(&now)
	timing := FeedTiming{ServerID: 7, ReadAt: read}
	tr.CardQueued(timing, read.Add(time.Second))
	tr.BatchFinished(7, read, 1)
	now = read.Add(batchLogTimeout + time.Second)
	tr.BatchFinished(7, now, 1) // the next batch settles the old one
	if len(*reports) != 2 || (*reports)[0].Complete || (*reports)[0].Posted != 0 || (*reports)[0].ReadToLastCardQueuedMs != 1000 {
		t.Fatalf("stale batch must be logged incomplete, then the new one: %+v", *reports)
	}
	// Cards posted for a batch the engine never finished (an interrupted read) are dropped
	// quietly once they age out, so the map cannot grow.
	tr.CardPosted(FeedTiming{ServerID: 7, ReadAt: now.Add(time.Second)}, now)
	now = now.Add(batchLogTimeout + time.Minute)
	tr.BatchFinished(7, now, 1)
	if n := tr.OpenBatches(7); n != 0 {
		t.Fatalf("open batches %d, want 0", n)
	}
}

// Per-server bound: with more than maxOpenBatches waiting, the oldest are logged incomplete.
func TestBatchTrackerBoundsOpenBatches(t *testing.T) {
	read := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := read
	tr, reports := newSinkTracker(&now)
	for i := 0; i < maxOpenBatches+3; i++ {
		at := read.Add(time.Duration(i) * time.Second)
		tr.CardQueued(FeedTiming{ServerID: 1, ReadAt: at}, at)
		tr.BatchFinished(1, at, 1)
	}
	if n := tr.OpenBatches(1); n != maxOpenBatches {
		t.Fatalf("open %d, want %d", n, maxOpenBatches)
	}
	if len(*reports) != 3 {
		t.Fatalf("%d batches logged, want the 3 oldest", len(*reports))
	}
}
