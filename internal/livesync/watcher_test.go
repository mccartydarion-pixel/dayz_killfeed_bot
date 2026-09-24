package livesync

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Champion Live Sync phase 2 acceptance tests for the per-source watchers. Log lines are real
// Champions formats (see testdata); paths follow the observed /games/<svc>/{noftp,ftproot} layout.

const (
	cfgDir     = "/games/svc_2/noftp/dayzps/config"
	ftpRoot    = "/games/svc_2/ftproot"
	admA       = cfgDir + "/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	rptA       = cfgDir + "/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT"
	rptB       = cfgDir + "/DayZServer_PS4_x64_2026-09-24_05-23-05.RPT"
	restartLog = ftpRoot + "/restart.log"
	rptHeader  = "Current time:  2026/09/24 04:15:07\nVersion 1.29.163709\n"
)

// --- fakes ---------------------------------------------------------------------------------------

type fakeRemote struct {
	mu       sync.Mutex
	files    map[string][]byte
	listSize map[string]int64 // a stale listing: the size Nitrado reports, if frozen
	hidden   map[string]bool  // not in any listing yet
	block    map[string]bool  // ReadLog never answers (until the caller's timeout)
	reads    map[string]int
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{files: map[string][]byte{}, listSize: map[string]int64{}, hidden: map[string]bool{}, block: map[string]bool{}, reads: map[string]int{}}
}

func (f *fakeRemote) set(p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = []byte(content)
}

func (f *fakeRemote) appendTo(p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = append(append([]byte(nil), f.files[p]...), content...)
}

func (f *fakeRemote) freezeListing(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listSize[p] = int64(len(f.files[p]))
}

func (f *fakeRemote) setHidden(p string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hidden[p] = v
}

func (f *fakeRemote) setBlock(p string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block[p] = v
}

func (f *fakeRemote) readCount(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[p]
}

func (f *fakeRemote) ListLogs(_ context.Context, _ string) ([]nitrado.LogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nitrado.LogFile
	for p, c := range f.files {
		if strings.HasSuffix(p, ".ADM") && !f.hidden[p] {
			out = append(out, nitrado.LogFile{Name: path.Base(p), Path: p, Directory: path.Dir(p), Size: int64(len(c))})
		}
	}
	return out, nil
}

func (f *fakeRemote) ListDir(_ context.Context, _ string, dir string) ([]nitrado.LogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nitrado.LogFile
	for p, c := range f.files {
		if path.Dir(p) != dir || f.hidden[p] {
			continue
		}
		size := int64(len(c))
		if s, ok := f.listSize[p]; ok {
			size = s
		}
		out = append(out, nitrado.LogFile{Name: path.Base(p), Path: p, Directory: dir, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRemote) ReadLog(ctx context.Context, _ string, p string) ([]byte, error) {
	f.mu.Lock()
	f.reads[p]++
	blocked := f.block[p]
	c, ok := f.files[p]
	c = append([]byte(nil), c...)
	f.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if !ok {
		return nil, errors.New("not found")
	}
	return c, nil
}

type memStore struct {
	mu        sync.Mutex
	sources   map[string]SourceState
	records   map[string]StoredRecord
	order     []string
	failNext  int
	commits   int
	offset    *int
	offsetSrc string
}

func newMemStore() *memStore {
	return &memStore{sources: map[string]SourceState{}, records: map[string]StoredRecord{}}
}

func (m *memStore) LoadSources(_ context.Context, _, _ int64) ([]SourceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SourceState
	for _, s := range m.sources {
		out = append(out, s)
	}
	return out, nil
}

func (m *memStore) CommitSource(_ context.Context, _, _ int64, src SourceState, recs []StoredRecord) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		return 0, errors.New("database unavailable")
	}
	m.commits++
	n := 0
	for _, r := range recs {
		if _, dup := m.records[r.EventID]; dup {
			continue
		}
		m.records[r.EventID] = r
		m.order = append(m.order, r.EventID)
		n++
	}
	m.sources[src.Family+"|"+src.SourceFile] = src
	return n, nil
}

func (m *memStore) SetServerUTCOffset(_ context.Context, _, _ int64, minutes int, from string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.offset, m.offsetSrc = &minutes, from
	return nil
}

func (m *memStore) find(fn func(StoredRecord) bool) []StoredRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []StoredRecord
	for _, id := range m.order {
		if r := m.records[id]; fn(r) {
			out = append(out, r)
		}
	}
	return out
}

