package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// Staging-equivalent harness for immediate killfeed delivery (no live
// Discord, Railway or Nitrado is reachable from the verification
// environment). Everything below the Discord HTTP boundary is production
// code: ADM engine (parse, dedupe), persistence queue (ack before publish),
// RotatingFeed, FeedSession and the real discordgo client. Only Discord's
// REST API is emulated, including its enforce_nonce idempotency and the
// failure modes under test. All timings are SYNTHETIC; none is live
// production latency.

// ---- emulated Discord messages API ----------------------------------------

type fakeMessage struct {
	ID, Nonce string
	Embed     discordgo.MessageEmbed
}

type discordFault struct {
	status     int     // 0 = none
	code       int     // Discord JSON error code
	retryAfter float64 // 429 only (seconds)
	hang       bool    // create the message, then respond after the client timeout
}

type discordEmu struct {
	mu        sync.Mutex
	next      int
	channels  map[string][]fakeMessage // visible messages per channel, oldest first
	postFault map[string][]discordFault
	delFault  map[string][]discordFault
	posts     map[string]int // POST requests per channel (including faulted)
	creates   map[string]int // messages actually created
	nonces    map[string]string
	latency   func() time.Duration
	hangFor   time.Duration
}

func newDiscordEmu() *discordEmu {
	return &discordEmu{channels: map[string][]fakeMessage{}, postFault: map[string][]discordFault{}, delFault: map[string][]discordFault{},
		posts: map[string]int{}, creates: map[string]int{}, nonces: map[string]string{}, latency: func() time.Duration { return 0 }}
}

func (d *discordEmu) takeFault(m map[string][]discordFault, ch string) discordFault {
	if q := m[ch]; len(q) > 0 {
		m[ch] = q[1:]
		return q[0]
	}
	return discordFault{}
}

func writeDiscordError(w http.ResponseWriter, f discordFault) {
	w.Header().Set("Content-Type", "application/json")
	if f.status == 429 {
		w.Header().Set("Retry-After", strconv.FormatFloat(f.retryAfter, 'f', 3, 64))
		w.WriteHeader(429)
		_, _ = fmt.Fprintf(w, `{"message":"You are being rate limited.","retry_after":%g,"global":false}`, f.retryAfter)
		return
	}
	w.WriteHeader(f.status)
	_, _ = fmt.Fprintf(w, `{"code":%d,"message":"emulated"}`, f.code)
}

