package discord

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeNamer records ChannelEdit calls and can inject errors. It is safe for
// concurrent use because the counter's debounce timer edits from its own
// goroutine. Each successful edit is also signalled on edited (if set) so tests
// can wait for the editor rather than sleeping.
type fakeNamer struct {
	mu          sync.Mutex
	renames     []string
	err         error
	channelName string
	edited      chan string
}

func (f *fakeNamer) ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return nil, err
	}
	f.renames = append(f.renames, data.Name)
	f.channelName = data.Name
	edited := f.edited
	f.mu.Unlock()
	if edited != nil {
		edited <- data.Name
	}
	return &discordgo.Channel{ID: channelID, Name: data.Name}, nil
}

func (f *fakeNamer) Channel(channelID string) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &discordgo.Channel{ID: channelID, Name: f.channelName, Type: discordgo.ChannelTypeGuildVoice}, nil
}

func (f *fakeNamer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.renames...)
}

func (f *fakeNamer) currentName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.channelName
}

func newTestCounter(namer VoiceChannelNamer) *VoiceChannelCounter {
	c := NewVoiceChannelCounter(namer, "vc-1")
	c.debounce = 0    // immediate for deterministic tests
	c.minInterval = 0 // rate-limit spacing is covered by its own tests
	return c
}

func known(n int) CounterReading { return CounterReading{Count: n, Slots: 18, Known: true} }

func TestOnlineCounterNameFormat(t *testing.T) {
	if got := known(2).Name(); got != "🟢・Online: 2/18" {
		t.Fatalf("unexpected name %q", got)
	}
	if got := (CounterReading{Slots: 18}).Name(); got != "⚪・Online: ?/18" {
		t.Fatalf("unexpected unknown name %q", got)
	}
	if got := OnlineCounterName(0, 0); got != "🟢・Online: 0" {
		t.Fatalf("unexpected no-capacity name %q", got)
	}
}

func TestParseOnlineCounterNameAcceptsCurrentAndLegacy(t *testing.T) {
	cases := map[string]struct {
		count int
		known bool
	}{
		"🟢・Online: 2/18":      {2, true},
		"🟢・Online: 0":         {0, true},
		"🟢・Online Players: 7": {7, true}, // legacy format from older builds
		"⚪・Online: ?/18":      {0, false},
		"general":             {0, false},
	}
	for name, want := range cases {
		count, ok := ParseOnlineCounterName(name)
		if count != want.count || ok != want.known {
			t.Fatalf("%q: got (%d,%v) want (%d,%v)", name, count, ok, want.count, want.known)
		}
	}
}

func TestCounterZeroToOne(t *testing.T) {
	n := &fakeNamer{}
	c := newTestCounter(n)
	c.Publish(known(1))
	c.flush()
	if len(n.recorded()) != 1 || n.recorded()[0] != OnlineCounterName(1, 18) {
		t.Fatalf("expected rename to count 1, got %v", n.recorded())
	}
	if c.LastPublished() != 1 {
		t.Fatalf("expected lastPublished=1, got %d", c.LastPublished())
	}
}

func TestCounterProgressionAndDown(t *testing.T) {
	n := &fakeNamer{}
	c := newTestCounter(n)
	for _, count := range []int{1, 2, 1, 0} {
		c.Publish(known(count))
		c.flush()
	}
	want := []int{1, 2, 1, 0}
	if len(n.recorded()) != len(want) {
		t.Fatalf("expected %d renames, got %d", len(want), len(n.recorded()))
	}
	for i, w := range want {
		if n.recorded()[i] != OnlineCounterName(w, 18) {
			t.Fatalf("rename %d: expected count %d, got %q", i, w, n.recorded()[i])
		}
	}
}

func TestCounterNoRenameWhenUnchanged(t *testing.T) {
	n := &fakeNamer{}
	c := newTestCounter(n)
	c.Publish(known(5))
	c.flush()
	c.Publish(known(5)) // same value
	c.flush()
	if len(n.recorded()) != 1 {
		t.Fatalf("expected no rename for unchanged count, got %d renames", len(n.recorded()))
	}
}