func (m *memStore) source(family, file string) (SourceState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sources[family+"|"+file]
	return s, ok
}

type sessionCall struct {
	bootLocal time.Time
	reason    string
}

type fakeSessions struct {
	mu    sync.Mutex
	start time.Time // the recorded session's local start
	ended bool
	calls []sessionCall
}

func (f *fakeSessions) EndADMSessionBefore(_ context.Context, _, _ int64, bootLocal time.Time, reason, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sessionCall{bootLocal, reason})
	if f.ended || !f.start.Before(bootLocal) {
		return false, nil
	}
	f.ended = true
	return true, nil
}

func (f *fakeSessions) endedBy() (bool, []sessionCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, append([]sessionCall(nil), f.calls...)
}

func testConfig() Config {
	return Config{
		GuildID: 7, ServerID: 1, ServiceID: "svc",
		ListEvery: 10 * time.Millisecond, Tick: 5 * time.Millisecond, OpTimeout: 150 * time.Millisecond,
		MaxBackoff: 60 * time.Millisecond, HealthLogEvery: time.Hour,
		Policies: []FamilyPolicy{
			{Family: FamilyRPT, ProbeEvery: 25 * time.Millisecond},
			{Family: FamilyRestart, ProbeEvery: 25 * time.Millisecond},
			{Family: FamilyScript, ProbeEvery: 25 * time.Millisecond},
			{Family: FamilyCrash, ProbeEvery: 25 * time.Millisecond},
		},
	}
}

func startSupervisor(t *testing.T, sup *Supervisor) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	return func() { cancel(); <-done }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func category(c string) func(StoredRecord) bool {
	return func(r StoredRecord) bool { return r.Category == c }
}

// --- tests ---------------------------------------------------------------------------------------

