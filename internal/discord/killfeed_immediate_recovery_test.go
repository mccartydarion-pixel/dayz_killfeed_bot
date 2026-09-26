package discord

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Rolling 10-card display and failure recovery for immediate delivery
// (P0 staging verification, sections 5 and 6), through the real discordgo
// client against the emulated Discord messages API. Assertions read the
// emulated CHANNEL STATE, not the feed's in-memory window.

type recoveryRig struct {
	kf, kf2 string
	emu     *discordEmu
	feed    *RotatingFeed
	store   SetupStore
	route   *string
	cancel  context.CancelFunc
}

func newRecoveryRig(t *testing.T, interval, clientTimeout time.Duration) *recoveryRig {
	t.Helper()
	freshLedger(t)
	oldBase, oldPause := feedRetryBase, feedFaultPause
	feedRetryBase, feedFaultPause = 20*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { feedRetryBase, feedFaultPause = oldBase, oldPause })
	emu := newDiscordEmu()
	s := newEmuSession(t, emu, clientTimeout)
	kf, kf2 := testChannel(t, emu, "kf"), testChannel(t, emu, "kf-v2")
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: kf})
	route := ""
	feed := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, interval, 10)
	feed.SetRouteChannelResolver(func() string { return route })
	feed.SetMode(FeedModeImmediate)
	ctx, cancel := context.WithCancel(context.Background())
	go feed.Run(ctx)
	// Stop the feed (and its retry timers) before the ledger is restored, so
	// nothing from this rig writes into the next test's ledger.
	t.Cleanup(func() { cancel(); feed.WaitDone() })
	return &recoveryRig{kf: kf, kf2: kf2, emu: emu, feed: feed, store: store, route: &route, cancel: cancel}
}

func card(n int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("kill-%02d", n),
		Description: fmt.Sprintf("**Killer%d** eliminated **Victim%d**", n, n),
		Color:       0xC0392B,
		Fields:      []*discordgo.MessageEmbedField{{Name: "Weapon", Value: "M4-A1", Inline: true}, {Name: "Distance", Value: fmt.Sprintf("%dm", 20+n), Inline: true}},
		Footer:      &discordgo.MessageEmbedFooter{Text: "Champion Killfeed"},
	}
}

func titlesRange(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("kill-%02d", i))
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ledger(route string) RouteDelivery {
	for _, r := range Deliveries.Snapshot() {
		if r.Route == route {
			return r
		}
	}
	return RouteDelivery{}
}

// TestImmediateRollingWindowChannelState: 15 distinct events.
func TestImmediateRollingWindowChannelState(t *testing.T) {
	rig := newRecoveryRig(t, 400*time.Millisecond, 5*time.Second)
	for n := 1; n <= 15; n++ {
		start := time.Now()
		rig.feed.EnqueueDetected(card(n), start)
		want := fmt.Sprintf("kill-%02d", n)
		eventually(t, "card "+want+" published", func() bool {
			got := rig.emu.titles(rig.kf)
			return len(got) > 0 && got[len(got)-1] == want
		})
		if d := time.Since(start); d > time.Second {
			t.Fatalf("card %d took %s to publish", n, d)
		}
		eventually(t, "at most 10 managed cards", func() bool { return len(rig.emu.titles(rig.kf)) <= 10 })
	}
	// Oldest removed first; the newest 10 remain in order.
	if got := rig.emu.titles(rig.kf); !reflect.DeepEqual(got, titlesRange(6, 15)) {
		t.Fatalf("channel state %v, want %v", got, titlesRange(6, 15))
	}
	// Presentation unchanged: the stored embed equals what the publisher built.
	rig.emu.mu.Lock()
	stored := rig.emu.channels[rig.kf][len(rig.emu.channels[rig.kf])-1].Embed
	rig.emu.mu.Unlock()
	want := card(15)
	want.Type = "rich"
	if stored.Title != want.Title || stored.Description != want.Description || stored.Color != want.Color ||
		len(stored.Fields) != 2 || stored.Fields[1].Value != want.Fields[1].Value || stored.Footer == nil || stored.Footer.Text != want.Footer.Text {
		t.Fatalf("embed changed in transit: %+v", stored)
	}
	if got := rig.emu.count(rig.emu.creates, rig.kf); got != 15 {
		t.Fatalf("expected exactly 15 cards created, got %d", got)
	}
	// Quiet period: after one interval every card expires (next tick).
	eventually(t, "quiet-period expiration empties the channel", func() bool { return len(rig.emu.titles(rig.kf)) == 0 })
}

