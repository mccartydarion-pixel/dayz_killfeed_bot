package embedrender

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// Source resolves the custom template of the installation that owns a game server.
// The lookup goes (guild, server) -> installation -> route template - never by guild
// alone, so two servers of one guild are independent - and returns the installation
// id even when there is no template (so a cached "none" can be invalidated).
type Source interface {
	ResolveTemplate(ctx context.Context, guildRowID, serverID int64, routeKey string) (installationID int64, cfg *embedtemplates.Config, err error)
}

const (
	// DefaultTTL bounds how stale a cached template can be after an out-of-process
	// change; in-process saves invalidate immediately.
	DefaultTTL = 30 * time.Second
	// errorBackoff keeps a failing database from being hit once per event.
	errorBackoff  = 5 * time.Second
	maxEntries    = 2048
	lookupTimeout = time.Second
	warnEvery     = time.Minute
)

type key struct {
	guildRowID, serverID int64
	routeKey             string
}

type state int

const (
	stateNone      state = iota // installation has no custom template for the route
	stateCustom                 // a valid custom template
	stateMalformed              // a stored template that fails validation
	stateError                  // the lookup failed
)

type entry struct {
	installationID int64
	cfg            *embedtemplates.Config
	state          state
	expires        time.Time
}

type flight struct {
	done chan struct{}
	e    entry
}

// Stats are cumulative counters (no player names, no template contents).
type Stats struct {
	CustomRender   int64 `json:"templateCustomRender"`   // a custom template was rendered
	DefaultRender  int64 `json:"templateDefaultRender"`  // no custom template: the Champion default was kept
	FallbackRender int64 `json:"templateFallbackRender"` // a template existed but the default was used (lookup/validation/render problem)
	RenderError    int64 `json:"templateRenderError"`    // the fallback was caused by a failed lookup or render
}

// Renderer is the one shared entry point publishers call. A nil *Renderer, or one
// created disabled, returns every default untouched - the rollout switch
// (CHAMPION_CUSTOM_EMBEDS_ENABLED) is simply whether one is wired.
type Renderer struct {
	source  Source
	ttl     time.Duration
	enabled bool
	now     func() time.Time

	custom, dflt, fallback, errs atomic.Int64

	mu       sync.Mutex
	entries  map[key]entry
	inflight map[key]*flight
	lastWarn map[string]time.Time
}

type Options struct {
	Source  Source
	TTL     time.Duration // <= 0: DefaultTTL
	Enabled bool
	Now     func() time.Time // tests
}

func New(o Options) *Renderer {
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Renderer{source: o.Source, ttl: o.TTL, enabled: o.Enabled && o.Source != nil, now: o.Now,
		entries: map[key]entry{}, inflight: map[key]*flight{}, lastWarn: map[string]time.Time{}}
}

// Enabled reports whether custom rendering is active.
func (r *Renderer) Enabled() bool { return r != nil && r.enabled }

// Stats returns a snapshot of the counters.
func (r *Renderer) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	return Stats{CustomRender: r.custom.Load(), DefaultRender: r.dflt.Load(), FallbackRender: r.fallback.Load(), RenderError: r.errs.Load()}
}

// Invalidate drops the cached state of one installation's route so the next event
// re-reads it. Called after an in-process save or reset.
func (r *Renderer) Invalidate(installationID int64, routeKey string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	for k, e := range r.entries {
		if k.routeKey == routeKey && e.installationID == installationID {
			delete(r.entries, k)
		}
	}
	r.mu.Unlock()
}

// InvalidateAll drops every cached entry.
func (r *Renderer) InvalidateAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries = map[key]entry{}
	r.mu.Unlock()
}

