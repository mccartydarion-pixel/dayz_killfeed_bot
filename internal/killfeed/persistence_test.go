package killfeed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakePersistenceStore records inserts and can simulate duplicates/failures.
type fakePersistenceStore struct {
	mu          sync.Mutex
	kills       []repository.KillRecord
	deaths      []repository.DeathRecord
	players     map[string]int64
	connects    []struct{ guildID, serverID, playerID int64 }
	disconnects int
	dupeOnKill  bool
	failKills   bool
}

func newFakePersistenceStore() *fakePersistenceStore {
	return &fakePersistenceStore{players: map[string]int64{}}
}

func (f *fakePersistenceStore) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.players[dayzID]; ok {
		return id, nil
	}
	id := int64(len(f.players) + 1)
	f.players[dayzID] = id
	return id, nil
}

func (f *fakePersistenceStore) InsertKill(ctx context.Context, k repository.KillRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failKills {
		return errors.New("db down")
	}
	if f.dupeOnKill {
		return repository.ErrDuplicate
	}
	for _, existing := range f.kills {
		if existing.Fingerprint == k.Fingerprint {
			return repository.ErrDuplicate
		}
	}
	f.kills = append(f.kills, k)
	return nil
}

func (f *fakePersistenceStore) InsertDeath(ctx context.Context, d repository.DeathRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.deaths {
		if existing.Fingerprint == d.Fingerprint {
			return repository.ErrDuplicate
		}
	}
	f.deaths = append(f.deaths, d)
	return nil
}

func (f *fakePersistenceStore) RecordConnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, struct{ guildID, serverID, playerID int64 }{guildID, serverID, playerID})
	return nil
}

func (f *fakePersistenceStore) RecordDisconnect(context.Context, int64, int64, int64, time.Time) error {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
	return nil
}

type sequenceParser struct{}

func (sequenceParser) ParseLine(line string) (*Event, error) {
	switch line {
	case "CONNECT":
		return &Event{Type: EventPlayerConnect, Player: &PlayerRef{ID: "p1", Name: "TCP"}}, nil
	case "KILL":
		return killEvent("v", "k", "M4-A1", 10, "16:40:12"), nil
	case "DISCONNECT":
		return &Event{Type: EventPlayerDisconnect, Player: &PlayerRef{ID: "p1", Name: "TCP"}}, nil
	default:
		return nil, nil
	}
}

func TestPersistenceFailureStopsLaterADMEventsUntilRecovery(t *testing.T) {
	store := newFakePersistenceStore()
	store.failKills = true
	queue := NewPersistenceQueueWithServerID(store, 1, 2, "session")
	var published int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go queue.Run(ctx)

	engine := NewEngine(nil, "service", sequenceParser{})
	engine.SetPersistence(queue)
	queue.SetKillPersistedHook(func(*Event) { published++ })
	engine.processLines([]string{"CONNECT", "KILL", "DISCONNECT"})
	if engine.PlayerTracker().OnlineCount() != 1 {
		t.Fatalf("expected connect to mutate tracker, got %d", engine.PlayerTracker().OnlineCount())
	}
	if len(store.connects) != 1 || store.disconnects != 0 {
		t.Fatalf("expected only connect persisted, connects=%d disconnects=%d", len(store.connects), store.disconnects)
	}
	if published != 0 {
		t.Fatalf("failed kill must not publish, got %d", published)
	}

	store.failKills = false
	engine.processLines([]string{"KILL", "DISCONNECT"})
	queue.Close()
	if len(store.kills) != 1 || published != 1 {
		t.Fatalf("expected one recovered kill/publication, kills=%d published=%d", len(store.kills), published)
	}
	if store.disconnects != 1 || engine.PlayerTracker().OnlineCount() != 0 {
		t.Fatalf("expected disconnect after recovery, disconnects=%d online=%d", store.disconnects, engine.PlayerTracker().OnlineCount())
	}
}

