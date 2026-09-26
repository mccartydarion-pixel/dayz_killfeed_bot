package discord

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// liveFeedAPI is a concurrency-safe fake that tracks which messages are
// currently visible in the channel.
type liveFeedAPI struct {
	mu      sync.Mutex
	next    int
	visible map[string]string // message id -> title
	posts   int
	deletes int
}

func newLiveFeedAPI() *liveFeedAPI { return &liveFeedAPI{visible: map[string]string{}} }

func (f *liveFeedAPI) ChannelMessagesBulkDelete(_ string, ids []string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.visible, id)
	}
	f.deletes++
	return nil
}

func (f *liveFeedAPI) ChannelMessageDelete(_ string, id string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.visible, id)
	f.deletes++
	return nil
}

func (f *liveFeedAPI) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("m%d", f.next)
	f.visible[id] = data.Embeds[0].Title
	f.posts++
	return &discordgo.Message{ID: id}, nil
}

func (f *liveFeedAPI) shown() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.visible), f.posts
}

func newLiveFeed(api *liveFeedAPI, interval time.Duration, mode string) *RotatingFeed {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "kf"})
	f := NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, interval, 10)
	f.SetMode(mode)
	return f
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestImmediateModePostsWithoutWaitingForTheCycle: in immediate mode a card
// is posted as soon as it is queued, not at the next interval.
func TestImmediateModePostsWithoutWaitingForTheCycle(t *testing.T) {
	freshLedger(t)
	api := newLiveFeedAPI()
	feed := newLiveFeed(api, time.Hour, FeedModeImmediate)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); feed.WaitDone() }()
	go feed.Run(ctx)

	feed.EnqueueDetected(&discordgo.MessageEmbed{Title: "kill-1"}, time.Now())
	waitFor(t, "immediate post", func() bool { _, posts := api.shown(); return posts == 1 })
	r := Deliveries.Snapshot()[0]
	if r.LatencySamples != 1 || r.LastQueueWaitMs > 1000 {
		t.Fatalf("expected a sub-second queue wait, got %+v", r)
	}
}

// TestImmediateModeKeepsTheRollingWindow: the presentation is preserved - at
// most maxItems cards shown, the oldest removed first.
func TestImmediateModeKeepsTheRollingWindow(t *testing.T) {
	freshLedger(t)
	api := newLiveFeedAPI()
	feed := newLiveFeed(api, time.Hour, FeedModeImmediate)
	for i := 1; i <= 13; i++ {
		feed.EnqueueDetected(&discordgo.MessageEmbed{Title: fmt.Sprintf("kill-%d", i)}, time.Time{})
		feed.postImmediate()
	}
	visible, posts := api.shown()
	if posts != 13 || visible != 10 {
		t.Fatalf("expected 13 posted and the newest 10 visible, got posts=%d visible=%d", posts, visible)
	}
	api.mu.Lock()
	for _, title := range api.visible {
		if title == "kill-1" || title == "kill-2" || title == "kill-3" {
			t.Errorf("oldest card %s should have been removed", title)
		}
	}
	api.mu.Unlock()
}

// TestImmediateModeEmptiesAfterAQuietInterval: like the rotating cycle, cards
// do not outlive one interval, so a quiet period leaves the channel empty.
func TestImmediateModeEmptiesAfterAQuietInterval(t *testing.T) {
	freshLedger(t)
	api := newLiveFeedAPI()
	feed := newLiveFeed(api, time.Minute, FeedModeImmediate)
	feed.EnqueueDetected(&discordgo.MessageEmbed{Title: "kill-1"}, time.Time{})
	feed.EnqueueDetected(&discordgo.MessageEmbed{Title: "kill-2"}, time.Time{})
	feed.postImmediate()
	feed.expireImmediate(time.Now().Add(30 * time.Second))
	if visible, _ := api.shown(); visible != 2 {
		t.Fatalf("cards younger than the interval must stay, got %d", visible)
	}
	feed.expireImmediate(time.Now().Add(61 * time.Second))
	if visible, _ := api.shown(); visible != 0 {
		t.Fatalf("cards older than the interval must be removed, got %d", visible)
	}
}

// TestDefaultModeIsUnchangedRotating: without opting in, nothing is posted
// before the cycle - the production behavior stays as it was.
func TestDefaultModeIsUnchangedRotating(t *testing.T) {
	freshLedger(t)
	api := newLiveFeedAPI()
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "kf"})
	feed := NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, time.Hour, 10)
	if feed.Mode() != FeedModeRotating {
		t.Fatalf("default mode must be rotating, got %s", feed.Mode())
	}
	ctx, cancel := context.WithCancel(context.Background())
	go feed.Run(ctx)
	feed.EnqueueDetected(&discordgo.MessageEmbed{Title: "kill-1"}, time.Now())
	time.Sleep(50 * time.Millisecond)
	if _, posts := api.shown(); posts != 0 {
		t.Fatal("rotating mode must not post before its cycle")
	}
	cancel()
	feed.WaitDone() // shutdown flush still delivers
	if _, posts := api.shown(); posts != 1 {
		t.Fatalf("shutdown flush must deliver the pending card, got %d posts", posts)
	}
}

// TestQueueWaitRotatingVersusImmediate is the measured before/after for the
// Discord queue stage, with the 10-minute cycle scaled to 400ms (the same
// Run loop and timers as production). Cards are queued at random-ish points in
// the cycle; rotating mode waits for the next tick, immediate mode does not.
func TestQueueWaitRotatingVersusImmediate(t *testing.T) {
	const interval = 400 * time.Millisecond
	measure := func(mode string) RouteDelivery {
		freshLedger(t)
		api := newLiveFeedAPI()
		feed := newLiveFeed(api, interval, mode)
		ctx, cancel := context.WithCancel(context.Background())
		go feed.Run(ctx)
		offsets := []time.Duration{50, 130, 90, 170} // ms between cards
		for i, gap := range offsets {
			time.Sleep(gap * time.Millisecond)
			feed.EnqueueDetected(&discordgo.MessageEmbed{Title: fmt.Sprintf("kill-%d", i)}, time.Now())
		}
		waitFor(t, "all cards delivered", func() bool { _, posts := api.shown(); return posts == len(offsets) })
		cancel()
		feed.WaitDone()
		return Deliveries.Snapshot()[0]
	}
	rot := measure(FeedModeRotating)
	imm := measure(FeedModeImmediate)
	t.Logf("queue wait, cycle scaled 10min->%s: rotating avg=%dms max=%dms | immediate avg=%dms max=%dms",
		interval, rot.AvgQueueWaitMs, rot.MaxQueueWaitMs, imm.AvgQueueWaitMs, imm.MaxQueueWaitMs)
	if imm.MaxQueueWaitMs >= rot.AvgQueueWaitMs || imm.MaxQueueWaitMs > 50 {
		t.Fatalf("immediate mode must remove the cycle wait: rotating %+v immediate %+v", rot, imm)
	}
}