// TestImmediateCleanupFailureIsRecordedAndRecovered: failed deletes are
// counted, retried and eventually converge to the 10-card window.
func TestImmediateCleanupFailureIsRecordedAndRecovered(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	for n := 1; n <= 10; n++ {
		rig.feed.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "10 cards", func() bool { return len(rig.emu.titles(rig.kf)) == 10 })
	rig.emu.mu.Lock()
	rig.emu.delFault[rig.kf] = []discordFault{{status: 500, code: 0}, {status: 500, code: 0}}
	rig.emu.mu.Unlock()
	rig.feed.EnqueueDetected(card(11), time.Now()) // evicts kill-01: delete fails
	eventually(t, "card 11 posted", func() bool {
		got := rig.emu.titles(rig.kf)
		return len(got) > 0 && got[len(got)-1] == "kill-11"
	})
	eventually(t, "failure recorded", func() bool { return ledger("KILLFEED").CleanupFailures >= 1 })
	if got := rig.emu.titles(rig.kf); len(got) != 11 {
		t.Fatalf("setup: expected the failed eviction to leave 11 cards, got %d", len(got))
	}
	// Recovery: the next activity retries the orphan.
	rig.feed.EnqueueDetected(card(12), time.Now())
	eventually(t, "orphan cleaned up, window back to 10", func() bool {
		got := rig.emu.titles(rig.kf)
		return reflect.DeepEqual(got, titlesRange(3, 12))
	})
	// The orphan gauge is updated when the retry pass returns, just after the
	// delete lands in the channel.
	eventually(t, "orphan backlog clear in the ledger", func() bool {
		r := ledger("KILLFEED")
		return r.OrphanedCards == 0 && r.AbandonedCleanup == 0 && r.CleanupFailures >= 1
	})
}

