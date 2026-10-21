package discord

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Immediate-mode grouping (docs/PERFORMANCE.md section 23): cards waiting
// together go out in one message, in order, within Discord's limits; the
// window counts cards and removes whole messages; the journal and the nonce
// keep a restart from doubling or orphaning a grouped message.

// noCoalesce turns the burst coalescing window off for a test, so grouping
// depends only on what is queued when the feed looks.
func noCoalesce(t *testing.T) {
	t.Helper()
	old := feedCoalesceWindow
	feedCoalesceWindow = 0
	t.Cleanup(func() { feedCoalesceWindow = old })
}

func newGroupFeed(api rotatingFeedAPI, maxItems int) *RotatingFeed {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "kf"})
	f := NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, time.Hour, maxItems)
	f.SetMode(FeedModeImmediate)
	return f
}

func titlesN(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("kill-%02d", i))
	}
	return out
}

func TestImmediateGroupsWaitingCardsInOrder(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := newLiveFeedAPI()
	feed := newGroupFeed(api, 50)
	for n := 1; n <= 25; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	want := [][]string{titlesN(1, 10), titlesN(11, 20), titlesN(21, 25)}
	if !reflect.DeepEqual(api.sent, want) {
		t.Fatalf("messages %v, want %v", api.sent, want)
	}
	if cards, msgs := api.shown(); cards != 25 || msgs != 3 {
		t.Fatalf("visible cards=%d messages=%d, want 25 in 3", cards, msgs)
	}
	feed.mu.Lock()
	window := append([]postedCard(nil), feed.window...)
	feed.mu.Unlock()
	if windowCards(window) != 25 || len(window) != 3 || window[0].cards != 10 || window[2].cards != 5 {
		t.Fatalf("window must count cards per message: %+v", window)
	}
	if r := ledger("KILLFEED"); r.Delivered != 25 || r.LatencySamples != 25 {
		t.Fatalf("every card counts as delivered on its own: %+v", r)
	}
}

func TestImmediateGroupStaysUnderDiscordCharacterLimit(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := newLiveFeedAPI()
	feed := newGroupFeed(api, 50)
	big := strings.Repeat("x", 2000)
	for n := 1; n <= 5; n++ {
		c := card(n)
		c.Description = big
		feed.EnqueueDetected(c, time.Now())
	}
	feed.postImmediate()
	// 2000 characters each: two fit under feedMaxMessageChars, three do not.
	want := [][]string{titlesN(1, 2), titlesN(3, 4), titlesN(5, 5)}
	if !reflect.DeepEqual(api.sent, want) {
		t.Fatalf("messages %v, want %v", api.sent, want)
	}
	for _, c := range []*discordgo.MessageEmbed{card(1), {Description: big, Fields: []*discordgo.MessageEmbedField{{Name: "a", Value: "b"}}, Footer: &discordgo.MessageEmbedFooter{Text: "f"}, Author: &discordgo.MessageEmbedAuthor{Name: "n"}}} {
		want := len([]rune(c.Title)) + len([]rune(c.Description))
		for _, f := range c.Fields {
			want += len([]rune(f.Name)) + len([]rune(f.Value))
		}
		if c.Footer != nil {
			want += len([]rune(c.Footer.Text))
		}
		if c.Author != nil {
			want += len([]rune(c.Author.Name))
		}
		if got := embedChars(c); got != want {
			t.Fatalf("embedChars=%d want %d", got, want)
		}
	}
	// A single card above the limit still goes out, alone.
	huge := card(9)
	huge.Description = strings.Repeat("y", feedMaxMessageChars+1)
	feed.EnqueueDetected(huge, time.Now())
	feed.EnqueueDetected(card(10), time.Now())
	feed.postImmediate()
	if got := api.sent[len(api.sent)-2:]; !reflect.DeepEqual(got, [][]string{{"kill-09"}, {"kill-10"}}) {
		t.Fatalf("oversized card must post alone: %v", got)
	}
}

func TestImmediateGroupNeverExceedsTenEmbeds(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := newLiveFeedAPI()
	feed := newGroupFeed(api, 100)
	feed.SetMaxCardsPerMessage(99) // clamped to Discord's 10
	for n := 1; n <= 30; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	if len(api.sent) != 3 {
		t.Fatalf("30 cards must need 3 messages, got %d", len(api.sent))
	}
	for _, m := range api.sent {
		if len(m) != feedMaxEmbedsPerMessage {
			t.Fatalf("message with %d embeds, max is %d", len(m), feedMaxEmbedsPerMessage)
		}
	}
}