// Requirement 3: new bytes are found by direct reads even when the Nitrado listing is stale, and
// bytes that existed before Champion watched the file are BACKFILL, not new events.
func TestWatcherFindsNewBytesDespiteStaleListing(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	remote.set(admA, "AdminLog started on 2026-09-24 at 04:15:07\n")
	remote.set(rptA, rptHeader+" 4:15:07.592 Localization not present: STR_DATE_FORMAT_SHORT\n")
	remote.freezeListing(rptA) // the listing never shows growth again
	sup := NewSupervisor(testConfig(), remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()

	waitFor(t, "initial RPT records", func() bool { return len(store.find(category(CategoryLocalization))) == 1 })
	for _, r := range store.find(func(r StoredRecord) bool { return r.Family == FamilyRPT }) {
		if r.Delivery != DeliveryBackfill {
			t.Fatalf("pre-existing bytes must be BACKFILL: %+v", r)
		}
	}
	remote.appendTo(rptA, " 5:21:14.888 [Server] :: termination in: 5\n")
	waitFor(t, "countdown via direct read", func() bool { return len(store.find(category(CategoryShutdownCountdown))) == 1 })
	got := store.find(category(CategoryShutdownCountdown))[0]
	if got.Delivery != DeliveryLive || got.VisibleAfter == nil || got.SourceID != "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT" {
		t.Fatalf("appended line must be LIVE with a visibility window and canonical source: %+v", got)
	}
	if got.Payload["secondsRemaining"] != "5" || got.SourceLocalTime == nil || got.SourceLocalTime.Format("15:04:05") != "05:21:14" {
		t.Fatalf("parsed fields: %+v", got)
	}
	waitFor(t, "listing lag diagnostic", func() bool {
		for _, h := range sup.Snapshot().Sources {
			if h.Family == FamilyRPT && h.ListingBehindBytes > 0 && h.State == StateFresh {
				return true
			}
		}
		return false
	})
}

// Requirements 1, 2: after a restart the new boot's files are attached, the old file is drained to
// its last line, and the previous boot's session is ended on written evidence.
func TestNewBootRotatesSourceDrainsOldAndEndsSession(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	sessions := &fakeSessions{start: time.Date(2026, 9, 24, 4, 15, 7, 0, time.UTC)}
	remote.set(admA, "x\n")
	remote.set(rptA, rptHeader)
	remote.set(rptB, "")
	remote.setHidden(rptB, true)
	sup := NewSupervisor(testConfig(), remote, store, sessions)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "boot A header", func() bool { return len(store.find(category(CategoryBootStarted))) == 1 })

	// Boot A shuts down; boot B's RPT appears in the same listing refresh.
	remote.appendTo(rptA, " 5:21:24.103  --- Termination successfully completed --- \n")
	remote.set(rptB, "Current time:  2026/09/24 05:23:05\nVersion 1.29.163709\n 5:23:05.532 Initializing stats manager.\n")
	remote.setHidden(rptB, false)

	waitFor(t, "boot B attached", func() bool {
		s, ok := store.source(FamilyRPT, "dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.RPT")
		return ok && s.Active && s.Checkpoint > 0
	})
	if n := len(store.find(category(CategoryShutdownComplete))); n != 1 {
		t.Fatalf("the old file's final line must be drained exactly once, got %d", n)
	}
	old, _ := store.source(FamilyRPT, "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT")
	if old.Active {
		t.Fatal("the previous boot's source must be retired")
	}
	for _, r := range store.find(func(r StoredRecord) bool { return r.BootID == "2026-09-24T05:23:05" }) {
		if r.Delivery != DeliveryLive {
			t.Fatalf("a file that appeared while watched is live from byte 0: %+v", r)
		}
	}
	ended, calls := sessions.endedBy()
	if !ended {
		t.Fatalf("session A must be ended; calls=%+v", calls)
	}
	var reasons []string
	for _, c := range calls {
		reasons = append(reasons, c.reason)
	}
	if !strings.Contains(strings.Join(reasons, ","), "newer_boot_file:RPT") && !strings.Contains(strings.Join(reasons, ","), "rpt_shutdown_complete") {
		t.Fatalf("session must end on written evidence: %v", reasons)
	}
	// Boot B's own file must never end boot B: its evidence time is two minutes before its stamp.
	for _, c := range calls {
		if c.reason == "newer_boot_file:RPT" && !c.bootLocal.Before(time.Date(2026, 9, 24, 5, 23, 5, 0, time.UTC)) {
			t.Fatalf("newer-boot evidence must carry the boot margin: %+v", c)
		}
	}
}

