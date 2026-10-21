package livesync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Give-up tests: a file Nitrado can never serve (observed: one old crash log answering 500 for
// hours) must stop being read, without touching a live source or another family. They drive the
// watcher step by step on a fake clock, so every delay is exact.

const (
	admB   = cfgDir + "/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM"
	crashA = cfgDir + "/crash_2026-09-24_04-15-10.log"
	crashB = cfgDir + "/crash_2026-09-24_05-23-09.log"
	// crashAID is the mount-independent identity of crashA.
	crashAID = "dayzps/config/crash_2026-09-24_04-15-10.log"
	crashBID = "dayzps/config/crash_2026-09-24_05-23-09.log"
)

// brokenRemote fails every read of the paths in broken, like a file Nitrado answers 500 for.
type brokenRemote struct {
	*fakeRemote
	mu     sync.Mutex
	broken map[string]bool
}

func (b *brokenRemote) setBroken(p string, v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.broken[p] = v
}

func (b *brokenRemote) ReadLog(ctx context.Context, svc, p string) ([]byte, error) {
	b.mu.Lock()
	bad := b.broken[p]
	b.mu.Unlock()
	if bad {
		b.fakeRemote.mu.Lock()
		b.fakeRemote.reads[p]++
		b.fakeRemote.mu.Unlock()
		return nil, errors.New("nitrado 500")
	}
	return b.fakeRemote.ReadLog(ctx, svc, p)
}

type giveUpRig struct {
	t      *testing.T
	remote *brokenRemote
	sup    *Supervisor
	now    time.Time
	ctx    context.Context
}

func newGiveUpRig(t *testing.T, policies []FamilyPolicy) *giveUpRig {
	t.Helper()
	r := &giveUpRig{t: t, remote: &brokenRemote{fakeRemote: newFakeRemote(), broken: map[string]bool{}},
		now: time.Date(2026, 9, 24, 6, 0, 0, 0, time.UTC), ctx: context.Background()}
	cfg := Config{GuildID: 7, ServerID: 1, ServiceID: "svc", MaxBackoff: 10 * time.Minute, GiveUpAfter: 4,
		GiveUpRetryEvery: 6 * time.Hour, Policies: policies, Now: func() time.Time { return r.now }}
	r.sup = NewSupervisor(cfg, r.remote, newMemStore(), nil)
	return r
}

func (r *giveUpRig) list() {
	r.t.Helper()
	if !r.sup.lister.refresh(r.ctx) {
		r.t.Fatal("listing failed")
	}
}

func (r *giveUpRig) watcher(family string) *familyWatcher {
	for _, p := range r.sup.cfg.Policies {
		if p.Family == family {
			return newFamilyWatcher(r.sup, p, nil)
		}
	}
	r.t.Fatalf("no policy for %s", family)
	return nil
}

func (r *giveUpRig) health(family string) SourceHealth {
	for _, h := range r.sup.Snapshot().Sources {
		if h.Family == family {
			return h
		}
	}
	r.t.Fatalf("no health for %s", family)
	return SourceHealth{}
}

// failOnce steps the watcher at its next allowed read and returns the delay it then scheduled.
func (r *giveUpRig) failOnce(w *familyWatcher) time.Duration {
	r.t.Helper()
	if w.active != nil && r.now.Before(w.active.nextRead) {
		r.now = w.active.nextRead
	}
	w.step(r.ctx)
	if w.active == nil {
		r.t.Fatal("no active source")
	}
	return w.active.nextRead.Sub(r.now)
}

func crashOnly() []FamilyPolicy {
	return []FamilyPolicy{
		{Family: FamilyRPT, ProbeEvery: 30 * time.Second},
		{Family: FamilyCrash, ProbeEvery: 2 * time.Minute, GiveUpHistorical: true},
	}
}

func TestHistoricalFileIsGivenUpThenRetriedRarelyAndRecovers(t *testing.T) {
	r := newGiveUpRig(t, crashOnly())
	r.remote.set(admA, "")
	r.remote.set(admB, "") // a later boot: crashA's boot is over
	r.remote.set(rptB, rptHeader)
	r.remote.set(crashA, string(fixture(t, "crash_2026-09-24_05-23-09.log")))
	r.remote.setBroken(crashA, true)
	r.list()
	crash, rpt := r.watcher(FamilyCrash), r.watcher(FamilyRPT)

	// Backoff first: 2m, 4m, 8m. The fourth failure gives the file up.
	for i, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
		if got := r.failOnce(crash); got != want {
			t.Fatalf("failure %d: next read in %s, want %s", i+1, got, want)
		}
		if h := r.health(FamilyCrash); h.GaveUp {
			t.Fatalf("gave up after only %d failures", i+1)
		}
	}
	if got := r.failOnce(crash); got != 6*time.Hour {
		t.Fatalf("after giving up the next read is in %s, want 6h", got)
	}
	h := r.health(FamilyCrash)
	wantReason := "gave up on " + crashAID + " after 4 failures: read_failed: error"
	if !h.GaveUp || h.GaveUpReason != wantReason || h.GaveUpAt == nil || !h.GaveUpAt.Equal(r.now) ||
		h.NextRetryAt == nil || !h.NextRetryAt.Equal(r.now.Add(6*time.Hour)) || h.State != StateFailing || h.ConsecutiveFailures != 4 {
		t.Fatalf("health after giving up: %+v", h)
	}
	gaveUpAt := *h.GaveUpAt

	// Nothing reads the file for six hours, however often the watcher ticks - and listing growth
	// does not bypass it.
	r.remote.appendTo(crashA, "more\n")
	r.list()
	for i := 0; i < 500; i++ {
		r.now = r.now.Add(40 * time.Second)
		crash.step(r.ctx)
	}
	if n := r.remote.readCount(crashA); n != 4 {
		t.Fatalf("a given-up file was read %d times, want 4", n)
	}

	// The rare retry: one read, and it stays given up with the original time.
	if got := r.failOnce(crash); got != 6*time.Hour {
		t.Fatalf("after a failed retry the next read is in %s, want 6h", got)
	}
	h = r.health(FamilyCrash)
	if n := r.remote.readCount(crashA); n != 5 || !h.GaveUp || !h.GaveUpAt.Equal(gaveUpAt) || h.ConsecutiveFailures != 5 {
		t.Fatalf("after the retry: reads=%d health=%+v", n, h)
	}

	// Another family on the same server was never affected.
	rpt.step(r.ctx)
	if hr := r.health(FamilyRPT); hr.State != StateFresh || hr.GaveUp || hr.ConsecutiveFailures != 0 {
		t.Fatalf("RPT health: %+v", hr)
	}

	// The file recovers: the next retry reads it and giving up is cleared at once.
	r.remote.setBroken(crashA, false)
	r.now = crash.active.nextRead
	crash.step(r.ctx)
	h = r.health(FamilyCrash)
	if h.GaveUp || h.GaveUpAt != nil || h.GaveUpReason != "" || h.NextRetryAt != nil || h.State != StateFresh ||
		h.ConsecutiveFailures != 0 || h.LastError != "" || h.ReadSize == 0 || crash.active.gaveUp || crash.active.fails != 0 {
		t.Fatalf("health after recovery: %+v", h)
	}
	if got := crash.active.nextRead.Sub(r.now); got != 2*time.Minute {
		t.Fatalf("after recovery the next read is in %s, want the probe interval", got)
	}
}

