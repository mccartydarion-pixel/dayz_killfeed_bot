package discord

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// stubCustomizer stands in for the renderer: it records what a publisher passes and
// returns a marker card (or the default when asked to).
type stubCustomizer struct {
	mu       sync.Mutex
	calls    []stubCall
	passDflt bool
}

type stubCall struct {
	guild, server int64
	route         string
	vars          map[string]string
}

func (s *stubCustomizer) Customize(_ context.Context, guild, server int64, route string, vars map[string]string, _ time.Time, def *discordgo.MessageEmbed) *discordgo.MessageEmbed {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := map[string]string{}
	for k, v := range vars {
		cp[k] = v
	}
	s.calls = append(s.calls, stubCall{guild, server, route, cp})
	if s.passDflt {
		return def
	}
	return &discordgo.MessageEmbed{Description: "CUSTOM " + route, Color: 7}
}

func (s *stubCustomizer) last() stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return stubCall{}
	}
	return s.calls[len(s.calls)-1]
}

func (s *stubCustomizer) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }

func names(id int64) string { return map[int64]string{1: "Northstar", 2: "Southgate"}[id] }

func killEvent() *killfeed.Event {
	d, streak := 86.44, 3
	return &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "Alice", ID: "a"}, Victim: &killfeed.PlayerRef{Name: "Bob", ID: "b"},
		Weapon: "M4-A1", Distance: &d, Ammo: "Bullet_556x45", KillerStreak: &streak}
}

func TestRouteKeysMatchTheTemplateRoutes(t *testing.T) {
	for _, k := range []string{routeKeyKillfeed, routeKeyHitfeed, routeKeyPveFeed, routeKeyConnections, routeKeyBountyTracking, routeKeyEconomy} {
		if !embedtemplates.ValidRoute(k) {
			t.Errorf("%s is not a template route", k)
		}
	}
}

// --- KILLFEED ---------------------------------------------------------------------------------

func TestKillCardIsTheDefaultWithoutACustomizer(t *testing.T) {
	p := &KillfeedPublisher{}
	p.SetRouting(nil, 7, 1)
	ev := killEvent()
	if !reflect.DeepEqual(p.killCard(ev), BuildKillEmbed(ev)) {
		t.Fatal("with no customizer the card must be exactly the existing Champion kill card")
	}
	// nil publisher pieces never panic.
	var np *KillfeedPublisher
	np.SetCustomizer(nil, "x")
}

func TestKillCardUsesTheCustomCardAndProvenVariables(t *testing.T) {
	stub := &stubCustomizer{}
	p := &KillfeedPublisher{}
	p.SetRouting(nil, 7, 1)
	p.SetCustomizer(stub, "Northstar")
	card := p.killCard(killEvent())
	if card.Description != "CUSTOM KILLFEED" {
		t.Fatalf("expected the custom card: %+v", card)
	}
	c := stub.last()
	if c.guild != 7 || c.server != 1 || c.route != "KILLFEED" {
		t.Fatalf("the installation is resolved from this worker's (guild, server): %+v", c)
	}
	want := map[string]string{"killer": "Alice", "victim": "Bob", "weapon": "M4-A1", "distance": "86.4m", "ammo": "Bullet_556x45", "streak": "3", "server_name": "Northstar"}
	for k, v := range want {
		if c.vars[k] != v {
			t.Errorf("%s = %q, want %q", k, c.vars[k], v)
		}
	}
	for k := range c.vars {
		if _, ok := want[k]; !ok && k != "timestamp" {
			t.Errorf("unexpected variable %q (only authoritative values may be passed)", k)
		}
	}
}

func TestKillVariablesAreAbsentWhenUnknown(t *testing.T) {
	stub := &stubCustomizer{}
	p := &KillfeedPublisher{}
	p.SetRouting(nil, 7, 1)
	p.SetCustomizer(stub, "")
	ev := &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "@@@"}, Victim: &killfeed.PlayerRef{Name: "Bob"}}
	p.killCard(ev)
	v := stub.last().vars
	for _, k := range []string{"weapon", "distance", "ammo", "streak", "server_name"} {
		if _, ok := v[k]; ok {
			t.Errorf("%s must be absent, not fabricated: %q", k, v[k])
		}
	}
	if v["killer"] != "Unknown" {
		t.Errorf("a name with nothing usable falls back to Unknown like the default card: %q", v["killer"])
	}
	zero := 0
	ev.KillerStreak = &zero
	p.killCard(ev)
	if _, ok := stub.last().vars["streak"]; ok {
		t.Error("a zero streak is not shown")
	}
}

