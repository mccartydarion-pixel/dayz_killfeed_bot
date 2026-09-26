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
	return &discordgo.Channel{ID: channelID, Name: f.channelName}, nil
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
	c.debounce = 0 // immediate for deterministic tests
	return c
}

func TestCounterZeroToOne(t *testing.T) {
	n := &fakeNamer{}
	c := newTestCounter(n)
	c.Publish(1)
	c.flush()
	if len(n.recorded()) != 1 || n.recorded()[0] != OnlineCounterName(1) {
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
		c.Publish(count)
		c.flush()
	}
	want := []int{1, 2, 1, 0}
	if len(n.recorded()) != len(want) {
		t.Fatalf("expected %d renames, got %d", len(want), len(n.recorded()))
	}
	for i, w := range want {
		if n.recorded()[i] != OnlineCounterName(w) {
			t.Fatalf("rename %d: expected count %d, got %q", i, w, n.recorded()[i])
		}
	}
}

func TestCounterNoRenameWhenUnchanged(t *testing.T) {
	n := &fakeNamer{}
	c := newTestCounter(n)
	c.Publish(5)
	c.flush()
	c.Publish(5) // same value
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
	c.Publish(1)
	c.Publish(2)
	c.Publish(3)

	select {
	case name := <-n.edited:
		if name != OnlineCounterName(3) {
			t.Fatalf("expected final rename to 3, got %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for debounced rename")
	}
	if got := n.recorded(); len(got) != 1 {
		t.Fatalf("expected 1 debounced rename, got %d: %v", len(got), got)
	}
}

func TestCounter403StopsRetryStorm(t *testing.T) {
	n := &fakeNamer{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 403}}}
	c := newTestCounter(n)
	c.Publish(1)
	c.flush()
	if !c.PermissionBlocked() {
		t.Fatal("expected permission-blocked after 403")
	}
	renamesAfter := len(n.recorded())
	// Subsequent publishes must not retry.
	c.Publish(2)
	c.flush()
	if len(n.recorded()) != renamesAfter {
		t.Fatalf("expected no retries after 403, got %d renames", len(n.recorded()))
	}
}

func TestCounter429HonorsRetryAfter(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"0.05"}}}
	n := &fakeNamer{err: &discordgo.RESTError{Response: resp}}
	c := newTestCounter(n)
	c.Publish(1)
	c.flush()
	// First attempt failed with 429 and scheduled a retry after the window.
	if c.retryAfter.IsZero() {
		t.Fatal("expected retryAfter to be set on 429")
	}
}

func TestCounterTransientRetries(t *testing.T) {
	n := &fakeNamer{err: errors.New("temporary network failure")}
	c := newTestCounter(n)
	c.Publish(1)
	c.flush()
	if c.UpdateErrors() == 0 {
		t.Fatal("expected the transient failure to be counted")
	}
}

func TestCounterReconcilesStaleActualChannel(t *testing.T) {
	n := &fakeNamer{channelName: OnlineCounterName(1)}
	c := newTestCounter(n)
	c.lastPublished = 0
	if err := c.Reconcile(0); err != nil {
		t.Fatal(err)
	}
	if c.LastPublished() != 0 || n.currentName() != OnlineCounterName(0) {
		t.Fatalf("expected actual channel reconciliation, published=%d name=%q", c.LastPublished(), n.currentName())
	}
}

func TestCounterFailedReconcileDoesNotAdvancePublishedState(t *testing.T) {
	n := &fakeNamer{channelName: OnlineCounterName(1), err: errors.New("edit failed")}
	c := newTestCounter(n)
	if err := c.Reconcile(0); err == nil {
		t.Fatal("expected reconcile failure")
	}
	if c.LastPublished() != 0 || c.LastPublishResult() == "SUCCESS" {
		t.Fatalf("published state advanced on failure: %d %s", c.LastPublished(), c.LastPublishResult())
	}
}

