package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

func TestHitfeedRouteKeyMatchesRoutingPackage(t *testing.T) {
	if routeKeyHitfeed != routing.RouteHitfeed {
		t.Fatalf("route key drifted: %q vs %q", routeKeyHitfeed, routing.RouteHitfeed)
	}
	if routing.RouteHitfeed != "HITFEED" {
		t.Fatalf("the SaaS vocabulary key changed: %q", routing.RouteHitfeed)
	}
}

// --- fakes -----------------------------------------------------------------

type sentHit struct {
	channel string
	embeds  []*discordgo.MessageEmbed
	mention *discordgo.MessageAllowedMentions
}

type fakeHitSender struct {
	mu      sync.Mutex
	sent    []sentHit
	fail    map[string]error // by channel
	panicOn string
}

func (f *fakeHitSender) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicOn == channelID {
		panic("discord client bug")
	}
	if err := f.fail[channelID]; err != nil {
		return nil, err
	}
	f.sent = append(f.sent, sentHit{channel: channelID, embeds: data.Embeds, mention: data.AllowedMentions})
	return &discordgo.Message{ID: fmt.Sprintf("m%d", len(f.sent)), ChannelID: channelID}, nil
}

func (f *fakeHitSender) messages(channel string) []sentHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentHit
	for _, s := range f.sent {
		if s.channel == channel {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeHitSender) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type hitClock struct{ t time.Time }

func (c *hitClock) now() time.Time          { return c.t }
func (c *hitClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type hitFixture struct {
	res    *keyedResolver
	sender *fakeHitSender
	clock  *hitClock
}

func newHitFixture() *hitFixture {
	return &hitFixture{res: newKeyedResolver(), sender: &fakeHitSender{fail: map[string]error{}}, clock: &hitClock{t: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}}
}

// publisher builds the hit feed of one server in guild row 7. It is activated
// by a first tick, exactly as Run does at startup.
func (f *hitFixture) publisher(serverID int64) *HitfeedPublisher {
	p := NewHitfeedPublisher(f.sender, f.res, 7, serverID)
	p.now = f.clock.now
	p.tick(false)
	return p
}

func hitEvent(attacker, victim string) *killfeed.Event {
	return &killfeed.Event{
		Type:     killfeed.EventPlayerHit,
		Attacker: &killfeed.PlayerRef{Name: attacker, ID: "id-" + attacker},
		Victim:   &killfeed.PlayerRef{Name: victim, ID: "id-" + victim},
	}
}

func fullHit(attacker, victim string) *killfeed.Event {
	ev := hitEvent(attacker, victim)
	d, dmg := 42.4, 28.0
	ev.Weapon, ev.Ammo, ev.HitZone, ev.Distance, ev.Damage = "M4-A1", "Bullet_556x45", "Torso", &d, &dmg
	return ev
}

func desc(m sentHit, i int) string { return m.embeds[i].Description }

// --- A/B/C: route, no route, lookup error ----------------------------------

// A: HITFEED route configured -> the hit is published to it, with its real fields.
func TestHitfeedPublishesToConfiguredRoute(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)

	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow + time.Second)
	p.tick(false)

	msgs := f.sender.messages("hit-chan")
	if len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("expected 1 message with 1 card, got %+v", msgs)
	}
	want := "🎯 **HIT**  Alice  ➜  Bob\nM4-A1 · 556x45 · 42m\nTorso · 1 hit · 28 dmg"
	if got := desc(msgs[0], 0); got != want {
		t.Fatalf("unexpected card:\n%q\nwant\n%q", got, want)
	}
	if msgs[0].mention == nil || len(msgs[0].mention.Parse) != 0 {
		t.Fatal("hit cards must not be able to ping anyone")
	}
}

// B: no route -> a safe no-op: nothing sent, nothing buffered, no KILLFEED fallback.
func TestHitfeedNoRouteIsSafeNoOp(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "KILLFEED", "kill-chan") // a KILLFEED route must NOT be borrowed
	p := f.publisher(1)

	for i := 0; i < 50; i++ {
		p.PublishHit(fullHit("Alice", "Bob"))
	}
	// Guilds without a route pay nothing: not even buffered between ticks.
	p.mu.Lock()
	buffered := len(p.open) + len(p.ready)
	p.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("no route: hits must not be buffered, got %d", buffered)
	}
	f.clock.advance(time.Minute)
	p.tick(false)

	if f.sender.total() != 0 {
		t.Fatalf("no HITFEED route: expected no messages, got %d", f.sender.total())
	}
}