type failingSource struct{}

func (failingSource) ResolveTemplate(context.Context, int64, int64, string) (int64, *embedtemplates.Config, error) {
	return 0, nil, errors.New("db down")
}

func TestKillCardFallsBackToTheDefaultWhenTheRendererFails(t *testing.T) {
	r := embedrender.New(embedrender.Options{Source: failingSource{}, Enabled: true})
	p := &KillfeedPublisher{}
	p.SetRouting(nil, 7, 1)
	p.SetCustomizer(r, "Northstar")
	ev := killEvent()
	if !reflect.DeepEqual(p.killCard(ev), BuildKillEmbed(ev)) {
		t.Fatal("a failed template lookup must publish the existing killfeed card")
	}
	if r.Stats().FallbackRender != 1 {
		t.Fatalf("stats: %+v", r.Stats())
	}
}

func TestKillCardNeedsAResolvedInstallationIdentity(t *testing.T) {
	stub := &stubCustomizer{}
	p := &KillfeedPublisher{}
	p.SetCustomizer(stub, "x") // no routing identity -> never customizes
	ev := killEvent()
	if !reflect.DeepEqual(p.killCard(ev), BuildKillEmbed(ev)) || stub.count() != 0 {
		t.Fatal("without a (guild, server) identity the default is used and the renderer is not consulted")
	}
}

// --- HITFEED ----------------------------------------------------------------------------------

func TestHitfeedCustomCardsKeepAggregationAndBatching(t *testing.T) {
	run := func(withCustom bool) (msgs, cards int, stub *stubCustomizer, f *hitFixture) {
		f = newHitFixture()
		f.res.set(7, 1, "HITFEED", "hit-chan")
		p := f.publisher(1)
		if withCustom {
			stub = &stubCustomizer{}
			p.SetCustomizer(stub, names)
		}
		// 12 distinct encounters; one of them has 5 hits (aggregated into one card).
		for i := 0; i < 12; i++ {
			p.PublishHit(fullHit("A"+string(rune('a'+i)), "Victim"))
		}
		for i := 0; i < 4; i++ {
			p.PublishHit(fullHit("Aa", "Victim"))
		}
		f.clock.advance(hitfeedWindow + time.Second)
		p.tick(false)
		for _, m := range f.sender.messages("hit-chan") {
			msgs++
			cards += len(m.embeds)
		}
		return
	}
	dm, dc, _, _ := run(false)
	cm, cc, stub, f := run(true)
	if dm != cm || dc != cc || cm != 1 || cc != hitfeedEmbedsPerMessage {
		t.Fatalf("custom rendering must not change batching/rate limits: default %d msgs/%d cards, custom %d/%d", dm, dc, cm, cc)
	}
	for _, e := range f.sender.messages("hit-chan")[0].embeds {
		if e.Description != "CUSTOM HITFEED" {
			t.Fatalf("every card in the message is rendered independently: %+v", e)
		}
	}
	if stub.count() != hitfeedEmbedsPerMessage {
		t.Fatalf("one render per card actually sent, got %d", stub.count())
	}
	if m := f.sender.messages("hit-chan")[0]; m.mention == nil || len(m.mention.Parse) != 0 {
		t.Fatal("AllowedMentions must stay empty")
	}
}

