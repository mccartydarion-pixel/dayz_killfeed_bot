package discord

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Champion Performance Phase 1, sections 4/5/26/39-42: burst/ordering/
// cross-server-parallelism regression tests against the existing publisher
// architecture (no new sequencer was introduced - the audit found ordering
// is already guaranteed by RotatingFeed's single-writer design: Enqueue only
// appends under a mutex, and the one Run() goroutine drains "pending" in
// append order). These tests lock that guarantee in and prove it holds under
// the exact scenarios the phase's requirements list.

// orderedFakeAPI is like fakeRotatingFeedAPI but records the embed's own
// Description (the ordering marker each test sets) in send order, so a test
// can assert on delivered SEQUENCE, not just count.
type orderedFakeAPI struct {
	mu       sync.Mutex
	nextID   int
	received []string // per-channel-agnostic send order of embed.Description
	byChan   map[string][]string
}

func newOrderedFakeAPI() *orderedFakeAPI { return &orderedFakeAPI{byChan: map[string][]string{}} }

// countIn returns how many messages have been sent to channelID so far -
// safe to call concurrently with the feed's own Run() goroutine (unlike the
// simpler fakeRotatingFeedAPI in rotating_feed_test.go, which every other
// test only ever calls flush() on synchronously from the test goroutine
// itself, so it has no locking of its own).
func (f *orderedFakeAPI) countIn(channelID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byChan[channelID])
}

func (f *orderedFakeAPI) ChannelMessagesBulkDelete(string, []string, ...discordgo.RequestOption) error {
	return nil
}

func (f *orderedFakeAPI) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	marker := ""
	if len(data.Embeds) > 0 {
		marker = data.Embeds[0].Description
	}
	f.received = append(f.received, marker)
	f.byChan[channelID] = append(f.byChan[channelID], marker)
	return &discordgo.Message{ID: fmt.Sprintf("msg-%d", f.nextID)}, nil
}

func newBurstFeed(api rotatingFeedAPI, guildID, channelID string, maxItems int) *RotatingFeed {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: guildID, KillfeedChannelID: channelID})
	return NewRotatingFeed(api, store, guildID, func(s *GuildSetup) string { return s.KillfeedChannelID }, time.Hour, maxItems)
}

// TestRotatingFeedOrderedBurstSingleServer is the section 26/40 assertion:
// for N events numbered 1..N enqueued for ONE server, the fake Discord sink
// must receive them in exactly that order - never e.g. 1,3,2. maxItems is
// set >= N so a single flush carries the whole burst (this test is about
// ORDER, not the separate rotating-window trim behavior already covered by
// rotating_feed_test.go).
func TestRotatingFeedOrderedBurstSingleServer(t *testing.T) {
	for _, n := range []int{10, 50, 100} {
		t.Run(fmt.Sprintf("burst_%d", n), func(t *testing.T) {
			api := newOrderedFakeAPI()
			feed := newBurstFeed(api, "g1", "chan-1", n+10)
			for i := 1; i <= n; i++ {
				feed.Enqueue(&discordgo.MessageEmbed{Description: fmt.Sprintf("%d", i)})
			}
			feed.flush()

			if len(api.received) != n {
				t.Fatalf("expected %d messages sent, got %d", n, len(api.received))
			}
			for i, got := range api.received {
				want := fmt.Sprintf("%d", i+1)
				if got != want {
					t.Fatalf("message %d: got marker %q, want %q (full sequence: %v)", i, got, want, api.received)
				}
			}
		})
	}
}

// slowChannelAPI blocks ChannelMessageSendComplex for one specific channel
// until released, while every other channel sends immediately - the fake
// sink section 41 asks for ("Server A being slow must not block Server B").
type slowChannelAPI struct {
	mu         sync.Mutex
	nextID     int
	blockedCh  string
	release    chan struct{}
	received   map[string][]string
	blockedHit chan struct{} // closed the moment the blocked channel's send is entered
}

func newSlowChannelAPI(blockedChannel string) *slowChannelAPI {
	return &slowChannelAPI{blockedCh: blockedChannel, release: make(chan struct{}), received: map[string][]string{}, blockedHit: make(chan struct{})}
}

func (f *slowChannelAPI) unblock() { close(f.release) }

func (f *slowChannelAPI) ChannelMessagesBulkDelete(string, []string, ...discordgo.RequestOption) error {
	return nil
}

func (f *slowChannelAPI) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if channelID == f.blockedCh {
		select {
		case <-f.blockedHit:
		default:
			close(f.blockedHit)
		}
		<-f.release // held until the test explicitly unblocks it
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	marker := ""
	if len(data.Embeds) > 0 {
		marker = data.Embeds[0].Description
	}
	f.received[channelID] = append(f.received[channelID], marker)
	return &discordgo.Message{ID: fmt.Sprintf("msg-%d", f.nextID)}, nil
}

