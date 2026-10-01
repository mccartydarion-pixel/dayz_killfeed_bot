package app

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeBlackBoxStore struct {
	visits, dismantles []string
	recorded           []string
}

func (f *fakeBlackBoxStore) MatchVisit(_ context.Context, _, _, playerID int64, _, _ float64) ([]repository.BlackBoxMatch, error) {
	f.visits = append(f.visits, "visit")
	return []repository.BlackBoxMatch{{BaseID: 7, PlayerID: playerID}}, nil
}

func (f *fakeBlackBoxStore) MatchDismantle(_ context.Context, _, _ int64, adm string, _, _ float64) ([]repository.BlackBoxMatch, error) {
	f.dismantles = append(f.dismantles, adm)
	return []repository.BlackBoxMatch{{BaseID: 7, PlayerID: 3}}, nil
}

func (f *fakeBlackBoxStore) Record(_ context.Context, _ repository.BlackBoxMatch, kind, name, detail string) error {
	f.recorded = append(f.recorded, kind+":"+name+":"+detail)
	return nil
}

func drainBlackBox(b *baseBlackBoxRecorder) {
	for {
		select {
		case j := <-b.queue:
			b.handle(context.Background(), j)
		default:
			return
		}
	}
}

func TestBaseBlackBoxRecorderFiltersAndRecords(t *testing.T) {
	store := &fakeBlackBoxStore{}
	b := newBaseBlackBoxRecorder(store, 1, 2)
	b.ObserveLocations([]killfeed.LocationSample{
		{ServerID: 2, PlayerID: 5, Gamertag: " Visitor ", X: 1, Z: 2},
		{ServerID: 9, PlayerID: 5}, // another server
		{ServerID: 2, PlayerID: 0}, // unknown player
	})
	pos := &killfeed.Position{X: 10, Y: 20}
	b.PublishBuild(&killfeed.Event{Build: &killfeed.BuildAction{Action: "Dismantled", Object: "Wall"}, Player: &killfeed.PlayerRef{ID: "adm-1", Name: "Raider", Position: pos}})
	b.PublishBuild(&killfeed.Event{Build: &killfeed.BuildAction{Action: "Built", Object: "Wall"}, Player: &killfeed.PlayerRef{ID: "adm-2", Position: pos}})
	b.PublishBuild(&killfeed.Event{Build: &killfeed.BuildAction{Action: "Dismantled", Object: "Wall"}, Player: &killfeed.PlayerRef{ID: "adm-3"}})
	drainBlackBox(b)
	if len(store.visits) != 1 || len(store.dismantles) != 1 || store.dismantles[0] != "adm-1" {
		t.Fatalf("filtering: visits=%v dismantles=%v", store.visits, store.dismantles)
	}
	want := []string{"VISIT:Visitor:", "DISMANTLE:Raider:Wall"}
	if len(store.recorded) != 2 || store.recorded[0] != want[0] || store.recorded[1] != want[1] {
		t.Fatalf("recorded: %v", store.recorded)
	}
}

func TestBaseBlackBoxRecorderNeverBlocksAndNilIsSafe(t *testing.T) {
	var nilRecorder *baseBlackBoxRecorder
	nilRecorder.ObserveLocations([]killfeed.LocationSample{{ServerID: 2, PlayerID: 1}})
	nilRecorder.PublishBuild(&killfeed.Event{})
	locationObserverFanout{nil, nilRecorder}.ObserveLocations([]killfeed.LocationSample{{ServerID: 2, PlayerID: 1}})

	b := newBaseBlackBoxRecorder(&fakeBlackBoxStore{}, 1, 2)
	samples := make([]killfeed.LocationSample, blackBoxQueueCap+10)
	for i := range samples {
		samples[i] = killfeed.LocationSample{ServerID: 2, PlayerID: int64(i + 1)}
	}
	b.ObserveLocations(samples)
	if b.dropped.Load() != 10 {
		t.Fatalf("dropped: %d", b.dropped.Load())
	}
}