func TestHitfeedVariablesAreProvenValuesOnly(t *testing.T) {
	f := newHitFixture()
	f.res.set(7, 1, "HITFEED", "hit-chan")
	p := f.publisher(1)
	stub := &stubCustomizer{}
	p.SetCustomizer(stub, names)
	p.PublishHit(fullHit("Alice", "Bob"))
	f.clock.advance(hitfeedWindow + time.Second)
	p.tick(false)
	v := stub.last().vars
	want := map[string]string{"attacker": "Alice", "killer": "Alice", "victim": "Bob", "weapon": "M4-A1", "ammo": "556x45", "distance": "42m", "hit_zone": "Torso", "damage": "28", "hits": "1", "server_name": "Northstar"}
	for k, val := range want {
		if v[k] != val {
			t.Errorf("%s = %q, want %q", k, v[k], val)
		}
	}
	if stub.last().server != 1 || stub.last().guild != 7 {
		t.Fatalf("identity: %+v", stub.last())
	}

	// Damage is never fabricated: a hit without a value leaves it absent, and a partial
	// sum is not presented as the total.
	f2 := newHitFixture()
	f2.res.set(7, 1, "HITFEED", "c")
	p2 := f2.publisher(1)
	stub2 := &stubCustomizer{}
	p2.SetCustomizer(stub2, nil)
	a := hitEvent("Al", "Bo") // no weapon/ammo/zone/distance/damage
	p2.PublishHit(a)
	b := fullHit("Al", "Bo")
	b.Weapon = "" // same key requires equal weapon: keep the encounter, mix damage presence
	p2.PublishHit(b)
	f2.clock.advance(hitfeedWindow + time.Second)
	p2.tick(false)
	v2 := stub2.last().vars
	if _, ok := v2["damage"]; ok {
		t.Errorf("damage must be absent when not every hit carried it: %v", v2["damage"])
	}
	for _, k := range []string{"server_name"} {
		if _, ok := v2[k]; ok {
			t.Errorf("%s must be absent with no server name", k)
		}
	}
}

// --- PVE_FEED ---------------------------------------------------------------------------------

func TestPveFeedCustomCardKeepsOwnershipAndTheOmittedNote(t *testing.T) {
	f := newPveFixture()
	f.res.set(7, 1, "PVE_FEED", "pve-chan")
	p := f.publisher(1)
	stub := &stubCustomizer{}
	p.SetCustomizer(stub, names)
	if !p.PublishPveDeath(suicide("Alice")) {
		t.Fatal("the feed's claim decision is unchanged by a template")
	}
	// An unproven cause is still never claimed, custom template or not.
	if p.PublishPveDeath(killfeed.PveDeathNotice{Cause: killfeed.DeathCause("UNKNOWN"), Name: "X"}) {
		t.Fatal("a template must not change which deaths belong to PVE_FEED")
	}
	p.notShown = 2
	p.tick(false)
	msgs := f.sender.messages("pve-chan")
	if len(msgs) != 1 || msgs[0].embeds[0].Description != "CUSTOM PVE_FEED\n… 2 earlier PvE deaths were not shown" {
		t.Fatalf("unexpected message: %+v", msgs)
	}
	c := stub.last()
	if c.vars["victim"] != "Alice" || c.vars["cause"] != "suicide" || c.vars["server_name"] != "Northstar" || c.route != "PVE_FEED" {
		t.Fatalf("pve variables: %+v", c)
	}
	if _, ok := c.vars["killer"]; ok {
		t.Fatal("no attacker exists for a PvE death")
	}
}

// With no route nothing is claimed - the customizer is never even consulted.
func TestPveFeedNoRouteNeverConsultsTheCustomizer(t *testing.T) {
	f := newPveFixture()
	p := f.publisher(1)
	stub := &stubCustomizer{}
	p.SetCustomizer(stub, names)
	if p.PublishPveDeath(suicide("Alice")) {
		t.Fatal("no route, no claim")
	}
	p.tick(false)
	if stub.count() != 0 || f.sender.total() != 0 {
		t.Fatal("nothing published, nothing customized")
	}
}

// --- CONNECTIONS ------------------------------------------------------------------------------

