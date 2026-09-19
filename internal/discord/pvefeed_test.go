package discord

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

func TestPveFeedRouteKeyMatchesRoutingPackage(t *testing.T) {
	if routeKeyPveFeed != routing.RoutePveFeed {
		t.Fatalf("route key drifted: %q vs %q", routeKeyPveFeed, routing.RoutePveFeed)
	}
	if routing.RoutePveFeed != "PVE_FEED" {
		t.Fatalf("the SaaS vocabulary key changed: %q", routing.RoutePveFeed)
	}
}

type pveFixture struct {
	res    *keyedResolver
	sender *fakeHitSender
}

func newPveFixture() *pveFixture {
	return &pveFixture{res: newKeyedResolver(), sender: &fakeHitSender{fail: map[string]error{}}}
}

// publisher builds the PVE feed of one server in guild row 7, activated by a
// first tick exactly as Run does at startup.
func (f *pveFixture) publisher(serverID int64) *PveFeedPublisher {
	p := NewPveFeedPublisher(f.sender, f.res, 7, serverID)
	p.tick(false)
	return p
}

func suicide(name string) killfeed.PveDeathNotice {
	return killfeed.PveDeathNotice{Cause: killfeed.DeathCauseSuicide, Name: name}
}

func (f *pveFixture) lastDescription(channel string) string {
	msgs := f.sender.messages(channel)
	if len(msgs) == 0 {
		return ""
	}
	last := msgs[len(msgs)-1]
	return last.embeds[0].Description
}

// --- B/C/D/E: message per proven cause ------------------------------------------------------

func TestPveFeedPublishesSuicideAndClaimsIt(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)

	if !p.PublishPveDeath(suicide("Alice")) {
		t.Fatal("with a route configured the feed must claim the suicide")
	}
	p.tick(false)

	msgs := f.sender.messages("pve-chan")
	if len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("expected 1 message with 1 card, got %+v", msgs)
	}
	if got, want := msgs[0].embeds[0].Description, "💀 **SUICIDE**\nAlice died by suicide."; got != want {
		t.Fatalf("unexpected card:\n%q\nwant\n%q", got, want)
	}
	if msgs[0].mention == nil || len(msgs[0].mention.Parse) != 0 {
		t.Fatal("PvE cards must not be able to ping anyone")
	}
}

func TestPveFeedRendersOnlyTheProvenCause(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)
	for _, tc := range []struct {
		cause killfeed.DeathCause
		want  string
	}{
		{killfeed.DeathCauseInfected, "☣️ **PVE DEATH**\nBob was killed by an infected."},
		{killfeed.DeathCauseAnimal, "🐺 **PVE DEATH**\nBob was killed by an animal."},
		{killfeed.DeathCauseEnvironment, "⚠️ **PVE DEATH**\nBob died to the environment."},
	} {
		if !p.PublishPveDeath(killfeed.PveDeathNotice{Cause: tc.cause, Name: "Bob"}) {
			t.Fatalf("%s must be claimed", tc.cause)
		}
		p.tick(false)
		if got := f.lastDescription("pve-chan"); got != tc.want {
			t.Fatalf("%s: unexpected card:\n%q\nwant\n%q", tc.cause, got, tc.want)
		}
	}
}

// F: an unproven or unsupported cause is never claimed and never rendered - the
// feed has no generic "some cause" card to make one up with.
func TestPveFeedNeverClaimsAnUnprovenCause(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)
	for _, cause := range []killfeed.DeathCause{"", "FALL", "DROWNING", "BLEEDING", "SOMETHING"} {
		if p.PublishPveDeath(killfeed.PveDeathNotice{Cause: cause, Name: "Bob"}) {
			t.Fatalf("cause %q must not be claimed", cause)
		}
	}
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("an unproven cause must publish nothing, got %+v", f.sender.sent)
	}
}

// --- G/H: no route / lookup error -----------------------------------------------------------------

func TestPveFeedNoRouteIsSafeNoOpAndClaimsNothing(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "KILLFEED", "kill-chan") // must NOT be borrowed
	f.res.set(7, 1, "HITFEED", "hit-chan")   // must NOT be borrowed
	p := f.publisher(1)

	if p.PublishPveDeath(suicide("Alice")) {
		t.Fatal("with no PVE_FEED route the feed must not claim the death (the legacy death feed keeps it)")
	}
	p.mu.Lock()
	queued := len(p.queue)
	p.mu.Unlock()
	if queued != 0 {
		t.Fatalf("no route: nothing may be queued, got %d", queued)
	}
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("no PVE_FEED route: expected no messages (no KILLFEED fallback), got %d", f.sender.total())
	}
}