// C: a failed lookup is also a no-op (never a fallback), and the feed recovers
// by itself once lookups work again.
func TestHitfeedLookupErrorIsSafeNoOpAndRecovers(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	f.res.set(7, 1, "KILLFEED", "kill-chan")
	f.res.fail(7, 1, "HITFEED", errors.New("db down"))
	p := f.publisher(1)

	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(time.Minute)
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("lookup error: expected no messages (and no KILLFEED fallback), got %d", f.sender.total())
	}

	f.res.fail(7, 1, "HITFEED", nil)
	p.tick(false) // route visible again
	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	if len(f.sender.messages("hit-chan")) != 1 {
		t.Fatalf("expected the feed to recover, got %+v", f.sender.sent)
	}
}

// --- D: multi-server --------------------------------------------------------

// Two servers of ONE guild, each with its own HITFEED channel: no leakage; a
// third server without a route publishes nothing (it does not inherit a sibling's).
func TestHitfeedTwoServersSameGuildNoCrossLeak(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "chan-A")
	f.res.set(7, 2, "HITFEED", "chan-B")
	a, b, c := f.publisher(1), f.publisher(2), f.publisher(3)

	a.PublishHit(fullHit("Alice", "Bob"))
	b.PublishHit(fullHit("Carol", "Dave"))
	c.PublishHit(fullHit("Erin", "Frank"))
	f.clock.advance(hitfeedWindow + time.Second)
	a.tick(false)
	b.tick(false)
	c.tick(false)

	ma, mb := f.sender.messages("chan-A"), f.sender.messages("chan-B")
	if len(ma) != 1 || len(mb) != 1 || f.sender.total() != 2 {
		t.Fatalf("expected one message per routed server only, got %+v", f.sender.sent)
	}
	if !strings.Contains(desc(ma[0], 0), "Alice") || strings.Contains(desc(ma[0], 0), "Carol") {
		t.Fatalf("server A's channel must only carry A's hits: %q", desc(ma[0], 0))
	}
	if !strings.Contains(desc(mb[0], 0), "Carol") || strings.Contains(desc(mb[0], 0), "Alice") {
		t.Fatalf("server B's channel must only carry B's hits: %q", desc(mb[0], 0))
	}
}

// A route belonging to another guild (cross-organization) never resolves here.
func TestHitfeedCrossGuildRouteIgnored(t *testing.T) {
	f := newHitFixture()
	f.res.set(99, 1, "HITFEED", "foreign-chan") // same server id, another guild row
	p := f.publisher(1)
	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(time.Minute)
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("a foreign guild's route must never receive hits, got %+v", f.sender.sent)
	}
}

// --- E: rapid hits / flood --------------------------------------------------

// Hundreds of hits in one exchange collapse into ONE card carrying the count
// and the latest values.
func TestHitfeedRapidHitsAggregateIntoOneCard(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)

	for i := 0; i < 300; i++ {
		ev := fullHit("Alice", "Bob")
		d := 100.0 + float64(i)
		ev.Distance = &d
		if i == 299 {
			ev.HitZone = "Head"
		}
		p.PublishHit(ev)
	}
	f.clock.advance(hitfeedWindow)
	p.tick(false)

	msgs := f.sender.messages("hit-chan")
	if len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("expected a single aggregated card, got %d messages", len(msgs))
	}
	got := desc(msgs[0], 0)
	if !strings.Contains(got, "300 hits") || !strings.Contains(got, "399m") || !strings.Contains(got, "Head") || !strings.Contains(got, "8400 dmg") {
		t.Fatalf("expected latest distance/zone, hit count and summed damage, got %q", got)
	}
}

// Nothing goes out before the window closes (bounded latency, no per-hit sends).
func TestHitfeedHoldsHitsUntilWindowCloses(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow - time.Second)
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatal("expected no message while the window is still open")
	}
	f.clock.advance(time.Second)
	p.tick(false)
	if f.sender.total() != 1 {
		t.Fatalf("expected the card once the window closed, got %d", f.sender.total())
	}
}

// A different weapon is a different encounter card.
func TestHitfeedSplitsEncountersByWeapon(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	a, b := fullHit("Alice", "Bob"), fullHit("Alice", "Bob")
	b.Weapon = "Mosin"
	p.PublishHit(a)
	p.PublishHit(b)
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	msgs := f.sender.messages("hit-chan")
	if len(msgs) != 1 || len(msgs[0].embeds) != 2 {
		t.Fatalf("expected 2 cards batched in 1 message, got %+v", msgs)
	}
}