func TestConnectionsSingleEventUsesTheTemplateAndBatchesKeepTheDefault(t *testing.T) {
	f := newConnFixture()
	f.res.set(7, 1, "CONNECTIONS", "conn-chan")
	p := f.publisher(1)
	stub := &stubCustomizer{}
	p.SetCustomizer(stub, names)

	p.PublishConnection(disconnected("Alice", 42*time.Minute))
	p.tick(false)
	if got := f.lastDescription("conn-chan"); got != "CUSTOM CONNECTIONS" {
		t.Fatalf("a lone event uses the template: %q", got)
	}
	v := stub.last().vars
	if v["player"] != "Alice" || v["event"] != "left" || v["event_type"] != "disconnected" || v["session"] != "42m" || v["server_name"] != "Northstar" {
		t.Fatalf("connection variables: %+v", v)
	}

	p.PublishConnection(connected("Bob"))
	p.tick(false)
	if v := stub.last().vars; v["event"] != "joined" || v["event_type"] != "connected" {
		t.Fatalf("connect variables: %+v", v)
	}
	if _, ok := stub.last().vars["session"]; ok {
		t.Fatal("a connect has no session")
	}

	before := stub.count()
	p.PublishConnection(connected("C1"))
	p.PublishConnection(connected("C2"))
	p.tick(false)
	got := f.lastDescription("conn-chan")
	if !strings.Contains(got, "C1") || !strings.Contains(got, "C2") || strings.HasPrefix(got, "CUSTOM") {
		t.Fatalf("a multi-player batch is a summary list and keeps the default card: %q", got)
	}
	if stub.count() != before {
		t.Fatal("the customizer is not consulted for a batch")
	}
	// A disconnect whose session is unknown or short leaves {{session}} absent.
	p.PublishConnection(disconnected("D", 10*time.Second))
	p.tick(false)
	if _, ok := stub.last().vars["session"]; ok {
		t.Fatal("a session under a minute is not shown")
	}
}

// --- BOUNTY_TRACKING / ECONOMY ------------------------------------------------------------------

func TestBountyTrackingCardsUseTheTemplatePerServerAndEventKind(t *testing.T) {
	res := newKeyedResolver()
	res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	res.set(7, 2, "BOUNTY_TRACKING", "track-2")
	sender := &fakeHitSender{fail: map[string]error{}}
	tr := NewBountyTracker(sender, res, func(context.Context) (int64, []int64, error) { return 7, []int64{1, 2}, nil })
	stub := &stubCustomizer{}
	tr.SetCustomizer(stub, names)

	d := 120.0
	tr.Notify(bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 1, Target: "Bob", Hunter: "Alice", Amount: 250000, Count: 2, Weapon: "M4-A1", Distance: &d})
	tr.Flush()
	if got := sender.messages("track-1"); len(got) != 1 || got[0].embeds[0].Description != "CUSTOM BOUNTY_TRACKING" {
		t.Fatalf("claim on server 1: %+v", got)
	}
	if len(sender.messages("track-2")) != 0 {
		t.Fatal("a claim goes only to the server the kill happened on")
	}
	c := stub.last()
	want := map[string]string{"status": "claimed", "target": "Bob", "victim": "Bob", "hunter": "Alice", "killer": "Alice", "amount": "250,000", "total": "250,000", "count": "2", "weapon": "M4-A1", "distance": "120m", "server_name": "Northstar"}
	for k, v := range want {
		if c.vars[k] != v {
			t.Errorf("%s = %q, want %q", k, c.vars[k], v)
		}
	}
	if c.server != 1 || c.guild != 7 {
		t.Fatalf("identity: %+v", c)
	}

	// A guild-wide event reaches every server's route; each card uses ITS server's identity.
	tr.Notify(bounties.Event{Kind: bounties.EventExpired, GuildID: 7, Target: "Bob", Amount: 100})
	tr.Flush()
	servers := map[int64]bool{}
	for _, call := range stub.calls {
		if call.vars["status"] == "expired" {
			servers[call.server] = true
			for _, k := range []string{"hunter", "killer", "total", "count", "weapon", "distance"} {
				if _, ok := call.vars[k]; ok {
					t.Errorf("claim-only variable %s must be absent on an expiry", k)
				}
			}
		}
	}
	if !servers[1] || !servers[2] {
		t.Fatalf("each server's route is rendered with its own identity: %v", servers)
	}
}

