package killfeed

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Champion Performance Phase 1.5 (docs/NITRADO_DELTA_READS.md): engine-level correctness tests for
// the partial-read integration. deltaLogSource extends fakeLogSource (engine_test.go) with a
// DeltaSource implementation that serves bytes directly out of in-memory content - it exists to
// test the ENGINE's delta wiring (offsets, chunking-through-the-tracker, fallback, checkpoints),
// not the internal/nitrado HTTP mechanisms themselves, which partial_read_test.go already covers
// independently.

type deltaLogSource struct {
	fakeLogSource
	deltaReads  int
	deltaFail   bool // force every ReadDelta call to report ok=false, to test the full-read fallback
	sawFrom     []int64
	sawTarget   []int64
}

func (f *deltaLogSource) ReadDelta(ctx context.Context, serviceID, path string, fromOffset, targetSize int64, mode nitrado.DeltaMode) (*nitrado.PartialReadResult, bool) {
	f.deltaReads++
	f.sawFrom = append(f.sawFrom, fromOffset)
	f.sawTarget = append(f.sawTarget, targetSize)
	if f.deltaFail {
		return nil, false
	}
	content := f.content
	if f.contentByPath != nil {
		if c, ok := f.contentByPath[path]; ok {
			content = c
		}
	}
	if fromOffset < 0 || targetSize > int64(len(content)) || fromOffset > targetSize {
		return nil, false
	}
	data := content[fromOffset:targetSize]
	return &nitrado.PartialReadResult{Data: data, RequestedOffset: fromOffset, StartOffset: fromOffset, EndOffset: targetSize, RemoteSize: targetSize, Method: "FAKE_DELTA", Complete: true}, true
}

// oracleParser recognizes any non-empty line as one PLAYER_HIT event, carrying the exact line text
// in Weapon so a test can compare recognized events by identity/order without depending on the real
// ADM grammar - deliberately dependency-free (never touches persistence/publisher, both nil in these
// tests - see engine.go's processLine: a PLAYER_HIT event is never persisted or published, so
// processLine's only side effects are dedupe/counters, safe with no other engine wiring).
type oracleParser struct{}

func (oracleParser) ParseLine(line string) (*Event, error) {
	if line == "" {
		return nil, nil
	}
	return &Event{Type: EventPlayerHit, Weapon: line}, nil
}

func newDeltaEngine(mode nitrado.DeltaMode, content string, size int64) (*Engine, *deltaLogSource) {
	modified := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	fake := &deltaLogSource{fakeLogSource: fakeLogSource{
		logs:    []nitrado.LogFile{{Name: "DayZServer_x64.ADM", Path: "/logs/DayZServer_x64.ADM", Size: size, Modified: modified, Type: "ADM"}},
		content: []byte(content),
	}}
	e := NewEngine(fake, "svc-delta", oracleParser{})
	e.deltaMode = mode
	return e, fake
}

// --- default-off safety (section 11) ----------------------------------------------------------

func TestEngineDeltaModeOffNeverCallsReadDelta(t *testing.T) {
	engine, fake := newDeltaEngine(nitrado.DeltaModeOff, "line one\nline two\n", int64(len("line one\nline two\n")))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fake.content = []byte("line one\nline two\nline three\n")
	fake.logs[0].Size = int64(len(fake.content))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.deltaReads != 0 {
		t.Fatalf("DeltaModeOff (the default) must never call ReadDelta, got %d calls", fake.deltaReads)
	}
	if fake.reads == 0 {
		t.Fatal("expected the unchanged full ReadLog path to have been used")
	}
}

// --- normal growth path (section 15) --------------------------------------------------------------