func TestPveFeedLookupErrorIsSafeNoOpAndRecovers(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	f.res.set(7, 1, "KILLFEED", "kill-chan")
	f.res.fail(7, 1, "PVE_FEED", errors.New("db down"))
	p := f.publisher(1)

	if p.PublishPveDeath(suicide("Alice")) {
		t.Fatal("a failed lookup must not claim")
	}
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("lookup error: expected no messages and no fallback, got %d", f.sender.total())
	}

	f.res.fail(7, 1, "PVE_FEED", nil)
	p.tick(false) // route visible again
	if !p.PublishPveDeath(suicide("Alice")) {
		t.Fatal("expected the feed to recover and claim")
	}
	p.tick(false)
	if len(f.sender.messages("pve-chan")) != 1 {
		t.Fatalf("expected delivery after recovery, got %+v", f.sender.sent)
	}
}

// --- I: multi-server -------------------------------------------------------------------------------

func TestPveFeedTwoServersSameGuildNoCrossLeak(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "chan-A")
	f.res.set(7, 2, "PVE_FEED", "chan-B")
	f.res.set(99, 3, "PVE_FEED", "foreign-chan") // another guild (cross-organization)
	a, b, c := f.publisher(1), f.publisher(2), f.publisher(3)

	if !a.PublishPveDeath(suicide("Alice")) || !b.PublishPveDeath(suicide("Bob")) {
		t.Fatal("routed servers must claim")
	}
	if c.PublishPveDeath(suicide("Carol")) { // server 3 has no route in THIS guild
		t.Fatal("a server without a route in its own guild must not claim")
	}
	a.tick(false)
	b.tick(false)
	c.tick(false)

	if f.sender.total() != 2 || len(f.sender.messages("foreign-chan")) != 0 {
		t.Fatalf("expected one message per routed server only, got %+v", f.sender.sent)
	}
	if d := f.lastDescription("chan-A"); !strings.Contains(d, "Alice") || strings.Contains(d, "Bob") {
		t.Fatalf("server A's channel must only carry A's deaths: %q", d)
	}
	if d := f.lastDescription("chan-B"); !strings.Contains(d, "Bob") || strings.Contains(d, "Alice") {
		t.Fatalf("server B's channel must only carry B's deaths: %q", d)
	}
}

// --- J: route change -----------------------------------------------------------------------------------

func TestPveFeedRouteChangeUsesNewChannelAndRemovalReleasesClaims(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "chan-old")
	p := f.publisher(1)

	p.PublishPveDeath(suicide("Alice"))
	p.tick(false)
	if len(f.sender.messages("chan-old")) != 1 {
		t.Fatal("setup: expected a card in chan-old")
	}

	f.res.set(7, 1, "PVE_FEED", "chan-new")
	p.tick(false) // picks up the new route
	p.PublishPveDeath(suicide("Bob"))
	p.tick(false)
	if len(f.sender.messages("chan-old")) != 1 || len(f.sender.messages("chan-new")) != 1 {
		t.Fatalf("expected the new channel used after the change, got %+v", f.sender.sent)
	}

	// Route removed: it stops claiming, so the legacy death feed takes over again.
	f.res.clear(7, 1, "PVE_FEED")
	p.tick(false)
	if p.PublishPveDeath(suicide("Carol")) {
		t.Fatal("after the route was removed the feed must stop claiming")
	}
	p.tick(false)
	if f.sender.total() != 2 {
		t.Fatalf("expected publishing to stop with the route, total=%d", f.sender.total())
	}
}

// --- K: Discord failure --------------------------------------------------------------------------------

func TestPveFeedDiscordFailureDoesNotPropagateAndRecovers(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	f.sender.fail["pve-chan"] = errors.New("discord 500")
	p := f.publisher(1)

	p.PublishPveDeath(suicide("Alice"))
	p.tick(false) // must not panic or block
	p.mu.Lock()
	failed := p.failedSends
	p.mu.Unlock()
	if failed != 1 || f.sender.total() != 0 {
		t.Fatalf("expected one counted failed send, failed=%d sent=%d", failed, f.sender.total())
	}

	delete(f.sender.fail, "pve-chan")
	p.PublishPveDeath(suicide("Bob"))
	p.tick(false)
	if len(f.sender.messages("pve-chan")) != 1 {
		t.Fatalf("expected delivery after Discord recovered, got %+v", f.sender.sent)
	}
}

func TestPveFeedSurvivesPanickingSender(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	f.sender.panicOn = "pve-chan"
	p := f.publisher(1)
	p.PublishPveDeath(suicide("Alice"))
	p.safeTick(false) // recovered, no panic

	f.sender.panicOn = ""
	p.PublishPveDeath(suicide("Bob"))
	p.safeTick(false)
	if len(f.sender.messages("pve-chan")) != 1 {
		t.Fatalf("expected the feed to keep working after a panic, got %+v", f.sender.sent)
	}
}

