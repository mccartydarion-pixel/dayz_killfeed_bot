package discord

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Restart recovery through the feed journal (release gates 1, 5 and 7), with
// the real discordgo client against the emulated Discord API. The journal is
// an in-memory stand-in with the same semantics as FeedCardRepository (that
// repository is covered against Postgres by feed_card_repository
// integration tests).

type memJournalRow struct {
	card             repository.FeedCard
	removed, dropped bool
	reason           string
}

type memJournal struct {
	mu         sync.Mutex
	rows       map[string][]*memJournalRow
	failPosted bool // simulates a crash between Discord's confirmation and MarkPosted
	failAll    bool
}

func newMemJournal() *memJournal { return &memJournal{rows: map[string][]*memJournalRow{}} }

var errJournalDown = errors.New("journal unavailable")

func (j *memJournal) Record(_ context.Context, key string, cards []repository.FeedCard) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failAll {
		return errJournalDown
	}
next:
	for _, c := range cards {
		for _, r := range j.rows[key] {
			if r.card.Nonce == c.Nonce {
				continue next
			}
		}
		j.rows[key] = append(j.rows[key], &memJournalRow{card: c})
	}
	return nil
}

func (j *memJournal) MarkPosted(_ context.Context, key, nonce, ch, id string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failAll || j.failPosted {
		return errJournalDown
	}
	for _, r := range j.rows[key] {
		if r.card.Nonce == nonce {
			r.card.ChannelID, r.card.MessageID, r.card.PostedAt = ch, id, at
		}
	}
	return nil
}

func (j *memJournal) MarkRemoved(_ context.Context, key string, ids []string, _ time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failAll {
		return errJournalDown
	}
	for _, r := range j.rows[key] {
		for _, id := range ids {
			if r.card.MessageID == id {
				r.removed = true
			}
		}
	}
	return nil
}

func (j *memJournal) MarkDropped(_ context.Context, key string, nonces []string, reason string, _ time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failAll {
		return errJournalDown
	}
	for _, r := range j.rows[key] {
		for _, n := range nonces {
			if r.card.Nonce == n && !r.dropped && !r.removed {
				r.dropped, r.reason = true, reason
			}
		}
	}
	return nil
}

func (j *memJournal) Open(_ context.Context, key string) ([]repository.FeedCard, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failAll {
		return nil, errJournalDown
	}
	var out []repository.FeedCard
	for _, r := range j.rows[key] {
		if !r.removed && !r.dropped {
			out = append(out, r.card)
		}
	}
	return out, nil
}

func (j *memJournal) Purge(context.Context, time.Time) (int64, error) { return 0, nil }

func (j *memJournal) open(key string) int {
	cards, _ := j.Open(context.Background(), key)
	return len(cards)
}

// startJournalFeed starts a feed on channel ch with the journal attached.
// resolve, when set, overrides the channel (return "" = no route).
func startJournalFeed(t *testing.T, mode string, interval time.Duration, ch string, j FeedJournal, resolve func() string) (*RotatingFeed, context.CancelFunc) {
	t.Helper()
	s := newTestSession(t, 5*time.Second)
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: ch})
	f := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, interval, 10)
	if resolve != nil {
		f.SetRouteChannelResolver(resolve)
	}
	f.SetMode(mode)
	f.SetJournal(j, "KILLFEED:1")
	ctx, cancel := context.WithCancel(context.Background())
	go f.Run(ctx)
	stopped := false
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); f.WaitDone(); stopped = true }) }
	t.Cleanup(func() {
		if !stopped {
			stop()
		}
	})
	return f, stop
}

// Gate 5: a crash with a queued card - the next process replays it.
func TestJournalCrashReplaysQueuedCards(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()

	// Process A never gets to post: its route is a channel Discord does not
	// know (404, so the feed pauses). That stands in for a crash before
	// delivery - Run's final pass cannot post either.
	noRoute := func() string { return "nowhere" }
	a, stopA := startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, noRoute)
	for n := 1; n <= 3; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "cards journaled before delivery", func() bool { return j.open("KILLFEED:1") == 3 })
	stopA()
	if got := rig.emu.titles(rig.kf); len(got) != 0 {
		t.Fatalf("setup: nothing may reach the channel before the restart, got %v", got)
	}

	// Process B: replays all three, in order, exactly once.
	startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	eventually(t, "queued cards replayed after restart", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(1, 3)) })
	if c := rig.emu.count(rig.emu.creates, rig.kf); c != 3 {
		t.Fatalf("replay created %d cards, want 3", c)
	}
	eventually(t, "ledger counts the replay", func() bool { return ledger("KILLFEED").Replayed == 3 })
}

// Gate 5: a crash after Discord created the card but before the journal
// recorded it - the replay reuses the nonce, so no second card appears.
func TestJournalCrashAfterCreateDoesNotDuplicate(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()
	j.failPosted = true
	a, stopA := startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	a.EnqueueDetected(card(1), time.Now())
	eventually(t, "created in Discord", func() bool { return len(rig.emu.titles(rig.kf)) == 1 })
	stopA()
	j.mu.Lock()
	j.failPosted = false
	j.mu.Unlock()
	if j.open("KILLFEED:1") != 1 {
		t.Fatal("setup: the card must still look unconfirmed in the journal")
	}
	startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	eventually(t, "replay confirmed", func() bool {
		cards, _ := j.Open(context.Background(), "KILLFEED:1")
		return len(cards) == 1 && cards[0].MessageID != ""
	})
	if c := rig.emu.count(rig.emu.creates, rig.kf); c != 1 {
		t.Fatalf("replay after a crash-after-create produced %d cards, want 1", c)
	}
	if got := rig.emu.titles(rig.kf); !reflect.DeepEqual(got, titlesRange(1, 1)) {
		t.Fatalf("channel %v", got)
	}
}

