package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeDiscordAPI is a local stand-in for Discord's REST API so the counter is
// exercised through the REAL discordgo client and the production SessionAPI
// (request building, error decoding, rate-limit handling) - with no network
// access and no real guild, channel or role touched.
type fakeDiscordAPI struct {
	mu       sync.Mutex
	names    map[string]string // channel id -> current name
	requests map[string]int    // "METHOD id" -> count
}

func (f *fakeDiscordAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	f.requests[r.Method+" "+id]++
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fail := func(status, code int, msg string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":` + strconv.Itoa(code) + `,"message":"` + msg + `"}`))
	}
	switch id {
	case "vc-gone":
		fail(404, 10003, "Unknown Channel")
		return
	case "vc-noperm":
		if r.Method == http.MethodPatch {
			fail(403, 50013, "Missing Permissions")
			return
		}
	case "vc-ratelimited":
		if r.Method == http.MethodPatch {
			w.Header().Set("Retry-After", "30")
			w.Header().Set("X-RateLimit-Bucket", "channel-rename")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"message":"You are being rate limited.","retry_after":30,"global":false}`))
			return
		}
	}
	ch := discordgo.Channel{ID: id, GuildID: "g1", Type: discordgo.ChannelTypeGuildVoice}
	switch id {
	case "vc-other-guild":
		ch.GuildID = "someone-elses-guild"
	case "vc-text":
		ch.Type = discordgo.ChannelTypeGuildText
	}
	f.mu.Lock()
	if r.Method == http.MethodPatch {
		body, _ := io.ReadAll(r.Body)
		var edit struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &edit)
		f.names[id] = edit.Name
	}
	ch.Name = f.names[id]
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(ch)
}

func (f *fakeDiscordAPI) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[key]
}

func (f *fakeDiscordAPI) name(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[id]
}

// newRealClientCounter wires a counter to the production SessionAPI over a
// real discordgo.Session pointed at the fake API.
func newRealClientCounter(t *testing.T, channelID string) (*VoiceChannelCounter, *fakeDiscordAPI) {
	t.Helper()
	fake := &fakeDiscordAPI{names: map[string]string{"vc-ok": "🟢・Online Players: 4"}, requests: map[string]int{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	old := discordgo.EndpointChannels
	discordgo.EndpointChannels = srv.URL + "/api/v10/channels/"
	t.Cleanup(func() { discordgo.EndpointChannels = old })
	s, err := discordgo.New("Bot staging-fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = srv.Client()
	s.MaxRestRetries = 0
	c := NewVoiceChannelCounter(NewSessionAPI(s), channelID)
	c.SetGuildID("g1")
	c.SetDebounce(0)
	c.SetMinRenameInterval(0)
	return c, fake
}

// TestCounterOverRealDiscordClient runs the counter's Discord scenarios
// through the real discordgo client: legacy-name migration, permanent faults,
// ownership rejection, and a non-blocking 429.
func TestCounterOverRealDiscordClient(t *testing.T) {
	t.Run("reconcile migrates the legacy name then stays idempotent", func(t *testing.T) {
		c, fake := newRealClientCounter(t, "vc-ok")
		if err := c.Reconcile(known(4)); err != nil {
			t.Fatal(err)
		}
		if got := fake.name("vc-ok"); got != known(4).Name() {
			t.Fatalf("expected the legacy name migrated to %q, got %q", known(4).Name(), got)
		}
		patches := fake.count("PATCH vc-ok")
		c.Publish(known(4)) // same reading: no rename
		time.Sleep(20 * time.Millisecond)
		if fake.count("PATCH vc-ok") != patches {
			t.Fatal("an unchanged count must not rename the channel")
		}
		c.Publish(known(5))
		deadline := time.Now().Add(2 * time.Second)
		for fake.name("vc-ok") != known(5).Name() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := fake.name("vc-ok"); got != known(5).Name() || c.Health().State != "OK" {
			t.Fatalf("expected rename to %q and OK health, got %q %+v", known(5).Name(), got, c.Health())
		}
	})

	for id, class := range map[string]string{"vc-gone": CounterFaultUnknownChannel, "vc-noperm": CounterFaultMissingPermissions} {
		t.Run("permanent fault "+class, func(t *testing.T) {
			c, fake := newRealClientCounter(t, id)
			_ = c.Reconcile(known(2))
			if c.Health().State != "CONFIG_FAULT" || c.Health().FaultClass != class {
				t.Fatalf("expected CONFIG_FAULT %s, got %+v", class, c.Health())
			}
			before := fake.count("GET "+id) + fake.count("PATCH "+id)
			for i := 0; i < 10; i++ {
				c.Publish(known(3 + i))
				_ = c.Reconcile(known(3 + i))
			}
			time.Sleep(30 * time.Millisecond)
			if after := fake.count("GET "+id) + fake.count("PATCH "+id); after != before {
				t.Fatalf("faulted counter kept calling Discord: %d -> %d requests", before, after)
			}
			// Recovery: repair binds a working channel.
			c.SetChannelID("vc-ok")
			if err := c.Reconcile(known(3)); err != nil || fake.name("vc-ok") != known(3).Name() {
				t.Fatalf("expected recovery on the repaired channel, err=%v name=%q", err, fake.name("vc-ok"))
			}
			if c.Health().State != "OK" {
				t.Fatalf("expected OK after repair, got %+v", c.Health())
			}
		})
	}

	for id, class := range map[string]string{"vc-other-guild": CounterFaultNotOwned, "vc-text": CounterFaultWrongType} {
		t.Run("ownership "+class, func(t *testing.T) {
			c, fake := newRealClientCounter(t, id)
			_ = c.Reconcile(known(2))
			if c.Health().FaultClass != class || fake.count("PATCH "+id) != 0 {
				t.Fatalf("expected %s and no rename, got %+v patches=%d", class, c.Health(), fake.count("PATCH "+id))
			}
		})
	}

	t.Run("429 returns immediately and is scheduled, not slept", func(t *testing.T) {
		c, _ := newRealClientCounter(t, "vc-ratelimited")
		start := time.Now()
		_ = c.Reconcile(known(2))
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("a 30s Retry-After blocked the caller for %s", elapsed)
		}
		if h := c.Health(); h.State != "RATE_LIMITED" || c.Faulted() {
			t.Fatalf("expected RATE_LIMITED (not a fault), got %+v", h)
		}
	})
}
