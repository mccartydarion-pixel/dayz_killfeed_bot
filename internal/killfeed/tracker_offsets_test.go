package killfeed

import "testing"

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