func (d *discordEmu) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v10/channels/"), "/")
	ch := parts[0]
	time.Sleep(d.latency())
	switch {
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "messages":
		var body struct {
			Embeds       []discordgo.MessageEmbed `json:"embeds"`
			Nonce        string                   `json:"nonce"`
			EnforceNonce bool                     `json:"enforce_nonce"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		d.posts[ch]++
		f := d.takeFault(d.postFault, ch)
		if f.status != 0 {
			d.mu.Unlock()
			writeDiscordError(w, f)
			return
		}
		// enforce_nonce: a repeated nonce returns the existing message.
		if id, ok := d.nonces[ch+"/"+body.Nonce]; ok && body.EnforceNonce && body.Nonce != "" {
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(discordgo.Message{ID: id, ChannelID: ch})
			return
		}
		d.next++
		id := strconv.Itoa(d.next)
		d.channels[ch] = append(d.channels[ch], fakeMessage{ID: id, Nonce: body.Nonce, Embed: body.Embeds[0]})
		d.creates[ch]++
		if body.Nonce != "" {
			d.nonces[ch+"/"+body.Nonce] = id
		}
		hang := d.hangFor
		d.mu.Unlock()
		if f.hang || hang > 0 {
			time.Sleep(hang)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(discordgo.Message{ID: id, ChannelID: ch})
	case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "bulk-delete":
		var body struct {
			Messages []string `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		d.mu.Lock()
		if f := d.takeFault(d.delFault, ch); f.status != 0 {
			d.mu.Unlock()
			writeDiscordError(w, f)
			return
		}
		d.remove(ch, body.Messages...)
		d.mu.Unlock()
		w.WriteHeader(204)
	case r.Method == http.MethodDelete && len(parts) == 3:
		d.mu.Lock()
		if f := d.takeFault(d.delFault, ch); f.status != 0 {
			d.mu.Unlock()
			writeDiscordError(w, f)
			return
		}
		d.remove(ch, parts[2])
		d.mu.Unlock()
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

func (d *discordEmu) remove(ch string, ids ...string) {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	keep := d.channels[ch][:0]
	for _, m := range d.channels[ch] {
		if !drop[m.ID] {
			keep = append(keep, m)
		}
	}
	d.channels[ch] = keep
}

func (d *discordEmu) titles(ch string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, m := range d.channels[ch] {
		out = append(out, m.Embed.Title)
	}
	return out
}

func (d *discordEmu) count(m map[string]int, ch string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return m[ch]
}

// newEmuSession returns a real discordgo session; channel requests reach the
// emulator through the test dispatcher (see testChannel).
func newEmuSession(t *testing.T, _ *discordEmu, clientTimeout time.Duration) *discordgo.Session {
	t.Helper()
	return newTestSession(t, clientTimeout)
}

// ---- pipeline under test ----------------------------------------------------

type harness struct {
	kfID, dfID   string
	emu          *discordEmu
	src          *e2eADM
	engine       *killfeed.Engine
	kf, df       *RotatingFeed
	kills, death *feedAdapter
	available    map[string]time.Time
	store        *okStore
	cancel       context.CancelFunc
}

func newHarness(t *testing.T, mode string, interval time.Duration, emu *discordEmu, clientTimeout time.Duration, route http.Handler) *harness {
	t.Helper()
	freshLedger(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newEmuSession(t, emu, clientTimeout)
	api := NewFeedSession(s)
	if route == nil {
		route = emu
	}
	kfID, dfID := testChannel(t, route, "kf"), testChannel(t, route, "df")
	setup := NewInMemorySetupStore()
	_ = setup.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: kfID, DeathChannelID: dfID})
	h := &harness{kfID: kfID, dfID: dfID, emu: emu, src: &e2eADM{content: "AdminLog started on 2026-09-24 at 16:00:00\n", mtime: time.Now()}, available: map[string]time.Time{}, store: &okStore{}, cancel: cancel}
	h.engine = killfeed.NewEngine(h.src, "svc", killfeed.NewADMParser())
	pq := killfeed.NewPersistenceQueueWithServerID(h.store, 1, 1, "svc")
	go pq.Run(ctx)
	h.engine.SetPersistence(pq)
	h.kf = NewRotatingFeed(api, setup, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, interval, 10)
	h.df = NewRotatingFeed(api, setup, "g1", func(s *GuildSetup) string { return s.DeathChannelID }, interval, 10)
	h.df.SetRoute("DEATH_FEED")
	h.kf.SetMode(mode)
	h.df.SetMode(mode)
	mk := func(f *RotatingFeed) *feedAdapter {
		return &feedAdapter{feed: f, sent: map[string]*discordgo.MessageEmbed{}, detected: map[string]time.Time{}, enqueued: map[string]time.Time{}}
	}
	h.kills, h.death = mk(h.kf), mk(h.df)
	h.engine.SetKillPublisher(killAdapter{h.kills})
	h.engine.SetDeathPublisher(deathAdapter{h.death})
	go h.kf.Run(ctx)
	go h.df.Run(ctx)
	t.Cleanup(func() { cancel(); h.kf.WaitDone(); h.df.WaitDone() })
	for i := 0; i < 2; i++ {
		if err := h.engine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// fixture event: an ADM line and the card title it must produce ("" for a
// duplicate/replay that must NOT produce a card).
type fixtureEvent struct{ line, title string }

var fixtureClock = time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)

func killLine(i int, at time.Time) fixtureEvent {
	v := fmt.Sprintf("Victim%03d", i)
	return fixtureEvent{fmt.Sprintf(`%s | Player "%s" (DEAD) (id=v%03d pos=<1.0, 2.0, 3.0>) killed by Player "Killer%d" (id=k%d pos=<4.0, 5.0, 6.0>) with M4-A1 from %d.0 meters`+"\n",
		at.Format("15:04:05"), v, i, i%7, i%7, 20+i), "kill:" + v}
}

func deathLine(i int, at time.Time) fixtureEvent {
	p := fmt.Sprintf("Starved%03d", i)
	return fixtureEvent{fmt.Sprintf(`%s | Player "%s" (DEAD) (id=s%03d pos=<7000.0, 1200.0, 8.0>) died. Stats> Water: 0 Energy: 0 Bleed sources: 0`+"\n",
		at.Format("15:04:05"), p, i), "death:" + p}
}

// buildFixture returns bursts of ADM lines: kills and deaths, rapid
// consecutive kills in one burst, in-burst duplicates, and later replays of
// earlier records. Deterministic for a seed.
func buildFixture(seed int64, events int) [][]fixtureEvent {
	r := rand.New(rand.NewSource(seed))
	var bursts [][]fixtureEvent
	var history []fixtureEvent
	clock := fixtureClock
	for n := 0; n < events; {
		size := 1 + r.Intn(4) // rapid consecutive events in one ADM write
		var burst []fixtureEvent
		for j := 0; j < size && n < events; j++ {
			clock = clock.Add(time.Duration(1+r.Intn(20)) * time.Second)
			var ev fixtureEvent
			if r.Intn(4) == 0 {
				ev = deathLine(n, clock)
			} else {
				ev = killLine(n, clock)
			}
			burst = append(burst, ev)
			history = append(history, ev)
			n++
			if r.Intn(10) == 0 { // duplicate line in the same write
				burst = append(burst, fixtureEvent{line: ev.line})
			}
		}
		if len(history) > 3 && r.Intn(6) == 0 { // replayed ADM record
			burst = append(burst, fixtureEvent{line: history[r.Intn(len(history)-1)].line})
		}
		bursts = append(bursts, burst)
	}
	return bursts
}

func (h *harness) feed(t *testing.T, burst []fixtureEvent) {
	t.Helper()
	var b strings.Builder
	for _, ev := range burst {
		b.WriteString(ev.line)
	}
	at := h.src.grow(b.String())
	for _, ev := range burst {
		if ev.title != "" {
			h.available[ev.title] = at
		}
	}
	if err := h.engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func expectedTitles(bursts [][]fixtureEvent) (kills, deaths []string) {
	for _, b := range bursts {
		for _, ev := range b {
			switch {
			case strings.HasPrefix(ev.title, "kill:"):
				kills = append(kills, ev.title)
			case strings.HasPrefix(ev.title, "death:"):
				deaths = append(deaths, ev.title)
			}
		}
	}
	return
}

// ---- latency ---------------------------------------------------------------

type stageSamples struct{ detection, persistence, queue, discord, detectToPublish []time.Duration }

func pct(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s))*p+0.9999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// latencyRecorder wraps the emulator to timestamp each create per card title.
type latencyRecorder struct {
	emu     *discordEmu
	mu      sync.Mutex
	started map[string]time.Time
	ended   map[string]time.Time
}

func (l *latencyRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Embeds []discordgo.MessageEmbed `json:"embeds"`
		}
		_ = json.Unmarshal(raw, &body)
		title := ""
		if len(body.Embeds) > 0 {
			title = body.Embeds[0].Title
		}
		start := time.Now()
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		l.emu.ServeHTTP(w, r)
		l.mu.Lock()
		if _, seen := l.started[title]; !seen {
			l.started[title] = start
		}
		l.ended[title] = time.Now()
		l.mu.Unlock()
		return
	}
	l.emu.ServeHTTP(w, r)
}