func TestEngineDeltaNormalGrowthFetchesOnlyNewBytes(t *testing.T) {
	// A 10-byte line record ("XXXXXXXXX\n") repeated so the base is exactly 100000 bytes and ends
	// on a line boundary (no pending partial line to complicate the offset arithmetic this test
	// checks).
	const line = "XXXXXXXXX\n"
	base := strings.Repeat(line, 100000/len(line))
	if len(base) != 100000 {
		t.Fatalf("test setup: expected exactly 100000 bytes, got %d", len(base))
	}
	engine, fake := newDeltaEngine(nitrado.DeltaModeAuto, base, int64(len(base)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil { // discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // first read (cold start: oldOffset==0, must use full read)
		t.Fatal(err)
	}
	if fake.deltaReads != 0 {
		t.Fatalf("the FIRST read of a file must always be a full read (cold start), got %d delta calls", fake.deltaReads)
	}
	if engine.tracker.LastByteOffset != 100000 {
		t.Fatalf("expected offset 100000 after first read, got %d", engine.tracker.LastByteOffset)
	}

	grown := base + strings.Repeat(line, 380) // +3800 bytes, also ending on a line boundary
	if len(grown)-len(base) != 3800 {
		t.Fatalf("test setup: expected exactly 3800 new bytes, got %d", len(grown)-len(base))
	}
	fake.content = []byte(grown)
	fake.logs[0].Size = int64(len(grown))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.deltaReads != 1 {
		t.Fatalf("expected exactly one delta read for the growth poll, got %d", fake.deltaReads)
	}
	if fake.sawFrom[0] != 100000 {
		t.Fatalf("expected the delta read to start at the processed offset 100000, got %d", fake.sawFrom[0])
	}
	if fake.sawTarget[0] != int64(len(grown)) {
		t.Fatalf("expected the delta read to target the new remote size %d, got %d", len(grown), fake.sawTarget[0])
	}
	if engine.tracker.LastByteOffset != int64(len(grown)) {
		t.Fatalf("expected offset to advance to the new end of file, got %d", engine.tracker.LastByteOffset)
	}
	stats := engine.DeltaStats()
	if stats.BytesReceived != 3800 {
		t.Fatalf("expected 3800 bytes received via delta, got %d", stats.BytesReceived)
	}
	if stats.FullReadBytesAvoided != 100000 {
		t.Fatalf("expected 100000 bytes avoided (the old offset worth of the file we did NOT re-download), got %d", stats.FullReadBytesAvoided)
	}
}

// --- partial line across chunks (section 18/38) -----------------------------------------------------

func TestEngineDeltaPartialLineCompletesAcrossPolls(t *testing.T) {
	// First poll: a complete line, then an in-progress final line with no terminator yet.
	first := "12:00:01 Player Bob hit Alice with M4A1\n12:00:02 Player Bob hit Al"
	engine, fake := newDeltaEngine(nitrado.DeltaModeAuto, first, int64(len(first)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // cold start: full read
		t.Fatal(err)
	}
	if engine.Stats().LinesDiscovered != 1 {
		t.Fatalf("expected only the one complete line counted, got %d", engine.Stats().LinesDiscovered)
	}
	firstEventCount := engine.metrics.EventsParsed

	// Second poll: the same line, now completed, arrives via a delta read.
	second := first + "ice with M4A1\n"
	fake.content = []byte(second)
	fake.logs[0].Size = int64(len(second))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.deltaReads != 1 {
		t.Fatalf("expected the growth poll to use a delta read, got %d delta calls", fake.deltaReads)
	}
	if engine.Stats().LinesDiscovered != 2 {
		t.Fatalf("expected exactly 2 lines total (the second line parsed exactly once after completion), got %d", engine.Stats().LinesDiscovered)
	}
	if engine.metrics.EventsParsed != firstEventCount+1 {
		t.Fatalf("expected exactly one NEW event parsed from the completed line, got %d new (was %d, now %d)",
			engine.metrics.EventsParsed-firstEventCount, firstEventCount, engine.metrics.EventsParsed)
	}
	if engine.tracker.LastByteOffset != int64(len(second)) {
		t.Fatalf("expected offset at end of the now-complete file, got %d", engine.tracker.LastByteOffset)
	}
}

// --- truncation (section 20/39) ---------------------------------------------------------------------

func TestEngineDeltaTruncationNeverSendsInvalidDeltaRequest(t *testing.T) {
	long := "0123456789\n0123456789\n" // 22 bytes, two complete lines
	engine, fake := newDeltaEngine(nitrado.DeltaModeAuto, long, int64(len(long)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if engine.tracker.LastByteOffset != int64(len(long)) {
		t.Fatalf("expected offset %d, got %d", len(long), engine.tracker.LastByteOffset)
	}

	// The remote file is now much shorter than the processed offset - a genuine truncation, not
	// growth. metadataLagsDirect would normally be consulted for a live client; the fake here has
	// no such lag, so this must be treated as truncation.
	short := "short\n"
	fake.content = []byte(short)
	fake.logs[0].Size = int64(len(short))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for i, from := range fake.sawFrom {
		if from < 0 || from > fake.sawTarget[i] {
			t.Fatalf("delta call %d requested an invalid range: from=%d target=%d", i, from, fake.sawTarget[i])
		}
	}
	if engine.tracker.LastByteOffset > int64(len(short)) {
		t.Fatalf("expected truncation to reset tracking to within the new (shorter) file, got offset %d for a %d-byte file", engine.tracker.LastByteOffset, len(short))
	}
}

// --- rotation (section 21/40) -----------------------------------------------------------------------

func TestEngineDeltaRotationNeverCarriesOldOffsetIntoNewFile(t *testing.T) {
	first := "AAAAAAAAA\n" // 10 bytes, one complete line
	engine, fake := newDeltaEngine(nitrado.DeltaModeAuto, first, int64(len(first)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if engine.tracker.LastByteOffset != 10 {
		t.Fatalf("expected offset 10 on the first file, got %d", engine.tracker.LastByteOffset)
	}

	// Rotate to a brand-new file with a DIFFERENT path. Matches the poll cadence proven in
	// TestEngineDetectsTruncationAndRotation: the old path 404s (re-enter discovery), then
	// rediscovery selects the rotated file, then the next poll actually reads it.
	second := "BBBBBB\n"
	fake.logs[0] = nitrado.LogFile{Name: "DayZServer_x64_2.ADM", Path: "/logs/DayZServer_x64_2.ADM", Size: int64(len(second)), Modified: fake.logs[0].Modified.Add(time.Hour), Type: "ADM"}
	fake.content = []byte(second)
	if err := engine.PollOnce(ctx); err != nil { // stat 404 -> re-enter discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // rediscover + select rotated file
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // read rotated file
		t.Fatal(err)
	}
	for i, from := range fake.sawFrom {
		if from == 10 {
			t.Fatalf("delta call %d requested offset 10 (the OLD file's offset) against the new file - must never carry an old file's byte offset into a different file", i)
		}
	}
	if engine.tracker.CurrentLogFile != "/logs/DayZServer_x64_2.ADM" {
		t.Fatalf("expected the tracker to have switched to the new file, got %q", engine.tracker.CurrentLogFile)
	}
}

// --- checkpoint restart resume (section 23/41) -----------------------------------------------------

type memCheckpointStore struct {
	saved map[string]DurableCheckpoint
}

func newMemCheckpointStore() *memCheckpointStore {
	return &memCheckpointStore{saved: map[string]DurableCheckpoint{}}
}
func (m *memCheckpointStore) key(guildID, serverID int64) string { return fmt.Sprintf("%d:%d", guildID, serverID) }
func (m *memCheckpointStore) LoadADMCheckpoint(ctx context.Context, guildID, serverID int64) (*DurableCheckpoint, error) {
	c, ok := m.saved[m.key(guildID, serverID)]
	if !ok {
		return nil, nil
	}
	cp := c
	return &cp, nil
}
func (m *memCheckpointStore) SaveADMCheckpoint(ctx context.Context, guildID, serverID int64, serviceID string, c DurableCheckpoint) error {
	m.saved[m.key(guildID, serverID)] = c
	return nil
}

// TestEngineDeltaCheckpointRestartResumesSafely models the concrete scenario from task section 23:
// filename=A.ADM, processedOffset=50000 (here: len(initial)), pendingPartialLine="" (both lines are
// complete), remote size grows past that offset across a restart.
//
// engine1 deliberately has NO checkpoint store attached before its first poll: attaching an empty
// store to a brand-new engine is itself the "first-ever-connect" case, which correctly triggers the
// cold-start-at-tail protection (engine.go's startAtTail/cold_start_baseline_required branch) and
// skips existing content - that is desired behavior (section 22), not what this test exercises. So
// engine1 stands in for "the process that already has an established, in-memory tracked offset" and
// its resulting checkpoint is persisted directly into the store, exactly as saveDurableCheckpoint
// would have written it had a store been attached. Only engine2 - the simulated restarted process -
// attaches the store before its first poll, which is the real restore path being tested.
func TestEngineDeltaCheckpointRestartResumesSafely(t *testing.T) {
	store := newMemCheckpointStore()
	initial := "line one\nline two\n"
	modified := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	engine1, fake1 := newDeltaEngine(nitrado.DeltaModeAuto, initial, int64(len(initial)))
	ctx := context.Background()
	if err := engine1.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine1.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	eventsAfterFirstEngine := engine1.metrics.EventsParsed
	if eventsAfterFirstEngine != 2 {
		t.Fatalf("expected 2 events from the first engine, got %d", eventsAfterFirstEngine)
	}
	if engine1.tracker.LastByteOffset != int64(len(initial)) {
		t.Fatalf("expected first engine to reach offset %d, got %d", len(initial), engine1.tracker.LastByteOffset)
	}
	_ = fake1

	checkpoint := DurableCheckpoint{
		Filename:         "/logs/DayZServer_x64.ADM",
		RemoteModifiedAt: modified,
		RemoteSize:       int64(len(initial)),
		ProcessedOffset:  engine1.tracker.LastByteOffset,
	}
	if err := store.SaveADMCheckpoint(ctx, 1, 1, "svc-delta", checkpoint); err != nil {
		t.Fatal(err)
	}

	// "Restart": a brand-new Engine, same checkpoint store, same file now grown (Modified advances
	// too, as Nitrado's listing does when a write lands in a later second).
	grown := initial + "line three\n"
	engine2, fake2 := newDeltaEngine(nitrado.DeltaModeAuto, grown, int64(len(grown)))
	fake2.logs[0].Modified = modified.Add(time.Minute)
	engine2.SetDurableCheckpoint(store, 1, 1)
	if err := engine2.PollOnce(ctx); err != nil { // discovery + checkpoint load
		t.Fatal(err)
	}
	if err := engine2.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if engine2.metrics.EventsParsed != 1 {
		t.Fatalf("expected exactly 1 NEW event (line three) after restart - no replay of line one/two, no skip, no duplicate. Got %d", engine2.metrics.EventsParsed)
	}
	if engine2.tracker.LastByteOffset != int64(len(grown)) {
		t.Fatalf("expected the resumed engine to reach the end of the grown file, got offset %d", engine2.tracker.LastByteOffset)
	}
	_ = fake2
}

// --- byte-for-byte oracle (section 43): the strongest correctness test ------------------------------

// TestEngineDeltaByteForByteOracle runs the SAME synthetic growth sequence through a full-download
// engine and a delta-mode engine and asserts identical parsed events (type+content+order), final
// offsets, and line counts. Any divergence means delta mode is unsafe.
func TestEngineDeltaByteForByteOracle(t *testing.T) {
	growth := []string{
		"12:00:00 Player Alice connected\n",
		"12:00:01 Player Bob connected\n",
		"12:00:02 Player Alice hit Bob with M4A1\n",
		"12:00:03 Player Alice killed Bob with M4A1\n",
		"12:00:04 Player Bob disconnected\n",
		"12:00:05 Player Charlie connected\n",
	}
	var full string
	var cumulative []string
	for _, line := range growth {
		full += line
		cumulative = append(cumulative, full)
	}

	full1, fake1 := newDeltaEngine(nitrado.DeltaModeOff, cumulative[0], int64(len(cumulative[0])))
	full2, fake2 := newDeltaEngine(nitrado.DeltaModeAuto, cumulative[0], int64(len(cumulative[0])))
	ctx := context.Background()

	drive := func(e *Engine, fake *deltaLogSource, size string) {
		fake.content = []byte(size)
		fake.logs[0].Size = int64(len(size))
		fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Second)
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Discovery + first (cold-start) read for both engines.
	if err := full1.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := full1.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := full2.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := full2.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	for _, state := range cumulative[1:] {
		drive(full1, fake1, state)
		drive(full2, fake2, state)
	}

	if full1.metrics.EventsParsed != full2.metrics.EventsParsed {
		t.Fatalf("event count diverged: full=%d delta=%d", full1.metrics.EventsParsed, full2.metrics.EventsParsed)
	}
	if full1.Stats().LinesDiscovered != full2.Stats().LinesDiscovered {
		t.Fatalf("line count diverged: full=%d delta=%d", full1.Stats().LinesDiscovered, full2.Stats().LinesDiscovered)
	}
	if full1.tracker.LastByteOffset != full2.tracker.LastByteOffset {
		t.Fatalf("final checkpoint offset diverged: full=%d delta=%d", full1.tracker.LastByteOffset, full2.tracker.LastByteOffset)
	}
	if full1.tracker.LastByteOffset != int64(len(full)) {
		t.Fatalf("expected both engines to reach end of file (%d), full=%d delta=%d", len(full), full1.tracker.LastByteOffset, full2.tracker.LastByteOffset)
	}
	if fake2.deltaReads == 0 {
		t.Fatal("expected the delta engine to actually have used delta reads at least once (otherwise this test proves nothing about delta mode)")
	}
	if fake1.reads <= fake2.reads {
		t.Fatalf("expected the delta engine to have made fewer full ReadLog calls than the full-mode engine, full-mode reads=%d delta-mode full-reads=%d", fake1.reads, fake2.reads)
	}
}

// --- 1000-poll load comparison (section 31/42) -------------------------------------------------------

func TestEngineDeltaLoadComparisonBytesTransferred(t *testing.T) {
	const polls = 1000
	const growthPerPoll = 7 // bytes/poll, deliberately not a clean divisor of maxChunkBytes
	base := "S" // 1-byte seed, no newline - never parsed as a line until later growth adds one

	fullEngine, fakeFull := newDeltaEngine(nitrado.DeltaModeOff, base, int64(len(base)))
	deltaEngine, fakeDelta := newDeltaEngine(nitrado.DeltaModeAuto, base, int64(len(base)))
	ctx := context.Background()
	for _, e := range []*Engine{fullEngine, deltaEngine} {
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var fullBytesTransferred, deltaBytesTransferred int64
	fullBytesTransferred += int64(len(base))
	deltaBytesTransferred += int64(len(base))
	fullContent, deltaContent := []byte(base), []byte(base)

	for i := 0; i < polls; i++ {
		chunk := []byte(fmt.Sprintf("%07d\n", i)) // 8 bytes; close enough to growthPerPoll for a realistic small-growth simulation
		fullContent = append(fullContent, chunk...)
		deltaContent = append(deltaContent, chunk...)

		fakeFull.content = fullContent
		fakeFull.logs[0].Size = int64(len(fullContent))
		fakeFull.logs[0].Modified = fakeFull.logs[0].Modified.Add(time.Second)
		beforeReads := fakeFull.reads
		if err := fullEngine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if fakeFull.reads > beforeReads {
			fullBytesTransferred += int64(len(fullContent)) // a full read transfers the WHOLE file every time
		}

		fakeDelta.content = deltaContent
		fakeDelta.logs[0].Size = int64(len(deltaContent))
		fakeDelta.logs[0].Modified = fakeDelta.logs[0].Modified.Add(time.Second)
		if err := deltaEngine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	deltaBytesTransferred += deltaEngine.DeltaStats().BytesReceived

	if fullEngine.metrics.EventsParsed != deltaEngine.metrics.EventsParsed {
		t.Fatalf("event count diverged over %d polls: full=%d delta=%d", polls, fullEngine.metrics.EventsParsed, deltaEngine.metrics.EventsParsed)
	}
	if fullEngine.tracker.LastByteOffset != deltaEngine.tracker.LastByteOffset {
		t.Fatalf("final offset diverged: full=%d delta=%d", fullEngine.tracker.LastByteOffset, deltaEngine.tracker.LastByteOffset)
	}
	if deltaBytesTransferred >= fullBytesTransferred {
		t.Fatalf("expected delta mode to transfer meaningfully fewer bytes over %d polls: full=%d delta=%d", polls, fullBytesTransferred, deltaBytesTransferred)
	}
	reduction := 100 * (1 - float64(deltaBytesTransferred)/float64(fullBytesTransferred))
	t.Logf("1000-poll load comparison: full=%d bytes, delta=%d bytes, reduction=%.1f%%, events full=%d delta=%d",
		fullBytesTransferred, deltaBytesTransferred, reduction, fullEngine.metrics.EventsParsed, deltaEngine.metrics.EventsParsed)
	if reduction < 90 {
		t.Fatalf("expected at least a 90%% bandwidth reduction for this small-per-poll-growth scenario, got %.1f%%", reduction)
	}
}
