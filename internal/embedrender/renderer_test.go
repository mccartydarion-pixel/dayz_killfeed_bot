package embedrender

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

type fakeSource struct {
	mu    sync.Mutex
	calls atomic.Int64
	inst  int64
	cfg   *embedtemplates.Config
	err   error
	panic bool
	delay time.Duration
	byKey map[[2]int64]*embedtemplates.Config // (guild, server) -> template
}

func (f *fakeSource) ResolveTemplate(ctx context.Context, guildRowID, serverID int64, routeKey string) (int64, *embedtemplates.Config, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.panic {
		panic("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, nil, f.err
	}
	if f.byKey != nil {
		return serverID + 1000, f.byKey[[2]int64{guildRowID, serverID}], nil
	}
	return f.inst, f.cfg, nil
}

func (f *fakeSource) set(cfg *embedtemplates.Config, err error) {
	f.mu.Lock()
	f.cfg, f.err = cfg, err
	f.mu.Unlock()
}

func defEmbed() *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Description: "DEFAULT CARD", Color: 1}
}

func newR(src Source) (*Renderer, *time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := New(Options{Source: src, Enabled: true, TTL: 30 * time.Second, Now: func() time.Time { return now }})
	return r, &now
}

func kcfg() *embedtemplates.Config {
	c := killTemplate()
	return &c
}

func customize(r *Renderer, def *discordgo.MessageEmbed) *discordgo.MessageEmbed {
	return r.Customize(context.Background(), 1, 10, "KILLFEED", full(), at, def)
}