// loadProfile paces bursts: normal is a busy-but-realistic server (about one
// event per second - above typical DayZ kill rates and below Discord's
// ~1 message/s sustained per-channel limit); stress is ~30 events/s, far
// beyond both, to show saturation behavior.
type loadProfile struct {
	name     string
	minGapMs int
	spanMs   int
}

var (
	profileNormal = loadProfile{"normal", 500, 3500}
	profileStress = loadProfile{"stress", 20, 120}
)

func runLatency(t *testing.T, mode string, interval time.Duration, bursts [][]fixtureEvent, seed int64, load loadProfile) (stageSamples, *harness) {
	t.Helper()
	emu := newDiscordEmu()
	r := rand.New(rand.NewSource(seed))
	var rmu sync.Mutex
	emu.latency = func() time.Duration { // 40-200ms per Discord call
		rmu.Lock()
		defer rmu.Unlock()
		return time.Duration(40+r.Intn(160)) * time.Millisecond
	}
	rec := &latencyRecorder{emu: emu, started: map[string]time.Time{}, ended: map[string]time.Time{}}
	h := newHarness(t, mode, interval, emu, 10*time.Second, rec)

	gap := rand.New(rand.NewSource(seed + 1))
	for _, b := range bursts {
		h.feed(t, b)
		time.Sleep(time.Duration(load.minGapMs+gap.Intn(load.spanMs)) * time.Millisecond)
	}
	wantK, wantD := expectedTitles(bursts)
	wait := 60 * time.Second
	if mode == FeedModeRotating {
		wait = 2*interval + 10*time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if emu.count(emu.creates, h.kfID) >= len(wantK) && emu.count(emu.creates, h.dfID) >= len(wantD) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let the last eviction deletes land: create-then-delete means the
	// channel briefly shows maxItems+1 cards.
	for settle := time.Now().Add(5 * time.Second); time.Now().Before(settle) && (len(emu.titles(h.kfID)) > 10 || len(emu.titles(h.dfID)) > 10); {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("%s: created kills=%d/%d deaths=%d/%d posts kf=%d df=%d pending kf=%d df=%d", mode,
		emu.count(emu.creates, h.kfID), len(wantK), emu.count(emu.creates, h.dfID), len(wantD),
		emu.count(emu.posts, h.kfID), emu.count(emu.posts, h.dfID), len(h.kf.pending), len(h.df.pending))
	var s stageSamples
	for _, a := range []*feedAdapter{h.kills, h.death} {
		a.mu.Lock()
		for title, det := range a.detected {
			rec.mu.Lock()
			start, ok := rec.started[title]
			end := rec.ended[title]
			rec.mu.Unlock()
			if !ok {
				continue
			}
			s.detection = append(s.detection, det.Sub(h.available[title]))
			s.persistence = append(s.persistence, a.enqueued[title].Sub(det))
			s.queue = append(s.queue, start.Sub(a.enqueued[title]))
			s.discord = append(s.discord, end.Sub(start))
			s.detectToPublish = append(s.detectToPublish, end.Sub(det))
		}
		a.mu.Unlock()
	}
	return s, h
}

// TestImmediateKillfeedStagingEquivalent is sections 3 and 4: the same
// ordered fixture through both modes under a normal and a stress load;
// percentiles per stage; exactly-once, ordering and independent routing.
// Runs only with KILLFEED_STAGING_HARNESS=1 (several minutes); CI keeps the
// faster TestKillfeedRotatingVersusImmediateEndToEnd.
func TestImmediateKillfeedStagingEquivalent(t *testing.T) {
	if os.Getenv("KILLFEED_STAGING_HARNESS") != "1" {
		t.Skip("set KILLFEED_STAGING_HARNESS=1 to run the staging-equivalent harness")
	}
	type result struct {
		load     loadProfile
		mode     string
		events   int
		kills    int
		deaths   int
		s        stageSamples
		created  int
		expected int
	}
	var results []result
	for _, load := range []struct {
		p      loadProfile
		events int
	}{{profileNormal, 60}, {profileStress, 120}} {
		bursts := buildFixture(42, load.events)
		wantK, wantD := expectedTitles(bursts)
		// Immediate runs with the PRODUCTION interval (10 min): its latency
		// does not depend on the cycle. Rotating cannot wait 10 real minutes
		// per cycle in a test, so its cycle is scaled to 3s.
		for _, run := range []struct {
			mode     string
			interval time.Duration
		}{{FeedModeImmediate, 10 * time.Minute}, {FeedModeRotating, 3 * time.Second}} {
			s, h := runLatency(t, run.mode, run.interval, bursts, 7, load.p)
			created := h.emu.count(h.emu.creates, h.kfID) + h.emu.count(h.emu.creates, h.dfID)
			results = append(results, result{load.p, run.mode, load.events, len(wantK), len(wantD), s, created, len(wantK) + len(wantD)})
			if run.mode != FeedModeImmediate {
				continue // rotating shows only the newest 10 per cycle by design (reported, not asserted)
			}
			t.Run(load.p.name+"/immediate exactly once, ordered, routed independently", func(t *testing.T) {
				if got := h.emu.count(h.emu.creates, h.kfID); got != len(wantK) {
					t.Fatalf("kills created %d, want exactly %d", got, len(wantK))
				}
				if got := h.emu.count(h.emu.creates, h.dfID); got != len(wantD) {
					t.Fatalf("deaths created %d, want exactly %d", got, len(wantD))
				}
				if got, want := h.emu.titles(h.kfID), wantK[len(wantK)-10:]; strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("killfeed channel\n got %v\nwant %v", got, want)
				}
				if got, want := h.emu.titles(h.dfID), wantD[max(0, len(wantD)-10):]; strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("deathfeed channel\n got %v\nwant %v", got, want)
				}
				for _, title := range h.emu.titles(h.kfID) {
					if !strings.HasPrefix(title, "kill:") {
						t.Fatalf("a %s card reached the killfeed channel", title)
					}
				}
				for _, title := range h.emu.titles(h.dfID) {
					if !strings.HasPrefix(title, "death:") {
						t.Fatalf("a %s card reached the deathfeed channel", title)
					}
				}
				h.store.mu.Lock()
				persisted := h.store.kills + h.store.deaths
				h.store.mu.Unlock()
				if persisted != len(wantK)+len(wantD) {
					t.Fatalf("persisted %d events, want %d (duplicates/replays dropped once)", persisted, len(wantK)+len(wantD))
				}
			})
		}
	}

	var b strings.Builder
	b.WriteString("\nSYNTHETIC measurements - emulated Discord (40-200ms per call), not live latency\n")
	fmt.Fprintf(&b, "%-7s | %-24s | %-27s | %8s | %8s | %8s | %8s\n", "load", "mode", "stage", "p50", "p95", "p99", "max")
	for _, r := range results {
		label := r.mode + fmt.Sprintf(" (%d/%d posted)", r.created, r.expected)
		if r.mode == FeedModeRotating {
			label = "rotating*" + fmt.Sprintf(" (%d/%d posted)", r.created, r.expected)
		}
		for _, st := range []struct {
			name string
			v    []time.Duration
		}{
			{"detection (avail->parsed)", r.s.detection},
			{"persistence (parsed->ack)", r.s.persistence},
			{"queue (ack->Discord call)", r.s.queue},
			{"Discord publication", r.s.discord},
			{"detect->published TOTAL", r.s.detectToPublish},
		} {
			fmt.Fprintf(&b, "%-7s | %-24s | %-27s | %8s | %8s | %8s | %8s\n", r.load.name, label, st.name, pct(st.v, .5).Round(time.Millisecond), pct(st.v, .95).Round(time.Millisecond), pct(st.v, .99).Round(time.Millisecond), pct(st.v, 1).Round(time.Millisecond))
		}
	}
	b.WriteString("* rotating: cycle scaled 10 min -> 3 s (production waits scale x200); posts only the newest 10 per cycle by design\n")
	t.Log(b.String())

	normal := results[0]
	if normal.mode != FeedModeImmediate || normal.load.name != "normal" {
		t.Fatal("unexpected result order")
	}
	if p := pct(normal.s.queue, .95); p >= 2*time.Second {
		t.Errorf("ACCEPTANCE FAIL (normal load): immediate queue p95 %s >= 2s", p)
	}
	if p := pct(normal.s.detectToPublish, .95); p >= 5*time.Second {
		t.Errorf("ACCEPTANCE FAIL (normal load): detect->publish p95 %s >= 5s", p)
	}
}