func TestCounterDebounceCollapsesBurst(t *testing.T) {
	n := &fakeNamer{edited: make(chan string, 8)}
	c := NewVoiceChannelCounter(n, "vc-1")
	c.debounce = 20 * time.Millisecond

	// Three rapid joins collapse into one rename to 3. Wait on the editor's
	// signal instead of sleeping so a slow scheduler cannot fail the test.
	c.Publish(known(1))
	c.Publish(known(2))
	c.Publish(known(3))

	select {
	case name := <-n.edited:
		if name != OnlineCounterName(3, 18) {
			t.Fatalf("expected final rename to 3, got %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for debounced rename")
	}
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("expected 1 debounced rename, got %d: %v", len(got), got)
	}
}

// Discord allows 2 renames per channel per 10 minutes. A second change inside
// the spacing window must be deferred (never sent early, never dropped), and a
// burst inside the window collapses to its latest value.
func TestCounterSpacesRenamesForDiscordRateLimit(t *testing.T) {
	n := &fakeNamer{edited: make(chan string, 8)}
	c := newTestCounter(n)
	c.minInterval = 150 * time.Millisecond
	c.Publish(known(1))
	c.flush()
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("expected first rename immediately, got %v", got)
	}
	<-n.edited
	c.Publish(known(2))
	c.Publish(known(3))
	c.flush() // inside the window: must not rename yet
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("renamed inside the spacing window: %v", got)
	}
	select {
	case name := <-n.edited:
		if name != OnlineCounterName(3, 18) {
			t.Fatalf("expected deferred rename to latest value 3, got %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred rename never happened")
	}
	if got := n.recorded(); len(got) != 2 {
		t.Fatalf("expected exactly 2 renames, got %v", got)
	}
}

func TestCounterUnknownReadingShownAsQuestionMark(t *testing.T) {
	n := &fakeNamer{}
	c := newTestCounter(n)
	c.Publish(known(2))
	c.flush()
	c.Publish(CounterReading{Slots: 18})
	c.flush()
	got := n.recorded()
	if len(got) != 2 || got[1] != "⚪・Online: ?/18" {
		t.Fatalf("expected unknown name, got %v", got)
	}
	if c.LastPublished() != 2 {
		t.Fatalf("an unknown reading must not overwrite the last known count, got %d", c.LastPublished())
	}
}

func TestCounter403StopsRetryStorm(t *testing.T) {
	n := &fakeNamer{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 403}}}
	c := newTestCounter(n)
	c.Publish(known(1))
	c.flush()
	if !c.PermissionBlocked() {
		t.Fatal("expected permission-blocked after 403")
	}
	renamesAfter := len(n.recorded())
	// Subsequent publishes must not retry.
	c.Publish(known(2))
	c.flush()
	if len(n.recorded()) != renamesAfter {
		t.Fatalf("expected no retries after 403, got %d renames", len(n.recorded()))
	}
}

func TestCounter429HonorsRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"0.05"}}}
	n := &fakeNamer{err: &discordgo.RESTError{Response: resp}}
	c := newTestCounter(n)
	c.Publish(known(1))
	c.flush()
	// First attempt failed with 429 and scheduled a retry after the window.
	if c.retryAfter.IsZero() {
		t.Fatal("expected retryAfter to be set on 429")
	}
}

// fastFailRenamer returns discordgo's RateLimitError (what the REST client
// yields with retry-on-ratelimit disabled) instead of sleeping.
type fastFailRenamer struct {
	fakeNamer
	rateLimited bool
}

func (f *fastFailRenamer) ChannelRename(channelID, name string) (*discordgo.Channel, error) {
	f.mu.Lock()
	limited := f.rateLimited
	f.mu.Unlock()
	if limited {
		return nil, &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{TooManyRequests: &discordgo.TooManyRequests{RetryAfter: 10 * time.Minute}, URL: "channels/vc-1"}}
	}
	return f.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: name})
}

// A rename rate limit must never block the caller (the ADM pipeline used to
// call the counter inline) - it returns immediately and defers the retry.
func TestCounterRateLimitDoesNotBlockCaller(t *testing.T) {
	n := &fastFailRenamer{rateLimited: true}
	c := newTestCounter(n)
	done := make(chan struct{})
	go func() {
		c.Publish(known(2))
		c.flush()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rate-limited rename blocked the caller")
	}
	if time.Until(c.retryAfter) < 9*time.Minute {
		t.Fatalf("expected Retry-After to be honoured, got %s", time.Until(c.retryAfter))
	}
	if c.UpdateErrors() != 1 || len(n.recorded()) != 0 {
		t.Fatalf("expected one failed attempt and no rename, errors=%d renames=%v", c.UpdateErrors(), n.recorded())
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
	}
	c.mu.Unlock()
}

