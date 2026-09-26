package discord

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// End-to-end killfeed delivery comparison (P0 staging verification,
// section 6): real ADM lines -> real engine (parse, dedupe) -> real
// persistence queue (ack before publish) -> RotatingFeed in each mode -> a
// fake Discord with per-call latency. Each stage is timed separately.

type e2eADM struct {
	mu      sync.Mutex
	content string
	mtime   time.Time
}

func (a *e2eADM) ListLogs(context.Context, string) ([]nitrado.LogFile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return []nitrado.LogFile{{Name: "DayZServer_x64.ADM", Path: "/logs/DayZServer_x64.ADM", Size: int64(len(a.content)), Modified: a.mtime, Type: "ADM"}}, nil
}

func (a *e2eADM) ReadLog(context.Context, string, string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return []byte(a.content), nil
}

func (a *e2eADM) grow(lines string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.content += lines
	a.mtime = time.Now()
	return a.mtime
}

// okStore acks every durable write (the real queue still orders and acks).
type okStore struct {
	mu     sync.Mutex
	kills  int
	deaths int
}

func (s *okStore) UpsertPlayer(context.Context, int64, string, string, time.Time) (int64, error) {
	return 1, nil
}
func (s *okStore) InsertKill(context.Context, repository.KillRecord) error {
	s.mu.Lock()
	s.kills++
	s.mu.Unlock()
	return nil
}
func (s *okStore) InsertDeath(context.Context, repository.DeathRecord) error {
	s.mu.Lock()
	s.deaths++
	s.mu.Unlock()
	return nil
}

// feedAdapter does exactly what KillfeedPublisher/DeathfeedPublisher do with a
// feed (EnqueueDetected with the event's DetectedAt); the embed title encodes
// the event so ordering can be checked.
type feedAdapter struct {
	feed     *RotatingFeed
	mu       sync.Mutex
	sent     map[string]*discordgo.MessageEmbed
	detected map[string]time.Time // Event.DetectedAt (ADM line parsed)
	enqueued map[string]time.Time // durable write acked, card queued
}

func (p *feedAdapter) enqueue(title string, ev *killfeed.Event) {
	embed := &discordgo.MessageEmbed{Title: title, Description: "Champion card for " + title}
	p.mu.Lock()
	p.sent[title] = embed
	p.detected[title] = ev.DetectedAt
	p.enqueued[title] = time.Now()
	p.mu.Unlock()
	p.feed.EnqueueDetected(embed, ev.DetectedAt)
}

type killAdapter struct{ *feedAdapter }

func (k killAdapter) PublishKill(ev *killfeed.Event) error {
	k.enqueue("kill:"+ev.Victim.Name, ev)
	return nil
}

type deathAdapter struct{ *feedAdapter }

func (d deathAdapter) PublishDeath(ev *killfeed.Event) error {
	d.enqueue("death:"+ev.Player.Name, ev)
	return nil
}

// slowDiscord records every posted card with send start/end, 20ms per call.
type slowDiscord struct {
	mu     sync.Mutex
	posted []postedRecord
	next   int
}

type postedRecord struct {
	channel, title string
	embed          *discordgo.MessageEmbed
	start, end     time.Time
}

func (s *slowDiscord) ChannelMessagesBulkDelete(string, []string, ...discordgo.RequestOption) error {
	return nil
}
func (s *slowDiscord) ChannelMessageDelete(string, string, ...discordgo.RequestOption) error {
	return nil
}
func (s *slowDiscord) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	start := time.Now()
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.posted = append(s.posted, postedRecord{channel: channelID, title: data.Embeds[0].Title, embed: data.Embeds[0], start: start, end: time.Now()})
	return &discordgo.Message{ID: fmt.Sprint(s.next)}, nil
}

func (s *slowDiscord) snapshot() []postedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]postedRecord(nil), s.posted...)
}

const (
	e2eKill1 = `16:40:12 | Player "Victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "Killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1 meters` + "\n"
	e2eKill2 = `16:41:30 | Player "Victim2" (DEAD) (id=v2 pos=<1.0, 2.0, 3.0>) killed by Player "Killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 40.0 meters` + "\n"
	e2eKill3 = `16:43:05 | Player "Victim3" (DEAD) (id=v3 pos=<1.0, 2.0, 3.0>) killed by Player "Killer2" (id=k2 pos=<4.0, 5.0, 6.0>) with SVD from 310.5 meters` + "\n"
	e2eDeath = `16:44:00 | Player "Starved" (DEAD) (id=s1 pos=<7000.0, 1200.0, 8.0>) died. Stats> Water: 0 Energy: 0 Bleed sources: 0` + "\n"
)

type modeResult struct {
	mode                                     string
	order                                    []string
	deaths                                   []string
	detectMs, persistMs, queueMs, deliveryMs []int64 // per card, per stage
	persistedKills, persistedDead            int
	embedsIdentical                          bool
}