// A single waiting card posts at once; a burst already in progress waits the
// coalescing window for the cards right behind it, then shares the message.
func TestImmediateSingleCardPostsAloneBurstCoalesces(t *testing.T) {
	freshLedger(t)
	old := feedCoalesceWindow
	feedCoalesceWindow = 300 * time.Millisecond
	t.Cleanup(func() { feedCoalesceWindow = old })
	api := newLiveFeedAPI()
	feed := newGroupFeed(api, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); feed.WaitDone() }()
	go feed.Run(ctx)

	start := time.Now()
	feed.EnqueueDetected(card(1), start)
	waitFor(t, "single card posted", func() bool { _, msgs := api.shown(); return msgs == 1 })
	if d := time.Since(start); d >= feedCoalesceWindow {
		t.Fatalf("a single card must not wait for the coalescing window, took %s", d)
	}

	// Two cards are queued while the feed is idle: it waits briefly, and the
	// third card that arrives in that window joins them.
	feed.mu.Lock()
	feed.pending = append(feed.pending, feedItem{embed: card(2), nonce: "n2", enqueuedAt: time.Now()}, feedItem{embed: card(3), nonce: "n3", enqueuedAt: time.Now()})
	feed.mu.Unlock()
	feed.poke()
	time.Sleep(60 * time.Millisecond)
	feed.EnqueueDetected(card(4), time.Now())
	waitFor(t, "burst posted", func() bool { _, msgs := api.shown(); return msgs == 2 })
	if got := api.sent[1]; !reflect.DeepEqual(got, titlesN(2, 4)) {
		t.Fatalf("burst must share one message: %v", got)
	}
}

// The window counts cards and evicts whole messages: with 8 shown and a
// group of 7 arriving in a 10-card window, the 8-card message goes.
func TestImmediateGroupWindowEvictsWholeMessages(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := newLiveFeedAPI()
	feed := newGroupFeed(api, 10)
	for n := 1; n <= 8; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	for n := 9; n <= 15; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	if cards, msgs := api.shown(); cards != 7 || msgs != 2 || api.deletes != 1 {
		t.Fatalf("visible=%d posted=%d deletes=%d, want the 8-card message replaced by the 7-card one", cards, msgs, api.deletes)
	}
	if got := api.visibleTitles(); !reflect.DeepEqual(got, titlesN(9, 15)) {
		t.Fatalf("visible %v", got)
	}
	// A group is never larger than the window itself.
	for n := 16; n <= 27; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	if got := api.sent[2:]; !reflect.DeepEqual(got, [][]string{titlesN(16, 25), titlesN(26, 27)}) {
		t.Fatalf("messages %v", got)
	}
	if cards, _ := api.shown(); cards != 2 {
		t.Fatalf("after a full 10-card message the 2-card one replaces it: visible=%d", cards)
	}
	// Singles after a group: the oldest message goes when the window is full.
	for n := 28; n <= 35; n++ {
		feed.EnqueueDetected(card(n), time.Now())
		feed.postImmediate()
	}
	if cards, _ := api.shown(); cards != 10 {
		t.Fatalf("visible=%d, want 10", cards)
	}
	feed.EnqueueDetected(card(36), time.Now())
	feed.postImmediate()
	if got := api.visibleTitles(); len(got) != 9 || api.deletes != 4 {
		t.Fatalf("the 2-card message must be the one evicted: visible=%v deletes=%d", got, api.deletes)
	}
}

