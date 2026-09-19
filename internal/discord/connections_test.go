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

func TestConnectionsRouteKeyMatchesRoutingPackage(t *testing.T) {
	if routeKeyConnections != routing.RouteConnections {
		t.Fatalf("route key drifted: %q vs %q", routeKeyConnections, routing.RouteConnections)
	}
	if routing.RouteConnections != "CONNECTIONS" {
		t.Fatalf("the SaaS vocabulary key changed: %q", routing.RouteConnections)
	}
}

type connFixture struct {
	res    *keyedResolver
	sender *fakeHitSender
}

func newConnFixture() *connFixture {
	return &connFixture{res: newKeyedResolver(), sender: &fakeHitSender{fail: map[string]error{}}}
}

// publisher builds the CONNECTIONS feed of one server in guild row 7, activated
// by a first tick exactly as Run does at startup.
func (f *connFixture) publisher(serverID int64) *ConnectionsPublisher {
	p := NewConnectionsPublisher(f.sender, f.res, 7, serverID)
	p.tick(false)
	return p
}

func connected(name string) killfeed.ConnectionNotice {
	return killfeed.ConnectionNotice{Kind: killfeed.ConnectionConnected, Name: name}
}

func disconnected(name string, session time.Duration) killfeed.ConnectionNotice {
	return killfeed.ConnectionNotice{Kind: killfeed.ConnectionDisconnected, Name: name, Session: session}
}

func (f *connFixture) lastDescription(channel string) string {
	msgs := f.sender.messages(channel)
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].embeds[0].Description
}

// --- A/B: connect and disconnect are published ---------------------------------------

func TestConnectionsPublishesConnect(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)

	p.PublishConnection(connected("Alice"))
	p.tick(false)

	msgs := f.sender.messages("conn-chan")
	if len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("expected 1 message, got %+v", msgs)
	}
	if got, want := msgs[0].embeds[0].Description, "🟢 **CONNECTED**\nAlice joined the server."; got != want {
		t.Fatalf("unexpected card:\n%q\nwant\n%q", got, want)
	}
	if msgs[0].mention == nil || len(msgs[0].mention.Parse) != 0 {
		t.Fatal("connection cards must not be able to ping anyone")
	}
}

func TestConnectionsPublishesDisconnectWithSessionWhenKnown(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)

	p.PublishConnection(disconnected("Alice", 42*time.Minute))
	p.tick(false)
	if got, want := f.lastDescription("conn-chan"), "🔴 **DISCONNECTED**\nAlice left the server.\nSession: 42m"; got != want {
		t.Fatalf("unexpected card:\n%q\nwant\n%q", got, want)
	}

	// Unknown or sub-minute sessions are omitted, never invented.
	for _, d := range []time.Duration{0, 30 * time.Second} {
		p.PublishConnection(disconnected("Bob", d))
		p.tick(false)
		if got, want := f.lastDescription("conn-chan"), "🔴 **DISCONNECTED**\nBob left the server."; got != want {
			t.Fatalf("session %v must be omitted:\n%q\nwant\n%q", d, got, want)
		}
	}
}

func TestFormatSession(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, ""}, {59 * time.Second, ""}, {time.Minute, "1m"}, {42*time.Minute + 30*time.Second, "42m"},
		{time.Hour, "1h"}, {65 * time.Minute, "1h 5m"}, {26*time.Hour + 3*time.Minute, "26h 3m"},
	} {
		if got := formatSession(tc.in); got != tc.want {
			t.Fatalf("formatSession(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- C/D: no route / lookup error ------------------------------------------------------

func TestConnectionsNoRouteIsSafeNoOp(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "KILLFEED", "kill-chan") // must NOT be borrowed
	f.res.set(7, 1, "HITFEED", "hit-chan")   // must NOT be borrowed
	p := f.publisher(1)

	for i := 0; i < 50; i++ {
		p.PublishConnection(connected(fmt.Sprintf("P%d", i)))
	}
	p.mu.Lock()
	queued := len(p.queue)
	p.mu.Unlock()
	if queued != 0 {
		t.Fatalf("no route: events must not be queued, got %d", queued)
	}
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("no CONNECTIONS route: expected no messages (no KILLFEED/HITFEED fallback), got %d", f.sender.total())
	}
}

func TestConnectionsLookupErrorIsSafeNoOpAndRecovers(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	f.res.set(7, 1, "KILLFEED", "kill-chan")
	f.res.fail(7, 1, "CONNECTIONS", errors.New("db down"))
	p := f.publisher(1)

	p.PublishConnection(connected("Alice"))
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("lookup error: expected no messages and no fallback, got %d", f.sender.total())
	}

	f.res.fail(7, 1, "CONNECTIONS", nil)
	p.tick(false) // route visible again
	p.PublishConnection(connected("Alice"))
	p.tick(false)
	if len(f.sender.messages("conn-chan")) != 1 {
		t.Fatalf("expected the feed to recover, got %+v", f.sender.sent)
	}
}