// TestCrossServerParallelismSlowServerDoesNotBlockOthers is the section
// 26/41 assertion: two independent servers, each with its OWN RotatingFeed
// (and therefore its own Run() goroutine - the existing per-worker wiring,
// not a shared queue), where server A's Discord sink hangs indefinitely.
// Server B's flush must still complete promptly. This is a structural
// property of "one publisher instance per server", not new code - this test
// proves that property, it doesn't add a new mechanism to satisfy it.
func TestCrossServerParallelismSlowServerDoesNotBlockOthers(t *testing.T) {
	api := newSlowChannelAPI("chan-A")
	feedA := newBurstFeed(api, "gA", "chan-A", 20)
	feedB := newBurstFeed(api, "gB", "chan-B", 20)

	for i := 1; i <= 5; i++ {
		feedA.Enqueue(&discordgo.MessageEmbed{Description: fmt.Sprintf("a%d", i)})
		feedB.Enqueue(&discordgo.MessageEmbed{Description: fmt.Sprintf("b%d", i)})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); feedA.flush() }() // will block inside the fake sink
	go func() { defer wg.Done(); feedB.flush() }() // must not wait on A

	// Server B's flush must complete quickly, well before A is ever unblocked.
	done := make(chan struct{})
	go func() {
		// Wait only for B's contribution: give the goroutines a moment to
		// start, then check B's received count directly rather than racing
		// on wg (which also waits on the still-blocked A).
		for {
			api.mu.Lock()
			n := len(api.received["chan-B"])
			api.mu.Unlock()
			if n == 5 {
				close(done)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-done:
		// Server B finished while A is still blocked - exactly the required property.
	case <-time.After(2 * time.Second):
		t.Fatal("server B was blocked by server A's slow Discord sink - cross-server parallelism is broken")
	}

	// Confirm A really was still stuck at this point (not a fluke of timing).
	api.mu.Lock()
	aCount := len(api.received["chan-A"])
	api.mu.Unlock()
	if aCount != 0 {
		t.Fatalf("expected server A to still be blocked with 0 delivered, got %d", aCount)
	}

	api.unblock()
	wg.Wait()

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.received["chan-A"]) != 5 || len(api.received["chan-B"]) != 5 {
		t.Fatalf("expected both servers to eventually deliver all 5 messages, got A=%v B=%v", api.received["chan-A"], api.received["chan-B"])
	}
	for i, got := range api.received["chan-A"] {
		if want := fmt.Sprintf("a%d", i+1); got != want {
			t.Fatalf("server A order: got %q, want %q (full: %v)", got, want, api.received["chan-A"])
		}
	}
	for i, got := range api.received["chan-B"] {
		if want := fmt.Sprintf("b%d", i+1); got != want {
			t.Fatalf("server B order: got %q, want %q (full: %v)", got, want, api.received["chan-B"])
		}
	}
}

// TestRotatingFeedRunGoroutineIsIndependentPerInstance proves the Run()
// ticker goroutine of one feed never touches another feed's state - the
// structural basis for cross-server parallelism (section 4: "parallelism
// ACROSS servers, ordering WITHIN one server").
func TestRotatingFeedRunGoroutineIsIndependentPerInstance(t *testing.T) {
	api := newOrderedFakeAPI() // one thread-safe fake shared by both feeds, keyed by channel
	feedA := NewRotatingFeed(api, mustStore(t, "gA", "chan-A"), "gA", func(s *GuildSetup) string { return s.KillfeedChannelID }, 5*time.Millisecond, 10)
	feedB := NewRotatingFeed(api, mustStore(t, "gB", "chan-B"), "gB", func(s *GuildSetup) string { return s.KillfeedChannelID }, 5*time.Millisecond, 10)

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	go feedA.Run(ctxA)
	go feedB.Run(ctxB)
	t.Cleanup(func() { cancelA(); cancelB(); feedA.WaitDone(); feedB.WaitDone() })

	feedA.Enqueue(&discordgo.MessageEmbed{Description: "only-a"})
	time.Sleep(30 * time.Millisecond) // a few ticks

	if api.countIn("chan-A") == 0 {
		t.Fatal("expected feed A to have flushed its own enqueued item")
	}
	if n := api.countIn("chan-B"); n != 0 {
		t.Fatalf("feed B must never send anything for an item enqueued only on feed A, got %d", n)
	}
}

func mustStore(t *testing.T, guildID, channelID string) *InMemorySetupStore {
	t.Helper()
	store := NewInMemorySetupStore()
	if err := store.Save(GuildSetup{GuildID: guildID, KillfeedChannelID: channelID}); err != nil {
		t.Fatal(err)
	}
	return store
}
