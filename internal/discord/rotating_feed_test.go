package discord

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeRotatingFeedAPI records sends and deletes without touching Discord.
type fakeRotatingFeedAPI struct {
	nextID       int
	sent         []string   // channel IDs, one per send
	deletedCalls [][]string // each bulk-delete call's message IDs
}

func (f *fakeRotatingFeedAPI) ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error {
	f.deletedCalls = append(f.deletedCalls, messages)
	return nil
}

func (f *fakeRotatingFeedAPI) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.nextID++
	f.sent = append(f.sent, channelID)
	return &discordgo.Message{ID: fmt.Sprintf("msg-%d", f.nextID)}, nil
}

func newTestRotatingFeed(api *fakeRotatingFeedAPI, channelID string) *RotatingFeed {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: channelID})
	return NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, 0, 10)
}

func TestRotatingFeedFirstFlushPostsWithoutDeleting(t *testing.T) {
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "chan-1")

	for i := 0; i < 3; i++ {
		feed.Enqueue(&discordgo.MessageEmbed{Title: fmt.Sprintf("kill-%d", i)})
	}
	feed.flush()

	if len(api.sent) != 3 {
		t.Fatalf("expected 3 messages sent, got %d", len(api.sent))
	}
	if len(api.deletedCalls) != 0 {
		t.Fatalf("expected no delete on the first cycle, got %v", api.deletedCalls)
	}
}

func TestRotatingFeedSecondFlushDeletesPreviousBatchFirst(t *testing.T) {
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "chan-1")

	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-1"})
	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-2"})
	feed.flush()
	firstBatch := append([]string(nil), feed.lastMsgIDs...)

	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-3"})
	feed.flush()

	if len(api.deletedCalls) != 1 {
		t.Fatalf("expected exactly one delete call, got %d", len(api.deletedCalls))
	}
	if fmt.Sprint(api.deletedCalls[0]) != fmt.Sprint(firstBatch) {
		t.Fatalf("expected the first batch's message IDs deleted, got %v want %v", api.deletedCalls[0], firstBatch)
	}
	if len(api.sent) != 3 { // 2 from first batch + 1 from second
		t.Fatalf("expected 3 total sends across both cycles, got %d", len(api.sent))
	}
}

func TestRotatingFeedKeepsOnlyMostRecentWhenOverCapacity(t *testing.T) {
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "chan-1")

	for i := 0; i < 15; i++ {
		feed.Enqueue(&discordgo.MessageEmbed{Title: fmt.Sprintf("kill-%d", i)})
	}
	feed.flush()

	if len(api.sent) != 10 {
		t.Fatalf("expected only the most recent 10 posted, got %d", len(api.sent))
	}
}

func TestRotatingFeedQuietCycleDeletesAndPostsNothing(t *testing.T) {
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "chan-1")

	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-1"})
	feed.flush()
	if len(api.sent) != 1 {
		t.Fatalf("expected 1 message from the first cycle, got %d", len(api.sent))
	}

	feed.flush() // nothing enqueued this cycle

	if len(api.deletedCalls) != 1 {
		t.Fatalf("expected the previous batch deleted on the quiet cycle, got %d delete calls", len(api.deletedCalls))
	}
	if len(api.sent) != 1 {
		t.Fatalf("expected no new messages posted on a quiet cycle, got %d total sends", len(api.sent))
	}
}

// TestRotatingFeedFlushesOnShutdown guards the redeploy data-loss bug: a kill
// enqueued mid-cycle must still reach Discord even if the process is
// restarted before the next scheduled flush, since it was already persisted
// to the database and would otherwise silently vanish from the channel.
func TestRotatingFeedFlushesOnShutdown(t *testing.T) {
	api := &fakeRotatingFeedAPI{}
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "chan-1"})
	feed := NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, time.Hour, 10)

	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-1"})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		feed.Run(ctx)
	}()
	cancel()

	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
	feed.WaitDone()

	if len(api.sent) != 1 {
		t.Fatalf("expected the pending embed flushed on shutdown, got %d sends", len(api.sent))
	}
}