func TestCounterTransientRetries(t *testing.T) {
	n := &fakeNamer{err: errors.New("temporary network failure")}
	c := newTestCounter(n)
	c.Publish(known(1))
	c.flush()
	if c.UpdateErrors() == 0 {
		t.Fatal("expected the transient failure to be counted")
	}
}

func TestCounterReconcilesStaleActualChannel(t *testing.T) {
	n := &fakeNamer{channelName: OnlineCounterName(1, 18)}
	c := newTestCounter(n)
	if err := c.Reconcile(known(0)); err != nil {
		t.Fatal(err)
	}
	if c.LastPublished() != 0 || n.currentName() != OnlineCounterName(0, 18) {
		t.Fatalf("expected actual channel reconciliation, published=%d name=%q", c.LastPublished(), n.currentName())
	}
}

func TestCounterReconcileMigratesLegacyName(t *testing.T) {
	n := &fakeNamer{channelName: "🟢・Online Players: 2"}
	c := newTestCounter(n)
	if err := c.Reconcile(known(2)); err != nil {
		t.Fatal(err)
	}
	if n.currentName() != "🟢・Online: 2/18" {
		t.Fatalf("expected legacy name to be migrated, got %q", n.currentName())
	}
}

func TestCounterReconcileNoEditWhenAlreadyCorrect(t *testing.T) {
	n := &fakeNamer{channelName: OnlineCounterName(2, 18)}
	c := newTestCounter(n)
	if err := c.Reconcile(known(2)); err != nil {
		t.Fatal(err)
	}
	if len(n.recorded()) != 0 || c.LastPublished() != 2 {
		t.Fatalf("expected no edit, got %v", n.recorded())
	}
	c.Publish(known(2))
	c.flush()
	if len(n.recorded()) != 0 {
		t.Fatalf("expected Publish of the shown value to be a no-op, got %v", n.recorded())
	}
}

func TestCounterFailedReconcileDoesNotAdvancePublishedState(t *testing.T) {
	n := &fakeNamer{channelName: OnlineCounterName(1, 18), err: errors.New("edit failed")}
	c := newTestCounter(n)
	if err := c.Reconcile(known(0)); err == nil {
		t.Fatal("expected reconcile failure")
	}
	if c.LastPublished() != 0 || c.LastPublishResult() == "SUCCESS" || c.LastName() != OnlineCounterName(1, 18) {
		t.Fatalf("published state advanced on failure: %d %s %q", c.LastPublished(), c.LastPublishResult(), c.LastName())
	}
}

// countingNamer counts every Discord call (edit and GET) and fails them all
// with err until err is cleared. channel overrides what the GET returns.
type countingNamer struct {
	mu      sync.Mutex
	err     error
	calls   int
	gets    int
	name    string
	channel *discordgo.Channel
}

func (f *countingNamer) ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	f.name = data.Name
	return &discordgo.Channel{ID: channelID, Name: data.Name}, nil
}

func (f *countingNamer) Channel(channelID string) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.err != nil {
		return nil, f.err
	}
	if f.channel != nil {
		return f.channel, nil
	}
	return &discordgo.Channel{ID: channelID, GuildID: "g1", Name: f.name, Type: discordgo.ChannelTypeGuildVoice}, nil
}

func (f *countingNamer) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls + f.gets
}

func unknownChannelErr() error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: 404},
		Message:  &discordgo.APIErrorMessage{Code: 10003, Message: "Unknown Channel"},
	}
}

// TestCounterUnknownChannelIsConfigFault reproduces the production
// "HTTP 404 Unknown Channel (10003)" loop: the fault must stop every further
// Discord call for that channel - rename and Reconcile GET - instead of
// retrying forever.
func TestCounterUnknownChannelIsConfigFault(t *testing.T) {
	n := &countingNamer{err: unknownChannelErr()}
	c := newTestCounter(n)
	c.Publish(known(3))
	c.flush()
	if !c.Faulted() {
		t.Fatal("expected a config fault after 404/10003")
	}
	h := c.Health()
	if h.State != "CONFIG_FAULT" || h.FaultClass != CounterFaultUnknownChannel || h.FaultChannelID != "vc-1" {
		t.Fatalf("unexpected health %+v", h)
	}
	before := n.total()
	for i := 0; i < 20; i++ {
		c.Publish(known(4 + i))
		c.flush()
		_ = c.Reconcile(known(4 + i))
	}
	if n.total() != before {
		t.Fatalf("faulted counter kept calling Discord: %d extra calls", n.total()-before)
	}
}