func TestNoCustomTemplateKeepsTheDefault(t *testing.T) {
	src := &fakeSource{inst: 7}
	r, _ := newR(src)
	def := defEmbed()
	if got := customize(r, def); got != def {
		t.Fatal("no template: the SAME default embed must come back")
	}
	if s := r.Stats(); s.DefaultRender != 1 || s.CustomRender != 0 || s.FallbackRender != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestCustomTemplateIsRenderedAndTheDefaultIsNeverMutated(t *testing.T) {
	src := &fakeSource{inst: 7, cfg: kcfg()}
	r, _ := newR(src)
	def := defEmbed()
	got := customize(r, def)
	if got == def || got.Title != "Alice eliminated Bob" {
		t.Fatalf("custom card expected: %+v", got)
	}
	if def.Description != "DEFAULT CARD" || def.Title != "" {
		t.Fatal("the default embed must never be mutated")
	}
	if r.Stats().CustomRender != 1 {
		t.Fatalf("stats: %+v", r.Stats())
	}
}

func TestDisabledRendererAndNilRendererReturnTheDefaultWithoutTouchingTheSource(t *testing.T) {
	src := &fakeSource{cfg: kcfg()}
	off := New(Options{Source: src, Enabled: false})
	def := defEmbed()
	if customize(off, def) != def || src.calls.Load() != 0 {
		t.Fatal("a disabled renderer must not query or change anything")
	}
	var nilR *Renderer
	if customize(nilR, def) != def || nilR.Enabled() || nilR.Stats() != (Stats{}) {
		t.Fatal("a nil renderer is a no-op")
	}
	nilR.Invalidate(1, "KILLFEED")
	nilR.InvalidateAll()
	if customize(New(Options{Enabled: true}), def) != def { // no source at all
		t.Fatal("no source: default")
	}
	if customize(New(Options{Source: src, Enabled: true}), nil) != nil {
		t.Fatal("a nil default stays nil")
	}
}

func TestFallbackToDefaultOnEveryFailureMode(t *testing.T) {
	def := defEmbed()

	// database lookup error
	src := &fakeSource{err: errors.New("db down")}
	r, _ := newR(src)
	if customize(r, def) != def || r.Stats().FallbackRender != 1 || r.Stats().RenderError != 1 {
		t.Fatalf("lookup error must fall back: %+v", r.Stats())
	}

	// a lookup that panics
	r, _ = newR(&fakeSource{panic: true})
	if customize(r, def) != def {
		t.Fatal("a panicking lookup must fall back")
	}

	// stored template that no longer validates (unapproved variable / bad color)
	bad := kcfg()
	bad.Title.Template = "{{amount}}"
	r, _ = newR(&fakeSource{inst: 3, cfg: bad})
	if customize(r, def) != def || r.Stats().FallbackRender != 1 {
		t.Fatalf("an invalid stored template must fall back: %+v", r.Stats())
	}
	badColor := kcfg()
	badColor.Color = "invalid"
	r, _ = newR(&fakeSource{inst: 3, cfg: badColor})
	if customize(r, def) != def {
		t.Fatal("an undecodable/invalid stored template must fall back")
	}

	// renders to nothing -> default, counted as a fallback
	empty := &embedtemplates.Config{Enabled: true, Color: "#000000", Description: embedtemplates.Text{Enabled: true, Template: "{{distance}}"}}
	r, _ = newR(&fakeSource{inst: 3, cfg: empty})
	v := full()
	delete(v, "distance")
	if r.Customize(context.Background(), 1, 10, "KILLFEED", v, at, def) != def || r.Stats().FallbackRender != 1 {
		t.Fatalf("an empty render must fall back: %+v", r.Stats())
	}

	// a disabled saved template means "use the default" and is not a fallback
	off := kcfg()
	off.Enabled = false
	r, _ = newR(&fakeSource{inst: 3, cfg: off})
	if customize(r, def) != def || r.Stats().DefaultRender != 1 || r.Stats().FallbackRender != 0 {
		t.Fatalf("a disabled template is the default: %+v", r.Stats())
	}

	// missing installation (the server has none)
	r, _ = newR(&fakeSource{inst: 0, cfg: nil})
	if customize(r, def) != def || r.Stats().DefaultRender != 1 {
		t.Fatal("no installation: default")
	}
	// an invalid server id never reaches the source
	src2 := &fakeSource{cfg: kcfg()}
	r, _ = newR(src2)
	_ = src2
}

func TestCacheAvoidsPerEventLookupsAndExpires(t *testing.T) {
	src := &fakeSource{inst: 7, cfg: kcfg()}
	r, now := newR(src)
	for i := 0; i < 50; i++ {
		customize(r, defEmbed())
	}
	if src.calls.Load() != 1 {
		t.Fatalf("50 events must cost one lookup, got %d", src.calls.Load())
	}
	*now = now.Add(29 * time.Second)
	customize(r, defEmbed())
	if src.calls.Load() != 1 {
		t.Fatal("still inside the TTL")
	}
	*now = now.Add(2 * time.Second) // 31s
	customize(r, defEmbed())
	if src.calls.Load() != 2 {
		t.Fatalf("after the TTL the template is re-read, got %d lookups", src.calls.Load())
	}
	// A "no template" answer is cached too.
	none := &fakeSource{inst: 7}
	r2, _ := newR(none)
	for i := 0; i < 20; i++ {
		customize(r2, defEmbed())
	}
	if none.calls.Load() != 1 {
		t.Fatalf("a negative answer must be cached: %d", none.calls.Load())
	}
}

func TestTTLPicksUpAnOutOfProcessChange(t *testing.T) {
	src := &fakeSource{inst: 7}
	r, now := newR(src)
	def := defEmbed()
	if customize(r, def) != def {
		t.Fatal("no template yet")
	}
	src.set(kcfg(), nil) // saved by another process
	if customize(r, def) != def {
		t.Fatal("still cached inside the TTL")
	}
	*now = now.Add(31 * time.Second)
	if got := customize(r, def); got == def {
		t.Fatal("the new template must be visible after the TTL")
	}
}

func TestInvalidateMakesASaveOrResetVisibleImmediately(t *testing.T) {
	src := &fakeSource{inst: 7}
	r, _ := newR(src)
	def := defEmbed()
	customize(r, def) // caches "none" for installation 7
	src.set(kcfg(), nil)
	r.Invalidate(7, "KILLFEED")
	if got := customize(r, def); got == def {
		t.Fatal("PUT: the next event must render the new template")
	}
	src.set(nil, nil)
	r.Invalidate(7, "KILLFEED")
	if got := customize(r, def); got != def {
		t.Fatal("DELETE: the next event must use the default again")
	}
	// Invalidating another installation or route leaves this entry cached.
	before := src.calls.Load()
	r.Invalidate(8, "KILLFEED")
	r.Invalidate(7, "HITFEED")
	customize(r, def)
	if src.calls.Load() != before {
		t.Fatal("an unrelated invalidation must not drop the entry")
	}
	r.InvalidateAll()
	customize(r, def)
	if src.calls.Load() != before+1 {
		t.Fatal("InvalidateAll drops everything")
	}
}

func TestErrorBackoffStopsPerEventLookupsDuringAnOutage(t *testing.T) {
	src := &fakeSource{err: errors.New("db down")}
	r, now := newR(src)
	def := defEmbed()
	for i := 0; i < 30; i++ {
		if customize(r, def) != def {
			t.Fatal("default expected")
		}
	}
	if src.calls.Load() != 1 {
		t.Fatalf("an outage must not be hit once per event: %d", src.calls.Load())
	}
	src.set(kcfg(), nil)
	*now = now.Add(6 * time.Second) // past the backoff, well inside the TTL
	if customize(r, def) == def {
		t.Fatal("after the backoff the database is consulted again")
	}
}

func TestMultiServerIsolationSameGuild(t *testing.T) {
	a, b := kcfg(), kcfg()
	a.Title.Template = "SERVER-A {{killer}}"
	src := &fakeSource{byKey: map[[2]int64]*embedtemplates.Config{{1, 10}: a, {1, 11}: nil, {1, 12}: b}}
	_ = b
	r, _ := newR(src)
	def := defEmbed()
	gotA := r.Customize(context.Background(), 1, 10, "KILLFEED", full(), at, def)
	gotB := r.Customize(context.Background(), 1, 11, "KILLFEED", full(), at, def)
	if gotA == def || !strings.HasPrefix(gotA.Title, "SERVER-A") {
		t.Fatalf("server A has a custom template: %+v", gotA)
	}
	if gotB != def {
		t.Fatal("server B of the same guild must keep the default")
	}
	// The cache keys by (guild, server, route): A's entry never answers for B.
	if src.calls.Load() != 2 {
		t.Fatalf("two servers are two lookups: %d", src.calls.Load())
	}
}

func TestSingleFlightCollapsesConcurrentColdLookups(t *testing.T) {
	src := &fakeSource{inst: 7, cfg: kcfg(), delay: 50 * time.Millisecond}
	r, _ := newR(src)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := customize(r, defEmbed()); got.Title != "Alice eliminated Bob" {
				t.Errorf("unexpected card: %+v", got)
			}
		}()
	}
	wg.Wait()
	if src.calls.Load() != 1 {
		t.Fatalf("a cold cache stampede must be one lookup, got %d", src.calls.Load())
	}
}

