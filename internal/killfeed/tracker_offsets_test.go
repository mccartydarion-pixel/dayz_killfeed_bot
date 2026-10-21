package killfeed

import (
	"context"
	"testing"
)

func TestCompleteLineOffsetsExcludePartialFinalLine(t *testing.T) {
	tracker := NewTracker("svc")
	tracker.LineBuffer = "connect\npartial"
	lines := tracker.DrainCompleteLinesWithOffsets(100)
	if len(lines) != 1 || lines[0].Text != "connect" || lines[0].EndOffset != 108 {
		t.Fatalf("unexpected complete line offsets: %#v", lines)
	}
	if tracker.LineBuffer != "partial" {
		t.Fatalf("expected partial line to remain buffered, got %q", tracker.LineBuffer)
	}
}

// TestEngineReadsGrowthWithinSameSecond: Nitrado's modified_at has
// one-second resolution, so a file that grew in the same second as the last
// read keeps its timestamp. The growth must still be read on the next poll -
// but a listing that overstates the file (listed size never matched by the
// download) must not cause a download on every poll.
func TestEngineReadsGrowthWithinSameSecond(t *testing.T) {
	first := "line one\n"
	engine, fake := newFakeEngine(first, int64(len(first)))
	ctx := context.Background()
	for i := 0; i < 2; i++ { // discovery, first read
		if err := engine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	readsBefore := fake.reads
	grown := first + "line two\n"
	fake.content = []byte(grown)
	fake.logs[0].Size = int64(len(grown)) // same Modified second
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.reads != readsBefore+1 || engine.tracker.LastByteOffset != int64(len(grown)) {
		t.Fatalf("same-second growth not read: reads %d->%d offset %d", readsBefore, fake.reads, engine.tracker.LastByteOffset)
	}

	// Listing overstates the file by 50 bytes the download never returns.
	fake.logs[0].Size = int64(len(grown)) + 50
	for i := 0; i < 10; i++ {
		if err := engine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if extra := fake.reads - (readsBefore + 1); extra > 1 {
		t.Fatalf("overstated listing caused %d downloads in 10 polls, want at most 1", extra)
	}
}