func TestEconomyCardsUseTheTemplate(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "eco-chan")
	stub := &stubCustomizer{}
	f.feed.SetCustomizer(stub, names)
	f.feed.Notify(economy.Event{Type: economy.TypeBountyClaim, GuildID: 7, ServerID: 1, PlayerName: "Alice", Amount: 125000, Credit: true, BalanceAfter: 340000})
	f.feed.Notify(economy.Event{Type: economy.TypeAdminDebit, GuildID: 7, ServerID: 1, PlayerName: "Bob", Amount: 25000})
	f.feed.Flush()
	msgs := f.sender.messages("eco-chan")
	if len(msgs) != 1 || len(msgs[0].embeds) != 2 || msgs[0].embeds[0].Description != "CUSTOM ECONOMY" {
		t.Fatalf("economy cards: %+v", msgs)
	}
	first, second := stub.calls[0].vars, stub.calls[1].vars
	if first["player"] != "Alice" || first["amount"] != "125,000" || first["balance"] != "340,000" || first["transaction_type"] != "Bounty reward" || first["server_name"] != "Northstar" {
		t.Fatalf("reward variables: %+v", first)
	}
	if _, ok := second["balance"]; ok {
		t.Fatal("the balance is only known for rewards; it must be absent for a debit")
	}
	if second["transaction_type"] != "Admin debit" {
		t.Fatalf("debit variables: %+v", second)
	}
}

func TestPublishersUseTheDefaultWhenTheCustomizerReturnsIt(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "eco-chan")
	f.feed.SetCustomizer(&stubCustomizer{passDflt: true}, names)
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "Alice", Amount: 50000, Credit: true})
	f.feed.Flush()
	if got := f.last("eco-chan"); got != "➕ **ADMIN CREDIT**\nAlice received 50,000 pts" {
		t.Fatalf("the default card must be exactly the existing one: %q", got)
	}
}

// special_kill and bounty_amount come from the same classification the default card uses.
func TestKillVariablesCarrySpecialKillAndBountyOnlyWhenTheEventDoes(t *testing.T) {
	stub := &stubCustomizer{}
	p := &KillfeedPublisher{}
	p.SetRouting(nil, 7, 1)
	p.SetCustomizer(stub, "Northstar")

	p.Card(killEvent()) // an ordinary kill: no bounty, whatever the presentation says
	if _, ok := stub.last().vars["bounty_amount"]; ok {
		t.Fatal("no bounty was claimed: bounty_amount must be absent")
	}
	ev := killEvent()
	ev.BountyClaimed, ev.BountyPoints = true, 250000
	p.Card(ev)
	v := stub.last().vars
	if v["bounty_amount"] != "250,000" {
		t.Fatalf("bounty_amount: %q", v["bounty_amount"])
	}
	if pres := BuildPresentation(ev); pres.Hero != "" && v["special_kill"] != pres.Hero {
		t.Fatalf("special_kill must be the default card's own hero label %q, got %q", pres.Hero, v["special_kill"])
	}
	ev.BountyClaimed, ev.BountyPoints = false, 250000
	p.Card(ev)
	if _, ok := stub.last().vars["bounty_amount"]; ok {
		t.Fatal("points without a claim are not a bounty")
	}
}

// --- real renderer, publisher behavior compared with and without customization ------------------

type staticSource struct {
	cfg map[string]*embedtemplates.Config
}

func (s staticSource) ResolveTemplate(_ context.Context, _, _ int64, route string) (int64, *embedtemplates.Config, error) {
	return 9, s.cfg[route], nil
}

func mustCfg(t *testing.T, route string, c embedtemplates.Config) *embedtemplates.Config {
	t.Helper()
	out, err := embedtemplates.Validate(c, route)
	if err != nil {
		t.Fatal(err)
	}
	return &out
}

func realRenderer(t *testing.T, cfgs map[string]*embedtemplates.Config) *embedrender.Renderer {
	return embedrender.New(embedrender.Options{Source: staticSource{cfgs}, Enabled: true})
}

