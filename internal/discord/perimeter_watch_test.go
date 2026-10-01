package discord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakePerimeterStore struct {
	matches  []repository.PerimeterMatch
	nextID   int64
	calls    int
	delivery map[int64]string
}

func (f *fakePerimeterStore) MatchPerimeter(context.Context, int64, int64, int64, float64, float64) ([]repository.PerimeterMatch, error) {
	f.calls++
	return f.matches, nil
}
func (f *fakePerimeterStore) RecordAlert(context.Context, repository.PerimeterMatch, int64, string) (int64, error) {
	return f.nextID, nil
}
func (f *fakePerimeterStore) MarkDelivery(_ context.Context, id int64, d string) error {
	if f.delivery == nil {
		f.delivery = map[int64]string{}
	}
	f.delivery[id] = d
	return nil
}

func drainPerimeter(p *PerimeterWatchPublisher) {
	for {
		select {
		case s := <-p.queue:
			p.handle(context.Background(), s)
		default:
			return
		}
	}
}

func TestPerimeterWatchQueuesOnlyThisServersPlayers(t *testing.T) {
	store := &fakePerimeterStore{}
	p := NewPerimeterWatchPublisher(store, &fakeDM{}, 1, 2)
	p.ObserveLocations([]killfeed.LocationSample{{ServerID: 9, PlayerID: 5}, {ServerID: 2, PlayerID: 0}, {ServerID: 2, PlayerID: 5, X: 1, Z: 2}})
	drainPerimeter(p)
	if store.calls != 1 {
		t.Fatalf("calls = %d", store.calls)
	}
	for i := 0; i < perimeterQueueCap+3; i++ {
		p.ObserveLocations([]killfeed.LocationSample{{ServerID: 2, PlayerID: 5}})
	}
	if p.Dropped() != 3 {
		t.Fatalf("dropped = %d", p.Dropped())
	}
}

func TestPerimeterWatchDeliveryOutcomes(t *testing.T) {
	linked := repository.PerimeterMatch{BaseID: 7, BaseName: "North Base", OwnerDiscordUserID: "owner", DistanceMeters: 120}
	sample := killfeed.LocationSample{ServerID: 2, PlayerID: 5, Gamertag: "Visitor", ObservedAt: time.Unix(1_800_000_000, 0)}

	store, dm := &fakePerimeterStore{matches: []repository.PerimeterMatch{linked}, nextID: 3}, &fakeDM{}
	p := NewPerimeterWatchPublisher(store, dm, 1, 2)
	p.ObserveLocations([]killfeed.LocationSample{sample})
	drainPerimeter(p)
	if store.delivery[3] != repository.BaseRaidDeliverySent || len(dm.to) != 1 {
		t.Fatalf("linked owner: %+v", store.delivery)
	}

	store, dm = &fakePerimeterStore{matches: []repository.PerimeterMatch{linked}, nextID: 0}, &fakeDM{}
	p = NewPerimeterWatchPublisher(store, dm, 1, 2)
	p.ObserveLocations([]killfeed.LocationSample{sample})
	drainPerimeter(p)
	if len(dm.to) != 0 {
		t.Fatal("cooldown alert delivered")
	}

	unlinked := linked
	unlinked.OwnerDiscordUserID = ""
	store, dm = &fakePerimeterStore{matches: []repository.PerimeterMatch{unlinked}, nextID: 4}, &fakeDM{}
	p = NewPerimeterWatchPublisher(store, dm, 1, 2)
	p.ObserveLocations([]killfeed.LocationSample{sample})
	drainPerimeter(p)
	if store.delivery[4] != repository.BaseRaidDeliveryOwnerNotLinked || len(dm.to) != 0 {
		t.Fatalf("unlinked owner: %+v", store.delivery)
	}
}

func TestPerimeterWatchMessageIsPlainAndSafe(t *testing.T) {
	msg := PerimeterWatchMessage("@everyone Base", "Champions", "@here `x`", 120, time.Unix(1_800_000_000, 0))
	e := msg.Embeds[0]
	all := e.Title + e.Description
	for _, f := range e.Fields {
		all += f.Value
	}
	if msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 || strings.Contains(all, "@everyone") || strings.Contains(all, "@here") || strings.Contains(all, "`") {
		t.Fatalf("unsafe: %q", all)
	}
	if !strings.Contains(all, "Someone is near") || !strings.Contains(all, "About 120 m") {
		t.Fatalf("copy: %q", all)
	}
}