// Gate 1 across a restart: 15 distinct kills split over two processes still
// leave exactly the newest ten visible.
func TestJournalRestartKeepsNewestTenVisible(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()
	a, stopA := startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	for n := 1; n <= 8; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "8 posted", func() bool { return len(rig.emu.titles(rig.kf)) == 8 })
	stopA()

	b, _ := startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	for n := 9; n <= 15; n++ {
		b.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "newest ten visible after the restart", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(6, 15)) })
	if c := rig.emu.count(rig.emu.creates, rig.kf); c != 15 {
		t.Fatalf("expected exactly 15 cards created, got %d", c)
	}
	eventually(t, "journal holds exactly the shown window", func() bool { return j.open("KILLFEED:1") == 10 })
}

// A card queued longer than the replay window is not posted as if live; it is
// dropped, counted and logged - not silently lost.
func TestJournalStaleQueuedCardIsDroppedAndCounted(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()
	_ = j.Record(context.Background(), "KILLFEED:1", []repository.FeedCard{
		{Nonce: "old", Embed: []byte(`{"title":"kill-01"}`), EnqueuedAt: time.Now().Add(-2 * feedReplayWindow)},
		{Nonce: "new", Embed: []byte(`{"title":"kill-02"}`), EnqueuedAt: time.Now().Add(-time.Minute)},
	})
	startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	eventually(t, "fresh card replayed", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(2, 2)) })
	eventually(t, "stale card counted as dropped", func() bool { return ledger("KILLFEED").Dropped == 1 })
	j.mu.Lock()
	defer j.mu.Unlock()
	if r := j.rows["KILLFEED:1"][0]; !r.dropped || r.reason != dropStaleAfterRestart {
		t.Fatalf("stale row %+v", r)
	}
}

// Gate 7 / rollback: a rotating process started after an immediate one
// removes the immediate cards and hands its queue to the cycle; the journal
// ends empty, so a later roll-forward starts clean.
func TestJournalRollbackToRotatingCleansImmediateCards(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()
	a, stopA := startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	for n := 1; n <= 5; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "5 immediate cards", func() bool { return len(rig.emu.titles(rig.kf)) == 5 })
	stopA()

	rot, _ := startJournalFeed(t, FeedModeRotating, 300*time.Millisecond, rig.kf, j, nil)
	eventually(t, "previous process's cards removed at startup", func() bool { return len(rig.emu.titles(rig.kf)) == 0 })
	if n := j.open("KILLFEED:1"); n != 0 {
		t.Fatalf("journal still has %d open rows after rollback", n)
	}
	rot.EnqueueDetected(card(6), time.Now())
	time.Sleep(100 * time.Millisecond)
	if got := len(rig.emu.titles(rig.kf)); got != 0 {
		t.Fatalf("rotating mode must not post before its cycle, channel has %d", got)
	}
	eventually(t, "rotating cycle posts", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(6, 6)) })
	if n := j.open("KILLFEED:1"); n != 0 {
		t.Fatalf("rotating mode must not journal, got %d open rows", n)
	}
}

// A journal outage never blocks delivery; it is counted.
func TestJournalFailureDoesNotBlockDelivery(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()
	j.failAll = true
	a, _ := startJournalFeed(t, FeedModeImmediate, time.Hour, rig.kf, j, nil)
	for n := 1; n <= 3; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "delivered despite the journal outage", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(1, 3)) })
	eventually(t, "journal failures counted", func() bool { return ledger("KILLFEED").JournalFailures > 0 })
}

// Gate 4: with a KILLFEED route, the death feed posts into the killfeed's
// channel (Channel System V2). Each feed keeps its own window: kill
// evictions never remove death cards and vice versa, and each feed's journal
// is separate.
func TestDeathFeedIndependentInSharedChannel(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	rig.cancel()
	rig.feed.WaitDone()
	j := newMemJournal()
	s := newTestSession(t, 5*time.Second)
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: rig.kf, DeathChannelID: rig.kf2})
	route := func() string { return rig.kf } // the shared KILLFEED route
	start := func(route string, key string) *RotatingFeed {
		f := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, time.Hour, 10)
		f.SetRoute(route)
		f.SetMode(FeedModeImmediate)
		f.SetJournal(j, key)
		ctx, cancel := context.WithCancel(context.Background())
		go f.Run(ctx)
		t.Cleanup(func() { cancel(); f.WaitDone() })
		return f
	}
	kills := start("KILLFEED", "KILLFEED:1")
	deaths := start("DEATH_FEED", "DEATH_FEED:1")
	kills.SetRouteChannelResolver(route)
	deaths.SetRouteChannelResolver(route)

	for n := 1; n <= 3; n++ {
		deaths.EnqueueDetected(card(100+n), time.Now())
	}
	eventually(t, "3 death cards", func() bool { return len(rig.emu.titles(rig.kf)) == 3 })
	for n := 1; n <= 15; n++ {
		kills.EnqueueDetected(card(n), time.Now())
	}
	want := append(titlesRange(101, 103), titlesRange(6, 15)...)
	eventually(t, "newest 10 kills plus the 3 deaths", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), want) })
	eventually(t, "journals independent (10 shown kills, 3 shown deaths)", func() bool {
		return j.open("KILLFEED:1") == 10 && j.open("DEATH_FEED:1") == 3
	})
	eventually(t, "ledger per route", func() bool {
		return ledger("KILLFEED").Delivered == 15 && ledger("DEATH_FEED").Delivered == 3
	})
}
