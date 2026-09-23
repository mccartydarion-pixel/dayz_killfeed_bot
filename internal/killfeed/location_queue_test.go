package killfeed

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeLocationStore is an in-memory LocationStore for unit testing, recording every insert call
// (and its batch contents/order) so tests can assert on ordering and batching directly.
type fakeLocationStore struct {
	mu        sync.Mutex
	nextID    int64
	playerIDs map[string]int64
	batches   [][]repository.LocationEventInput
	failNext  bool
}

func newFakeLocationStore() *fakeLocationStore {
	return &fakeLocationStore{playerIDs: map[string]int64{}}
}

func (s *fakeLocationStore) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.playerIDs[dayzID]; ok {
		return id, nil
	}
	s.nextID++
	s.playerIDs[dayzID] = s.nextID
	return s.nextID, nil
}

func (s *fakeLocationStore) InsertLocationEvents(ctx context.Context, events []repository.LocationEventInput) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext {
		s.failNext = false
		return 0, context.DeadlineExceeded
	}
	cp := make([]repository.LocationEventInput, len(events))
	copy(cp, events)
	s.batches = append(s.batches, cp)
	return len(events), nil
}

func (s *fakeLocationStore) allRecords() []repository.LocationEventInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []repository.LocationEventInput
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

func (s *fakeLocationStore) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.batches)
}

func posEvent(playerID string, eventType EventType, x, y, z float64, at time.Time) *Event {
	return &Event{Type: eventType, Timestamp: at, Player: &PlayerRef{ID: playerID, Name: "Player-" + playerID, Position: &Position{X: x, Y: y, Z: z}}}
}

func TestLocationQueueEnqueueAndFlushOnSize(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	defer q.Close()

	base := time.Now()
	for i := 0; i < locationBatchSize; i++ {
		q.EnqueueEvent(posEvent("p1", EventPlayerConnect, float64(i), 1, 2, base.Add(time.Duration(i)*time.Second)))
	}
	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected a batch to flush once locationBatchSize candidates were enqueued")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := len(store.allRecords()); got != locationBatchSize {
		t.Fatalf("expected %d records persisted, got %d", locationBatchSize, got)
	}
}

func TestLocationQueueFlushesOnTickerWithoutReachingBatchSize(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	defer q.Close()

	q.EnqueueEvent(posEvent("p1", EventPlayerConnect, 1, 1, 1, time.Now()))
	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected the periodic ticker to flush a single below-threshold candidate")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := len(store.allRecords()); got != 1 {
		t.Fatalf("expected 1 record persisted, got %d", got)
	}
}

func TestLocationQueueClosePreservesEnqueueOrderAndFlushesRemainder(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	base := time.Now()
	const n = 250 // spans multiple batches (locationBatchSize=100) plus a partial remainder
	for i := 0; i < n; i++ {
		q.EnqueueEvent(posEvent("p1", EventPlayerHit, float64(i), 0, 0, base.Add(time.Duration(i)*time.Millisecond)))
	}
	q.Close() // must drain and flush every remaining candidate before returning

	records := store.allRecords()
	if len(records) != n {
		t.Fatalf("expected all %d candidates persisted after Close, got %d", n, len(records))
	}
	for i, r := range records {
		if r.X != float64(i) {
			t.Fatalf("expected strict enqueue order preserved (task: no event reordering), record %d has X=%v, want %v", i, r.X, float64(i))
		}
	}
}

func TestLocationQueueDropsOnFullQueueAndCountsIt(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	// No Run() goroutine started: nothing drains the channel, so it fills up deterministically.
	base := time.Now()
	for i := 0; i < maxLocationQueue+50; i++ {
		q.EnqueueEvent(posEvent("p1", EventPlayerConnect, float64(i), 0, 0, base.Add(time.Duration(i)*time.Second)))
	}
	health := q.Health()
	if health.Dropped == 0 {
		t.Fatal("expected some candidates to be dropped once the bounded queue filled up")
	}
	if health.Seen != int64(maxLocationQueue+50) {
		t.Fatalf("expected seen to count every EnqueueEvent call regardless of drop, got %d", health.Seen)
	}
	if health.Depth > health.Capacity {
		t.Fatalf("depth %d must never exceed capacity %d", health.Depth, health.Capacity)
	}
}

func TestLocationQueueEnqueueEventSkipsNilPositionAndEmptyID(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	defer q.Close()

	q.EnqueueEvent(&Event{Type: EventPlayerConnect, Timestamp: time.Now(), Player: &PlayerRef{ID: "p1", Name: "NoPos"}})
	q.EnqueueEvent(&Event{Type: EventPlayerConnect, Timestamp: time.Now(), Player: &PlayerRef{ID: "", Name: "NoID", Position: &Position{X: 1, Y: 1, Z: 1}}})
	time.Sleep(50 * time.Millisecond)
	if health := q.Health(); health.Seen != 0 {
		t.Fatalf("expected 0 candidates enqueued for events with no position or no player id, got %d", health.Seen)
	}
}

