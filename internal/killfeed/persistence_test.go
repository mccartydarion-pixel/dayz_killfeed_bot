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
	mu         sync.Mutex
	kills      []repository.KillRecord
	deaths     []repository.DeathRecord
	players    map[string]int64
	dupeOnKill bool
	failKills  bool
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
