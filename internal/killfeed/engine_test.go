package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

type fakeLogSource struct {
	logs    []nitrado.LogFile
	content []byte
}

func (f *fakeLogSource) ListLogs(ctx context.Context, serviceID string) ([]nitrado.LogFile, error) {
	return f.logs, nil
}

func (f *fakeLogSource) ReadLog(ctx context.Context, serviceID string, path string) ([]byte, error) {
	return f.content, nil
}

func newFakeEngine(content string, size int64) (*Engine, *fakeLogSource) {
	modified := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	fake := &fakeLogSource{
		logs: []nitrado.LogFile{{
			Name:     "DayZServer_x64.ADM",
			Path:     "/logs/DayZServer_x64.ADM",
			Size:     size,
			Modified: modified,
			Type:     "ADM",
		}},
		content: []byte(content),
	}
	return NewEngine(fake, "svc-1", &PlaceholderParser{}), fake
}

func TestEngineAdvancesOffsetAcrossGrowingFile(t *testing.T) {
	engine, fake := newFakeEngine("line one\nline two\n", int64(len("line one\nline two\n")))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("first poll failed: %v", err)
	}
	stats := engine.Stats()
	if !stats.LogSourceFound {
		t.Fatal("expected log source to be marked found")
	}
	if stats.LinesDiscovered != 2 {
		t.Fatalf("expected 2 lines discovered, got %d", stats.LinesDiscovered)
	}
	if engine.tracker.LastByteOffset != int64(len("line one\nline two\n")) {
		t.Fatalf("expected offset to advance to end of file, got %d", engine.tracker.LastByteOffset)
	}

	// Unchanged file: no new lines, offset stays put.
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("second poll failed: %v", err)
	}
	if engine.Stats().LinesDiscovered != 2 {
		t.Fatalf("expected duplicate prevention to keep 2 lines, got %d", engine.Stats().LinesDiscovered)
	}

	// Growing file: only the new bytes are consumed.
	grown := "line one\nline two\nline three\n"
	fake.content = []byte(grown)
	fake.logs[0].Size = int64(len(grown))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("third poll failed: %v", err)
	}
	stats = engine.Stats()
	if stats.LinesDiscovered != 3 {
		t.Fatalf("expected 3 lines discovered after growth, got %d", stats.LinesDiscovered)
	}
	if engine.tracker.LastByteOffset != int64(len(grown)) {
		t.Fatalf("expected offset at end of grown file, got %d", engine.tracker.LastByteOffset)
	}
}

func TestEngineBuffersPartialLines(t *testing.T) {
	engine, fake := newFakeEngine("complete line\npartial li", int64(len("complete line\npartial li")))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	if got := engine.Stats().LinesDiscovered; got != 1 {
		t.Fatalf("expected only the complete line to count, got %d", got)
	}

	fake.content = []byte("complete line\npartial line done\n")
	fake.logs[0].Size = int64(len(fake.content))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("second poll failed: %v", err)
	}
	if got := engine.Stats().LinesDiscovered; got != 2 {
		t.Fatalf("expected buffered partial line to complete, got %d total lines", got)
	}
}

func TestEngineDetectsTruncationAndRotation(t *testing.T) {
	engine, fake := newFakeEngine("aaaaaaaa\nbbbbbbbb\n", 18)
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll failed: %v", err)
	}

	// Truncation: same file, smaller size than the stored offset.
	fake.content = []byte("cc\n")
	fake.logs[0].Size = 3
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("truncation poll failed: %v", err)
	}
	if engine.tracker.LastByteOffset != 3 {
		t.Fatalf("expected offset reset to truncated end, got %d", engine.tracker.LastByteOffset)
	}

	// Rotation: a different file becomes the newest candidate.
	fake.logs[0].Path = "/logs/DayZServer_x64_1.ADM"
	fake.logs[0].Name = "DayZServer_x64_1.ADM"
	fake.content = []byte("rotated line\n")
	fake.logs[0].Size = int64(len(fake.content))
	fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatalf("rotation poll failed: %v", err)
	}
	if engine.tracker.CurrentLogFile != "/logs/DayZServer_x64_1.ADM" {
		t.Fatalf("expected tracker to follow rotated file, got %q", engine.tracker.CurrentLogFile)
	}
}

func TestEngineSurvivesRecoverableFailures(t *testing.T) {
	engine, fake := newFakeEngine("x\n", 2)
	fake.logs = nil

	if err := engine.PollOnce(context.Background()); err != nil {
		t.Fatalf("empty log list must be recoverable, got: %v", err)
	}
}