// A grouped message is journaled per card under one message id; after a
// restart the window is taken back as that message, and evicting it closes
// every one of its rows - no orphan, no double post.
func TestImmediateGroupJournalRestartTakesBackWholeMessage(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := newLiveFeedAPI()
	j := newMemJournal()
	a := newGroupFeed(api, 10)
	a.SetJournal(j, "KILLFEED:1")
	for n := 1; n <= 3; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	a.postImmediate()
	rows, _ := j.Open(context.Background(), "KILLFEED:1")
	if len(rows) != 3 || rows[0].MessageID == "" || rows[1].MessageID != rows[0].MessageID || rows[2].MessageID != rows[0].MessageID {
		t.Fatalf("grouped cards must share the message id in the journal: %+v", rows)
	}

	b := newGroupFeed(api, 10)
	b.SetJournal(j, "KILLFEED:1")
	b.restore()
	b.mu.Lock()
	window := append([]postedCard(nil), b.window...)
	b.mu.Unlock()
	if len(window) != 1 || window[0].cards != 3 || window[0].id != rows[0].MessageID {
		t.Fatalf("restored window must be the one 3-card message: %+v", window)
	}
	for n := 4; n <= 11; n++ {
		b.EnqueueDetected(card(n), time.Now())
	}
	b.postImmediate()
	if got := api.visibleTitles(); !reflect.DeepEqual(got, titlesN(4, 11)) {
		t.Fatalf("3 + 8 > 10: the restored message must be evicted whole, visible %v", got)
	}
	if n := j.open("KILLFEED:1"); n != 8 {
		t.Fatalf("journal must close all 3 rows of the evicted message, open=%d", n)
	}
	if c := api.posts; c != 2 {
		t.Fatalf("no card may be posted twice across the restart: %d messages", c)
	}
}

// A restart with a grouped message over the window keeps the newest whole
// messages within maxItems cards and removes the rest.
func TestImmediateGroupRestoreTrimsWholeMessages(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := newLiveFeedAPI()
	j := newMemJournal()
	a := newGroupFeed(api, 50)
	a.SetJournal(j, "KILLFEED:1")
	for n := 1; n <= 4; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	a.postImmediate() // one 4-card message
	for n := 5; n <= 7; n++ {
		a.EnqueueDetected(card(n), time.Now())
	}
	a.postImmediate() // one 3-card message
	a.EnqueueDetected(card(8), time.Now())
	a.postImmediate() // a single

	b := newGroupFeed(api, 5) // the next process shows at most 5 cards
	b.SetJournal(j, "KILLFEED:1")
	b.restore()
	if got := api.visibleTitles(); !reflect.DeepEqual(got, titlesN(5, 8)) {
		t.Fatalf("newest whole messages within 5 cards: visible %v", got)
	}
	if n := j.open("KILLFEED:1"); n != 4 {
		t.Fatalf("journal open rows %d, want the 4 shown", n)
	}
}

// Crash between Discord creating a grouped message and the journal recording
// it: the replay re-sends the group under the same nonce, Discord returns the
// existing message with its cards, and nothing is posted twice - also when
// the replay groups differently.
func TestImmediateGroupReplayAfterCrashDoesNotDuplicate(t *testing.T) {
	for _, replayGroup := range []int{10, 2} {
		t.Run(fmt.Sprintf("replay_group_%d", replayGroup), func(t *testing.T) {
			freshLedger(t)
			noCoalesce(t)
			emu := newDiscordEmu()
			ch := testChannel(t, emu, "kf-group")
			j := newMemJournal()
			j.failPosted = true
			start := func(group int) *RotatingFeed {
				s := newTestSession(t, 5*time.Second)
				store := NewInMemorySetupStore()
				_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: ch})
				f := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, time.Hour, 10)
				f.SetMode(FeedModeImmediate)
				f.SetMaxCardsPerMessage(group)
				f.SetJournal(j, "KILLFEED:1")
				return f
			}
			a := start(10)
			for n := 1; n <= 3; n++ {
				a.EnqueueDetected(card(n), time.Now())
			}
			a.postImmediate()
			if got := emu.messages(ch); !reflect.DeepEqual(got, [][]string{titlesN(1, 3)}) {
				t.Fatalf("setup: one 3-card message, got %v", got)
			}
			j.mu.Lock()
			j.failPosted = false
			j.mu.Unlock()
			if rows, _ := j.Open(context.Background(), "KILLFEED:1"); len(rows) != 3 || rows[0].MessageID != "" {
				t.Fatalf("setup: the cards must still look unconfirmed, %+v", rows)
			}

			b := start(replayGroup)
			ctx, cancel := context.WithCancel(context.Background())
			go b.Run(ctx)
			t.Cleanup(func() { cancel(); b.WaitDone() })
			eventually(t, "replay confirmed every card", func() bool {
				rows, _ := j.Open(context.Background(), "KILLFEED:1")
				if len(rows) != 3 {
					return false
				}
				for _, r := range rows {
					if r.MessageID == "" {
						return false
					}
				}
				return true
			})
			if got := emu.messages(ch); !reflect.DeepEqual(got, [][]string{titlesN(1, 3)}) {
				t.Fatalf("replay must not post again: %v", got)
			}
			if c := emu.count(emu.creates, ch); c != 1 {
				t.Fatalf("messages created %d, want 1", c)
			}
			b.mu.Lock()
			pending, window := len(b.pending), append([]postedCard(nil), b.window...)
			b.mu.Unlock()
			if pending != 0 || len(window) != 1 || window[0].cards != 3 {
				t.Fatalf("pending=%d window=%+v, want nothing pending and the one 3-card message", pending, window)
			}
		})
	}
}