func TestLocationQueueOneEventWithMultiplePositionedPlayersEnqueuesEach(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	defer q.Close()

	ev := &Event{
		Type: EventPlayerKill, Timestamp: time.Now(),
		Killer: &PlayerRef{ID: "killer1", Name: "Killer", Position: &Position{X: 1, Y: 2, Z: 3}},
		Victim: &PlayerRef{ID: "victim1", Name: "Victim", Position: &Position{X: 4, Y: 5, Z: 6}},
	}
	q.EnqueueEvent(ev)
	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected the ticker to flush both candidates")
		case <-time.After(10 * time.Millisecond):
		}
	}
	records := store.allRecords()
	if len(records) != 2 {
		t.Fatalf("expected a kill with both killer and victim positions to enqueue 2 location candidates, got %d", len(records))
	}
	for _, r := range records {
		if r.EventType != LocationEventKill {
			t.Fatalf("expected event type KILL, got %s", r.EventType)
		}
	}
}

func TestLocationQueueYIsAlwaysPopulatedWhenPositionPresent(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	defer q.Close()

	q.EnqueueEvent(posEvent("p1", EventPlayerConnect, 10, 20, 30, time.Now()))
	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected a flush")
		case <-time.After(10 * time.Millisecond):
		}
	}
	records := store.allRecords()
	// Position{10,20,30} is ADM "pos=<10, 20, 30>": altitude is the third value.
	if len(records) != 1 || records[0].Y == nil || *records[0].Y != 30 {
		t.Fatalf("expected Y (altitude) = 30 to be populated, got %+v", records)
	}
}

// TestLocationQueuePersistsADMAxesAsMapXZ pins the ADM axis order end to end: a real ADM line is
// "pos=<east, north, altitude>" (DayZ's PluginAdminLog prints engine [0],[2],[1]), so the persisted
// row must carry x=east, z=north, y=altitude - never altitude in z.
func TestLocationQueuePersistsADMAxesAsMapXZ(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	defer q.Close()

	ev, err := NewADMParser().ParseLine(`16:25:41 | Player "NoxiKillNewB" (id=n002 pos=<7504.7, 1334.4, 0.9>) is connected`)
	if err != nil || ev == nil {
		t.Fatalf("parse: ev=%v err=%v", ev, err)
	}
	q.EnqueueEvent(ev)
	deadline := time.After(2 * time.Second)
	for store.batchCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("expected a flush")
		case <-time.After(10 * time.Millisecond):
		}
	}
	records := store.allRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	r := records[0]
	if r.X != 7504.7 || r.Z != 1334.4 || r.Y == nil || *r.Y != 0.9 {
		t.Fatalf("expected x=7504.7 z=1334.4 y=0.9, got x=%v z=%v y=%v", r.X, r.Z, r.Y)
	}
}

func TestPositionMapAccessorsFollowADMOrder(t *testing.T) {
	p := Position{X: 7434.4, Y: 1401.4, Z: 5.7}
	if p.MapX() != 7434.4 || p.MapZ() != 1401.4 || p.Altitude() != 5.7 {
		t.Fatalf("got MapX=%v MapZ=%v Altitude=%v", p.MapX(), p.MapZ(), p.Altitude())
	}
}

func TestLocationEventTypeForMapping(t *testing.T) {
	cases := map[EventType]string{
		EventPlayerConnect:     LocationEventConnect,
		EventPlayerDisconnect:  LocationEventDisconnect,
		EventPlayerHit:         LocationEventHit,
		EventPlayerKill:        LocationEventKill,
		EventPlayerDeath:       LocationEventDeath,
		EventSuicideAction:     LocationEventDeath,
		EventPlayerRespawn:     LocationEventRespawn,
		EventPlayerUnconscious: LocationEventUnconscious,
		EventPlayerConscious:   LocationEventOther,
	}
	for in, want := range cases {
		if got := locationEventTypeFor(in); got != want {
			t.Errorf("locationEventTypeFor(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestLocationQueueNilSafe(t *testing.T) {
	var q *LocationQueue
	q.EnqueueEvent(posEvent("p1", EventPlayerConnect, 1, 1, 1, time.Now())) // must not panic
	if h := q.Health(); h != (LocationQueueHealth{}) {
		t.Fatalf("expected zero-value health from a nil queue, got %+v", h)
	}
	q.Close() // must not panic or hang
}

// TestLocationQueueZoneIntrusionUsesADMHorizontalAxes feeds a real ADM line through the queue into
// the intrusion engine: a zone centred on the player's east/north coordinates must trigger. Under
// the old mapping (z = ADM altitude) the player sat ~1330m south of the zone and never tripped it.
func TestLocationQueueZoneIntrusionUsesADMHorizontalAxes(t *testing.T) {
	store := newFakeLocationStore()
	q := NewLocationQueue(store, 1, 5)
	istore := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 7500, 1330, 50, 300, "chan-1")}})
	pub := &fakePublisher{}
	q.SetIntrusionEngine(NewIntrusionEngine(istore, cache, nil, pub))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	ev, err := NewADMParser().ParseLine(`16:25:41 | Player "NoxiKillNewB" (id=n002 pos=<7504.7, 1334.4, 0.9>) is connected`)
	if err != nil || ev == nil {
		t.Fatalf("parse: ev=%v err=%v", ev, err)
	}
	q.EnqueueEvent(ev)
	q.Close()

	if len(istore.created) != 1 || pub.count() != 1 {
		t.Fatalf("expected the zone at (7500,1330) r=50 to register 1 intrusion + 1 alert, got created=%d published=%d", len(istore.created), pub.count())
	}
}
