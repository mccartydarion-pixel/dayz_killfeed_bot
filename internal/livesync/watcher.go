package livesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md section 7): continuous ingestion of the
// non-ADM DayZ/Nitrado logs. The ADM keeps its own engine (internal/killfeed); this supervisor runs
// beside it and shares nothing with it but the HTTP client, which has no shared lock.
//
// Shape:
//   - one directory lister goroutine refreshes the listing of the log directories and publishes
//     snapshots. Nobody waits on it: a stalled listing only makes snapshots older.
//   - one goroutine per family (RPT, script, crash, restart.log) owns its current source file, its
//     durable checkpoint and its failure backoff. Every network call has its own timeout, so a
//     stalled family can delay only itself.
//   - each family reads when the listing shows growth AND, independently, on a direct-read probe
//     interval, because Nitrado listing metadata can lag the file by many minutes (observed 25+
//     minutes on 2026-09-24). New bytes are therefore found even when the listing is stale.
//   - reads are full downloads parsed from the checkpoint: Nitrado ignores offset/count on the
//     signed download URL (verified 2026-09-24), so no partial read is trusted.
//
// Nothing here fabricates an observation: a record exists only for a complete line DayZ or Nitrado
// wrote, times come only from the line (or the offset restart.log states), and a boot session is
// ended only on written evidence.

// Remote is the read-only Nitrado surface the supervisor uses. *nitrado.Client implements it.
type Remote interface {
	ListLogs(ctx context.Context, serviceID string) ([]nitrado.LogFile, error)
	ListDir(ctx context.Context, serviceID, dir string) ([]nitrado.LogFile, error)
	ReadLog(ctx context.Context, serviceID, path string) ([]byte, error)
}

// FamilyPolicy is one family's read schedule.
type FamilyPolicy struct {
	Family string
	// ProbeEvery is the direct-read interval used even when the listing shows no growth.
	ProbeEvery time.Duration
}

// DefaultPolicies: RPT and restart.log carry boot/shutdown evidence and are probed most often;
// script logs change rarely; crash logs are written once per boot on Champions.
func DefaultPolicies() []FamilyPolicy {
	return []FamilyPolicy{
		{Family: FamilyRPT, ProbeEvery: 30 * time.Second},
		{Family: FamilyRestart, ProbeEvery: 30 * time.Second},
		{Family: FamilyScript, ProbeEvery: 60 * time.Second},
		{Family: FamilyCrash, ProbeEvery: 120 * time.Second},
	}
}

// Config configures one server's supervisor.
type Config struct {
	GuildID, ServerID int64
	ServiceID         string
	ListEvery         time.Duration // directory listing refresh (default 20s)
	Tick              time.Duration // family scheduling tick (default 2s)
	OpTimeout         time.Duration // per network call (default 60s)
	LateAfter         time.Duration // an event read this long after it happened is BACKFILL (default 10m)
	MaxBackoff        time.Duration // failure backoff cap (default 10m)
	HealthLogEvery    time.Duration // periodic source_health log (default 5m)
	Policies          []FamilyPolicy
	Now               func() time.Time
}