func TestCounterUnknownChannelDetectedOnReconcileGET(t *testing.T) {
	n := &countingNamer{err: unknownChannelErr()}
	c := newTestCounter(n)
	if err := c.Reconcile(known(2)); err == nil {
		t.Fatal("expected reconcile error")
	}
	if !c.Faulted() || c.Health().FaultClass != CounterFaultUnknownChannel {
		t.Fatalf("expected UNKNOWN_CHANNEL fault, got %+v", c.Health())
	}
}

// TestCounterRebindClearsFaultAndPublishes: repair (binding a different
// channel) is the reconciliation a fault waits for; re-binding the same dead
// ID is not.
func TestCounterRebindClearsFaultAndPublishes(t *testing.T) {
	n := &countingNamer{err: unknownChannelErr()}
	c := newTestCounter(n)
	c.Publish(known(3))
	c.flush()
	if !c.Faulted() {
		t.Fatal("expected fault")
	}
	c.SetChannelID("vc-1")
	if !c.Faulted() {
		t.Fatal("re-binding the same channel must not clear the fault")
	}
	n.mu.Lock()
	n.err = nil
	n.mu.Unlock()
	c.SetChannelID("vc-2")
	if c.Faulted() || c.Health().State == "CONFIG_FAULT" {
		t.Fatal("binding a new channel must clear the fault")
	}
	c.Publish(known(3))
	c.flush()
	if n.name != known(3).Name() {
		t.Fatalf("new channel was not written: %q", n.name)
	}
}

func TestCounterMissingAccessCodeIsPermanent(t *testing.T) {
	n := &countingNamer{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 403}, Message: &discordgo.APIErrorMessage{Code: 50001}}}
	c := newTestCounter(n)
	c.Publish(known(1))
	c.flush()
	if c.Health().FaultClass != CounterFaultMissingPermissions {
		t.Fatalf("expected MISSING_PERMISSIONS, got %+v", c.Health())
	}
}

// TestCounterRejectsChannelOfAnotherGuildOrWrongType: ownership validation.
// A stored channel ID that resolves to another guild's channel, or to a text
// channel, is never renamed.
func TestCounterRejectsChannelOfAnotherGuildOrWrongType(t *testing.T) {
	for name, ch := range map[string]*discordgo.Channel{
		CounterFaultNotOwned:  {ID: "vc-1", GuildID: "other-guild", Type: discordgo.ChannelTypeGuildVoice, Name: "🟢・Online: 1"},
		CounterFaultWrongType: {ID: "vc-1", GuildID: "g1", Type: discordgo.ChannelTypeGuildText, Name: "general"},
	} {
		t.Run(name, func(t *testing.T) {
			n := &countingNamer{channel: ch}
			c := newTestCounter(n)
			c.SetGuildID("g1")
			if err := c.Reconcile(known(2)); err == nil {
				t.Fatal("expected ownership rejection")
			}
			if c.Health().FaultClass != name {
				t.Fatalf("expected %s, got %+v", name, c.Health())
			}
			if n.calls != 0 {
				t.Fatalf("a rejected channel must never be renamed, got %d edits", n.calls)
			}
		})
	}
}

// TestCounterTransientRetriesAreBounded: 5xx gets exponential retries capped
// at maxCounterTransientRetries, never an unbounded loop.
func TestCounterTransientRetriesAreBounded(t *testing.T) {
	n := &countingNamer{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 502}}}
	c := newTestCounter(n)
	c.retryBackoff = time.Millisecond
	c.Publish(known(1))
	c.flush()
	time.Sleep(300 * time.Millisecond)
	if got := n.total(); got != 1+maxCounterTransientRetries {
		t.Fatalf("expected %d attempts (1 + %d retries), got %d", 1+maxCounterTransientRetries, maxCounterTransientRetries, got)
	}
	if c.Faulted() {
		t.Fatal("a transient failure must never be recorded as a config fault")
	}
}
