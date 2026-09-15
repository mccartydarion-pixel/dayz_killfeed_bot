package killfeed

import (
	"context"
	"testing"
)

type coldStartCheckpointStore struct{ checkpoint *DurableCheckpoint }

func (s *coldStartCheckpointStore) LoadADMCheckpoint(context.Context, int64, int64) (*DurableCheckpoint, error) {
	return s.checkpoint, nil
}
func (s *coldStartCheckpointStore) SaveADMCheckpoint(_ context.Context, _ int64, _ int64, _ string, checkpoint DurableCheckpoint) error {
	s.checkpoint = &checkpoint
	return nil
}

func TestColdStartMismatchedCheckpointBaselinesCurrentADM(t *testing.T) {
	engine, fake := newFakeEngine("historical connect\nhistorical disconnect\n", 39)
	store := &coldStartCheckpointStore{checkpoint: &DurableCheckpoint{Filename: "/logs/old.ADM", ProcessedOffset: 100}}
	engine.SetDurableCheckpoint(store, 1, 2)
	if err := engine.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.tracker.LastByteOffset != fake.logs[0].Size {
		t.Fatalf("expected current ADM baseline at remote EOF, got %d", engine.tracker.LastByteOffset)
	}
	if engine.diagnostics == nil || !engine.diagnostics.Snapshot().ColdStartBaseline {
		t.Fatal("expected cold-start baseline diagnostic")
	}
}