// Flood protection is independent of presentation: the same burst produces the same number
// of messages, cards and dropped encounters/backlog cards whether or not a template is used,
// and every delivered card is the custom one.
func TestHitfeedFloodProtectionIsIdenticalWithACustomTemplate(t *testing.T) {
	tmpl := mustCfg(t, "HITFEED", embedtemplates.Config{Enabled: true, Color: "#C98B3D",
		Title:  embedtemplates.Text{Enabled: true, Template: "HIT {{attacker}} > {{victim}}"},
		Fields: []embedtemplates.Field{{Key: "d", Label: "Damage", Enabled: true, Template: "{{damage}}", Order: 0}, {Key: "z", Label: "Zone", Enabled: true, Template: "{{hit_zone}}", Order: 1}}})
	run := func(custom bool) (msgs, cards int, droppedOpen, droppedReady int64, f *hitFixture) {
		f = newHitFixture()
		f.res.set(7, 1, "HITFEED", "hit-chan")
		p := f.publisher(1)
		if custom {
			p.SetCustomizer(realRenderer(t, map[string]*embedtemplates.Config{"HITFEED": tmpl}), names)
		}
		// hitfeedMaxOpen+50 distinct encounters in one window, then drain over several ticks.
		for i := 0; i < hitfeedMaxOpen+50; i++ {
			p.PublishHit(fullHit("A"+strconvI(i), "V"))
		}
		f.clock.advance(hitfeedWindow + time.Second)
		for tick := 0; tick < 30; tick++ {
			p.tick(false)
			f.clock.advance(hitfeedTick)
		}
		for _, m := range f.sender.messages("hit-chan") {
			msgs++
			cards += len(m.embeds)
			for _, e := range m.embeds {
				if len(e.Description) > 0 && custom && !strings.HasPrefix(e.Title, "HIT ") {
					t.Fatalf("a custom run delivered a default card: %+v", e)
				}
			}
		}
		return msgs, cards, p.droppedOpen, p.droppedReady, f
	}
	dm, dc, dOpen, dReady, _ := run(false)
	cm, cc, cOpen, cReady, f := run(true)
	if dm != cm || dc != cc || dOpen != cOpen || dReady != cReady {
		t.Fatalf("flood behavior must not depend on the template: default %d/%d drops %d/%d, custom %d/%d drops %d/%d", dm, dc, dOpen, dReady, cm, cc, cOpen, cReady)
	}
	if dOpen != 50 || dc != hitfeedMaxReady || cm > 10 {
		t.Fatalf("sanity: open-encounter cap and backlog cap still bite (dropped %d, cards %d, msgs %d)", dOpen, dc, cm)
	}
	for _, m := range f.sender.messages("hit-chan") {
		if len(m.embeds) > hitfeedEmbedsPerMessage || m.mention == nil || len(m.mention.Parse) != 0 {
			t.Fatalf("cards per message and mention safety are unchanged: %d %+v", len(m.embeds), m.mention)
		}
	}
}

func strconvI(i int) string {
	return strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

// Classification and claiming are decided before any presentation: the same notices are
// claimed, queued and ordered whether or not a PVE_FEED template exists.
func TestPveClassificationAndOrderUnchangedByTemplates(t *testing.T) {
	tmpl := mustCfg(t, "PVE_FEED", embedtemplates.Config{Enabled: true, Color: "#8C6A2D", Title: embedtemplates.Text{Enabled: true, Template: "PVE {{victim}} {{cause}}"}})
	drive := func(custom bool) (claims []bool, titles []string) {
		f := newPveFixture()
		f.res.set(7, 1, "PVE_FEED", "pve-chan")
		p := f.publisher(1)
		if custom {
			p.SetCustomizer(realRenderer(t, map[string]*embedtemplates.Config{"PVE_FEED": tmpl}), names)
		}
		for _, n := range []killfeed.PveDeathNotice{suicide("A"), {Cause: killfeed.DeathCause("UNKNOWN"), Name: "B"}, suicide("C"), {Cause: killfeed.DeathCauseInfected, Name: "D"}} {
			claims = append(claims, p.PublishPveDeath(n))
		}
		p.tick(false)
		for _, m := range f.sender.messages("pve-chan") {
			for _, e := range m.embeds {
				titles = append(titles, e.Title+"|"+e.Description)
			}
		}
		return
	}
	dClaims, dCards := drive(false)
	cClaims, cCards := drive(true)
	if !reflect.DeepEqual(dClaims, cClaims) || len(dCards) != len(cCards) {
		t.Fatalf("classification/claims/queue must be identical: %v %v vs %v %v", dClaims, dCards, cClaims, cCards)
	}
	if !cClaims[0] || cClaims[1] || !cClaims[2] {
		t.Fatalf("an unproven cause is never claimed: %v", cClaims)
	}
	if cCards[0] != "PVE A suicide|" || cCards[1] != "PVE C suicide|" {
		t.Fatalf("custom PVE cards, in order: %v", cCards)
	}
}