// --- E: multi-server -------------------------------------------------------------------

func TestConnectionsTwoServersSameGuildNoCrossLeak(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "chan-A")
	f.res.set(7, 2, "CONNECTIONS", "chan-B")
	f.res.set(99, 3, "CONNECTIONS", "foreign-chan") // another guild (cross-organization)
	a, b, c := f.publisher(1), f.publisher(2), f.publisher(3)

	a.PublishConnection(connected("Alice"))
	b.PublishConnection(connected("Bob"))
	c.PublishConnection(connected("Carol")) // server 3 has no route in THIS guild
	a.tick(false)
	b.tick(false)
	c.tick(false)

	if f.sender.total() != 2 || len(f.sender.messages("foreign-chan")) != 0 {
		t.Fatalf("expected one message per routed server only, got %+v", f.sender.sent)
	}
	if d := f.lastDescription("chan-A"); !strings.Contains(d, "Alice") || strings.Contains(d, "Bob") {
		t.Fatalf("server A's channel must only carry A's players: %q", d)
	}
	if d := f.lastDescription("chan-B"); !strings.Contains(d, "Bob") || strings.Contains(d, "Alice") {
		t.Fatalf("server B's channel must only carry B's players: %q", d)
	}
}

// --- G: route change -------------------------------------------------------------------

func TestConnectionsRouteChangeUsesNewChannel(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "chan-old")
	p := f.publisher(1)

	p.PublishConnection(connected("Alice"))
	p.tick(false)
	if len(f.sender.messages("chan-old")) != 1 {
		t.Fatal("setup: expected a card in chan-old")
	}

	f.res.set(7, 1, "CONNECTIONS", "chan-new")
	p.tick(false) // picks up the new route
	p.PublishConnection(connected("Bob"))
	p.tick(false)
	if len(f.sender.messages("chan-old")) != 1 || len(f.sender.messages("chan-new")) != 1 {
		t.Fatalf("expected the new channel used after the change, got %+v", f.sender.sent)
	}

	f.res.clear(7, 1, "CONNECTIONS")
	p.tick(false)
	p.PublishConnection(connected("Carol"))
	p.tick(false)
	if f.sender.total() != 2 {
		t.Fatalf("expected publishing to stop with the route, total=%d", f.sender.total())
	}
}

// --- H: Discord failure ------------------------------------------------------------------

func TestConnectionsDiscordFailureDoesNotPropagateAndRecovers(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	f.sender.fail["conn-chan"] = errors.New("discord 500")
	p := f.publisher(1)

	p.PublishConnection(connected("Alice"))
	p.tick(false) // must not panic or block
	p.mu.Lock()
	failed := p.failedSends
	p.mu.Unlock()
	if failed != 1 || f.sender.total() != 0 {
		t.Fatalf("expected one counted failed send, failed=%d sent=%d", failed, f.sender.total())
	}

	delete(f.sender.fail, "conn-chan")
	p.PublishConnection(connected("Bob"))
	p.tick(false)
	if len(f.sender.messages("conn-chan")) != 1 {
		t.Fatalf("expected delivery after Discord recovered, got %+v", f.sender.sent)
	}
}

