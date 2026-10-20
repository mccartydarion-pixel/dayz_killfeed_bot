package discord

import (
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// A delivered kill card lands in the latency statistics with every stage the engine knew, under
// the event's server and this feed's name. Only timestamps travel: the statistics hold no name.
func TestFeedRecordsStageLatencyWhenDiscordAccepts(t *testing.T) {
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "chan-latency")
	feed.SetLatencyServerID(880042)

	read := time.Now().Add(-4 * time.Second)
	ev := &killfeed.Event{Type: killfeed.EventPlayerKill, ServerID: 880041, LoggedAt: read.Add(-3 * time.Minute), ReadAt: read, DetectedAt: read.Add(10 * time.Millisecond)}
	feed.EnqueueTimed(&discordgo.MessageEmbed{Title: "kill"}, killfeed.TimingOf(ev))
	if got := killfeed.FeedLatency.SnapshotServer(880041); len(got) != 0 {
		t.Fatalf("recorded before Discord accepted the post: %+v", got)
	}
	feed.flush()

	got := killfeed.FeedLatency.SnapshotServer(880041)
	if len(got) != 1 || got[0].Feed != "KILLFEED" || got[0].LastHour.Cards != 1 || got[0].CardsWithoutServerClock != 0 {
		t.Fatalf("snapshot: %+v", got)
	}
	w := got[0].LastHour
	if w.GameLogToBotReadMs.P50Ms != 180_000 {
		t.Fatalf("game log -> bot read = %d ms, want 180000", w.GameLogToBotReadMs.P50Ms)
	}
	if ms := w.BotReadToDiscordAcceptedMs.P50Ms; ms < 4000 || ms > 9000 {
		t.Fatalf("bot read -> Discord accepted = %d ms, want about 4000", ms)
	}
	if ms := w.GameLogToDiscordAcceptedMs.P50Ms; ms < 184_000 || ms > 189_000 {
		t.Fatalf("game log -> Discord accepted = %d ms, want about 184000", ms)
	}
	if w.BotReadToQueuedMs.Samples != 1 || w.QueuedToDiscordAcceptedMs.Samples != 1 {
		t.Fatalf("middle stages missing: %+v", w)
	}

	// A card that carries no server (restored from the journal after a restart, or queued by a
	// caller that only knows the parse time) is counted under the feed's own server.
	feed.EnqueueDetected(&discordgo.MessageEmbed{Title: "restored"}, time.Now().Add(-time.Second))
	feed.flush()
	own := killfeed.FeedLatency.SnapshotServer(880042)
	if len(own) != 1 || own[0].LastHour.Cards != 1 || own[0].CardsWithoutServerClock != 1 || own[0].LastHour.BotReadToDiscordAcceptedMs.Samples != 1 {
		t.Fatalf("feed-level server: %+v", own)
	}
	if again := killfeed.FeedLatency.SnapshotServer(880041); again[0].DeliveredSinceStart != 1 {
		t.Fatalf("the second card was counted for the wrong server: %+v", again)
	}
}

// refusingFeedAPI answers every post with 400: rejected at once, never retried.
type refusingFeedAPI struct{ fakeRotatingFeedAPI }

func (f *refusingFeedAPI) ChannelMessageSendComplex(string, *discordgo.MessageSend, ...discordgo.RequestOption) (*discordgo.Message, error) {
	return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}}
}

// A post Discord refuses is not a delivery and must not appear in the statistics.
func TestFeedDoesNotRecordLatencyForAFailedPost(t *testing.T) {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "chan-refused"})
	feed := NewRotatingFeed(&refusingFeedAPI{}, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, 0, 10)
	feed.SetRoute("LATENCY_TEST_REFUSED") // keep this failure out of the shared KILLFEED ledger entry
	feed.EnqueueTimed(&discordgo.MessageEmbed{Title: "kill"}, killfeed.FeedTiming{ServerID: 880051, ReadAt: time.Now()})
	feed.flush()
	if got := killfeed.FeedLatency.SnapshotServer(880051); len(got) != 0 {
		t.Fatalf("a failed post was recorded as delivered: %+v", got)
	}
}