func (c *Config) defaults() {
	if c.ListEvery <= 0 {
		c.ListEvery = 20 * time.Second
	}
	if c.Tick <= 0 {
		c.Tick = 2 * time.Second
	}
	if c.OpTimeout <= 0 {
		c.OpTimeout = 60 * time.Second
	}
	if c.LateAfter <= 0 {
		c.LateAfter = 10 * time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 10 * time.Minute
	}
	if c.HealthLogEvery <= 0 {
		c.HealthLogEvery = 5 * time.Minute
	}
	if len(c.Policies) == 0 {
		c.Policies = DefaultPolicies()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Supervisor runs the per-family watchers of one server.
type Supervisor struct {
	cfg      Config
	remote   Remote
	store    Store
	sessions SessionEnder
	lister   *dirLister

	mu        sync.RWMutex
	health    map[string]*SourceHealth
	offsetMin *int // server UTC offset in minutes, learned from restart.log
	sessionEv []SessionEvidence
	started   time.Time
}

// NewSupervisor builds a supervisor. sessions may be nil (no session ending).
func NewSupervisor(cfg Config, remote Remote, store Store, sessions SessionEnder) *Supervisor {
	cfg.defaults()
	s := &Supervisor{cfg: cfg, remote: remote, store: store, sessions: sessions, health: map[string]*SourceHealth{}}
	s.lister = &dirLister{sup: s, snaps: map[string]dirSnapshot{}, firstSeen: map[string]time.Time{}, initial: map[string]bool{}}
	for _, p := range cfg.Policies {
		s.health[p.Family] = &SourceHealth{Family: p.Family, State: StateNoSource}
	}
	return s
}

// Run blocks until ctx ends. Each family and the lister run in their own goroutine.
func (s *Supervisor) Run(ctx context.Context) {
	if s == nil || s.remote == nil || s.store == nil {
		return
	}
	s.started = s.cfg.Now()
	stored := map[string][]SourceState{}
	loadCtx, cancel := context.WithTimeout(ctx, s.cfg.OpTimeout)
	if rows, err := s.store.LoadSources(loadCtx, s.cfg.GuildID, s.cfg.ServerID); err != nil {
		slog.Warn("component=livesync", "event", "sources_load_failed", "server_id", s.cfg.ServerID, "err", err.Error())
	} else {
		for _, r := range rows {
			stored[r.Family] = append(stored[r.Family], r)
		}
	}
	cancel()
	slog.Info("component=livesync", "event", "supervisor_started", "server_id", s.cfg.ServerID, "families", len(s.cfg.Policies))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.lister.run(ctx) }()
	for _, p := range s.cfg.Policies {
		w := newFamilyWatcher(s, p, stored[p.Family])
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.logHealthLoop(ctx) }()
	wg.Wait()
}

// --- directory lister ----------------------------------------------------------------------------

type dirSnapshot struct {
	files []nitrado.LogFile
	at    time.Time
}

type dirLister struct {
	sup  *Supervisor
	mu   sync.RWMutex
	dirs []string
	// snaps holds the last SUCCESSFUL listing of each directory.
	snaps map[string]dirSnapshot
	// firstSeen: when a canonical file first appeared in any listing. initial: it was present in
	// the first successful listing of its directory (it existed before Champion watched it).
	firstSeen map[string]time.Time
	initial   map[string]bool
	lastErr   string
	lastOKAt  time.Time
}

// discoverDirs finds the log directories from the ADM files Nitrado lists (no path is guessed):
// every directory holding an ADM, plus the ftproot mount root when the ADM path shows the
// /<root>/{noftp,ftproot}/ layout (restart.log lives there on Nitrado console services). A root
// directory is only ever LISTED; a file in it is read only if the listing returns it.
func discoverDirs(logs []nitrado.LogFile) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, lf := range logs {
		if ClassifySource(lf.Path).Family != FamilyADM {
			continue
		}
		add(path.Dir(lf.Path))
		for _, m := range mountMarkers {
			if i := strings.Index(lf.Path, m); i >= 0 {
				add(lf.Path[:i] + "/ftproot")
			}
		}
	}
	sort.Strings(out)
	return out
}

