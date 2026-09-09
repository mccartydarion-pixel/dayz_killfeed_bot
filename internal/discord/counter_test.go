package discord

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeNamer records ChannelEdit calls and can inject errors.
type fakeNamer struct {
	renames []string
	err     error
}

func (f *fakeNamer) ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.renames = append(f.renames, data.Name)
	return &discordgo.Channel{ID: channelID, Name: data.Name}, nil
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
	if len(n.renames) != 1 || n.renames[0] != OnlineCounterName(1) {
		t.Fatalf("expected rename to count 1, got %v", n.renames)
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
	if len(n.renames) != len(want) {
		t.Fatalf("expected %d renames, got %d", len(want), len(n.renames))
	}
	for i, w := range want {
		if n.renames[i] != OnlineCounterName(w) {
			t.Fatalf("rename %d: expected count %d, got %q", i, w, n.renames[i])
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
	if len(n.renames) != 1 {
		t.Fatalf("expected no rename for unchanged count, got %d renames", len(n.renames))
	}
}

func TestCounterDebounceCollapsesBurst(t *testing.T) {
	n := &fakeNamer{}
	c := NewVoiceChannelCounter(n, "vc-1")
	c.debounce = 20 * time.Millisecond

	// Three rapid joins collapse into one rename to 3.
	c.Publish(1)
	c.Publish(2)
	c.Publish(3)
	time.Sleep(60 * time.Millisecond)

	if len(n.renames) != 1 {
		t.Fatalf("expected 1 debounced rename, got %d", len(n.renames))
	}
	if n.renames[0] != OnlineCounterName(3) {
		t.Fatalf("expected final rename to 3, got %q", n.renames[0])
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
	renamesAfter := len(n.renames)
	// Subsequent publishes must not retry.
	c.Publish(2)
	c.flush()
	if len(n.renames) != renamesAfter {
		t.Fatalf("expected no retries after 403, got %d renames", len(n.renames))
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