// countingNamer counts every Discord call (edit and GET) and fails them all
// with err until err is cleared.
type countingNamer struct {
	mu    sync.Mutex
	err   error
	calls int
	gets  int
	name  string
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
	return &discordgo.Channel{ID: channelID, Name: f.name}, nil
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
// Discord call for that channel - both the debounced rename and the
// per-presence-change Reconcile GET - instead of retrying forever.
func TestCounterUnknownChannelIsConfigFault(t *testing.T) {
	n := &countingNamer{err: unknownChannelErr()}
	c := newTestCounter(n)
	c.Publish(3)
	c.flush()
	if !c.Faulted() {
		t.Fatal("expected a config fault after 404/10003")
	}
	h := c.Health()
	if h.State != "CONFIG_FAULT" || h.FaultClass != CounterFaultUnknownChannel || h.FaultChannelID != "vc-1" {
		t.Fatalf("unexpected health %+v", h)
	}
	before := n.total()
	for i := 0; i < 20; i++ { // twenty presence changes
		c.Publish(4 + i)
		c.flush()
		_ = c.Reconcile(4 + i)
	}
	if n.total() != before {
		t.Fatalf("faulted counter kept calling Discord: %d extra calls", n.total()-before)
	}
}

// TestCounterUnknownChannelDetectedOnReconcileGET covers the other entry
// point: the Channel GET inside Reconcile hitting a deleted channel.
func TestCounterUnknownChannelDetectedOnReconcileGET(t *testing.T) {
	n := &countingNamer{err: unknownChannelErr()}
	c := newTestCounter(n)
	if err := c.Reconcile(2); err == nil {
		t.Fatal("expected reconcile error")
	}
	if !c.Faulted() || c.Health().FaultClass != CounterFaultUnknownChannel {
		t.Fatalf("expected UNKNOWN_CHANNEL fault, got %+v", c.Health())
	}
}

// TestCounterRebindClearsFaultAndPublishes: repair (a new channel binding)
// is the reconciliation a fault waits for; the new channel must receive the
// current count even when it equals the last count on the dead channel.
func TestCounterRebindClearsFaultAndPublishes(t *testing.T) {
	n := &countingNamer{}
	c := newTestCounter(n)
	c.Publish(2)
	c.flush()
	if n.name != OnlineCounterName(2) {
		t.Fatalf("setup publish failed: %q", n.name)
	}
	n.err = unknownChannelErr()
	c.Publish(3)
	c.flush()
	if !c.Faulted() {
		t.Fatal("expected fault")
	}
	// Re-binding the SAME dead ID (what the legacy per-event bind does) must not clear it.
	c.SetChannelID("vc-1")
	if !c.Faulted() {
		t.Fatal("re-binding the same channel must not clear the fault")
	}
	n.err = nil
	c.SetChannelID("vc-2")
	if c.Faulted() || c.Health().State == "CONFIG_FAULT" {
		t.Fatal("binding a new channel must clear the fault")
	}
	c.Publish(2) // same as the last successful count on vc-1
	c.flush()
	if n.name != OnlineCounterName(2) {
		t.Fatalf("new channel was not written: %q", n.name)
	}
}

// TestCounterMissingAccessCodeIsPermanent: 50001 arrives as 403 but is
// classified by its JSON code too.
func TestCounterMissingAccessCodeIsPermanent(t *testing.T) {
	n := &countingNamer{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 403}, Message: &discordgo.APIErrorMessage{Code: 50001}}}
	c := newTestCounter(n)
	c.Publish(1)
	c.flush()
	if c.Health().FaultClass != CounterFaultMissingPermissions {
		t.Fatalf("expected MISSING_PERMISSIONS, got %+v", c.Health())
	}
}

// TestCounterTransientRetriesAreBounded: 5xx gets exponential retries capped
// at maxCounterTransientRetries, never an unbounded loop.
func TestCounterTransientRetriesAreBounded(t *testing.T) {
	n := &countingNamer{err: &discordgo.RESTError{Response: &http.Response{StatusCode: 502}}}
	c := newTestCounter(n)
	c.retryBackoff = time.Millisecond
	c.Publish(1)
	c.flush()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if c.Health().State != "RETRYING" && !c.Health().LastAttemptAt.IsZero() {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := n.total(); got != 1+maxCounterTransientRetries {
		t.Fatalf("expected %d attempts (1 + %d retries), got %d", 1+maxCounterTransientRetries, maxCounterTransientRetries, got)
	}
	if c.Faulted() {
		t.Fatal("a transient failure must never be recorded as a config fault")
	}
}

// TestCounterRateLimitIsNotAFault keeps 429 on the Retry-After path.
func TestCounterRateLimitIsNotAFault(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"30"}}}
	n := &countingNamer{err: &discordgo.RESTError{Response: resp}}
	c := newTestCounter(n)
	c.Publish(1)
	c.flush()
	if c.Faulted() {
		t.Fatal("429 must not be a config fault")
	}
	if c.Health().State != "RATE_LIMITED" {
		t.Fatalf("expected RATE_LIMITED, got %s", c.Health().State)
	}
	c.flush() // inside the window: must not call Discord again
	if n.total() != 1 {
		t.Fatalf("expected no call inside Retry-After window, got %d", n.total())
	}
}