func TestConnectionsSurvivesPanickingSender(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	f.sender.panicOn = "conn-chan"
	p := f.publisher(1)
	p.PublishConnection(connected("Alice"))
	p.safeTick(false) // recovered, no panic

	f.sender.panicOn = ""
	p.PublishConnection(connected("Bob"))
	p.safeTick(false)
	if len(f.sender.messages("conn-chan")) != 1 {
		t.Fatalf("expected the feed to keep working after a panic, got %+v", f.sender.sent)
	}
}

// PublishConnection never waits on Discord: a hung sender cannot delay it.
func TestConnectionsPublishNeverTouchesDiscord(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)
	f.sender.mu.Lock() // the sender is "hung": any send would deadlock this test
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			p.PublishConnection(connected(fmt.Sprintf("P%d", i)))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("PublishConnection blocked on the Discord sender")
	}
	f.sender.mu.Unlock()
}

// --- I: bursts -------------------------------------------------------------------------------

// A restart burst is bounded on every axis: one message per tick, at most 20
// events in it, at most 300 queued, oldest dropped, no goroutine per event, and
// every player keeps a line of their own.
func TestConnectionsBurstIsBoundedAndKeepsPlayersSeparate(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)

	goroutinesBefore := runtime.NumGoroutine()
	const events = 1000
	for i := 0; i < events; i++ {
		p.PublishConnection(connected(fmt.Sprintf("Player%04d", i)))
	}
	if delta := runtime.NumGoroutine() - goroutinesBefore; delta > 2 {
		t.Fatalf("a burst must not spawn goroutines, got +%d", delta)
	}
	p.mu.Lock()
	queued, dropped := len(p.queue), p.dropped
	p.mu.Unlock()
	if queued != connectionsMaxQueue || dropped != events-connectionsMaxQueue {
		t.Fatalf("expected queue capped at %d with %d dropped, got queued=%d dropped=%d", connectionsMaxQueue, events-connectionsMaxQueue, queued, dropped)
	}

	seen := map[string]bool{}
	for tick := 1; tick <= 40; tick++ {
		p.tick(false)
		msgs := f.sender.messages("conn-chan")
		if len(msgs) > tick {
			t.Fatalf("tick %d produced more than one message per tick (total %d)", tick, len(msgs))
		}
	}
	msgs := f.sender.messages("conn-chan")
	lines := 0
	for _, m := range msgs {
		if len(m.embeds) != 1 {
			t.Fatalf("expected one embed per message, got %d", len(m.embeds))
		}
		for _, line := range strings.Split(m.embeds[0].Description, "\n") {
			if strings.HasSuffix(line, " connected") {
				lines++
				if seen[line] {
					t.Fatalf("player line repeated: %q", line)
				}
				seen[line] = true
			}
		}
		if n := strings.Count(m.embeds[0].Description, "🟢"); n > connectionsLinesPerMessage {
			t.Fatalf("a message carried %d events, max %d", n, connectionsLinesPerMessage)
		}
	}
	if lines != connectionsMaxQueue {
		t.Fatalf("expected exactly the %d retained events delivered, got %d", connectionsMaxQueue, lines)
	}
	// Oldest dropped, newest kept - and every retained player still has their own line.
	if seen["🟢 Player0000 connected"] || !seen[fmt.Sprintf("🟢 Player%04d connected", events-1)] {
		t.Fatal("expected the oldest events dropped and the newest kept")
	}
	// The first message says how many earlier events were not shown - once.
	if first := msgs[0].embeds[0].Description; !strings.Contains(first, fmt.Sprintf("%d earlier connection events were not shown", events-connectionsMaxQueue)) {
		t.Fatalf("expected the first message to report the dropped events, got %q", first)
	}
	notShownReports := 0
	for _, m := range msgs {
		if strings.Contains(m.embeds[0].Description, "not shown") {
			notShownReports++
		}
	}
	if notShownReports != 1 {
		t.Fatalf("expected the drop to be reported exactly once, got %d", notShownReports)
	}
}