// Customize returns the embed to publish for one event: the custom rendering when
// the installation owning (guildRowID, serverID) has an enabled template for routeKey
// and it renders, otherwise def UNCHANGED (never mutated). It never fails, never
// blocks longer than the lookup timeout, never touches the network, and recovers
// from any panic - a template can never cost an event its card.
func (r *Renderer) Customize(ctx context.Context, guildRowID, serverID int64, routeKey string, vars map[string]string, at time.Time, def *discordgo.MessageEmbed) (out *discordgo.MessageEmbed) {
	if r == nil || !r.enabled || def == nil {
		return def
	}
	out = def
	defer func() {
		if rec := recover(); rec != nil {
			r.errs.Add(1)
			r.fallback.Add(1)
			r.warn(0, routeKey, "render_panic")
			out = def
		}
	}()
	e := r.lookup(ctx, key{guildRowID, serverID, routeKey})
	switch e.state {
	case stateNone:
		r.dflt.Add(1)
		return def
	case stateError:
		r.fallback.Add(1)
		r.errs.Add(1)
		return def
	case stateMalformed:
		r.fallback.Add(1)
		return def
	}
	emb, err := Render(*e.cfg, routeKey, approvedOnly(routeKey, vars), at)
	if err != nil {
		if err == ErrNotRenderable {
			// A disabled template means "use the default"; an empty render is a fallback.
			if !e.cfg.Enabled {
				r.dflt.Add(1)
				return def
			}
			r.fallback.Add(1)
			r.warn(e.installationID, routeKey, "empty_render")
			return def
		}
		r.fallback.Add(1)
		r.errs.Add(1)
		r.warn(e.installationID, routeKey, "render_error")
		return def
	}
	r.custom.Add(1)
	return emb
}

// approvedOnly keeps only the variables the route approves and sanitizes each value,
// so a publisher can neither pass an extra variable nor an unsanitized one.
func approvedOnly(routeKey string, vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for _, name := range embedtemplates.Variables(routeKey) {
		if v, ok := vars[name]; ok {
			if s := SanitizeValue(v); s != "" {
				out[name] = s
			}
		}
	}
	return out
}

// lookup is the cached, single-flight template read.
func (r *Renderer) lookup(ctx context.Context, k key) entry {
	now := r.now()
	r.mu.Lock()
	if e, ok := r.entries[k]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e
	}
	if f, ok := r.inflight[k]; ok { // another goroutine is already reading it
		r.mu.Unlock()
		select {
		case <-f.done:
			return f.e
		case <-ctx.Done():
			return entry{state: stateError}
		}
	}
	f := &flight{done: make(chan struct{})}
	r.inflight[k] = f
	r.mu.Unlock()

	f.e = r.load(ctx, k, now)

	r.mu.Lock()
	delete(r.inflight, k)
	if len(r.entries) >= maxEntries {
		for ek, e := range r.entries {
			if !now.Before(e.expires) {
				delete(r.entries, ek)
			}
		}
		if len(r.entries) >= maxEntries {
			r.entries = map[key]entry{}
		}
	}
	r.entries[k] = f.e
	r.mu.Unlock()
	close(f.done)
	return f.e
}

func (r *Renderer) load(ctx context.Context, k key, now time.Time) (e entry) {
	defer func() {
		if rec := recover(); rec != nil {
			e = entry{state: stateError, expires: now.Add(errorBackoff)}
			r.warn(0, k.routeKey, "lookup_panic")
		}
	}()
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	instID, cfg, err := r.source.ResolveTemplate(lctx, k.guildRowID, k.serverID, k.routeKey)
	if err != nil {
		r.warn(0, k.routeKey, "lookup_error")
		return entry{state: stateError, expires: now.Add(errorBackoff)}
	}
	if cfg == nil {
		return entry{installationID: instID, state: stateNone, expires: now.Add(r.ttl)}
	}
	// A stored template must still satisfy the validator (e.g. the approved variable
	// set may have changed): otherwise it is treated as malformed, not rendered.
	normalized, verr := embedtemplates.Validate(*cfg, k.routeKey)
	if verr != nil {
		r.warn(instID, k.routeKey, "stored_template_invalid")
		return entry{installationID: instID, state: stateMalformed, expires: now.Add(r.ttl)}
	}
	return entry{installationID: instID, cfg: &normalized, state: stateCustom, expires: now.Add(r.ttl)}
}

// warn logs a fallback reason at most once a minute per (installation, route, reason):
// ids and reasons only - never template contents or player data.
func (r *Renderer) warn(installationID int64, routeKey, reason string) {
	id := fmt.Sprintf("%d|%s|%s", installationID, routeKey, reason)
	now := r.now()
	r.mu.Lock()
	if last, ok := r.lastWarn[id]; ok && now.Sub(last) < warnEvery {
		r.mu.Unlock()
		return
	}
	if len(r.lastWarn) > 1024 {
		r.lastWarn = map[string]time.Time{}
	}
	r.lastWarn[id] = now
	r.mu.Unlock()
	slog.Warn("component=embedrender", "event", "embed_template_fallback", "installation_id", installationID, "route_key", routeKey, "fallback_reason", reason)
}