// A flood of distinct encounters is bounded on every axis: open encounters are
// capped, the backlog is capped, and the channel gets at most one message of at
// most ten cards per tick.
func TestHitfeedFloodIsBoundedAndRateLimited(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)

	// 1000 hits from 300 different attackers: only hitfeedMaxOpen encounters are kept.
	for i := 0; i < 1000; i++ {
		p.PublishHit(fullHit(fmt.Sprintf("P%d", i%300), "Victim"))
	}
	p.mu.Lock()
	open, droppedOpen := len(p.open), p.droppedOpen
	p.mu.Unlock()
	if open != hitfeedMaxOpen || droppedOpen == 0 {
		t.Fatalf("expected open encounters capped at %d with overflow counted, got open=%d dropped=%d", hitfeedMaxOpen, open, droppedOpen)
	}

	f.clock.advance(hitfeedWindow)
	for tick := 1; tick <= 25; tick++ {
		p.tick(false)
		msgs := f.sender.messages("hit-chan")
		for _, m := range msgs {
			if len(m.embeds) > hitfeedEmbedsPerMessage {
				t.Fatalf("a message carried %d cards, max is %d", len(m.embeds), hitfeedEmbedsPerMessage)
			}
		}
		if len(msgs) > tick {
			t.Fatalf("tick %d produced more than one message per tick (total %d)", tick, len(msgs))
		}
	}
	// The backlog cap (100) bounds the total ever sent, however hot the fight was.
	total := 0
	for _, m := range f.sender.messages("hit-chan") {
		total += len(m.embeds)
	}
	if total != hitfeedMaxReady {
		t.Fatalf("expected the backlog cap of %d cards to bound output, got %d", hitfeedMaxReady, total)
	}
	p.mu.Lock()
	droppedReady := p.droppedReady
	p.mu.Unlock()
	if droppedReady != int64(hitfeedMaxOpen-hitfeedMaxReady) {
		t.Fatalf("expected %d oldest backlog cards dropped and counted, got %d", hitfeedMaxOpen-hitfeedMaxReady, droppedReady)
	}
}

// --- F: route change ---------------------------------------------------------

// A changed HITFEED route is used on the next tick with no restart, including
// for an encounter that was already open; removing the route stops publishing.
func TestHitfeedRouteChangeUsesNewChannel(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "chan-old")
	p := f.publisher(1)

	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	if len(f.sender.messages("chan-old")) != 1 {
		t.Fatal("setup: expected a card in chan-old")
	}

	// Customer changes the route; an encounter is already open.
	p.PublishHit(fullHit("Carol", "Dave"))
	f.res.set(7, 1, "HITFEED", "chan-new")
	p.tick(false) // picks up the new route
	f.clock.advance(hitfeedWindow)
	p.tick(false)

	if len(f.sender.messages("chan-old")) != 1 || len(f.sender.messages("chan-new")) != 1 {
		t.Fatalf("expected the new channel used after the change, got %+v", f.sender.sent)
	}

	// Route removed: hits stop being published, buffered ones are dropped.
	f.res.clear(7, 1, "HITFEED")
	p.tick(false)
	p.PublishHit(fullHit("Erin", "Frank"))
	f.clock.advance(time.Minute)
	p.tick(false)
	if f.sender.total() != 2 {
		t.Fatalf("expected publishing to stop with the route, total=%d", f.sender.total())
	}
}

// --- G: Discord failure ------------------------------------------------------

// A Discord send failure is logged and swallowed; later hits still go out once
// Discord is back. Nothing propagates to the caller (the ADM worker).
func TestHitfeedDiscordFailureDoesNotPropagateAndRecovers(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	f.sender.fail["hit-chan"] = errors.New("discord 500")
	p := f.publisher(1)

	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow)
	p.tick(false) // must not panic or block
	p.mu.Lock()
	failed := p.failedSends
	p.mu.Unlock()
	if failed != 1 || f.sender.total() != 0 {
		t.Fatalf("expected one counted failed send and no delivery, failed=%d sent=%d", failed, f.sender.total())
	}

	delete(f.sender.fail, "hit-chan")
	p.PublishHit(fullHit("Carol", "Dave"))
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	if len(f.sender.messages("hit-chan")) != 1 {
		t.Fatalf("expected delivery after Discord recovered, got %+v", f.sender.sent)
	}
}

// Even a panic inside the Discord client cannot escape the feed's goroutine.
func TestHitfeedSurvivesPanickingSender(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	f.sender.panicOn = "hit-chan"
	p := f.publisher(1)
	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow)
	p.safeTick(false) // recovered, no panic

	f.sender.panicOn = ""
	p.PublishHit(fullHit("Carol", "Dave"))
	f.clock.advance(hitfeedWindow)
	p.safeTick(false)
	if len(f.sender.messages("hit-chan")) != 1 {
		t.Fatalf("expected the feed to keep working after a panic, got %+v", f.sender.sent)
	}
}