// A small batch (>1 event) uses the batched card; players stay on separate lines,
// mixed connects/disconnects keep their order.
func TestConnectionsBatchFormat(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)
	p.PublishConnection(connected("PlayerA"))
	p.PublishConnection(connected("PlayerB"))
	p.PublishConnection(disconnected("PlayerC", 12*time.Minute))
	p.PublishConnection(disconnected("PlayerD", 0))
	p.tick(false)

	want := "🔌 **SERVER CONNECTIONS**\n🟢 PlayerA connected\n🟢 PlayerB connected\n🔴 PlayerC disconnected · 12m\n🔴 PlayerD disconnected"
	if got := f.lastDescription("conn-chan"); got != want {
		t.Fatalf("unexpected batch:\n%q\nwant\n%q", got, want)
	}
	if f.sender.total() != 1 {
		t.Fatalf("a small burst must be ONE message, got %d", f.sender.total())
	}
}

// --- J: mention safety ---------------------------------------------------------------------------

func TestConnectionsNamesCannotMention(t *testing.T) {
	names := []string{"@everyone", "@here", "<@123456789012345678>", "<@&987654321098765432>", "#general", "Bad\x00Name\x07", strings.Repeat("A", 200)}
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)

	// Single-event cards.
	for _, n := range names {
		p.PublishConnection(connected(n))
		p.tick(false)
		p.PublishConnection(disconnected(n, 5*time.Minute))
		p.tick(false)
	}
	// And the batched card.
	for _, n := range names {
		p.PublishConnection(connected(n))
	}
	p.tick(false)

	for _, m := range f.sender.messages("conn-chan") {
		if m.mention == nil || len(m.mention.Parse) != 0 || len(m.mention.Users) != 0 || len(m.mention.Roles) != 0 {
			t.Fatalf("AllowedMentions must be empty, got %+v", m.mention)
		}
		d := m.embeds[0].Description
		for _, banned := range []string{"@", "#", "\x00", "\x07"} {
			if strings.Contains(d, banned) {
				t.Fatalf("player name leaked %q into %q", banned, d)
			}
		}
		if strings.Contains(d, strings.Repeat("A", 60)) {
			t.Fatalf("over-long name must be truncated: %q", d)
		}
	}
	if len(f.sender.messages("conn-chan")) == 0 {
		t.Fatal("expected messages to inspect")
	}
}

// --- K: only the two authoritative kinds ------------------------------------------------------------

func TestConnectionsIgnoresUnknownKindsAndNeverInfersReconnect(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)

	p.PublishConnection(killfeed.ConnectionNotice{Kind: "RECONNECTED", Name: "Alice"})
	p.PublishConnection(killfeed.ConnectionNotice{Name: "Alice"})
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("unknown kinds must be ignored, got %+v", f.sender.sent)
	}

	// Disconnect then connect is rendered as exactly that - never as "RECONNECTED".
	p.PublishConnection(disconnected("Alice", 3*time.Minute))
	p.PublishConnection(connected("Alice"))
	p.tick(false)
	got := f.lastDescription("conn-chan")
	if strings.Contains(strings.ToLower(got), "reconnect") || !strings.Contains(got, "🔴 Alice disconnected") || !strings.Contains(got, "🟢 Alice connected") {
		t.Fatalf("expected plain disconnect + connect lines, got %q", got)
	}
}

// --- lifecycle -----------------------------------------------------------------------------------------

func TestConnectionsRunFlushesQueueOnShutdown(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := NewConnectionsPublisher(f.sender, f.res, 7, 1)

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
	p.PublishConnection(connected("Alice"))
	cancel()
	<-done
	if len(f.sender.messages("conn-chan")) != 1 {
		t.Fatalf("expected the queued notice flushed on shutdown, got %+v", f.sender.sent)
	}
}

func TestConnectionsNilSafety(t *testing.T) {
	var p *ConnectionsPublisher
	p.PublishConnection(connected("A"))
	p.Run(context.Background())
	p.Flush()
	q := NewConnectionsPublisher(nil, nil, 7, 1)
	q.PublishConnection(connected("A")) // inactive: no-op
	q.Flush()
}