func runKillfeedMode(t *testing.T, mode string, interval time.Duration) modeResult {
	t.Helper()
	freshLedger(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &e2eADM{content: "AdminLog started on 2026-09-24 at 16:30:00\n", mtime: time.Now()}
	engine := killfeed.NewEngine(src, "svc", killfeed.NewADMParser())
	store := &okStore{}
	pq := killfeed.NewPersistenceQueueWithServerID(store, 1, 1, "svc")
	go pq.Run(ctx)
	engine.SetPersistence(pq)

	api := &slowDiscord{}
	setup := NewInMemorySetupStore()
	_ = setup.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "kf", DeathChannelID: "df"})
	kf := NewRotatingFeed(api, setup, "g1", func(s *GuildSetup) string { return s.KillfeedChannelID }, interval, 10)
	df := NewRotatingFeed(api, setup, "g1", func(s *GuildSetup) string { return s.DeathChannelID }, interval, 10)
	df.SetRoute("DEATH_FEED")
	kf.SetMode(mode)
	df.SetMode(mode)
	newAdapter := func(f *RotatingFeed) *feedAdapter {
		return &feedAdapter{feed: f, sent: map[string]*discordgo.MessageEmbed{}, detected: map[string]time.Time{}, enqueued: map[string]time.Time{}}
	}
	kills, deaths := newAdapter(kf), newAdapter(df)
	engine.SetKillPublisher(killAdapter{kills})
	engine.SetDeathPublisher(deathAdapter{deaths})
	go kf.Run(ctx)
	go df.Run(ctx)

	for i := 0; i < 2; i++ { // discovery + baseline read
		if err := engine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Source availability: the moment each line lands in the ADM file. The
	// engine is polled right after (production polls every 10s).
	available := map[string]time.Time{}
	grow := func(line, title string) {
		available[title] = src.grow(line)
		if err := engine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	grow(e2eKill1, "kill:Victim1")
	grow(e2eKill2+e2eKill1, "kill:Victim2") // e2eKill1 again: a replayed line
	time.Sleep(30 * time.Millisecond)
	grow(e2eKill3, "kill:Victim3")
	grow(e2eDeath, "death:Starved")

	want := 4
	deadline := time.Now().Add(5 * time.Second)
	for len(api.snapshot()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(interval / 2) // anything extra (a duplicate) would have been posted by now
	cancel()
	kf.WaitDone()
	df.WaitDone()

	res := modeResult{mode: mode, embedsIdentical: true}
	store.mu.Lock()
	res.persistedKills, res.persistedDead = store.kills, store.deaths
	store.mu.Unlock()
	ledger := map[string]RouteDelivery{}
	for _, r := range Deliveries.Snapshot() {
		ledger[r.Route] = r
	}
	for _, p := range api.snapshot() {
		if p.channel == "df" {
			res.deaths = append(res.deaths, p.title)
		} else {
			res.order = append(res.order, p.title)
		}
		a := kills
		if kills.sent[p.title] == nil {
			a = deaths
		}
		if a.sent[p.title] != p.embed {
			res.embedsIdentical = false
		}
		// Stages: source available -> parsed (detection) -> durable ack
		// (persistence) -> Discord call start (queue) -> call end (delivery).
		res.detectMs = append(res.detectMs, a.detected[p.title].Sub(available[p.title]).Milliseconds())
		res.persistMs = append(res.persistMs, a.enqueued[p.title].Sub(a.detected[p.title]).Milliseconds())
		res.queueMs = append(res.queueMs, p.start.Sub(a.enqueued[p.title]).Milliseconds())
		res.deliveryMs = append(res.deliveryMs, p.end.Sub(p.start).Milliseconds())
	}
	if ledger["KILLFEED"].LatencySamples != 3 || ledger["DEATH_FEED"].LatencySamples != 1 {
		t.Fatalf("ledger must record latency per route: %+v", ledger)
	}
	return res
}

func TestKillfeedRotatingVersusImmediateEndToEnd(t *testing.T) {
	const interval = 400 * time.Millisecond // production 10 min, scaled
	var results []modeResult
	for _, mode := range []string{FeedModeRotating, FeedModeImmediate} {
		r := runKillfeedMode(t, mode, interval)
		results = append(results, r)
		t.Run(mode, func(t *testing.T) {
			if strings.Join(r.order, ",") != "kill:Victim1,kill:Victim2,kill:Victim3" {
				t.Fatalf("kill order/dedupe wrong: %v", r.order)
			}
			if strings.Join(r.deaths, ",") != "death:Starved" {
				t.Fatalf("death feed wrong: %v", r.deaths)
			}
			if r.persistedKills != 3 || r.persistedDead != 1 {
				t.Fatalf("persistence (the C.A.S.E. input) must be identical: kills=%d deaths=%d", r.persistedKills, r.persistedDead)
			}
			if !r.embedsIdentical {
				t.Fatal("the posted embed must be exactly the card the publisher built")
			}
		})
	}

	// Stage split per mode, milliseconds (max over the 4 cards). The cycle is
	// scaled 10 min -> 400ms; the Discord call has 20ms of simulated latency.
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-10s | %-9s | %-11s | %-11s | %-9s\n", "mode", "detection", "persistence", "queue", "delivery")
	for _, r := range results {
		fmt.Fprintf(&b, "%-10s | %-9d | %-11d | %-11d | %-9d\n", r.mode, maxI(r.detectMs), maxI(r.persistMs), maxI(r.queueMs), maxI(r.deliveryMs))
	}
	t.Log(b.String())
	rot, imm := results[0], results[1]
	if maxI(imm.queueMs) >= minI(rot.queueMs) {
		t.Fatalf("immediate mode must remove the cycle wait: rotating %v immediate %v", rot.queueMs, imm.queueMs)
	}
	if maxI(imm.detectMs) > 200 || maxI(rot.detectMs) > 200 {
		t.Fatalf("detection and persistence must not depend on the delivery mode: %v / %v", rot.detectMs, imm.detectMs)
	}
}

func maxI(v []int64) int64 {
	var m int64
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func minI(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}