func TestFileOfTheCurrentBootIsNeverGivenUp(t *testing.T) {
	r := newGiveUpRig(t, crashOnly())
	r.remote.set(admB, "")
	r.remote.set(crashB, "x\n") // same boot as the newest ADM (stamped 4 s later)
	r.remote.setBroken(crashB, true)
	r.list()
	crash := r.watcher(FamilyCrash)
	var last time.Duration
	for i := 0; i < 40; i++ {
		last = r.failOnce(crash)
	}
	h := r.health(FamilyCrash)
	if h.GaveUp || crash.active.gaveUp || last != 10*time.Minute || h.ConsecutiveFailures != 40 || h.State != StateFailing {
		t.Fatalf("current-boot file: last delay %s, health %+v", last, h)
	}

	// Its boot ends (a later boot's ADM is listed): the next failure gives it up.
	r.remote.set(cfgDir+"/DayZServer_PS4_x64_2026-09-24_09-00-00.ADM", "")
	r.list()
	if got := r.failOnce(crash); got != 6*time.Hour || !r.health(FamilyCrash).GaveUp {
		t.Fatalf("after its boot ended: next read in %s, health %+v", got, r.health(FamilyCrash))
	}
}

func TestLiveFamiliesAreNeverGivenUp(t *testing.T) {
	// The production policies: RPT and restart.log end boot sessions and are never given up, even
	// when the RPT's boot looks over (a newer ADM is listed but no newer RPT yet).
	for _, p := range DefaultPolicies() {
		if want := p.Family == FamilyScript || p.Family == FamilyCrash; p.GiveUpHistorical != want {
			t.Fatalf("%s: GiveUpHistorical=%v, want %v", p.Family, p.GiveUpHistorical, want)
		}
	}
	r := newGiveUpRig(t, DefaultPolicies())
	r.remote.set(admA, "")
	r.remote.set(admB, "")
	r.remote.set(rptA, rptHeader)
	r.remote.set(restartLog, "x\n")
	r.remote.setBroken(rptA, true)
	r.remote.setBroken(restartLog, true)
	r.list()
	for _, family := range []string{FamilyRPT, FamilyRestart} {
		w := r.watcher(family)
		var last time.Duration
		for i := 0; i < 40; i++ {
			last = r.failOnce(w)
		}
		if h := r.health(family); h.GaveUp || w.active.gaveUp || last != 10*time.Minute || h.ConsecutiveFailures != 40 {
			t.Fatalf("%s: last delay %s, health %+v", family, last, h)
		}
	}
}

