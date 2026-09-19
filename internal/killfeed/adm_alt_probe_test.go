package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

const (
	probeOldKill = `16:40:12 | Player "oldvictim" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "oldkiller" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
	probeNewKill = `16:55:40 | Player "newvictim" (DEAD) (id=v2 pos=<1.0, 2.0, 3.0>) killed by Player "newkiller" (id=k2 pos=<4.0, 5.0, 6.0>) with AKM from 12.5 meters`
)

// staleProductionEngine reproduces the production state: A is selected and
// proven stale, and every listed candidate (including the genuinely live B)
// reports unchanged directory metadata, so metadata-only ranking sees only
// stale files and keeps returning to A.
func staleProductionEngine(t *testing.T, extra ...string) (*Engine, *fakeLogSource, *recordingPublisher) {
	t.Helper()
	modified := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	contentA := "A baseline\n"
	contentB := probeOldKill + "\n"

	fake := &fakeLogSource{contentByPath: map[string][]byte{"/A.ADM": []byte(contentA), "/B.ADM": []byte(contentB)}}
	// A is listed newest, exactly like the production symptom.
	fake.logs = []nitrado.LogFile{
		{Name: "A.ADM", Path: "/A.ADM", Size: int64(len(contentA)), Modified: modified.Add(2 * time.Minute), Type: "ADM"},
		{Name: "B.ADM", Path: "/B.ADM", Size: int64(len(contentB)), Modified: modified, Type: "ADM"},
	}
	for i, name := range extra {
		path := "/" + name
		fake.contentByPath[path] = []byte("static " + name + "\n")
		fake.logs = append(fake.logs, nitrado.LogFile{Name: name, Path: path, Size: 10, Modified: modified.Add(-time.Duration(i+1) * time.Minute), Type: "ADM"})
	}

	engine := NewEngine(fake, "svc-1", NewADMParser())
	pub := &recordingPublisher{}
	engine.SetKillPublisher(pub)
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil { // discovery selects A (newest)
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read of A
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != "/A.ADM" {
		t.Fatalf("expected A selected initially, got %q", got)
	}
	return engine, fake, pub
}

// forceStaleRound makes the next poll give up on the selected source and run
// discovery, clearing the throttles a real 60s wait would clear.
func forceStaleRound(t *testing.T, engine *Engine) {
	t.Helper()
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	engine.lastStaleRediscoveryAt = time.Time{}
	engine.lastStaleProbeAt = time.Time{}
	engine.lastAltProbeAt = time.Time{}
	if err := engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStaleSelectionSwitchesToCandidateWhoseContentGrows(t *testing.T) {
	engine, fake, pub := staleProductionEngine(t)

	forceStaleRound(t, engine) // round 1: baseline only
	if got := engine.Stats().SelectedPath; got != "/A.ADM" {
		t.Fatalf("a baseline read must not switch sources, got %q", got)
	}
	if len(pub.kills) != 0 {
		t.Fatalf("history in the baseline must not publish, got %d kills", len(pub.kills))
	}

	// B is really being written even though its directory metadata never moves.
	fake.contentByPath["/B.ADM"] = []byte(probeOldKill + "\n" + probeNewKill + "\n")
	forceStaleRound(t, engine) // round 2: growth observed

	if got := engine.Stats().SelectedPath; got != "/B.ADM" {
		t.Fatalf("expected the growing candidate to be selected, got %q", got)
	}
	if engine.selectionReason != "direct_probe_growth" {
		t.Fatalf("unexpected selection reason %q", engine.selectionReason)
	}
	if len(pub.kills) != 1 || pub.kills[0].Victim == nil || pub.kills[0].Victim.Name != "newvictim" {
		t.Fatalf("expected exactly the new kill to be published (history skipped), got %d: %+v", len(pub.kills), pub.kills)
	}
	if want := int64(len(probeOldKill+"\n") + len(probeNewKill+"\n")); engine.tracker.LastByteOffset != want {
		t.Fatalf("expected checkpoint at EOF %d, got %d", want, engine.tracker.LastByteOffset)
	}
}

func TestStaleSelectionRetainedWhenNoCandidateGrows(t *testing.T) {
	engine, _, pub := staleProductionEngine(t)
	for i := 0; i < 3; i++ {
		forceStaleRound(t, engine)
	}
	if got := engine.Stats().SelectedPath; got != "/A.ADM" {
		t.Fatalf("static candidates must never displace the retained source, got %q", got)
	}
	if len(pub.kills) != 0 {
		t.Fatalf("no kills expected, got %d", len(pub.kills))
	}
}

func TestSwitchIgnoresGrowthWithoutACompleteLine(t *testing.T) {
	engine, fake, _ := staleProductionEngine(t)
	forceStaleRound(t, engine)
	fake.contentByPath["/B.ADM"] = []byte(probeOldKill + "\n" + "16:55:4") // partial write only
	forceStaleRound(t, engine)
	if got := engine.Stats().SelectedPath; got != "/A.ADM" {
		t.Fatalf("a partial line is not proof of activity, got %q", got)
	}
}

func TestAlternativeProbeIsBoundedAndThrottled(t *testing.T) {
	engine, fake, _ := staleProductionEngine(t, "C.ADM", "D.ADM", "E.ADM", "F.ADM")
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	engine.lastStaleRediscoveryAt = time.Time{}
	engine.lastAltProbeAt = time.Time{}
	fake.reads = 0
	if err := engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One read for the selected source's own stale probe, plus at most
	// maxAlternativeProbes alternatives.
	if fake.reads > 1+maxAlternativeProbes {
		t.Fatalf("expected at most %d reads, got %d", 1+maxAlternativeProbes, fake.reads)
	}
	before := fake.reads
	if got := engine.probeAlternatives(context.Background(), engine.rankCandidates(fake.logs, time.Now())); got != nil {
		t.Fatal("unexpected switch")
	}
	if fake.reads != before {
		t.Fatalf("a second probe within the interval must not read again (%d -> %d)", before, fake.reads)
	}
}

func TestSwitchTreatsLaggingMetadataAsNotATruncation(t *testing.T) {
	engine, fake, _ := staleProductionEngine(t)
	forceStaleRound(t, engine)
	fake.contentByPath["/B.ADM"] = []byte(probeOldKill + "\n" + probeNewKill + "\n")
	forceStaleRound(t, engine)
	if got := engine.Stats().SelectedPath; got != "/B.ADM" {
		t.Fatalf("expected B selected, got %q", got)
	}
	offset := engine.tracker.LastByteOffset
	lines := engine.linesDiscovered

	// Directory metadata still reports the tiny old size, below our offset.
	for i := range fake.logs {
		if fake.logs[i].Path == "/B.ADM" {
			fake.logs[i].Size = 5
			fake.logs[i].Modified = fake.logs[i].Modified.Add(time.Second)
		}
	}
	if err := engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.tracker.LastByteOffset != offset {
		t.Fatalf("lagging metadata must not reset the checkpoint (%d -> %d)", offset, engine.tracker.LastByteOffset)
	}
	if engine.linesDiscovered != lines {
		t.Fatalf("history was replayed: %d lines discovered after the switch, %d before", engine.linesDiscovered, lines)
	}
}

// TestRestartRotationMigratesToNewActiveFile is the DayZ restart case: the old
// ADM goes static, a new ADM appears (UNKNOWN, so it wins immediately) and is
// followed as it grows.
func TestRestartRotationMigratesToNewActiveFile(t *testing.T) {
	engine, fake, pub := staleProductionEngine(t)
	newer := time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC)
	fake.contentByPath["/C.ADM"] = []byte(probeNewKill + "\n")
	fake.logs = append([]nitrado.LogFile{{Name: "C.ADM", Path: "/C.ADM", Size: int64(len(probeNewKill) + 1), Modified: newer, Type: "ADM"}}, fake.logs...)
	forceStaleRound(t, engine)
	if got := engine.Stats().SelectedPath; got != "/C.ADM" {
		t.Fatalf("expected the new ADM to be adopted, got %q", got)
	}
	if err := engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected the new file's kill once, got %d", len(pub.kills))
	}
}
