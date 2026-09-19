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