// Requirement 9 (family level): a stalled RPT read cannot delay restart.log, and shows FAILING.
func TestStalledFamilyDoesNotBlockOtherFamilies(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	remote.set(admA, "x\n")
	remote.set(rptA, rptHeader)
	remote.setBlock(rptA, true)
	remote.set(restartLog, "Thu, 24 Sep 2026 04:13:32 -0400 Server restart requested (WINDOWS)\n")
	sup := NewSupervisor(testConfig(), remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()

	waitFor(t, "restart.log history", func() bool { return len(store.find(category(CategoryRestartRequested))) == 1 })
	remote.appendTo(restartLog, "Thu, 24 Sep 2026 04:14:31 -0400 [dayzps] [DayZTypesLimiter] No changes to types.xml required\n")
	waitFor(t, "restart.log keeps flowing", func() bool { return len(store.find(category(CategoryPreStartCheck))) == 1 })
	waitFor(t, "RPT marked FAILING", func() bool {
		for _, h := range sup.Snapshot().Sources {
			if h.Family == FamilyRPT && h.State == StateFailing && strings.HasPrefix(h.LastError, "read_failed: timeout") {
				return true
			}
		}
		return false
	})
	if len(store.find(func(r StoredRecord) bool { return r.Family == FamilyRPT })) != 0 {
		t.Fatal("a stalled source must not produce records")
	}
	// Recovery: once the stall clears, the RPT catches up from its checkpoint.
	remote.setBlock(rptA, false)
	waitFor(t, "RPT recovers", func() bool { return len(store.find(category(CategoryBootStarted))) == 1 })
}

// Requirement 6 + checkpoint integrity: a failed commit never advances the checkpoint, the retry
// persists every record exactly once, and UNKNOWN lines are kept with source identity and offset.
func TestCommitFailureRetriesWithoutLossOrDuplicates(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	store.failNext = 3
	content := rptHeader + " 4:15:30.991 Warning Message: Cannot open object bmp.p3d\n 5:21:24.200 Something new and unrecognized\n"
	remote.set(admA, "x\n")
	remote.set(rptA, content)
	sup := NewSupervisor(testConfig(), remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "commit after failures", func() bool {
		s, ok := store.source(FamilyRPT, "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT")
		return ok && s.Checkpoint == int64(len(content))
	})
	all := store.find(func(r StoredRecord) bool { return r.Family == FamilyRPT })
	if len(all) != 4 { // boot, version header, missing model, unknown
		t.Fatalf("every complete line exactly once, got %d", len(all))
	}
	unknown := store.find(category(CategoryUnknown))
	if len(unknown) != 1 || unknown[0].Offset != int64(len(content)) || unknown[0].Status != StatusUnknown || unknown[0].Evidence == "" {
		t.Fatalf("UNKNOWN kept with its byte offset: %+v", unknown)
	}
}

// A trailing partial line is not consumed until DayZ finishes writing it.
func TestPartialLineWaitsForItsNewline(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	remote.set(admA, "x\n")
	remote.set(rptA, rptHeader+" 5:21:14.888 [Server] :: termi")
	sup := NewSupervisor(testConfig(), remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "header", func() bool { return len(store.find(category(CategoryBootStarted))) == 1 })
	time.Sleep(80 * time.Millisecond)
	if n := len(store.find(category(CategoryShutdownCountdown))); n != 0 {
		t.Fatal("a partial line must not be parsed")
	}
	s, _ := store.source(FamilyRPT, "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT")
	if s.Checkpoint != int64(len(rptHeader)) {
		t.Fatalf("checkpoint must stop at the last newline: %d", s.Checkpoint)
	}
	remote.appendTo(rptA, "nation in: 5\n")
	waitFor(t, "completed line", func() bool { return len(store.find(category(CategoryShutdownCountdown))) == 1 })
}

// A Champion restart resumes from the durable checkpoint: nothing is re-ingested, and bytes written
// while it was down are ingested once.
func TestResumeFromDurableCheckpoint(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	remote.set(admA, "x\n")
	remote.set(rptA, rptHeader)
	stop := startSupervisor(t, NewSupervisor(testConfig(), remote, store, nil))
	waitFor(t, "first run", func() bool { return len(store.find(category(CategoryBootStarted))) == 1 })
	stop()
	before := len(store.find(func(StoredRecord) bool { return true }))
	remote.appendTo(rptA, " 5:21:20.713 ENGINE       : Destroying game\n")
	stop = startSupervisor(t, NewSupervisor(testConfig(), remote, store, nil))
	defer stop()
	waitFor(t, "resumed", func() bool { return len(store.find(category(CategoryEngineDestroy))) == 1 })
	time.Sleep(60 * time.Millisecond)
	if after := len(store.find(func(StoredRecord) bool { return true })); after != before+1 {
		t.Fatalf("resume must add exactly the new line: before=%d after=%d", before, after)
	}
}

// A file truncated or replaced in place restarts from byte 0 instead of addressing stale offsets.
func TestTruncatedSourceRestartsFromZero(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	remote.set(admA, "x\n")
	remote.set(restartLog, "Thu, 24 Sep 2026 04:13:32 -0400 Server restart requested (WINDOWS)\nThu, 24 Sep 2026 04:14:31 -0400 [dayzps] [DayZTypesLimiter] No changes to types.xml required\n")
	sup := NewSupervisor(testConfig(), remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "history", func() bool { return len(store.find(category(CategoryPreStartCheck))) == 1 })
	remote.set(restartLog, "Thu, 24 Sep 2026 05:22:27 -0400 Server stop requested (Webinterface)\n")
	waitFor(t, "post-truncation line", func() bool { return len(store.find(category(CategoryStopRequested))) == 1 })
	s, _ := store.source(FamilyRestart, "restart.log")
	if s.Checkpoint != int64(len("Thu, 24 Sep 2026 05:22:27 -0400 Server stop requested (Webinterface)\n")) {
		t.Fatalf("checkpoint after truncation: %d", s.Checkpoint)
	}
}

// Requirements 2, 5: restart.log teaches the UTC offset; a line read long after it happened is
// BACKFILL; a pre-start check (the old DayZ process is gone) ends the running session while a
// restart REQUEST alone does not.
func TestRestartLogClockLatenessAndSessionEvidence(t *testing.T) {
	remote, store := newFakeRemote(), newMemStore()
	zone := time.FixedZone("server", -4*3600)
	now := time.Now().In(zone)
	sessionStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.UTC).Add(-30 * time.Minute)
	sessions := &fakeSessions{start: sessionStart}
	remote.set(admA, "x\n")
	remote.set(restartLog, "")
	sup := NewSupervisor(testConfig(), remote, store, sessions)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "restart.log attached", func() bool { _, ok := store.source(FamilyRestart, "restart.log"); return ok })

	stamp := func(t time.Time) string { return t.Format("Mon, 02 Jan 2006 15:04:05 -0700") }
	remote.appendTo(restartLog, stamp(now.Add(-2*time.Hour))+" Server restart requested (WINDOWS)\n")
	waitFor(t, "late request", func() bool { return len(store.find(category(CategoryRestartRequested))) == 1 })
	if r := store.find(category(CategoryRestartRequested))[0]; r.Delivery != DeliveryBackfill {
		t.Fatalf("a line describing an event two hours old is BACKFILL even if newly read: %+v", r)
	}
	if ended, _ := sessions.endedBy(); ended {
		t.Fatal("a restart request alone must not end the session")
	}
	remote.appendTo(restartLog, stamp(now)+" [dayzps] [DayZTypesLimiter] No changes to types.xml required\n")
	waitFor(t, "pre-start check", func() bool { return len(store.find(category(CategoryPreStartCheck))) == 1 })
	r := store.find(category(CategoryPreStartCheck))[0]
	if r.Delivery != DeliveryLive {
		t.Fatalf("a current pre-start line is LIVE: %+v", r)
	}
	waitFor(t, "session ended", func() bool { e, _ := sessions.endedBy(); return e })
	_, calls := sessions.endedBy()
	last := calls[len(calls)-1]
	if last.reason != "restart_log_pre_start_check" || last.bootLocal.Format("15:04:05") != now.Format("15:04:05") {
		t.Fatalf("session end must cite the pre-start line's server-local time: %+v", last)
	}
	store.mu.Lock()
	off := store.offset
	store.mu.Unlock()
	if off == nil || *off != -240 {
		t.Fatalf("UTC offset learned from restart.log: %v", off)
	}
	snap := sup.Snapshot()
	if snap.UTCOffsetMinutes == nil || *snap.UTCOffsetMinutes != -240 {
		t.Fatalf("snapshot offset: %+v", snap.UTCOffsetMinutes)
	}
}

