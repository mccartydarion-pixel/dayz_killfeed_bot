package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// newRotationFastPathEngine builds an engine already selected on `selected`,
// ready to exercise checkForNewerLog directly - the same fast path
// production hits every rescanInterval while polling, independent of the
// staleGiveUpAfter give-up cycle in pollSelected.
func newRotationFastPathEngine(selected nitrado.LogFile) (*Engine, *fakeLogSource) {
	fake := &fakeLogSource{}
	e := NewEngine(fake, "svc-1", testParser{})
	e.state = StatePolling
	e.selected = &selected
	e.logSourceFound = true
	e.tracker.UpdateCheckpoint(e.serviceID, selected.Path, selected.Size, selected.Modified, selected.Size)
	return e, fake
}

// TestCheckForNewerLogReproducesProductionRegression is the exact production
// failure from live Railway logs (2026-09-17 ~01:47 UTC): A (newest by
// Modified) was proven stale and discovery correctly switched to B; ~13s
// later checkForNewerLog's old "newest wins" rule switched straight back to
// A solely because Nitrado still reported it as newer by Modified time. It
// must now stay on B - a known-stale, evidence-free file cannot win the fast
// path either, not just full discovery.
func TestCheckForNewerLogReproducesProductionRegression(t *testing.T) {
	now := time.Date(2026, 9, 17, 1, 47, 0, 0, time.UTC)
	pathA, pathB := "/A.ADM", "/B.ADM"
	a := nitrado.LogFile{Name: "A.ADM", Path: pathA, Directory: "/", Size: 100, Modified: now} // newest by time
	b := nitrado.LogFile{Name: "B.ADM", Path: pathB, Directory: "/", Size: 50, Modified: now.Add(-time.Hour)}

	e, fake := newRotationFastPathEngine(b)
	// Discovery already proved A stale and switched to B before this fast
	// path runs - exactly the production sequence.
	e.staleMarks = map[string]staleMark{pathA: {At: now, Size: a.Size, Modified: a.Modified}}
	e.candidateHistory = map[string]candidateObservation{
		pathA: {Size: a.Size, Modified: a.Modified},
		pathB: {Size: b.Size, Modified: b.Modified},
	}
	fake.logs = []nitrado.LogFile{a, b}

	e.checkForNewerLog(context.Background())

	if e.selected.Path != pathB {
		t.Fatalf("expected to remain on B; known-stale-but-newer A must not win the fast path, got %q", e.selected.Path)
	}
}

// TestCheckForNewerLogAllowsStaleSourceBackAfterGrowth is scenario B: a
// stale, newer-by-time file that later genuinely grows becomes eligible
// again, even via the fast path - no permanent blacklist.
func TestCheckForNewerLogAllowsStaleSourceBackAfterGrowth(t *testing.T) {
	now := time.Date(2026, 9, 17, 1, 47, 0, 0, time.UTC)
	pathA, pathB := "/A.ADM", "/B.ADM"
	aStale := nitrado.LogFile{Name: "A.ADM", Path: pathA, Directory: "/", Size: 100, Modified: now}
	b := nitrado.LogFile{Name: "B.ADM", Path: pathB, Directory: "/", Size: 50, Modified: now.Add(-time.Hour)}

	e, fake := newRotationFastPathEngine(b)
	e.staleMarks = map[string]staleMark{pathA: {At: now, Size: aStale.Size, Modified: aStale.Modified}}
	e.candidateHistory = map[string]candidateObservation{
		pathA: {Size: aStale.Size, Modified: aStale.Modified},
		pathB: {Size: b.Size, Modified: b.Modified},
	}

	aGrown := aStale
	aGrown.Size = 400
	aGrown.Modified = now.Add(time.Minute)
	fake.logs = []nitrado.LogFile{aGrown, b}

	e.checkForNewerLog(context.Background())

	if e.selected.Path != pathA {
		t.Fatalf("expected to switch back to A once it showed real growth, stayed on %q", e.selected.Path)
	}
}

// TestCheckForNewerLogSwitchesToGrowingRotatedFile is scenario C: a
// genuinely new/rotated candidate that shows real growth must strongly
// outrank a known-stale current source - promptly, via the fast path,
// without waiting for the full staleGiveUpAfter discovery cycle.
func TestCheckForNewerLogSwitchesToGrowingRotatedFile(t *testing.T) {
	now := time.Date(2026, 9, 17, 1, 47, 0, 0, time.UTC)
	pathA, pathC := "/A.ADM", "/C.ADM"
	a := nitrado.LogFile{Name: "A.ADM", Path: pathA, Directory: "/", Size: 100, Modified: now}
	cSmall := nitrado.LogFile{Name: "C.ADM", Path: pathC, Directory: "/", Size: 10, Modified: now.Add(-30 * time.Minute)}

	e, fake := newRotationFastPathEngine(a)
	e.staleMarks = map[string]staleMark{pathA: {At: now, Size: a.Size, Modified: a.Modified}}
	e.candidateHistory = map[string]candidateObservation{
		pathA: {Size: a.Size, Modified: a.Modified},
		pathC: {Size: cSmall.Size, Modified: cSmall.Modified},
	}

	cGrown := cSmall
	cGrown.Size = 500
	cGrown.Modified = now
	fake.logs = []nitrado.LogFile{a, cGrown}

	e.checkForNewerLog(context.Background())

	if e.selected.Path != pathC {
		t.Fatalf("expected the growing rotated file to win promptly, got %q", e.selected.Path)
	}
}

