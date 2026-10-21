package killfeed

import (
	"context"
	"sync"
	"testing"
	"time"
)

type playersHook struct {
	mu     sync.Mutex
	counts []int
}

func (h *playersHook) fn(count int) {
	h.mu.Lock()
	h.counts = append(h.counts, count)
	h.mu.Unlock()
}

func (h *playersHook) last() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.counts) == 0 {
		return 0, false
	}
	return h.counts[len(h.counts)-1], true
}

const (
	bootAHeader = "AdminLog started on 2026-09-24 at 08:08:14\n"
	bootBHeader = "AdminLog started on 2026-09-24 at 09:17:09\n"
	aliceOn     = `08:10:00 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>) is connected` + "\n"
	bobOn       = `08:10:00 | Player "Bob" (id=b002 pos=<4.0, 5.0, 6.0>) is connected` + "\n"
	bobOff      = `08:12:00 | Player "Bob" (id=b002 pos=<4.0, 5.0, 6.0>) has been disconnected` + "\n"
	carolOn     = `09:20:00 | Player "Carol" (id=c003 pos=<7.0, 8.0, 9.0>) is connected` + "\n"
	// A complete snapshot listing Alice and Dave (Dave joined before Champion started reading).
	snapshotAliceDave = "08:15:00 | ##### PlayerList log: 2 players\n" +
		`08:15:00 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>)` + "\n" +
		`08:15:00 | Player "Dave" (id=d004 pos=<1.0, 2.0, 3.0>)` + "\n" +
		"08:15:00 | #####\n"
)

