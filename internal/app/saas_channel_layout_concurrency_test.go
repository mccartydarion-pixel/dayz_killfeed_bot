package app

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
)

// slowLayoutGuild is a layout fake whose live checks take time, as Discord's do, and that counts
// how many run at once.
type slowLayoutGuild struct {
	*layoutGuildFake
	inFlight, peak atomic.Int32
	mu             sync.Mutex
	verified       map[string]int
}

func (s *slowLayoutGuild) Verify(guildID, channelID string) discord.Verification {
	now := s.inFlight.Add(1)
	for {
		peak := s.peak.Load()
		if now <= peak || s.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	time.Sleep(15 * time.Millisecond)
	s.inFlight.Add(-1)
	s.mu.Lock()
	s.verified[channelID]++
	s.mu.Unlock()
	return s.layoutGuildFake.Verify(guildID, channelID)
}

// The website gives the layout status a few seconds. Checked one destination after another, a
// full layout's Discord requests took longer than that and the page showed "could not load".
func TestInspectChannelLayoutChecksDestinationsTogether(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	runLayout(t, g, w, auditProducers(), panelsPosted(g, w))

	sequential, err := inspectChannelLayout(g, "g", w.existing(), auditProducers())
	if err != nil {
		t.Fatal(err)
	}
	slow := &slowLayoutGuild{layoutGuildFake: g, verified: map[string]int{}}
	together, err := inspectChannelLayout(slow, "g", w.existing(), auditProducers())
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(sequential, together) {
		t.Fatal("checking destinations together must report exactly what checking them in turn does, in the same order")
	}
	if len(slow.verified) < 2 {
		t.Fatalf("expected several text destinations to be checked, got %d", len(slow.verified))
	}
	for id, n := range slow.verified {
		if n != 1 {
			t.Fatalf("channel %s was checked %d times", id, n)
		}
	}
	if peak := slow.peak.Load(); peak < 2 || peak > layoutCheckConcurrency {
		t.Fatalf("expected between 2 and %d checks at once, saw %d", layoutCheckConcurrency, peak)
	}
}

// A destination that fails its live check is still reported broken, with the reason.
func TestInspectChannelLayoutStillReportsABrokenDestination(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	killfeed := w.routes["KILLFEED"]
	if killfeed == "" {
		t.Fatal("the layout should have mapped KILLFEED")
	}
	g.missing[killfeed] = []string{"Send Messages"}

	status, err := inspectChannelLayout(g, "g", w.existing(), auditProducers())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range status {
		if d.ChannelID != killfeed {
			continue
		}
		found = true
		if d.Health != HealthBroken || d.Detail == "" || d.Checks == nil || d.Checks.BotCanSend {
			t.Fatalf("a channel the bot cannot post in must be reported broken with a reason, got %+v", d)
		}
	}
	if !found {
		t.Fatal("the killfeed destination is missing from the report")
	}
}
