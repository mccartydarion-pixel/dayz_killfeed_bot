package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func newRankingEngine() *Engine {
	return &Engine{}
}

// TestRankCandidatesFallsBackToNewestWhenNoEvidence is scenario A's baseline:
// with no history and no stale marks, ranking reproduces today's "newest
// modified" behavior exactly (selectBestCandidate's rule), so a genuinely
// growing/first-seen source is never second-guessed.
func TestRankCandidatesFallsBackToNewestWhenNoEvidence(t *testing.T) {
	e := newRankingEngine()
	now := time.Now()
	logs := []nitrado.LogFile{
		{Path: "/new.ADM", Size: 4, Modified: now},
		{Path: "/old.ADM", Size: 300000, Modified: now.Add(-2 * time.Hour)},
	}
	ranked := e.rankCandidates(logs, now)
	if ranked[0].File.Path != "/new.ADM" {
		t.Fatalf("expected newest-by-modified fallback, got %q", ranked[0].File.Path)
	}
	if ranked[0].State != candidateUnknown {
		t.Fatalf("expected UNKNOWN with no prior history, got %s", ranked[0].State)
	}
}

// TestRankCandidatesPrefersActivityOverNewestTimestamp is scenario E: two
// candidates, one newer by modified time but static, one older but actively
// growing - the actively growing one must win.
func TestRankCandidatesPrefersActivityOverNewestTimestamp(t *testing.T) {
	e := newRankingEngine()
	now := time.Now()
	older := nitrado.LogFile{Path: "/older.ADM", Size: 200, Modified: now.Add(-time.Hour)}
	newer := nitrado.LogFile{Path: "/newer.ADM", Size: 50, Modified: now}

	e.candidateHistory = map[string]candidateObservation{
		older.Path: {Size: 100, Modified: older.Modified}, // grew 100 -> 200
		newer.Path: {Size: 50, Modified: newer.Modified},  // unchanged
	}

	ranked := e.rankCandidates([]nitrado.LogFile{newer, older}, now)
	if ranked[0].File.Path != older.Path {
		t.Fatalf("expected the actively growing candidate to win, got %q (state=%s reason=%s)", ranked[0].File.Path, ranked[0].State, ranked[0].Reason)
	}
	if ranked[0].State != candidateActive {
		t.Fatalf("expected ACTIVE state, got %s", ranked[0].State)
	}
}

// TestRankCandidatesDemotesKnownStaleSource covers scenarios B and F: a
// known-stale current source must rank below a candidate with real growth
// evidence, or even below a completely unknown brand-new (e.g. rotated) file.
func TestRankCandidatesDemotesKnownStaleSource(t *testing.T) {
	now := time.Now()
	staleFile := nitrado.LogFile{Path: "/stale.ADM", Size: 100, Modified: now.Add(-time.Hour)}

	t.Run("unknown rotated candidate outranks known-stale (scenario F)", func(t *testing.T) {
		e := newRankingEngine()
		e.staleMarks = map[string]staleMark{staleFile.Path: {At: now, Size: staleFile.Size, Modified: staleFile.Modified}}
		rotated := nitrado.LogFile{Path: "/rotated.ADM", Size: 10, Modified: now}
		ranked := e.rankCandidates([]nitrado.LogFile{staleFile, rotated}, now)
		if ranked[0].File.Path != rotated.Path {
			t.Fatalf("expected the unknown rotated file to outrank the known-stale one, got %q", ranked[0].File.Path)
		}
		if ranked[len(ranked)-1].State != candidateStale {
			t.Fatalf("expected the stale file ranked last and classified STALE, got %s", ranked[len(ranked)-1].State)
		}
	})

	t.Run("actively growing candidate outranks known-stale (scenario B)", func(t *testing.T) {
		e := newRankingEngine()
		e.staleMarks = map[string]staleMark{staleFile.Path: {At: now, Size: staleFile.Size, Modified: staleFile.Modified}}
		growing := nitrado.LogFile{Path: "/growing.ADM", Size: 500, Modified: now.Add(-2 * time.Hour)} // older by modified, but growing
		e.candidateHistory = map[string]candidateObservation{growing.Path: {Size: 10, Modified: growing.Modified}}
		ranked := e.rankCandidates([]nitrado.LogFile{staleFile, growing}, now)
		if ranked[0].File.Path != growing.Path {
			t.Fatalf("expected the actively growing candidate to outrank the known-stale one, got %q", ranked[0].File.Path)
		}
	})
}