func TestPersistBeforePublish(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")

	var published []string
	pq.SetKillPersistedHook(func(ev *Event) {
		published = append(published, ev.Killer.Name)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12"))
	pq.Close()

	if len(store.kills) != 1 {
		t.Fatalf("expected 1 persisted kill, got %d", len(store.kills))
	}
	if len(published) != 1 || published[0] != "MmeyAFK_7" {
		t.Fatalf("expected exactly 1 publish after persist, got %v", published)
	}
}

// TestDeathPersistedHookFiresForDeathAndSuicideOnly proves the death-feed
// hook (mirroring onKillPersisted) fires for PLAYER_DEATH and SUICIDE_ACTION
// after a successful durable insert, and does not fire for a plain connect.
func TestDeathPersistedHookFiresForDeathAndSuicideOnly(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")

	var published []EventType
	pq.SetDeathPersistedHook(func(ev *Event) { published = append(published, ev.Type) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(&Event{Type: EventPlayerDeath, Player: &PlayerRef{Name: "victim1", ID: "v1"}, TimeOfDay: "10:00:00"})
	pq.Enqueue(&Event{Type: EventSuicideAction, Player: &PlayerRef{Name: "victim2", ID: "v2"}, TimeOfDay: "10:01:00"})
	pq.Enqueue(&Event{Type: EventPlayerConnect, Player: &PlayerRef{Name: "victim3", ID: "v3"}})
	pq.Close()

	if len(store.deaths) != 2 {
		t.Fatalf("expected 2 persisted deaths, got %d", len(store.deaths))
	}
	if len(published) != 2 || published[0] != EventPlayerDeath || published[1] != EventSuicideAction {
		t.Fatalf("expected hook to fire for death then suicide only, got %v", published)
	}
}

// fakeDeathPostProcessor records calls and stamps a fixed CombatRecord onto
// the event, mirroring how app.go's real implementation enriches it.
type fakeDeathPostProcessor struct {
	calls   int
	lastRec repository.DeathRecord
}

func (f *fakeDeathPostProcessor) ProcessPersistedDeath(ctx context.Context, rec repository.DeathRecord, ev *Event) {
	f.calls++
	f.lastRec = rec
	ev.PlayerStats = &CombatRecord{Kills: 5, Deaths: 2}
}

// TestDeathPostProcessorEnrichesEventBeforePublish proves ProcessPersistedDeath
// runs after a successful durable insert and before the death-feed hook fires,
// with its enrichment visible to that hook - mirroring how kills already work
// via KillPostProcessor.
func TestDeathPostProcessorEnrichesEventBeforePublish(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")
	processor := &fakeDeathPostProcessor{}
	pq.SetDeathPostProcessor(processor)

	var published *Event
	pq.SetDeathPersistedHook(func(ev *Event) { published = ev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(&Event{Type: EventPlayerDeath, Player: &PlayerRef{Name: "victim1", ID: "v1"}, TimeOfDay: "10:00:00"})
	pq.Close()

	if processor.calls != 1 {
		t.Fatalf("expected ProcessPersistedDeath called once, got %d", processor.calls)
	}
	if processor.lastRec.GuildID != 1 {
		t.Fatalf("expected the record's guild ID propagated, got %d", processor.lastRec.GuildID)
	}
	if published == nil || published.PlayerStats == nil || published.PlayerStats.Kills != 5 {
		t.Fatalf("expected the hook to see the processor's enrichment, got %+v", published)
	}
}

// TestDuplicateDeathNotPublished proves durable dedupe suppresses a republish
// for a death event, same as TestDuplicateKillNotPublished for kills.
func TestDuplicateDeathNotPublished(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")

	var published int
	pq.SetDeathPersistedHook(func(*Event) { published++ })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	ev := &Event{Type: EventPlayerDeath, Player: &PlayerRef{Name: "victim1", ID: "v1"}, TimeOfDay: "10:00:00"}
	pq.Enqueue(ev)
	pq.Enqueue(ev) // same fingerprint -> durable dedupe must suppress second publish
	pq.Close()

	if len(store.deaths) != 1 {
		t.Fatalf("expected 1 persisted death, got %d", len(store.deaths))
	}
	if published != 1 {
		t.Fatalf("expected exactly 1 publish (duplicate suppressed), got %d", published)
	}
}

func TestDuplicateKillNotPublished(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")

	var published int
	pq.SetKillPersistedHook(func(ev *Event) { published++ })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	ev := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")
	pq.Enqueue(ev)
	pq.Enqueue(ev) // same fingerprint -> durable dedupe must suppress second publish
	pq.Close()

	if len(store.kills) != 1 {
		t.Fatalf("expected 1 persisted kill, got %d", len(store.kills))
	}
	if published != 1 {
		t.Fatalf("expected exactly 1 publish (duplicate suppressed), got %d", published)
	}
}

func TestDBOutageDoesNotPublish(t *testing.T) {
	store := newFakePersistenceStore()
	store.failKills = true
	pq := NewPersistenceQueue(store, 1, "sess-1")

	var published int
	pq.SetKillPersistedHook(func(ev *Event) { published++ })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(killEvent("v", "k", "M4-A1", 10, "16:40:12"))
	pq.Close()

	if published != 0 {
		t.Fatalf("expected no publish when DB insert failed, got %d", published)
	}
}

func TestEnqueueAndWaitReturnsPersistenceFailure(t *testing.T) {
	store := newFakePersistenceStore()
	store.failKills = true
	pq := NewPersistenceQueue(store, 1, "sess-ack")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)
	err := pq.EnqueueAndWait(context.Background(), killEvent("v", "k", "M4-A1", 10, "16:40:12"))
	pq.Close()
	if err == nil {
		t.Fatal("expected persistence acknowledgement failure")
	}
}

func TestPersistenceQueueOverflowDropsNewest(t *testing.T) {
	store := newFakePersistenceStore()
	// Don't run the worker so the queue fills.
	pq := NewPersistenceQueue(store, 1, "sess-1")

	// Fill the queue to capacity.
	for i := 0; i < maxPersistenceQueue; i++ {
		if !pq.Enqueue(killEvent("v", "k", "M4-A1", float64(i), "16:40:12")) {
			break
		}
	}
	// Next enqueue must be dropped (deterministic overflow) and counted.
	if pq.Enqueue(killEvent("overflow", "k", "M4-A1", 999, "16:40:13")) {
		t.Fatal("expected overflow enqueue to be rejected")
	}
	if pq.Dropped() != 1 {
		t.Fatalf("expected dropped=1, got %d", pq.Dropped())
	}
}

func TestPlayerUpsertTracksIdentity(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(&Event{Type: EventPlayerConnect, Player: &PlayerRef{ID: "p1", Name: "Ceiyxe"}})
	pq.Enqueue(&Event{Type: EventPlayerConnect, Player: &PlayerRef{ID: "p1", Name: "CeiyxeNew"}})
	pq.Close()

	if len(store.players) != 1 {
		t.Fatalf("expected 1 unique player by ID, got %d", len(store.players))
	}
}

func TestPlayerConnectPersistsServerActivity(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueueWithServerID(store, 11, 22, "service-22")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(&Event{Type: EventPlayerConnect, Player: &PlayerRef{ID: "p1", Name: "TCP"}})
	pq.Close()

	if len(store.connects) != 1 {
		t.Fatalf("expected one persisted connect activity row, got %d", len(store.connects))
	}
	got := store.connects[0]
	if got.guildID != 11 || got.serverID != 22 || got.playerID == 0 {
		t.Fatalf("unexpected activity scope: %+v", got)
	}
}