// TestCheckForNewerLogDoesNotFlap is scenario D: once switched, repeated
// poll cycles observing the exact same unchanged candidate set must not keep
// re-switching (A -> B -> A -> B ...).
func TestCheckForNewerLogDoesNotFlap(t *testing.T) {
	now := time.Date(2026, 9, 17, 1, 47, 0, 0, time.UTC)
	pathA, pathB := "/A.ADM", "/B.ADM"
	a := nitrado.LogFile{Name: "A.ADM", Path: pathA, Directory: "/", Size: 100, Modified: now}
	b := nitrado.LogFile{Name: "B.ADM", Path: pathB, Directory: "/", Size: 50, Modified: now.Add(-time.Hour)}

	e, fake := newRotationFastPathEngine(b)
	e.staleMarks = map[string]staleMark{pathA: {At: now, Size: a.Size, Modified: a.Modified}}
	e.candidateHistory = map[string]candidateObservation{
		pathA: {Size: a.Size, Modified: a.Modified},
		pathB: {Size: b.Size, Modified: b.Modified},
	}
	fake.logs = []nitrado.LogFile{a, b}

	for i := 0; i < 4; i++ {
		e.checkForNewerLog(context.Background())
		if e.selected.Path != pathB {
			t.Fatalf("call %d: expected to stay on B without flapping, got %q", i, e.selected.Path)
		}
	}
}

// TestCheckForNewerLogPreservesCheckpointAndDedupe covers scenarios E and F
// through the fast path specifically (not full discovery): switching away
// from and back to a source must resume from its own prior offset and must
// never republish an already-published kill.
func TestCheckForNewerLogPreservesCheckpointAndDedupe(t *testing.T) {
	now := time.Date(2026, 9, 17, 1, 47, 0, 0, time.UTC)
	pathA, pathB := "/A.ADM", "/B.ADM"
	killLine := `16:40:12 | Player "victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
	contentA := killLine + "\n"

	a := nitrado.LogFile{Name: "A.ADM", Path: pathA, Directory: "/", Size: int64(len(contentA)), Modified: now}
	b := nitrado.LogFile{Name: "B.ADM", Path: pathB, Directory: "/", Size: 50, Modified: now.Add(-time.Hour)}

	fake := &fakeLogSource{contentByPath: map[string][]byte{pathA: []byte(contentA)}}
	e := NewEngine(fake, "svc-1", NewADMParser())
	pub := &recordingPublisher{}
	e.SetKillPublisher(pub)
	e.state = StatePolling
	e.selected = &a
	e.logSourceFound = true

	// Manually read A once via the normal poll path so its kill is published
	// and its checkpoint advances to EOF, mirroring real traffic before any
	// rotation check happens.
	fake.logs = []nitrado.LogFile{a}
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected 1 published kill from A, got %d", len(pub.kills))
	}
	offsetAfterA := e.tracker.LastByteOffset
	if offsetAfterA != a.Size {
		t.Fatalf("expected A's offset at EOF (%d), got %d", a.Size, offsetAfterA)
	}

	// A is proven stale; B has never been observed before (genuinely
	// UNKNOWN, not previously-seen-unchanged) so it still gets a first look
	// and the fast path switches to it.
	e.staleMarks = map[string]staleMark{pathA: {At: now, Size: a.Size, Modified: a.Modified}}
	e.candidateHistory = map[string]candidateObservation{
		pathA: {Size: a.Size, Modified: a.Modified},
	}
	fake.logs = []nitrado.LogFile{a, b}
	e.checkForNewerLog(context.Background())
	if e.selected.Path != pathB {
		t.Fatalf("expected the fast path to switch to B, got %q", e.selected.Path)
	}

	// A later shows new evidence (grew) and becomes eligible again via the
	// fast path.
	grownA := contentA + killLine + " again\n"
	fake.contentByPath[pathA] = []byte(grownA)
	aGrown := a
	aGrown.Size = int64(len(grownA))
	aGrown.Modified = now.Add(time.Minute)
	fake.logs = []nitrado.LogFile{aGrown, b}
	e.checkForNewerLog(context.Background())
	if e.selected.Path != pathA {
		t.Fatalf("expected the fast path to switch back to revived A, got %q", e.selected.Path)
	}
	if e.tracker.LastByteOffset != offsetAfterA {
		t.Fatalf("expected A to resume from its own prior offset %d, got %d", offsetAfterA, e.tracker.LastByteOffset)
	}

	// Reading A's new tail must not republish the original kill.
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected the original kill to never be republished, got %d total publishes", len(pub.kills))
	}
}