func (l *dirLister) run(ctx context.Context) {
	fails := 0
	for {
		wait := l.sup.cfg.ListEvery
		if !l.refresh(ctx) {
			fails++
			wait = backoff(l.sup.cfg.ListEvery, fails, l.sup.cfg.MaxBackoff)
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// refresh lists every known directory (discovering them first when needed). ok=false when no
// listing succeeded this pass.
func (l *dirLister) refresh(ctx context.Context) bool {
	s := l.sup
	l.mu.RLock()
	dirs := append([]string(nil), l.dirs...)
	l.mu.RUnlock()
	if len(dirs) == 0 {
		opCtx, cancel := context.WithTimeout(ctx, s.cfg.OpTimeout)
		logs, err := s.remote.ListLogs(opCtx, s.cfg.ServiceID)
		cancel()
		if err != nil {
			l.setErr("discovery_failed: " + errorClass(err))
			return false
		}
		dirs = discoverDirs(logs)
		if len(dirs) == 0 {
			l.setErr("no_adm_directory_listed")
			return false
		}
		l.mu.Lock()
		l.dirs = dirs
		l.mu.Unlock()
		slog.Info("component=livesync", "event", "directories_discovered", "server_id", s.cfg.ServerID, "count", len(dirs))
	}
	anyOK := false
	for _, d := range dirs {
		opCtx, cancel := context.WithTimeout(ctx, s.cfg.OpTimeout)
		files, err := s.remote.ListDir(opCtx, s.cfg.ServiceID, d)
		cancel()
		if err != nil {
			l.setErr("list_failed: " + errorClass(err))
			continue
		}
		anyOK = true
		now := s.cfg.Now()
		l.mu.Lock()
		_, hadSnapshot := l.snaps[d]
		l.snaps[d] = dirSnapshot{files: files, at: now}
		for _, f := range files {
			id := CanonicalSourceID(f.Path)
			if _, ok := l.firstSeen[id]; !ok {
				l.firstSeen[id] = now
				if !hadSnapshot {
					l.initial[id] = true
				}
			}
		}
		l.lastOKAt = now
		l.lastErr = ""
		l.mu.Unlock()
	}
	return anyOK
}

func (l *dirLister) setErr(msg string) {
	l.mu.Lock()
	l.lastErr = msg
	l.mu.Unlock()
}

// listed is one logical file merged across mounts.
type listed struct {
	nitrado.LogFile
	info           SourceInfo
	existedAtStart bool
	listedAt       time.Time
}

// candidates returns the family's files from the latest snapshots, one per canonical identity: the
// mount copy with the larger size wins (mounts lag independently), noftp on a tie.
func (l *dirLister) candidates(family string) []listed {
	l.mu.RLock()
	defer l.mu.RUnlock()
	byID := map[string]listed{}
	for _, snap := range l.snaps {
		for _, f := range snap.files {
			info := ClassifySource(f.Path)
			if info.Family != family {
				continue
			}
			cur, ok := byID[info.CanonicalID]
			if ok && (cur.Size > f.Size || (cur.Size == f.Size && strings.Contains(cur.Path, "/noftp/"))) {
				continue
			}
			byID[info.CanonicalID] = listed{LogFile: f, info: info, existedAtStart: l.initial[info.CanonicalID], listedAt: snap.at}
		}
	}
	out := make([]listed, 0, len(byID))
	for _, c := range byID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return newerThan(out[i].info, out[j].info) })
	return out
}

// newerThan orders by the filename's boot stamp (newest first); unstamped files sort last.
func newerThan(a, b SourceInfo) bool {
	switch {
	case a.FileLocalStart != nil && b.FileLocalStart != nil:
		if !a.FileLocalStart.Equal(*b.FileLocalStart) {
			return a.FileLocalStart.After(*b.FileLocalStart)
		}
	case a.FileLocalStart != nil:
		return true
	case b.FileLocalStart != nil:
		return false
	}
	return a.CanonicalID > b.CanonicalID
}

// --- per-family watcher --------------------------------------------------------------------------

type familyWatcher struct {
	sup    *Supervisor
	policy FamilyPolicy
	stored map[string]SourceState
	active *sourceRuntime
	// draining: retired sources whose final bytes could not be read at rotation; retried on
	// their own schedule until one read succeeds.
	draining []*sourceRuntime
}

type sourceRuntime struct {
	state SourceState
	info  SourceInfo
	// firstReadIsBackfill: the file existed before it was watched, so everything in the first
	// read is history (BACKFILL).
	firstReadIsBackfill bool
	listingSize         int64
	listingModified     time.Time
	nextRead            time.Time
	fails               int
}

func newFamilyWatcher(s *Supervisor, p FamilyPolicy, stored []SourceState) *familyWatcher {
	w := &familyWatcher{sup: s, policy: p, stored: map[string]SourceState{}}
	var active *SourceState
	for i := range stored {
		w.stored[stored[i].SourceFile] = stored[i]
		if stored[i].Active && (active == nil || newerThan(ClassifySource(stored[i].RemotePath), ClassifySource(active.RemotePath))) {
			active = &stored[i]
		}
	}
	if active != nil {
		// Resume the durable source immediately: direct-read probes continue even before (or
		// without) a fresh directory listing.
		w.active = &sourceRuntime{state: *active, info: ClassifySource(active.RemotePath), nextRead: s.cfg.Now()}
	}
	return w
}

func (w *familyWatcher) run(ctx context.Context) {
	t := time.NewTicker(w.sup.cfg.Tick)
	defer t.Stop()
	for {
		w.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *familyWatcher) step(ctx context.Context) {
	now := w.sup.cfg.Now()
	cands := w.sup.lister.candidates(w.policy.Family)
	if len(cands) > 0 {
		newest := cands[0]
		switch {
		case w.active == nil:
			w.attach(newest, now)
		case newest.info.CanonicalID != w.active.state.SourceFile && newerThan(newest.info, w.active.info):
			w.rotate(ctx, newest, now)
		}
		for _, c := range cands {
			if w.active != nil && c.info.CanonicalID == w.active.state.SourceFile {
				w.active.listingSize, w.active.listingModified = c.Size, c.Modified
				if c.Path != w.active.state.RemotePath && c.Size > w.active.state.ReadSize {
					w.active.state.RemotePath = c.Path // the other mount is ahead
				}
			}
		}
	}
	w.retryDrains(ctx, now)
	if w.active == nil {
		w.updateHealth(func(h *SourceHealth) { h.State = StateNoSource })
		return
	}
	if now.Before(w.active.nextRead) && w.active.listingSize <= w.active.state.ReadSize {
		return
	}
	if w.active.fails > 0 && now.Before(w.active.nextRead) {
		return // in failure backoff: listing growth does not bypass it
	}
	why := "probe"
	if w.active.listingSize > w.active.state.ReadSize {
		why = "listing_growth"
	}
	w.read(ctx, w.active, why)
}

func (w *familyWatcher) retryDrains(ctx context.Context, now time.Time) {
	kept := w.draining[:0]
	for _, rt := range w.draining {
		if now.Before(rt.nextRead) {
			kept = append(kept, rt)
			continue
		}
		if !w.read(ctx, rt, "rotation_drain_retry") && rt.fails < 10 {
			kept = append(kept, rt)
		}
	}
	w.draining = kept
}

func (w *familyWatcher) attach(c listed, now time.Time) {
	st, ok := w.stored[c.info.CanonicalID]
	rt := &sourceRuntime{info: c.info, nextRead: now, listingSize: c.Size, listingModified: c.Modified}
	if ok {
		st.RemotePath, st.Active = c.Path, true
		rt.state = st
	} else {
		rt.state = SourceState{Family: w.policy.Family, SourceFile: c.info.CanonicalID, RemotePath: c.Path,
			FileLocalStart: c.info.FileLocalStart, Active: true, AttachedAt: now}
		// A file present before Champion first listed its directory is history until proven
		// otherwise; a file that appeared while watching is live from byte 0.
		rt.firstReadIsBackfill = c.existedAtStart
	}
	w.active = rt
	slog.Info("component=livesync", "event", "source_attached", "server_id", w.sup.cfg.ServerID, "family", w.policy.Family,
		"file", c.info.CanonicalID, "checkpoint", rt.state.Checkpoint, "existed_at_start", c.existedAtStart)
	w.sup.newBootFileEvidence(c.info, w.policy.Family)
}

// rotate drains the old file once (its last bytes), retires it, and attaches the newer file.
func (w *familyWatcher) rotate(ctx context.Context, newer listed, now time.Time) {
	old := w.active
	drained := w.read(ctx, old, "rotation_drain")
	old.state.Active = false
	opCtx, cancel := context.WithTimeout(ctx, w.sup.cfg.OpTimeout)
	if _, err := w.sup.store.CommitSource(opCtx, w.sup.cfg.GuildID, w.sup.cfg.ServerID, old.state, nil); err != nil {
		slog.Warn("component=livesync", "event", "source_retire_failed", "server_id", w.sup.cfg.ServerID, "family", w.policy.Family, "err", err.Error())
	}
	cancel()
	w.stored[old.state.SourceFile] = old.state
	if !drained {
		w.draining = append(w.draining, old)
	}
	slog.Info("component=livesync", "event", "source_rotated", "server_id", w.sup.cfg.ServerID, "family", w.policy.Family,
		"from", old.state.SourceFile, "to", newer.info.CanonicalID, "drained_to", old.state.Checkpoint)
	w.updateHealth(func(h *SourceHealth) { h.Rotations++ })
	w.attach(newer, now)
}

// read downloads one source file, parses the complete lines after the checkpoint and commits the
// records and the new checkpoint in one transaction. ok=false leaves the checkpoint unchanged.
func (w *familyWatcher) read(ctx context.Context, rt *sourceRuntime, why string) (ok bool) {
	s := w.sup
	current := rt == w.active
	opCtx, cancel := context.WithTimeout(ctx, s.cfg.OpTimeout)
	data, err := s.remote.ReadLog(opCtx, s.cfg.ServiceID, rt.state.RemotePath)
	cancel()
	now := s.cfg.Now()
	if err != nil {
		w.fail(rt, now, "read_failed: "+errorClass(err))
		return false
	}
	st := rt.state // the working copy; rt.state changes only after a successful commit
	size := int64(len(data))
	prevRead, prevSize := st.LastReadAt, st.ReadSize
	truncated := size < st.Checkpoint
	if truncated {
		// Replaced or truncated in place: the old checkpoint no longer addresses these bytes.
		slog.Warn("component=livesync", "event", "source_truncated", "server_id", s.cfg.ServerID, "family", w.policy.Family,
			"file", st.SourceFile, "checkpoint", st.Checkpoint, "size", size)
		st.Checkpoint, st.BackfillUntil, prevSize = 0, 0, 0
	}
	if rt.firstReadIsBackfill {
		st.BackfillUntil = size
	}
	res, consumed := w.parse(data[st.Checkpoint:], st)
	recs := make([]StoredRecord, 0, len(res.Records))
	var eventLags []time.Duration
	var windows []time.Duration
	for _, r := range res.Records {
		env := r.Envelope(Scope{ServerID: s.cfg.ServerID}, w.policy.Family, st.SourceFile, bootID(rt.info), now, now)
		sr := StoredRecord{Envelope: env, Delivery: DeliveryLive}
		if prevRead != nil && r.Offset > prevSize && !truncated {
			v := *prevRead
			sr.VisibleAfter = &v
		}
		utc := s.eventUTC(r)
		if r.Offset <= st.BackfillUntil || (utc != nil && now.Sub(*utc) > s.cfg.LateAfter) {
			sr.Delivery = DeliveryBackfill
		}
		if sr.Delivery == DeliveryLive {
			if utc != nil {
				eventLags = append(eventLags, now.Sub(*utc))
			}
			if sr.VisibleAfter != nil {
				windows = append(windows, now.Sub(*sr.VisibleAfter))
			}
		}
		recs = append(recs, sr)
	}
	st.Checkpoint += consumed
	st.ReadSize = size
	st.LastReadAt = &now
	if size > prevSize {
		st.LastGrowthAt = &now
	}
	opCtx, cancel = context.WithTimeout(ctx, s.cfg.OpTimeout)
	inserted, err := s.store.CommitSource(opCtx, s.cfg.GuildID, s.cfg.ServerID, st, recs)
	cancel()
	committed := s.cfg.Now()
	if err != nil {
		w.fail(rt, now, "commit_failed")
		return false
	}
	st.Records += int64(inserted)
	rt.state = st
	rt.firstReadIsBackfill = false
	rt.fails = 0
	rt.nextRead = now.Add(w.policy.ProbeEvery)
	w.stored[st.SourceFile] = st

	live, backfill, unknown := 0, 0, 0
	var lastRecordAt *time.Time
	for _, r := range recs {
		if r.Delivery == DeliveryLive {
			live++
		} else {
			backfill++
		}
		if r.Status == StatusUnknown {
			unknown++
		}
		if r.SourceLocalTime != nil {
			t := *r.SourceLocalTime
			lastRecordAt = &t
		}
	}
	w.updateHealth(func(h *SourceHealth) {
		h.Reads++
		h.ReadBytes += size
		h.Records += int64(inserted)
		h.Live += int64(live)
		h.Backfill += int64(backfill)
		h.Unknown += int64(unknown)
		if len(recs) > 0 {
			h.latency.add(eventLags, windows, committed.Sub(now))
		}
		if !current {
			return // a draining predecessor: counters only, the health describes the current file
		}
		h.State, h.LastError, h.ConsecutiveFailures = StateFresh, "", 0
		h.SourceFile, h.RemotePath, h.FileLocalStart = st.SourceFile, st.RemotePath, st.FileLocalStart
		h.Checkpoint, h.ReadSize, h.ListingSize = st.Checkpoint, st.ReadSize, rt.listingSize
		if !rt.listingModified.IsZero() {
			m := rt.listingModified
			h.ListingModified = &m
		}
		h.LastReadAt, h.LastGrowthAt = st.LastReadAt, st.LastGrowthAt
		if lastRecordAt != nil {
			h.LastRecordLocalTime = lastRecordAt
		}
	})
	if inserted > 0 || !current {
		slog.Info("component=livesync", "event", "source_read", "server_id", s.cfg.ServerID, "family", w.policy.Family,
			"file", st.SourceFile, "reason", why, "size", size, "checkpoint", st.Checkpoint, "records", inserted,
			"live", live, "backfill", backfill, "unknown", unknown, "persist_ms", committed.Sub(now).Milliseconds())
	}
	s.learnOffset(recs)
	s.sessionEvidence(ctx, w.policy.Family, recs)
	return true
}

func (w *familyWatcher) fail(rt *sourceRuntime, now time.Time, msg string) {
	rt.fails++
	rt.nextRead = now.Add(backoff(w.policy.ProbeEvery, rt.fails, w.sup.cfg.MaxBackoff))
	if rt == w.active {
		w.updateHealth(func(h *SourceHealth) {
			h.ConsecutiveFailures, h.LastError = rt.fails, msg
			h.LastFailureAt = &now
			if rt.fails >= 3 {
				h.State = StateFailing
			}
		})
	}
	if rt.fails == 1 || rt.fails%10 == 0 {
		slog.Warn("component=livesync", "event", "source_read_failed", "server_id", w.sup.cfg.ServerID, "family", w.policy.Family,
			"file", rt.state.SourceFile, "failures", rt.fails, "err", msg)
	}
}

func (w *familyWatcher) parse(content []byte, st SourceState) (ParseResult, int64) {
	switch w.policy.Family {
	case FamilyRPT:
		return ParseRPT(content, st.Checkpoint, st.FileLocalStart)
	case FamilyScript, FamilyCrash:
		year := 0
		if st.FileLocalStart != nil {
			year = st.FileLocalStart.Year()
		}
		return ParseScriptLog(content, st.Checkpoint, year)
	case FamilyRestart:
		return ParseRestartLog(content, st.Checkpoint)
	}
	return ParseResult{}, 0
}

func (w *familyWatcher) updateHealth(fn func(*SourceHealth)) {
	w.sup.mu.Lock()
	defer w.sup.mu.Unlock()
	h := w.sup.health[w.policy.Family]
	if h == nil {
		h = &SourceHealth{Family: w.policy.Family}
		w.sup.health[w.policy.Family] = h
	}
	fn(h)
}

// bootID is the filename boot stamp of a stamped source ("" for restart.log).
func bootID(info SourceInfo) string {
	if info.FileLocalStart == nil {
		return ""
	}
	return info.FileLocalStart.Format("2006-01-02T15:04:05")
}

func backoff(base time.Duration, fails int, max time.Duration) time.Duration {
	d := base
	for i := 1; i < fails && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// errorClass reduces an error to a safe label (never a URL, token or path).
func errorClass(err error) string {
	var re *nitrado.RequestError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &re):
		return fmt.Sprintf("nitrado_%s_%d", re.Kind, re.StatusCode)
	}
	return "error"
}

// --- clock and session evidence ------------------------------------------------------------------

// eventUTC is the record's UTC time: stated by the source, or its server-local time converted with
// the offset restart.log stated. Nil when neither is known.
func (s *Supervisor) eventUTC(r Record) *time.Time {
	if r.SourceUTC != nil {
		return r.SourceUTC
	}
	if r.SourceLocalTime == nil {
		return nil
	}
	s.mu.RLock()
	off := s.offsetMin
	s.mu.RUnlock()
	if off == nil {
		return nil
	}
	u := r.SourceLocalTime.Add(-time.Duration(*off) * time.Minute)
	return &u
}

// learnOffset adopts the UTC offset stated by the newest restart.log line that states one.
func (s *Supervisor) learnOffset(recs []StoredRecord) {
	var best *StoredRecord
	for i := range recs {
		r := &recs[i]
		if r.Payload["clock"] == "stated_offset" && r.SourceUTC != nil && r.SourceLocalTime != nil {
			best = r
		}
	}
	if best == nil {
		return
	}
	minutes := int(best.SourceLocalTime.Sub(*best.SourceUTC).Round(time.Minute) / time.Minute)
	s.mu.Lock()
	changed := s.offsetMin == nil || *s.offsetMin != minutes
	if changed {
		s.offsetMin = &minutes
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.OpTimeout)
	defer cancel()
	if err := s.store.SetServerUTCOffset(ctx, s.cfg.GuildID, s.cfg.ServerID, minutes, fmt.Sprintf("%s@%d", best.SourceID, best.Offset)); err != nil {
		slog.Warn("component=livesync", "event", "server_clock_store_failed", "server_id", s.cfg.ServerID, "err", err.Error())
		return
	}
	slog.Info("component=livesync", "event", "server_clock_learned", "server_id", s.cfg.ServerID, "utc_offset_minutes", minutes)
}

// SessionEvidence is one piece of written evidence that ended (or would end) a boot session.
type SessionEvidence struct {
	At        time.Time // when Champion acted on it
	BootLocal time.Time // server-local time the evidence proves the old session was over by
	Reason    string
	Evidence  string // source identity + offset, never a raw line
	Ended     bool
}

// bootMargin: files of one boot are stamped within seconds of each other (ADM, RPT, then script
// ~4s later), and boots on a live server are many minutes apart - CorrelateBoots uses the same
// two-minute window.
const bootMargin = 2 * time.Minute

// newBootFileEvidence: a newly listed file of a later boot proves every session that started more
// than bootMargin before it has ended.
func (s *Supervisor) newBootFileEvidence(info SourceInfo, family string) {
	if info.FileLocalStart == nil {
		return
	}
	s.endSession(info.FileLocalStart.Add(-bootMargin), "newer_boot_file:"+family, info.CanonicalID)
}

// sessionEvidence acts on shutdown/boot records: an RPT "Termination successfully completed", or a
// restart.log pre-start check / host reboot (both written only while no DayZ process of the old
// boot runs). A restart REQUEST alone ends nothing - the server keeps running through its
// countdown.
func (s *Supervisor) sessionEvidence(_ context.Context, family string, recs []StoredRecord) {
	for _, r := range recs {
		var reason string
		switch {
		case family == FamilyRPT && r.Category == CategoryShutdownComplete:
			reason = "rpt_shutdown_complete"
		case family == FamilyRestart && (r.Category == CategoryPreStartCheck || r.Category == CategoryHostReboot):
			reason = "restart_log_" + strings.ToLower(r.Category)
		default:
			continue
		}
		local := r.SourceLocalTime
		if local == nil && r.SourceUTC != nil {
			s.mu.RLock()
			off := s.offsetMin
			s.mu.RUnlock()
			if off != nil {
				l := r.SourceUTC.Add(time.Duration(*off) * time.Minute)
				local = &l
			}
		}
		if local == nil {
			continue // no server-local time: cannot be placed against the session, never guessed
		}
		s.endSession(*local, reason, fmt.Sprintf("%s@%d", r.SourceID, r.Offset))
	}
}

func (s *Supervisor) endSession(bootLocal time.Time, reason, evidence string) {
	if s.sessions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.OpTimeout)
	defer cancel()
	ended, err := s.sessions.EndADMSessionBefore(ctx, s.cfg.GuildID, s.cfg.ServerID, bootLocal, reason, evidence)
	if err != nil {
		slog.Warn("component=livesync", "event", "session_end_failed", "server_id", s.cfg.ServerID, "err", err.Error())
		return
	}
	if !ended {
		return
	}
	ev := SessionEvidence{At: s.cfg.Now(), BootLocal: bootLocal, Reason: reason, Evidence: evidence, Ended: true}
	s.mu.Lock()
	s.sessionEv = append(s.sessionEv, ev)
	if len(s.sessionEv) > 20 {
		s.sessionEv = s.sessionEv[len(s.sessionEv)-20:]
	}
	s.mu.Unlock()
	slog.Info("component=livesync", "event", "adm_session_ended", "server_id", s.cfg.ServerID, "reason", reason, "evidence", evidence,
		"boot_local", bootLocal.Format("2006-01-02T15:04:05"))
}