// TestRankCandidatesDoesNotHealStaleSourceWithoutNewEvidence is scenario C:
// rediscovering the exact same stale path with unchanged metadata must NOT be
// treated as freshly healthy, and must not be silently un-marked.
func TestRankCandidatesDoesNotHealStaleSourceWithoutNewEvidence(t *testing.T) {
	e := newRankingEngine()
	now := time.Now()
	staleFile := nitrado.LogFile{Path: "/only.ADM", Size: 100, Modified: now.Add(-time.Hour)}
	e.staleMarks = map[string]staleMark{staleFile.Path: {At: now, Size: staleFile.Size, Modified: staleFile.Modified}}

	ranked := e.rankCandidates([]nitrado.LogFile{staleFile}, now)
	if ranked[0].State != candidateStale {
		t.Fatalf("expected the sole unchanged candidate to still classify STALE, got %s", ranked[0].State)
	}
	if ranked[0].Reason != "known_stale_no_new_evidence" {
		t.Fatalf("expected known_stale_no_new_evidence reason, got %q", ranked[0].Reason)
	}
	if _, stillMarked := e.staleMarks[staleFile.Path]; !stillMarked {
		t.Fatal("expected the stale mark to remain in place absent new evidence")
	}
}

// TestRankCandidatesReeligibleAfterNewEvidence is scenario D: a previously
// stale source that later shows growth becomes eligible again - never a
// permanent blacklist.
func TestRankCandidatesReeligibleAfterNewEvidence(t *testing.T) {
	e := newRankingEngine()
	now := time.Now()
	path := "/revived.ADM"
	e.staleMarks = map[string]staleMark{path: {At: now.Add(-time.Minute), Size: 100, Modified: now.Add(-time.Hour)}}
	e.candidateHistory = map[string]candidateObservation{path: {Size: 100, Modified: now.Add(-time.Hour)}}

	grown := nitrado.LogFile{Path: path, Size: 250, Modified: now}
	ranked := e.rankCandidates([]nitrado.LogFile{grown}, now)
	if ranked[0].State != candidateActive {
		t.Fatalf("expected the revived source to classify ACTIVE, got %s (reason=%s)", ranked[0].State, ranked[0].Reason)
	}
	if _, stillMarked := e.staleMarks[path]; stillMarked {
		t.Fatal("expected the stale mark to be cleared once new evidence appears")
	}
}

// TestRankCandidatesNoActiveCandidateStaysDegraded is scenario H: with every
// candidate known-stale and no better alternative, a selection is still made
// (never refuse outright - there is nothing better to offer), but every
// candidate's state must keep reporting STALE, never a healthy-sounding
// fallback that could mislead the pipeline classification upstream.
func TestRankCandidatesNoActiveCandidateStaysDegraded(t *testing.T) {
	e := newRankingEngine()
	now := time.Now()
	a := nitrado.LogFile{Path: "/a.ADM", Size: 100, Modified: now.Add(-2 * time.Hour)}
	b := nitrado.LogFile{Path: "/b.ADM", Size: 50, Modified: now.Add(-3 * time.Hour)}
	e.staleMarks = map[string]staleMark{
		a.Path: {At: now, Size: a.Size, Modified: a.Modified},
		b.Path: {At: now, Size: b.Size, Modified: b.Modified},
	}
	ranked := e.rankCandidates([]nitrado.LogFile{a, b}, now)
	if len(ranked) != 2 {
		t.Fatalf("expected both candidates ranked, got %d", len(ranked))
	}
	for _, r := range ranked {
		if r.State != candidateStale {
			t.Fatalf("expected every candidate to remain classified STALE, got %s for %s", r.State, r.File.Path)
		}
	}
	if ranked[0].File.Path != a.Path {
		t.Fatalf("expected the newest-by-metadata candidate to win as last resort, got %q", ranked[0].File.Path)
	}
}