// PublishHit itself never blocks on Discord: a hung sender does not delay it.
func TestHitfeedPublishHitNeverTouchesDiscord(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	f.sender.mu.Lock() // the sender is "hung": any send would deadlock this test
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			p.PublishHit(fullHit("Alice", "Bob"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("PublishHit blocked on the Discord sender")
	}
	f.sender.mu.Unlock()
}

// --- H: no kill cards ---------------------------------------------------------

// Kills (and every non-hit event) are ignored: the HITFEED never renders a kill.
func TestHitfeedIgnoresKillsAndOtherEvents(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)

	kill := fullHit("Alice", "Bob")
	kill.Type = killfeed.EventPlayerKill
	kill.Killer = kill.Attacker
	death := &killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: "Bob"}}
	p.PublishHit(kill)
	p.PublishHit(death)
	p.PublishHit(nil)
	f.clock.advance(time.Minute)
	p.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("kills/deaths must never be published to the hit feed, got %+v", f.sender.sent)
	}
}

// The lethal hit line (victim already DEAD) is still just a hit card.
func TestHitfeedLethalHitRendersAsHitNotKill(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	ev := fullHit("Alice", "Bob")
	ev.Dead = true
	p.PublishHit(ev)
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	got := strings.ToLower(desc(f.sender.messages("hit-chan")[0], 0))
	if !strings.Contains(got, "hit") || strings.Contains(got, "kill") || strings.Contains(got, "dead") || strings.Contains(got, "eliminat") {
		t.Fatalf("a lethal hit must render as a plain hit card, got %q", got)
	}
}

// --- I: missing optional fields -----------------------------------------------

func TestHitfeedMissingOptionalFieldsAreOmittedCleanly(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)

	p.PublishHit(hitEvent("Alice", "Bob")) // only who-hit-whom
	f.clock.advance(hitfeedWindow)
	p.tick(false)

	got := desc(f.sender.messages("hit-chan")[0], 0)
	if got != "🎯 **HIT**  Alice  ➜  Bob\n1 hit" {
		t.Fatalf("expected a minimal valid card, got %q", got)
	}
	for _, banned := range []string{"unknown", "Unknown", " · ", "dmg", "0m"} {
		if strings.Contains(got, banned) {
			t.Fatalf("a missing field must be omitted, not filled in (%q found): %q", banned, got)
		}
	}
}

// Damage is only shown when every hit in the card carried it - a partial sum
// is never presented as the total.
func TestHitfeedPartialDamageIsNotShown(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	p.PublishHit(fullHit("Alice", "Bob"))
	noDamage := fullHit("Alice", "Bob")
	noDamage.Damage = nil
	p.PublishHit(noDamage)
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	got := desc(f.sender.messages("hit-chan")[0], 0)
	if strings.Contains(got, "dmg") || !strings.Contains(got, "2 hits") {
		t.Fatalf("expected a hit count without a misleading damage total, got %q", got)
	}
}

// HP and coordinates are parsed but never rendered; names cannot ping.
func TestHitfeedDoesNotShowHPCoordinatesOrMentions(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	ev := fullHit("@everyone", "#general")
	hp := 42.5
	ev.HP = &hp
	ev.Attacker.Position = &killfeed.Position{X: 1234.5, Y: 6789.5, Z: 1}
	p.PublishHit(ev)
	f.clock.advance(hitfeedWindow)
	p.tick(false)
	got := desc(f.sender.messages("hit-chan")[0], 0)
	if strings.Contains(got, "@") || strings.Contains(got, "#") {
		t.Fatalf("mention/channel characters must be stripped: %q", got)
	}
	if strings.Contains(got, "42.5") || strings.Contains(got, "1234") || strings.Contains(got, "6789") {
		t.Fatalf("HP and coordinates must not be shown: %q", got)
	}
}

// --- lifecycle ----------------------------------------------------------------

// Run activates the feed at startup and flushes still-open encounters when the
// worker stops.
func TestHitfeedRunFlushesOpenEncountersOnShutdown(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := NewHitfeedPublisher(f.sender, f.res, 7, 1)
	p.now = f.clock.now

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
	p.PublishHit(fullHit("Alice", "Bob")) // window still open
	cancel()
	<-done
	if len(f.sender.messages("hit-chan")) != 1 {
		t.Fatalf("expected the open encounter flushed on shutdown, got %+v", f.sender.sent)
	}
}

func TestHitfeedNilSafety(t *testing.T) {
	var p *HitfeedPublisher
	p.PublishHit(fullHit("A", "B"))
	p.Run(context.Background())
	if NewHitfeedPublisher(nil, nil, 7, 1) == nil {
		t.Fatal("constructor must tolerate nil deps")
	}
	q := NewHitfeedPublisher(nil, nil, 7, 1)
	q.PublishHit(fullHit("A", "B")) // inactive: no-op
	q.tick(true)
}