// rejectingAPI refuses any message with more than one embed, and any message
// holding the card titled bad, with Discord's 400.
type rejectingAPI struct {
	*liveFeedAPI
	bad string
}

func (r *rejectingAPI) ChannelMessageSendComplex(ch string, data *discordgo.MessageSend, opts ...discordgo.RequestOption) (*discordgo.Message, error) {
	reject := len(data.Embeds) > 1
	for _, e := range data.Embeds {
		if e.Title == r.bad {
			reject = true
		}
	}
	if reject {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: 400}, Message: &discordgo.APIErrorMessage{Code: 50035, Message: "Invalid Form Body"}}
	}
	return r.liveFeedAPI.ChannelMessageSendComplex(ch, data, opts...)
}

// A rejected grouped message is retried card by card, so only the card Discord
// refuses on its own is dropped.
func TestImmediateRejectedGroupFallsBackToSingleCards(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	api := &rejectingAPI{liveFeedAPI: newLiveFeedAPI(), bad: "kill-02"}
	feed := newGroupFeed(api, 50)
	for n := 1; n <= 3; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	if !reflect.DeepEqual(api.sent, [][]string{{"kill-01"}, {"kill-03"}}) {
		t.Fatalf("messages %v, want the two acceptable cards alone", api.sent)
	}
	if r := ledger("KILLFEED"); r.Delivered != 2 {
		t.Fatalf("ledger %+v", r)
	}
	feed.mu.Lock()
	pending := len(feed.pending)
	feed.mu.Unlock()
	if pending != 0 {
		t.Fatalf("rejected card must be dropped, pending=%d", pending)
	}
}

// Every card of a grouped message is recorded in the journal before the
// message is posted, and marked posted under that message's id.
func TestImmediateGroupJournalsEveryCardBeforePosting(t *testing.T) {
	freshLedger(t)
	noCoalesce(t)
	j := newMemJournal()
	var order []string
	var mu sync.Mutex
	api := &observingAPI{liveFeedAPI: newLiveFeedAPI(), onSend: func() {
		mu.Lock()
		order = append(order, fmt.Sprintf("send:%d", j.open("KILLFEED:1")))
		mu.Unlock()
	}}
	feed := newGroupFeed(api, 50)
	feed.SetJournal(j, "KILLFEED:1")
	for n := 1; n <= 12; n++ {
		feed.EnqueueDetected(card(n), time.Now())
	}
	feed.postImmediate()
	if !reflect.DeepEqual(order, []string{"send:12", "send:12"}) {
		t.Fatalf("all 12 cards must be journaled before the first message is sent: %v", order)
	}
	rows, _ := j.Open(context.Background(), "KILLFEED:1")
	ids := map[string]int{}
	for _, r := range rows {
		ids[r.MessageID]++
	}
	if len(ids) != 2 || ids["m1"] != 10 || ids["m2"] != 2 {
		t.Fatalf("journal rows per message %v", ids)
	}
}

type observingAPI struct {
	*liveFeedAPI
	onSend func()
}

func (o *observingAPI) ChannelMessageSendComplex(ch string, data *discordgo.MessageSend, opts ...discordgo.RequestOption) (*discordgo.Message, error) {
	o.onSend()
	return o.liveFeedAPI.ChannelMessageSendComplex(ch, data, opts...)
}

// Rotating mode is untouched by grouping: one message per card at the cycle.
func TestRotatingModeStillPostsOneCardPerMessage(t *testing.T) {
	freshLedger(t)
	api := newLiveFeedAPI()
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "kf"})
	feed := NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, time.Hour, 10)
	for n := 1; n <= 4; n++ {
		feed.Enqueue(card(n))
	}
	feed.flush()
	if len(api.sent) != 4 {
		t.Fatalf("rotating mode must post one message per card, got %v", api.sent)
	}
	for _, m := range api.sent {
		if len(m) != 1 {
			t.Fatalf("rotating message with %d embeds", len(m))
		}
	}
}