// TestImmediateFailureRecovery covers 429, 403, 404, 5xx, network timeout,
// route change and restart during queued delivery.
func TestImmediateFailureRecovery(t *testing.T) {
	t.Run("429 is honored and the card is delivered once", func(t *testing.T) {
		rig := newRecoveryRig(t, time.Hour, 5*time.Second)
		rig.emu.mu.Lock()
		rig.emu.postFault[rig.kf] = []discordFault{{status: 429, retryAfter: 0.2}}
		rig.emu.mu.Unlock()
		rig.feed.EnqueueDetected(card(1), time.Now())
		eventually(t, "delivered after the rate limit", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(1, 1)) })
		if c := rig.emu.count(rig.emu.creates, rig.kf); c != 1 {
			t.Fatalf("expected 1 card, got %d", c)
		}
	})

	t.Run("5xx gets bounded retries then delivers in order", func(t *testing.T) {
		rig := newRecoveryRig(t, time.Hour, 5*time.Second)
		// More failures than one deliver() budget (3 attempts): the feed's
		// retry timer must pick it up without waiting for a new event.
		rig.emu.mu.Lock()
		rig.emu.postFault[rig.kf] = []discordFault{{status: 502}, {status: 502}, {status: 502}, {status: 503}}
		rig.emu.mu.Unlock()
		rig.feed.EnqueueDetected(card(1), time.Now())
		rig.feed.EnqueueDetected(card(2), time.Now())
		eventually(t, "both delivered in order", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(1, 2)) })
		if posts := rig.emu.count(rig.emu.posts, rig.kf); posts != 6 {
			t.Fatalf("expected 4 failed + 2 successful POSTs, got %d", posts)
		}
		// The ledger records after Discord's response reaches the client,
		// a moment after the emulator stores the message.
		eventually(t, "ledger counts exactly the 2 confirmed deliveries", func() bool { return ledger("KILLFEED").Delivered == 2 })
		time.Sleep(50 * time.Millisecond)
		if r := ledger("KILLFEED"); r.Delivered != 2 {
			t.Fatalf("ledger delivered %d, want 2", r.Delivered)
		}
	})

	t.Run("network timeout never duplicates a card (nonce)", func(t *testing.T) {
		rig := newRecoveryRig(t, time.Hour, 150*time.Millisecond)
		rig.emu.mu.Lock()
		rig.emu.postFault[rig.kf] = []discordFault{{hang: true}}
		rig.emu.hangFor = 300 * time.Millisecond // created, but the reply misses the client timeout
		rig.emu.mu.Unlock()
		rig.feed.EnqueueDetected(card(1), time.Now())
		eventually(t, "a POST retried after the timeout", func() bool { return rig.emu.count(rig.emu.posts, rig.kf) >= 2 })
		rig.emu.mu.Lock()
		rig.emu.hangFor = 0
		rig.emu.mu.Unlock()
		eventually(t, "delivered", func() bool { return ledger("KILLFEED").Delivered == 1 })
		if c := rig.emu.count(rig.emu.creates, rig.kf); c != 1 {
			t.Fatalf("timeout retry created %d cards, want exactly 1", c)
		}
		if got := rig.emu.titles(rig.kf); !reflect.DeepEqual(got, titlesRange(1, 1)) {
			t.Fatalf("channel %v", got)
		}
	})

	for _, fault := range []struct {
		name         string
		status, code int
	}{{"403 missing permissions", 403, 50013}, {"404 unknown channel", 404, 10003}} {
		t.Run(fault.name+" pauses without a request loop, then recovers", func(t *testing.T) {
			rig := newRecoveryRig(t, time.Hour, 5*time.Second)
			rig.emu.mu.Lock()
			rig.emu.postFault[rig.kf] = []discordFault{{status: fault.status, code: fault.code}}
			rig.emu.mu.Unlock()
			rig.feed.EnqueueDetected(card(1), time.Now())
			eventually(t, "fault hit", func() bool { return ledger("KILLFEED").LastErrorClass == DeliveryConfigFault })
			posts := rig.emu.count(rig.emu.posts, rig.kf)
			for n := 2; n <= 20; n++ { // 19 more events during the fault
				rig.feed.EnqueueDetected(card(n), time.Now())
			}
			time.Sleep(100 * time.Millisecond)
			if got := rig.emu.count(rig.emu.posts, rig.kf); got != posts {
				t.Fatalf("configuration fault produced %d extra requests", got-posts)
			}
			if r := ledger("KILLFEED"); r.Delivered != 0 || r.Failed != 1 {
				t.Fatalf("undelivered cards must not count as delivered: %+v", r)
			}
			// Channel repaired: after the pause, every held card is delivered
			// in order, the window keeps the newest 10.
			eventually(t, "recovered after the pause", func() bool { return reflect.DeepEqual(rig.emu.titles(rig.kf), titlesRange(11, 20)) })
			if c := rig.emu.count(rig.emu.creates, rig.kf); c != 20 {
				t.Fatalf("expected all 20 cards delivered exactly once, got %d", c)
			}
		})
	}

	t.Run("route change moves the window and delivers immediately", func(t *testing.T) {
		rig := newRecoveryRig(t, time.Hour, 5*time.Second)
		for n := 1; n <= 3; n++ {
			rig.feed.EnqueueDetected(card(n), time.Now())
		}
		eventually(t, "3 in legacy channel", func() bool { return len(rig.emu.titles(rig.kf)) == 3 })
		*rig.route = rig.kf2
		rig.feed.EnqueueDetected(card(4), time.Now())
		eventually(t, "old channel cleared, new card in the routed channel", func() bool {
			return len(rig.emu.titles(rig.kf)) == 0 && reflect.DeepEqual(rig.emu.titles(rig.kf2), titlesRange(4, 4))
		})
	})

	t.Run("restart during queued delivery: graceful shutdown delivers, crash loses only the card", func(t *testing.T) {
		rig := newRecoveryRig(t, time.Hour, 5*time.Second)
		rig.emu.mu.Lock()
		rig.emu.postFault[rig.kf] = []discordFault{{status: 503}, {status: 503}, {status: 503}}
		rig.emu.mu.Unlock()
		rig.feed.EnqueueDetected(card(1), time.Now())
		eventually(t, "card queued after a transient failure", func() bool { return rig.emu.count(rig.emu.posts, rig.kf) >= 3 })
		// Graceful shutdown (deploy): Run's final pass delivers what is queued.
		rig.cancel()
		rig.feed.WaitDone()
		if got := rig.emu.titles(rig.kf); !reflect.DeepEqual(got, titlesRange(1, 1)) {
			t.Fatalf("graceful shutdown must deliver queued cards, channel %v", got)
		}
		// A crash (no final pass) cannot deliver in-memory cards: document it.
		crashed := newRecoveryRig(t, time.Hour, 5*time.Second)
		crashed.emu.mu.Lock()
		crashed.emu.postFault[crashed.kf] = []discordFault{{status: 503}, {status: 503}, {status: 503}}
		crashed.emu.mu.Unlock()
		crashed.feed.EnqueueDetected(card(1), time.Now())
		eventually(t, "queued", func() bool { return crashed.emu.count(crashed.emu.posts, crashed.kf) >= 3 })
		crashed.feed.mu.Lock()
		lost := len(crashed.feed.pending)
		crashed.feed.mu.Unlock()
		if lost != 1 || ledger("KILLFEED").Delivered != 0 {
			t.Fatalf("expected exactly one undelivered in-memory card and no false success, pending=%d", lost)
		}
	})
}

