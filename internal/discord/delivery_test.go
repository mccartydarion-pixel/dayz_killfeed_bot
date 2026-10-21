package discord

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// scriptedSender returns errs[i] for the i-th call (nil = success).
type scriptedSender struct {
	errs  []error
	calls int
}

func (s *scriptedSender) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return &discordgo.Message{ID: "m"}, nil
}

func restErr(status, code int, retryAfter string) error {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	e := &discordgo.RESTError{Response: &http.Response{StatusCode: status, Header: h}}
	if code != 0 {
		e.Message = &discordgo.APIErrorMessage{Code: code}
	}
	return e
}

func freshLedger(t *testing.T) {
	t.Helper()
	old := Deliveries
	Deliveries = &DeliveryLedger{}
	t.Cleanup(func() { Deliveries = old })
}

func TestClassifyDeliveryError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{restErr(404, 10003, ""), DeliveryConfigFault},
		{restErr(403, 50001, ""), DeliveryConfigFault},
		{restErr(403, 50013, ""), DeliveryConfigFault},
		{restErr(404, 0, ""), DeliveryConfigFault},
		{restErr(429, 0, "1"), DeliveryRateLimited},
		{restErr(502, 0, ""), DeliveryTransient},
		{restErr(400, 50035, ""), DeliveryRejected},
		{errors.New("dial tcp: i/o timeout"), DeliveryTransient},
	}
	for _, c := range cases {
		if got := ClassifyDeliveryError(c.err); got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
}

// TestDeliveryInvalidChannelIsNotRetried: an Unknown Channel is a
// configuration fault - one attempt, recorded, never an endless retry.
func TestDeliveryInvalidChannelIsNotRetried(t *testing.T) {
	freshLedger(t)
	s := &scriptedSender{errs: []error{restErr(404, 10003, ""), nil}}
	if _, err := deliverMessage(s, "KILLFEED", "dead", &discordgo.MessageSend{}); err == nil {
		t.Fatal("expected failure")
	}
	if s.calls != 1 {
		t.Fatalf("config fault must not be retried, got %d calls", s.calls)
	}
	snap := Deliveries.Snapshot()
	if len(snap) != 1 || snap[0].State() != DeliveryConfigFault || snap[0].Failed != 1 || snap[0].ChannelID != "dead" {
		t.Fatalf("ledger: %+v", snap)
	}
}

// TestDeliveryTransientRetriesAreBoundedWithBackoff: 5xx retries with
// exponential backoff and stops at deliveryAttempts.
func TestDeliveryTransientRetriesAreBoundedWithBackoff(t *testing.T) {
	freshLedger(t)
	var waits []time.Duration
	old := deliverySleep
	deliverySleep = func(d time.Duration) { waits = append(waits, d) }
	defer func() { deliverySleep = old }()

	s := &scriptedSender{errs: []error{restErr(502, 0, ""), restErr(503, 0, ""), restErr(500, 0, ""), nil}}
	if _, err := deliverMessage(s, "HITFEED", "c", &discordgo.MessageSend{}); err == nil {
		t.Fatal("expected failure after the bounded attempts")
	}
	if s.calls != deliveryAttempts {
		t.Fatalf("expected %d attempts, got %d", deliveryAttempts, s.calls)
	}
	if len(waits) != deliveryAttempts-1 || waits[1] != 2*waits[0] {
		t.Fatalf("expected exponential backoff between attempts, got %v", waits)
	}
	if st := Deliveries.Snapshot()[0].State(); st != "FAILING" {
		t.Fatalf("expected FAILING, got %s", st)
	}
}

// TestDeliveryRecoversAfterTransientFailure: a retry that succeeds is a
// delivery, and clears the failing state.
func TestDeliveryRecoversAfterTransientFailure(t *testing.T) {
	freshLedger(t)
	s := &scriptedSender{errs: []error{restErr(502, 0, "")}}
	if _, err := deliverMessage(s, "CONNECTIONS", "c", &discordgo.MessageSend{}); err != nil {
		t.Fatal(err)
	}
	r := Deliveries.Snapshot()[0]
	if r.State() != "OK" || r.Delivered != 1 || r.LastSuccessAt.IsZero() {
		t.Fatalf("ledger: %+v", r)
	}
}

// TestDeliveryHonorsRetryAfter: a 429 waits exactly Retry-After; one longer
// than the cap fails fast instead of stalling the feed goroutine.
func TestDeliveryHonorsRetryAfter(t *testing.T) {
	freshLedger(t)
	var waits []time.Duration
	old := deliverySleep
	deliverySleep = func(d time.Duration) { waits = append(waits, d) }
	defer func() { deliverySleep = old }()

	s := &scriptedSender{errs: []error{restErr(429, 0, "1.5")}}
	if _, err := deliverMessage(s, "KILLFEED", "c", &discordgo.MessageSend{}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 1500*time.Millisecond {
		t.Fatalf("expected a 1.5s Retry-After wait, got %v", waits)
	}
	waits = nil
	long := &scriptedSender{errs: []error{restErr(429, 0, "600")}}
	if _, err := deliverMessage(long, "KILLFEED", "c", &discordgo.MessageSend{}); err == nil {
		t.Fatal("expected fail-fast on an over-cap Retry-After")
	}
	if len(waits) != 0 || long.calls != 1 {
		t.Fatalf("must not sleep 600s: waits=%v calls=%d", waits, long.calls)
	}
}

type failingRotatingAPI struct {
	fakeRotatingFeedAPI
	fail error
}

func (f *failingRotatingAPI) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return f.fakeRotatingFeedAPI.ChannelMessageSendComplex(channelID, data, options...)
}

// TestRotatingFeedKeepsUndeliveredItems: kills that could not be posted in
// one cycle (Discord outage, broken route) are retried next cycle instead of
// being silently discarded.
func TestRotatingFeedKeepsUndeliveredItems(t *testing.T) {
	freshLedger(t)
	api := &failingRotatingAPI{fail: restErr(503, 0, "")}
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "chan-1"})
	feed := NewRotatingFeed(api, store, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, 0, 10)
	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-1"})
	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-2"})
	feed.flush()
	if len(api.sent) != 0 {
		t.Fatal("setup: nothing should be sent during the outage")
	}
	api.fail = nil
	feed.flush()
	if len(api.sent) != 2 {
		t.Fatalf("expected both kills delivered after recovery, got %d", len(api.sent))
	}

	// A configuration fault stops the cycle at the first item.
	api.fail = restErr(404, 10003, "")
	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-3"})
	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill-4"})
	feed.flush()
	feed.mu.Lock()
	kept := len(feed.pending)
	feed.mu.Unlock()
	if kept != 2 {
		t.Fatalf("expected both items kept for a repaired route, got %d", kept)
	}
}