// The latency window only counts LIVE records and never invents a sample.
func TestLatencySummaryIsMeasuredOnly(t *testing.T) {
	var w latencyWindow
	if s := w.summary(); s.Samples != 0 || s.EventSamples != 0 || s.EventToDetectP50Sec != 0 {
		t.Fatalf("empty window: %+v", s)
	}
	w.add([]time.Duration{2 * time.Second, 4 * time.Second, 30 * time.Second}, []time.Duration{time.Second}, 15*time.Millisecond)
	s := w.summary()
	if s.EventSamples != 3 || s.EventToDetectP50Sec != 4 || s.EventToDetectMaxSec != 30 || s.VisibleSamples != 1 || s.DetectToPersistP50Ms != 15 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestDiscoverDirsListsOnlyObservedLayout(t *testing.T) {
	dirs := discoverDirs([]nitrado.LogFile{
		{Path: admA},
		{Path: "/games/svc_2/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"},
		{Path: "/games/svc_2/ftproot/dayzps/config/script_2026-09-24_04-15-10.log"},
	})
	want := []string{"/games/svc_2/ftproot", "/games/svc_2/ftproot/dayzps/config", cfgDir}
	if strings.Join(dirs, ",") != strings.Join(want, ",") {
		t.Fatalf("dirs: %v", dirs)
	}
}