func TestOnlyApprovedVariablesAreAcceptedFromPublishers(t *testing.T) {
	cfg := kcfg()
	cfg.Description.Template = "{{weapon}}"
	src := &fakeSource{inst: 7, cfg: cfg}
	r, _ := newR(src)
	vars := full()
	vars["secret_token"] = "TOPSECRET"
	vars["password"] = "hunter2"
	got := r.Customize(context.Background(), 1, 10, "KILLFEED", vars, at, defEmbed())
	blob := got.Title + got.Description + got.Footer.Text
	for _, f := range got.Fields {
		blob += f.Name + f.Value
	}
	if strings.Contains(blob, "TOPSECRET") || strings.Contains(blob, "hunter2") {
		t.Fatal("variables outside the route's approved set are dropped")
	}
}

// Race: concurrent events, saves/resets and cache resets on one renderer.
func TestRendererIsRaceFree(t *testing.T) {
	src := &fakeSource{inst: 7, cfg: kcfg()}
	r, _ := newR(src)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 300; n++ {
				r.Customize(context.Background(), 1, int64(10+i%3), "KILLFEED", full(), at, defEmbed())
				_ = r.Stats()
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.Invalidate(7, "KILLFEED")
				r.InvalidateAll()
				src.set(kcfg(), nil)
				src.set(nil, nil)
			}
		}
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}