// PublishPveDeath runs on the persistence goroutine: it must never wait on Discord.
func TestPveFeedPublishNeverTouchesDiscord(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)
	f.sender.mu.Lock() // the sender is "hung": any send would deadlock this test
	done := make(chan struct{})
	go func() {
		for i := 0; i < 300; i++ {
			p.PublishPveDeath(suicide(fmt.Sprintf("P%d", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("PublishPveDeath blocked on the Discord sender")
	}
	f.sender.mu.Unlock()
}

// --- bounds ------------------------------------------------------------------------------------------------

func TestPveFeedBurstIsBounded(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)

	before := runtime.NumGoroutine()
	const events = 300
	for i := 0; i < events; i++ {
		p.PublishPveDeath(suicide(fmt.Sprintf("Player%03d", i)))
	}
	if delta := runtime.NumGoroutine() - before; delta > 2 {
		t.Fatalf("a burst must not spawn goroutines, got +%d", delta)
	}
	p.mu.Lock()
	queued, dropped := len(p.queue), p.dropped
	p.mu.Unlock()
	if queued != pveMaxQueue || dropped != events-pveMaxQueue {
		t.Fatalf("expected queue capped at %d with %d dropped, got queued=%d dropped=%d", pveMaxQueue, events-pveMaxQueue, queued, dropped)
	}

	for tick := 1; tick <= 20; tick++ {
		p.tick(false)
		if n := len(f.sender.messages("pve-chan")); n > tick {
			t.Fatalf("tick %d produced more than one message per tick (total %d)", tick, n)
		}
	}
	cards, reports := 0, 0
	seen := map[string]bool{}
	for _, m := range f.sender.messages("pve-chan") {
		if len(m.embeds) > pveEmbedsPerMessage {
			t.Fatalf("a message carried %d cards, max %d", len(m.embeds), pveEmbedsPerMessage)
		}
		for _, e := range m.embeds {
			cards++
			for _, line := range strings.Split(e.Description, "\n") {
				if strings.HasSuffix(line, " died by suicide.") {
					if seen[line] {
						t.Fatalf("player repeated: %q", line)
					}
					seen[line] = true
				}
				if strings.Contains(line, "were not shown") {
					reports++
				}
			}
		}
	}
	if cards != pveMaxQueue {
		t.Fatalf("expected exactly the %d retained cards delivered, got %d", pveMaxQueue, cards)
	}
	if seen["Player000 died by suicide."] || !seen[fmt.Sprintf("Player%03d died by suicide.", events-1)] {
		t.Fatal("expected the oldest dropped and the newest kept")
	}
	if reports != 1 {
		t.Fatalf("expected the drop reported exactly once, got %d", reports)
	}
}

// --- M: mention safety -----------------------------------------------------------------------------------------

func TestPveFeedNamesCannotMention(t *testing.T) {
	names := []string{"@everyone", "@here", "<@123456789012345678>", "<@&987654321098765432>", "#general", "Bad\x00Name\x07", strings.Repeat("A", 200)}
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)

	for _, cause := range []killfeed.DeathCause{killfeed.DeathCauseSuicide, killfeed.DeathCauseInfected, killfeed.DeathCauseAnimal, killfeed.DeathCauseEnvironment} {
		for _, n := range names {
			p.PublishPveDeath(killfeed.PveDeathNotice{Cause: cause, Name: n})
		}
		for len(f.sender.sent) < 1 || p.queueLen() > 0 {
			p.tick(false)
		}
	}
	for _, m := range f.sender.messages("pve-chan") {
		if m.mention == nil || len(m.mention.Parse) != 0 || len(m.mention.Users) != 0 || len(m.mention.Roles) != 0 {
			t.Fatalf("AllowedMentions must be empty, got %+v", m.mention)
		}
		for _, e := range m.embeds {
			for _, banned := range []string{"@", "#", "\x00", "\x07"} {
				if strings.Contains(e.Description, banned) {
					t.Fatalf("player name leaked %q into %q", banned, e.Description)
				}
			}
			if strings.Contains(e.Description, strings.Repeat("A", 60)) {
				t.Fatalf("over-long name must be truncated: %q", e.Description)
			}
		}
	}
	if len(f.sender.messages("pve-chan")) == 0 {
		t.Fatal("expected messages to inspect")
	}
}

func (p *PveFeedPublisher) queueLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// --- lifecycle -----------------------------------------------------------------------------------------------------

func TestPveFeedRunFlushesQueueOnShutdown(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := NewPveFeedPublisher(f.sender, f.res, 7, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !p.active.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !p.active.Load() {
		t.Fatal("Run must activate the feed at startup")
	}
	p.PublishPveDeath(suicide("Alice"))
	cancel()
	<-done
	if len(f.sender.messages("pve-chan")) != 1 {
		t.Fatalf("expected the queued notice flushed on shutdown, got %+v", f.sender.sent)
	}
}

func TestPveFeedNilSafety(t *testing.T) {
	var p *PveFeedPublisher
	if p.PublishPveDeath(suicide("A")) {
		t.Fatal("a nil feed claims nothing")
	}
	p.Run(context.Background())
	p.Flush()
	q := NewPveFeedPublisher(nil, nil, 7, 1)
	if q.PublishPveDeath(suicide("A")) {
		t.Fatal("an inactive feed claims nothing")
	}
	q.Flush()
}
