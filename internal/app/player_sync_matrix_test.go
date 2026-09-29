package app

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Player synchronization matrix (P0 staging verification, section 3): real
// ADM files parsed by the real engine, a scripted Nitrado, and the real
// counter loop. Count, source, confidence (Known) and freshness are asserted
// independently for every scenario, and the whole matrix is logged as a table.

const admDir = "/games/svc/noftp/dayzps/config"

// admFiles serves several ADM files (boots) to a real engine; files can grow.
type admFiles struct {
	mu    sync.Mutex
	files map[string]string
	mtime map[string]time.Time
}

func newADMFiles() *admFiles {
	return &admFiles{files: map[string]string{}, mtime: map[string]time.Time{}}
}

func (a *admFiles) put(name, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[admDir+"/"+name] = content
	a.mtime[admDir+"/"+name] = time.Now()
}

func (a *admFiles) append(name, more string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[admDir+"/"+name] += more
	a.mtime[admDir+"/"+name] = time.Now()
}

func (a *admFiles) entries() []nitrado.LogFile {
	var out []nitrado.LogFile
	for p, c := range a.files {
		out = append(out, nitrado.LogFile{Name: path.Base(p), Path: p, Directory: admDir, Size: int64(len(c)), Modified: a.mtime[p], Type: "ADM"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

func (a *admFiles) ListLogs(context.Context, string) ([]nitrado.LogFile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entries(), nil
}

func (a *admFiles) ListLogsInDir(_ context.Context, _ string, _ string) ([]nitrado.LogFile, error) {
	return a.ListLogs(context.Background(), "")
}

func (a *admFiles) StatFile(_ context.Context, _ string, p string) (*nitrado.LogFile, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries() {
		if e.Path == p {
			e := e
			return &e, nil
		}
	}
	return nil, &nitrado.RequestError{Op: "stat", Kind: nitrado.KindNotFound, StatusCode: 404}
}

func (a *admFiles) ReadLog(_ context.Context, _ string, p string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.files[p]
	if !ok {
		return nil, &nitrado.RequestError{Op: "read", Kind: nitrado.KindNotFound, StatusCode: 404}
	}
	return []byte(c), nil
}

const (
	bootA  = "DayZServer_PS4_x64_2026-09-24_08-08-14.ADM"
	bootB  = "DayZServer_PS4_x64_2026-09-24_09-17-09.ADM"
	hdrA   = "AdminLog started on 2026-09-24 at 08:08:14\n"
	hdrB   = "AdminLog started on 2026-09-24 at 09:17:09\n"
	alice  = `08:10:00 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>) is connected` + "\n"
	aliceX = `08:12:00 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>) has been disconnected` + "\n"
	bob    = `08:10:30 | Player "Bob" (id=b002 pos=<4.0, 5.0, 6.0>) is connected` + "\n"
	carol  = `09:20:00 | Player "Carol" (id=c003 pos=<7.0, 8.0, 9.0>) is connected` + "\n"
	// aliceBack is a real reconnect: a later line. (Re-appending the original
	// 08:10:00 connect line is a replay, which the engine's deduplicator drops.)
	aliceBack = `08:13:00 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>) is connected` + "\n"
)

func snapshotOf(names ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "08:15:00 | ##### PlayerList log: %d players\n", len(names))
	for i, n := range names {
		fmt.Fprintf(&b, "08:15:00 | Player \"%s\" (id=%s pos=<1.0, 2.0, 3.0>)\n", n, []string{"a001", "b002", "c003"}[i])
	}
	b.WriteString("08:15:00 | #####\n")
	return b.String()
}

type syncRow struct {
	scenario, source, admState, freshness string
	count                                 string
	known                                 bool
}

func TestPlayerSynchronizationMatrix(t *testing.T) {
	var rows []syncRow
	record := func(name string, h *counterHarness) onlineCounterStatus {
		st := h.app.OnlineCounterStatus()
		count := "unknown"
		if st.Reading.Known {
			count = fmt.Sprint(st.Reading.Count)
		}
		fresh := "evaluated " + time.Since(st.EvaluatedAt).Truncate(time.Millisecond).String() + " ago"
		if proven := h.engine.PlayerListStats().LastCompleteSnapshotAt; !proven.IsZero() {
			fresh += ", ADM list " + time.Since(proven).Truncate(time.Millisecond).String() + " old"
		}
		rows = append(rows, syncRow{name, st.Source, st.ADMState, fresh, count, st.Reading.Known})
		return st
	}
	expect := func(t *testing.T, st onlineCounterStatus, known bool, count int, source string) {
		t.Helper()
		if st.Reading.Known != known || (known && st.Reading.Count != count) || st.Source != source {
			t.Fatalf("got known=%v count=%d source=%s; want known=%v count=%d source=%s", st.Reading.Known, st.Reading.Count, st.Source, known, count, source)
		}
		if st.EvaluatedAt.IsZero() || time.Since(st.EvaluatedAt) > time.Minute {
			t.Fatalf("stale evaluation timestamp %v", st.EvaluatedAt)
		}
	}
	nitradoDown := errors.New("nitrado: 503")
	withADM := func(t *testing.T, h *counterHarness, files *admFiles, polls int) {
		t.Helper()
		h.engine = killfeed.NewEngine(files, "19806451", killfeed.NewADMParser())
		for i := 0; i < polls; i++ {
			if err := h.engine.PollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		h.app.registerCounterSource(42, counterSource{serviceID: "19806451", live: h.live, engine: h.engine})
	}

	for _, n := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("%d players (Nitrado)", n), func(t *testing.T) {
			h := newCounterHarness("")
			h.live.set(started(n), nil)
			h.eval(t)
			expect(t, record(fmt.Sprintf("%d players", n), h), true, n, counterSourceNitrado)
		})
	}

	t.Run("duplicate connections", func(t *testing.T) {
		h := newCounterHarness("")
		files := newADMFiles()
		files.put(bootA, hdrA+alice+alice+bob+bob+snapshotOf("Alice", "Bob")+alice)
		withADM(t, h, files, 3)
		h.live.set(nitrado.GameserverLive{}, nitradoDown)
		h.eval(t)
		expect(t, record("duplicate connect lines (ADM only)", h), true, 2, counterSourceADMPlayerList)
	})

	t.Run("disconnect and reconnect", func(t *testing.T) {
		h := newCounterHarness("")
		files := newADMFiles()
		files.put(bootA, hdrA+alice+bob+snapshotOf("Alice", "Bob"))
		withADM(t, h, files, 3)
		files.append(bootA, aliceX)
		_ = h.engine.PollOnce(context.Background())
		h.live.set(nitrado.GameserverLive{}, nitradoDown)
		h.eval(t)
		expect(t, record("disconnect (ADM only)", h), true, 1, counterSourceADMPlayerList)
		files.append(bootA, aliceBack)
		_ = h.engine.PollOnce(context.Background())
		h.eval(t)
		expect(t, record("reconnect (ADM only)", h), true, 2, counterSourceADMPlayerList)
		// A replayed (identical) connect line is deduplicated: still 2.
		files.append(bootA, aliceBack)
		_ = h.engine.PollOnce(context.Background())
		h.eval(t)
		expect(t, record("replayed reconnect line (ADM only)", h), true, 2, counterSourceADMPlayerList)
	})

	t.Run("server restart recovery", func(t *testing.T) {
		h := newCounterHarness("")
		files := newADMFiles()
		files.put(bootA, hdrA+alice+bob+snapshotOf("Alice", "Bob"))
		h.engine = killfeed.NewEngine(files, "19806451", killfeed.NewADMParser())
		cleared := -1
		h.engine.OnNewBoot(func(n int) { cleared = n })
		if err := h.engine.PollOnce(context.Background()); err != nil { // discovery selects A
			t.Fatal(err)
		}
		// Restart: boot B appears; the next poll's boot scan drains A and selects B.
		files.put(bootB, hdrB)
		for i := 0; i < 3; i++ {
			if err := h.engine.PollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if cleared != 2 {
			t.Fatalf("expected the previous boot's 2 players cleared, got %d", cleared)
		}
		h.app.registerCounterSource(42, counterSource{serviceID: "19806451", live: h.live, engine: h.engine})
		h.live.set(nitrado.GameserverLive{}, nitradoDown)
		h.eval(t)
		expect(t, record("restart, before reconnects (ADM only)", h), true, 0, counterSourceADMBootReset)
		files.append(bootB, carol)
		for i := 0; i < 2; i++ {
			_ = h.engine.PollOnce(context.Background())
		}
		h.eval(t)
		expect(t, record("restart, one reconnect (ADM only)", h), true, 1, counterSourceADMBootReset)
		h.live.set(started(1), nil)
		h.eval(t)
		expect(t, record("restart, Nitrado back", h), true, 1, counterSourceNitrado)
	})

	t.Run("missing ADM data", func(t *testing.T) {
		h := newCounterHarness("")
		// Worker registered but no ADM read yet; Nitrado fine.
		h.live.set(started(3), nil)
		h.eval(t)
		expect(t, record("no ADM data, Nitrado fine", h), true, 3, counterSourceNitrado)
		// Both missing: unknown, never 0.
		h.live.set(nitrado.GameserverLive{}, nitradoDown)
		h.eval(t)
		st := record("no ADM data, Nitrado down", h)
		expect(t, st, false, 0, counterSourceUnknown)
		if !st.Held {
			t.Fatal("the last known value must be held, not replaced by 0")
		}
	})

	t.Run("stale Nitrado data", func(t *testing.T) {
		h := newCounterHarness("")
		h.live.set(started(5), nil)
		h.eval(t)
		h.waitName(t, "🟢・Online: 5/18")
		// Nitrado stops answering: the old 5 is never re-used as a reading.
		h.live.set(nitrado.GameserverLive{}, nitradoDown)
		h.eval(t)
		st := record("Nitrado previously 5, now failing", h)
		expect(t, st, false, 0, counterSourceUnknown)
		// A response for another service is refused too.
		h.live.set(nitrado.GameserverLive{ServiceID: 999, Status: "started", Slots: 18, PlayerCurrent: intp(7), PlayerMax: 18}, nil)
		h.eval(t)
		st = record("Nitrado answer for another service", h)
		expect(t, st, false, 0, counterSourceUnknown)
		if !st.NitradoWrongService {
			t.Fatal("expected the wrong-service response to be flagged")
		}
		// Server restarting: query block empty, not a count.
		h.live.set(nitrado.GameserverLive{ServiceID: 19806451, Status: "restarting", Slots: 18}, nil)
		h.eval(t)
		expect(t, record("Nitrado restarting (no query)", h), false, 0, counterSourceUnknown)
	})

	t.Run("conflicting Nitrado and ADM", func(t *testing.T) {
		h := newCounterHarness("")
		files := newADMFiles()
		files.put(bootA, hdrA+alice+snapshotOf("Alice"))
		withADM(t, h, files, 3)
		h.live.set(started(2), nil)
		h.eval(t)
		st := record("Nitrado 2 vs ADM list 1", h)
		expect(t, st, true, 2, counterSourceNitrado)
		if st.Disagreement == nil || st.Disagreement.ADMCount != 1 || st.Disagreement.NitradoCount != 2 {
			t.Fatalf("expected a recorded disagreement, got %+v", st.Disagreement)
		}
	})

	var b strings.Builder
	fmt.Fprintf(&b, "\n%-42s | %-7s | %-5s | %-22s | %-18s | %s\n", "scenario", "count", "known", "source", "ADM evidence", "freshness")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-42s | %-7s | %-5v | %-22s | %-18s | %s\n", r.scenario, r.count, r.known, r.source, r.admState, r.freshness)
	}
	t.Log(b.String())
}