func pollN(t *testing.T, e *Engine, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := e.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// grow appends to a file and advances its listing mtime, like a live ADM.
func grow(f *bootFake, p, add string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bf := f.files[p]
	bf.content = append(bf.content, []byte(add)...)
	bf.modified = at
}

// TestPresenceUnknownIsNotReportedAsKnown: a worker that starts reading an
// ADM mid-session only knows a lower bound (players it saw connect). That
// must be labelled UNKNOWN - the app then never publishes it - until a
// complete PlayerList snapshot confirms who is actually online.
func TestPresenceUnknownIsNotReportedAsKnown(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	pathA := noftpCfg + "/" + bootAName
	f.put(pathA, bootAHeader+aliceOn, t0)
	e := NewEngine(f, "svc", NewADMParser())
	hook := &playersHook{}
	e.OnPlayersChanged(hook.fn)
	pollN(t, e, 3)

	if got := e.PlayerTracker().OnlineCount(); got != 1 {
		t.Fatalf("tracker lower bound: %d", got)
	}
	if ev := e.PresenceEvidence(); ev.Known || ev.State != PresenceUnknown {
		t.Fatalf("presence must be UNKNOWN before any snapshot, got %+v", ev)
	}

	grow(f, pathA, snapshotAliceDave, t0.Add(7*time.Minute))
	pollN(t, e, 1)
	ev := e.PresenceEvidence()
	if !ev.Known || ev.State != PresenceSnapshotConfirmed || ev.EvidenceAt.IsZero() {
		t.Fatalf("expected SNAPSHOT_CONFIRMED, got %+v", ev)
	}
	if got := e.PlayerTracker().OnlineCount(); got != 2 {
		t.Fatalf("snapshot must reconcile to Alice+Dave, got %d", got)
	}
	if last, ok := hook.last(); !ok || last != 2 {
		t.Fatalf("players hook must fire with the confirmed count, got %v %v", last, ok)
	}
}

// TestSnapshotConfirmationPublishesEvenWithoutCountChange: a snapshot that
// agrees with the tracker changes no numbers, but turns the count into a fact
// - the hook must still fire once so the counter is published.
func TestSnapshotConfirmationPublishesEvenWithoutCountChange(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	pathA := noftpCfg + "/" + bootAName
	f.put(pathA, bootAHeader+aliceOn, t0)
	e := NewEngine(f, "svc", NewADMParser())
	hook := &playersHook{}
	e.OnPlayersChanged(hook.fn)
	pollN(t, e, 3)
	before := len(hook.counts)
	grow(f, pathA, "08:15:00 | ##### PlayerList log: 1 players\n"+`08:15:00 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>)`+"\n08:15:00 | #####\n", t0.Add(7*time.Minute))
	pollN(t, e, 1)
	if len(hook.counts) != before+1 {
		t.Fatalf("expected exactly one publish on confirmation, got %d new", len(hook.counts)-before)
	}
	if !e.PresenceEvidence().Known {
		t.Fatal("expected known presence")
	}
}

// TestRestartInvalidatesPreviousSessionPresence: a verified newer boot is a
// server restart, which disconnects everyone. The previous session's players
// must not be carried into the new boot (they used to be, with
// presence_retained=true, until the next snapshot).
func TestRestartInvalidatesPreviousSessionPresence(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	pathA := noftpCfg + "/" + bootAName
	pathB := noftpCfg + "/" + bootBName
	f.put(pathA, bootAHeader+aliceOn+bobOn, t0)
	e := NewEngine(f, "svc", NewADMParser())
	hook := &playersHook{}
	e.OnPlayersChanged(hook.fn)
	pollN(t, e, 3)
	grow(f, pathA, snapshotAliceDave, t0.Add(5*time.Minute)) // confirms Alice + Dave (Bob gone)
	pollN(t, e, 1)
	if got := e.PlayerTracker().OnlineCount(); got != 2 {
		t.Fatalf("setup: %d", got)
	}

	// Restart: boot B appears with only its header.
	f.put(pathB, bootBHeader, t0.Add(69*time.Minute))
	forceScan(e, false)
	pollN(t, e, 1)
	if e.selected == nil || e.selected.Name != bootBName {
		t.Fatalf("boot B must be selected: %+v", e.selected)
	}
	if got := e.PlayerTracker().OnlineCount(); got != 0 {
		t.Fatalf("restart must clear the previous session's presence, still %d online", got)
	}
	ev := e.PresenceEvidence()
	if ev.State != PresenceBootReset || !ev.Known {
		t.Fatalf("expected BOOT_RESET known presence, got %+v", ev)
	}
	if last, _ := hook.last(); last != 0 {
		t.Fatalf("restart must publish 0 (known-empty), got %d", last)
	}

	// New session activity builds presence from scratch.
	grow(f, pathB, carolOn, t0.Add(72*time.Minute))
	pollN(t, e, 2)
	if got := e.PlayerTracker().OnlineCount(); got != 1 {
		t.Fatalf("expected Carol only, got %d", got)
	}
}

// TestRotationWithoutNewerBootRetainsPresence keeps the existing guarantee:
// a mount-alias or same-boot source switch is not a restart.
func TestRotationWithoutNewerBootRetainsPresence(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	pathA := noftpCfg + "/" + bootAName
	f.put(pathA, bootAHeader+aliceOn+snapshotAliceDave, t0)
	e := NewEngine(f, "svc", NewADMParser())
	pollN(t, e, 3)
	// Re-accepting the same boot (e.g. listing-gap retention) is not a restart.
	e.acceptBoot(*e.selected)
	if got := e.PlayerTracker().OnlineCount(); got != 2 {
		t.Fatalf("same-boot reselection must retain presence, got %d", got)
	}
	if e.PresenceEvidence().State != PresenceSnapshotConfirmed {
		t.Fatalf("state changed on same-boot reselection: %+v", e.PresenceEvidence())
	}
}

// TestConnectEventsAloneNeverMakePresenceKnown: after a mid-session start the
// tracker only holds players seen connecting. No amount of connect and
// disconnect lines turns that lower bound into a known count - only a
// complete PlayerList snapshot or a verified restart does. (The counter then
// relies on Nitrado's live query, or shows unknown.)
func TestConnectEventsAloneNeverMakePresenceKnown(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	pathA := noftpCfg + "/" + bootAName
	f.put(pathA, bootAHeader+aliceOn+bobOn+bobOff, t0)
	e := NewEngine(f, "svc", NewADMParser())
	pollN(t, e, 3)
	for i := 0; i < 5; i++ {
		grow(f, pathA, bobOn+bobOff, t0.Add(time.Duration(i+1)*time.Minute))
		pollN(t, e, 1)
	}
	if ev := e.PresenceEvidence(); ev.Known || ev.State != PresenceUnknown {
		t.Fatalf("events alone must leave presence UNKNOWN, got %+v", ev)
	}
	if got := e.PlayerTracker().OnlineCount(); got != 1 {
		t.Fatalf("tracker lower bound should be Alice only, got %d", got)
	}
}

// TestTwoSimultaneousPlayersDuplicatesAndReconnect covers two players joining
// in the same second, duplicate lines, and a disconnect/reconnect cycle.
func TestTwoSimultaneousPlayersDuplicatesAndReconnect(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	pathA := noftpCfg + "/" + bootAName
	f.put(pathA, bootAHeader+aliceOn+bobOn+aliceOn+bobOn, t0) // same-second joins, then duplicates
	e := NewEngine(f, "svc", NewADMParser())
	pollN(t, e, 3)
	if got := e.PlayerTracker().OnlineCount(); got != 2 {
		t.Fatalf("two simultaneous players with duplicate lines: %d", got)
	}
	grow(f, pathA, bobOff+bobOff, t0.Add(time.Minute)) // duplicate disconnect is idempotent
	pollN(t, e, 1)
	if got := e.PlayerTracker().OnlineCount(); got != 1 {
		t.Fatalf("after Bob disconnect: %d", got)
	}
	grow(f, pathA, `08:13:00 | Player "Bob" (id=b002 pos=<4.0, 5.0, 6.0>) is connected`+"\n", t0.Add(2*time.Minute))
	pollN(t, e, 1)
	if got := e.PlayerTracker().OnlineCount(); got != 2 {
		t.Fatalf("after Bob reconnect: %d", got)
	}
}