func TestGivenUpFileMakesWayForANewerFile(t *testing.T) {
	r := newGiveUpRig(t, crashOnly())
	r.remote.set(admA, "")
	r.remote.set(admB, "")
	r.remote.set(crashA, "x\n")
	r.remote.setBroken(crashA, true)
	r.list()
	crash := r.watcher(FamilyCrash)
	for i := 0; i < 4; i++ {
		r.failOnce(crash)
	}
	if !r.health(FamilyCrash).GaveUp {
		t.Fatal("not given up")
	}

	// A newer crash log appears: the watcher moves to it without reading the old file again.
	r.remote.set(crashB, string(fixture(t, "crash_2026-09-24_05-23-09.log")))
	r.list()
	r.now = r.now.Add(time.Minute)
	crash.step(r.ctx)
	h := r.health(FamilyCrash)
	if h.SourceFile != crashBID || h.State != StateFresh || h.GaveUp || h.GaveUpReason != "" || h.ConsecutiveFailures != 0 || h.Rotations != 1 {
		t.Fatalf("health after moving on: %+v", h)
	}
	if n := r.remote.readCount(crashA); n != 4 {
		t.Fatalf("the given-up file was read %d times, want 4", n)
	}
	// The old file keeps only its rare retry, and a late recovery still drains it.
	if len(crash.draining) != 1 || !crash.draining[0].gaveUp {
		t.Fatalf("draining: %+v", crash.draining)
	}
	r.now = r.now.Add(5 * time.Hour)
	crash.step(r.ctx)
	if n := r.remote.readCount(crashA); n != 4 {
		t.Fatalf("retried before six hours: %d reads", n)
	}
	r.remote.setBroken(crashA, false)
	r.now = r.now.Add(time.Hour)
	crash.step(r.ctx)
	if n := r.remote.readCount(crashA); n != 5 || len(crash.draining) != 0 {
		t.Fatalf("after recovery: reads=%d draining=%d", n, len(crash.draining))
	}
	if h := r.health(FamilyCrash); h.SourceFile != crashBID || h.State != StateFresh {
		t.Fatalf("the current file's health changed: %+v", h)
	}
}

func TestGivenUpDrainsAreBounded(t *testing.T) {
	r := newGiveUpRig(t, crashOnly())
	w := r.watcher(FamilyCrash)
	for i := 0; i < maxGivenUpDrains+3; i++ {
		w.draining = append(w.draining, &sourceRuntime{gaveUp: true, nextRead: r.now.Add(time.Hour),
			state: SourceState{SourceFile: strings.Repeat("x", i+1)}})
	}
	w.retryDrains(r.ctx, r.now)
	if len(w.draining) != maxGivenUpDrains || w.draining[0].state.SourceFile != "xxxx" {
		t.Fatalf("kept %d given-up drains, first %q", len(w.draining), w.draining[0].state.SourceFile)
	}
}

func TestBackoffDoublesUpToTheCap(t *testing.T) {
	base, max := 2*time.Minute, 10*time.Minute
	for fails, want := range map[int]time.Duration{1: 2 * time.Minute, 2: 4 * time.Minute, 3: 8 * time.Minute, 4: max, 5: max, 500: max} {
		if got := backoff(base, fails, max); got != want {
			t.Fatalf("backoff after %d failures = %s, want %s", fails, got, want)
		}
	}
}