// TestEngineKeepsGrowingCurrentSource is scenario A end-to-end: a genuinely
// growing selected source must never trigger rediscovery or a source switch.
func TestEngineKeepsGrowingCurrentSource(t *testing.T) {
	engine, fake := newFakeEngine("line one\n", int64(len("line one\n")))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read
		t.Fatal(err)
	}
	selected := engine.Stats().SelectedPath

	for i := 0; i < 5; i++ {
		grown := "line one\n"
		for j := 0; j <= i; j++ {
			grown += "line more\n"
		}
		fake.content = []byte(grown)
		fake.logs[0].Size = int64(len(grown))
		fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
		if err := engine.PollOnce(ctx); err != nil {
			t.Fatalf("growth poll %d failed: %v", i, err)
		}
		if engine.Stats().State != StatePolling {
			t.Fatalf("expected to remain polling while growing, got %s", engine.Stats().State)
		}
		if engine.Stats().SelectedPath != selected {
			t.Fatalf("expected the selected source to stay put while growing, got %q", engine.Stats().SelectedPath)
		}
	}
}

// TestEngineSwitchesAwayFromStaleSourceToActiveCandidate reproduces the
// actual production bug end-to-end (scenario B): a selected source goes
// stale (proven via probe), a second candidate shows real growth across
// discovery passes, and the engine must switch to it instead of blindly
// reselecting the known-dead file because it still looks "newest" by
// metadata.
func TestEngineSwitchesAwayFromStaleSourceToActiveCandidate(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	pathA, pathB := "/A.ADM", "/B.ADM"
	contentA := "baseline\n"

	fake := &fakeLogSource{
		logs: []nitrado.LogFile{{Name: "A.ADM", Path: pathA, Size: int64(len(contentA)), Modified: now, Type: "ADM"}},
		contentByPath: map[string][]byte{
			pathA: []byte(contentA),
		},
	}
	engine := NewEngine(fake, "svc-1", NewADMParser())
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery selects A
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read of A
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != pathA {
		t.Fatalf("expected A selected initially, got %q", got)
	}

	// A goes quiet (never changes again) and is later proven stale by the
	// direct probe. B appears and, across two discovery passes, shows real
	// growth - the exact evidence a genuinely live source produces.
	engine.lastLogChange = time.Now().Add(-6 * time.Minute)
	contentB := "b line one\n"
	fake.logs = []nitrado.LogFile{
		{Name: "B.ADM", Path: pathB, Size: int64(len(contentB)), Modified: now.Add(time.Minute), Type: "ADM"},
		{Name: "A.ADM", Path: pathA, Size: int64(len(contentA)), Modified: now, Type: "ADM"}, // unchanged
	}
	fake.contentByPath[pathB] = []byte(contentB)

	if err := engine.PollOnce(ctx); err != nil { // gives up on A, discovers B+A, B wins (unknown beats stale)
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != pathB {
		t.Fatalf("expected the engine to switch to B once A was proven stale, got %q", got)
	}
	// B has no growth history of its own yet (first time seen) - it wins on
	// being merely UNKNOWN rather than demoted, which is the actual fix: A no
	// longer wins just because it still looks "newest" once proven stale.
	if reason := engine.selectionReason; reason == "known_stale_no_new_evidence" {
		t.Fatalf("expected B to not be classified as the known-stale source, got reason %q", reason)
	}
}

// TestEngineDoesNotHealStaleSourceReselectedAlone proves scenario C/H
// end-to-end: with only the stale file available, the engine still selects
// it (nothing else to offer) but the diagnostics classification is not
// falsely healed - it keeps reporting the inactive-source classification.
func TestEngineDoesNotHealStaleSourceReselectedAlone(t *testing.T) {
	engine, _ := newFakeEngine("baseline\n", int64(len("baseline\n")))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery selects the only file
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read
		t.Fatal(err)
	}

	engine.lastLogChange = time.Now().Add(-6 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil { // gives up, rediscovers, only candidate is itself
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != "/logs/DayZServer_x64.ADM" {
		t.Fatalf("expected the sole candidate reselected, got %q", got)
	}
	if engine.selectionReason != "known_stale_no_new_evidence" {
		t.Fatalf("expected the stale reselection to be labeled honestly, got %q", engine.selectionReason)
	}
	snapshot := engine.diagnostics.Snapshot()
	if snapshot.ProbeClassification != "WRONG_OR_INACTIVE_ADM_SOURCE" {
		t.Fatalf("expected the pipeline to keep reporting the inactive-source classification, got %q", snapshot.ProbeClassification)
	}
}

// TestStaleRediscoveryIsThrottled proves the fix does not turn a stuck,
// quiet source into a full-tree-discovery spam loop: once a poll has already
// forced rediscovery for a still-stale source, another poll immediately after
// must not call ListLogs again before staleProbeInterval has passed.
func TestStaleRediscoveryIsThrottled(t *testing.T) {
	engine, fake := newFakeEngine("baseline\n", int64(len("baseline\n")))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	engine.lastLogChange = time.Now().Add(-6 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil { // forces one rediscovery
		t.Fatal(err)
	}
	readsAfterFirstGiveUp := fake.reads

	engine.lastLogChange = time.Now().Add(-6 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.reads != readsAfterFirstGiveUp {
		t.Fatalf("expected the immediately-following poll to be throttled (no extra reads), got %d after %d", fake.reads, readsAfterFirstGiveUp)
	}
}

// TestSourceSwitchResumesFromOwnCheckpointWithoutDuplicatePublish covers
// scenarios G and I together: switching away from a source and back to it
// later must resume from ITS OWN previously tracked offset (never another
// file's, never byte 0 for content already safely processed), and the
// already-published kill it contains must never be published a second time.
func TestSourceSwitchResumesFromOwnCheckpointWithoutDuplicatePublish(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	pathA, pathB := "/A.ADM", "/B.ADM"
	killLine := `16:40:12 | Player "victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
	contentA := killLine + "\n"

	fake := &fakeLogSource{
		logs: []nitrado.LogFile{{Name: "A.ADM", Path: pathA, Size: int64(len(contentA)), Modified: now, Type: "ADM"}},
		contentByPath: map[string][]byte{
			pathA: []byte(contentA),
		},
	}
	engine := NewEngine(fake, "svc-1", NewADMParser())
	pub := &recordingPublisher{}
	engine.SetKillPublisher(pub)
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery selects A
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // reads + publishes the kill
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected exactly 1 published kill from A, got %d", len(pub.kills))
	}
	offsetAfterA := engine.tracker.LastByteOffset
	if offsetAfterA != int64(len(contentA)) {
		t.Fatalf("expected A's offset at EOF, got %d", offsetAfterA)
	}

	// A goes stale; B appears and wins (unknown beats known-stale).
	engine.lastLogChange = time.Now().Add(-6 * time.Minute)
	contentB := "b line one\n"
	fake.logs = []nitrado.LogFile{
		{Name: "B.ADM", Path: pathB, Size: int64(len(contentB)), Modified: now.Add(time.Minute), Type: "ADM"},
		{Name: "A.ADM", Path: pathA, Size: int64(len(contentA)), Modified: now, Type: "ADM"},
	}
	fake.contentByPath[pathB] = []byte(contentB)
	if err := engine.PollOnce(ctx); err != nil { // switches to B
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != pathB {
		t.Fatalf("expected B selected, got %q", got)
	}
	if err := engine.PollOnce(ctx); err != nil { // reads B
		t.Fatal(err)
	}

	// B goes stale too; A shows new evidence (grew) and becomes eligible again.
	// Reset the rediscovery throttle: in production, minutes of real time pass
	// between distinct staleness events, but this test forces both within
	// milliseconds, which would otherwise be (correctly) rate-limited.
	engine.lastStaleRediscoveryAt = time.Time{}
	engine.lastStaleProbeAt = time.Time{}
	engine.lastLogChange = time.Now().Add(-6 * time.Minute)
	grownA := contentA + killLine + " again\n" // A grew, but the ORIGINAL kill line content is still the same first bytes
	fake.contentByPath[pathA] = []byte(grownA)
	fake.logs = []nitrado.LogFile{
		{Name: "A.ADM", Path: pathA, Size: int64(len(grownA)), Modified: now.Add(2 * time.Minute), Type: "ADM"}, // grew vs last observation
		{Name: "B.ADM", Path: pathB, Size: int64(len(contentB)), Modified: now.Add(time.Minute), Type: "ADM"},   // unchanged since selection
	}
	if err := engine.PollOnce(ctx); err != nil { // switches back to A
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != pathA {
		t.Fatalf("expected the engine to switch back to revived A, got %q", got)
	}
	if engine.tracker.LastByteOffset != offsetAfterA {
		t.Fatalf("expected A to resume from its own prior offset %d instead of replaying from 0, got %d", offsetAfterA, engine.tracker.LastByteOffset)
	}

	if err := engine.PollOnce(ctx); err != nil { // reads only A's new tail bytes
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected the original kill to never be published a second time, got %d total publishes", len(pub.kills))
	}
}