// TestRollbackImmediateToRotating is section 7: a service running immediate
// mode is restarted with KILLFEED_DELIVERY_MODE unset. The new process runs
// the rotating cycle on the same channel: it posts only at its cycle, never
// duplicates, and never creates a channel (feeds only post and delete).
// Cards the previous process left in the channel stay: neither mode removes
// another process's cards (pre-existing for rotating mode too - its last
// batch survives every restart).
func TestRollbackImmediateToRotating(t *testing.T) {
	rig := newRecoveryRig(t, time.Hour, 5*time.Second)
	for n := 1; n <= 5; n++ {
		rig.feed.EnqueueDetected(card(n), time.Now())
	}
	eventually(t, "immediate cards posted", func() bool { return len(rig.emu.titles(rig.kf)) == 5 })
	rig.cancel()
	rig.feed.WaitDone()

	// Restart with the variable unset: rotating mode, cycle scaled to 300ms.
	s := newTestSession(t, 5*time.Second)
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: rig.kf})
	rot := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, 300*time.Millisecond, 10)
	if rot.Mode() != FeedModeRotating {
		t.Fatalf("an unset mode must be rotating, got %s", rot.Mode())
	}
	ctx, cancel := context.WithCancel(context.Background())
	go rot.Run(ctx)
	t.Cleanup(func() { cancel(); rot.WaitDone() })
	start := time.Now()
	rot.EnqueueDetected(card(6), start)
	time.Sleep(100 * time.Millisecond)
	if got := len(rig.emu.titles(rig.kf)); got != 5 {
		t.Fatalf("rotating mode must not post before its cycle, channel has %d", got)
	}
	eventually(t, "posted at the cycle", func() bool {
		got := rig.emu.titles(rig.kf)
		return len(got) == 6 && got[5] == "kill-06"
	})
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("expected the rotating cycle wait, posted after %s", waited)
	}
	if c := rig.emu.count(rig.emu.creates, rig.kf); c != 6 {
		t.Fatalf("expected 6 cards created in total (no duplicates), got %d", c)
	}
	// Next cycle: rotating replaces only its own batch; the previous process's
	// five cards remain (documented limitation).
	rot.EnqueueDetected(card(7), time.Now())
	eventually(t, "second cycle replaced its own batch", func() bool {
		got := rig.emu.titles(rig.kf)
		return len(got) == 6 && got[5] == "kill-07"
	})
	if got := rig.emu.titles(rig.kf); !reflect.DeepEqual(got[:5], titlesRange(1, 5)) {
		t.Fatalf("expected the previous process's cards untouched, got %v", got)
	}
}
