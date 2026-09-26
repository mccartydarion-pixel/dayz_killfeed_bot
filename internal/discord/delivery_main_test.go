package discord

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// TestMain keeps delivery retry backoff out of test wall-clock time (tests
// that need real sleeps set deliverySleep themselves), and points discordgo's
// channel endpoints at a local dispatcher for the whole run: no test can ever
// reach the real Discord API, even from a goroutine that outlives its test.
func TestMain(m *testing.M) {
	deliverySleep = func(time.Duration) {}
	srv := httptest.NewServer(testDiscord)
	discordgo.EndpointChannels = srv.URL + "/api/v10/channels/"
	code := m.Run()
	srv.Close()
	os.Exit(code)
}

// discordDispatcher routes emulated Discord channel requests to the handler
// registered for that channel ID; unknown channels get Discord's 404.
type discordDispatcher struct {
	mu     sync.RWMutex
	routes map[string]http.Handler
}

var testDiscord = &discordDispatcher{routes: map[string]http.Handler{}}

func (d *discordDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v10/channels/")
	id := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		id = rest[:i]
	}
	d.mu.RLock()
	h := d.routes[id]
	d.mu.RUnlock()
	if h == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":10003,"message":"Unknown Channel"}`))
		return
	}
	h.ServeHTTP(w, r)
}

var channelSeq atomic.Int64

// testChannel returns a channel ID unique to this test run and routes it to h
// until the test ends. The base name is kept before "~" for readability.
func testChannel(t *testing.T, h http.Handler, base string) string {
	t.Helper()
	id := fmt.Sprintf("%s~%d", base, channelSeq.Add(1))
	testDiscord.mu.Lock()
	testDiscord.routes[id] = h
	testDiscord.mu.Unlock()
	t.Cleanup(func() {
		testDiscord.mu.Lock()
		delete(testDiscord.routes, id)
		testDiscord.mu.Unlock()
	})
	return id
}

// channelBase strips the per-test suffix from a testChannel ID.
func channelBase(id string) string {
	if i := strings.Index(id, "~"); i >= 0 {
		return id[:i]
	}
	return id
}

// newTestSession returns a real discordgo session; its channel requests go to
// the dispatcher.
func newTestSession(t *testing.T, clientTimeout time.Duration) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot test-harness")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Timeout: clientTimeout}
	return s
}
