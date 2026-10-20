package killfeed

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// stagedStore is a persistence store whose kill insert returns an id, so the post-processor runs.
type stagedStore struct {
	*fakePersistenceStore
	nextID int64
}

func (s *stagedStore) InsertKillReturning(ctx context.Context, k repository.KillRecord) (int64, error) {
	if err := s.InsertKill(ctx, k); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID, nil
}

// stagedRecorder is a two-stage post-processor that records the order of everything and holds
// its finish until released.
type stagedRecorder struct {
	mu      sync.Mutex
	order   []string
	release chan struct{}
}

func (r *stagedRecorder) note(s string) {
	r.mu.Lock()
	r.order = append(r.order, s)
	r.mu.Unlock()
}

func (r *stagedRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func (r *stagedRecorder) ProcessPersistedKill(ctx context.Context, killID int64, rec repository.KillRecord, ev *Event) {
	r.note("legacy")
}

func (r *stagedRecorder) ProcessPersistedKillStaged(ctx context.Context, killID int64, rec repository.KillRecord, ev *Event) func(context.Context) {
	r.note("card-inputs")
	return func(context.Context) {
		r.note("finish-start")
		<-r.release
		r.note("finish-end")
	}
}

func (r *stagedRecorder) ProcessPersistedDeath(ctx context.Context, rec repository.DeathRecord, ev *Event) {
	r.note("legacy-death")
}

func (r *stagedRecorder) ProcessPersistedDeathStaged(ctx context.Context, rec repository.DeathRecord, ev *Event) func(context.Context) {
	r.note("death-card-inputs")
	return func(context.Context) {
		r.note("death-finish-start")
		<-r.release
		r.note("death-finish-end")
	}
}

func stagedKillEvent(victim string, at time.Time) *Event {
	return &Event{Type: EventPlayerKill, Timestamp: at,
		Killer: &PlayerRef{ID: "k1", Name: "Killer"}, Victim: &PlayerRef{ID: victim, Name: victim}}
}

func waitReturns(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// The card is queued before the writes nothing on it depends on; those still run in log order
// and before the engine is answered, so EnqueueAndWait keeps meaning "all of this event's work
// is done" and the next event never overtakes them.
func TestPersistenceQueuesCardBeforeFinishAndAnswersAfter(t *testing.T) {
	store := &stagedStore{fakePersistenceStore: newFakePersistenceStore()}
	rec := &stagedRecorder{release: make(chan struct{})}
	pq := NewPersistenceQueueWithServerID(store, 1, 1, "s")
	pq.SetKillPostProcessor(rec)
	pq.SetDeathPostProcessor(rec)
	pq.SetKillPersistedHook(func(ev *Event) { rec.note("card-queued") })
	pq.SetDeathPersistedHook(func(ev *Event) { rec.note("death-card-queued") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	now := time.Now()
	first := make(chan error, 1)
	go func() { first <- pq.EnqueueAndWait(ctx, stagedKillEvent("v1", now)) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.seen()) < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := rec.seen(); len(got) != 3 || got[0] != "card-inputs" || got[1] != "card-queued" || got[2] != "finish-start" {
		t.Fatalf("the card must be queued before finish starts: %v", got)
	}
	select {
	case err := <-first:
		t.Fatalf("the engine was answered while finish was still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(rec.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if got := rec.seen(); len(got) != 4 || got[3] != "finish-end" {
		t.Fatalf("order %v", got)
	}

	// Deaths take the same two stages.
	rec.release = make(chan struct{})
	close(rec.release)
	death := &Event{Type: EventPlayerDeath, Timestamp: now.Add(2 * time.Second), Player: &PlayerRef{ID: "d1", Name: "Dead"}}
	if err := waitReturns(t, "death", func() error { return pq.EnqueueAndWait(ctx, death) }); err != nil {
		t.Fatal(err)
	}
	got := rec.seen()
	tail := got[len(got)-4:]
	if tail[0] != "death-card-inputs" || tail[1] != "death-card-queued" || tail[2] != "death-finish-start" || tail[3] != "death-finish-end" {
		t.Fatalf("death stages %v", tail)
	}
	store.mu.Lock()
	kills, deaths := len(store.kills), len(store.deaths)
	store.mu.Unlock()
	if kills != 1 || deaths != 1 {
		t.Fatalf("persisted kills=%d deaths=%d", kills, deaths)
	}
}

// A duplicate kill neither publishes nor runs either stage, as before.
func TestPersistenceDuplicateKillSkipsBothStages(t *testing.T) {
	store := &stagedStore{fakePersistenceStore: newFakePersistenceStore()}
	rec := &stagedRecorder{release: make(chan struct{})}
	close(rec.release)
	pq := NewPersistenceQueueWithServerID(store, 1, 1, "s")
	pq.SetKillPostProcessor(rec)
	pq.SetKillPersistedHook(func(ev *Event) { rec.note("card-queued") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)
	now := time.Now()
	for i := 0; i < 2; i++ {
		if err := waitReturns(t, "kill", func() error { return pq.EnqueueAndWait(ctx, stagedKillEvent("v1", now)) }); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.seen(); len(got) != 4 {
		t.Fatalf("the replayed line must not run the stages again: %v", got)
	}
}

// A post-processor without the staged form keeps the single-call path.
type legacyOnly struct{ r *stagedRecorder }

func (l legacyOnly) ProcessPersistedKill(ctx context.Context, killID int64, rec repository.KillRecord, ev *Event) {
	l.r.note("legacy")
}

func TestPersistenceLegacyPostProcessorStillRuns(t *testing.T) {
	store := &stagedStore{fakePersistenceStore: newFakePersistenceStore()}
	rec := &stagedRecorder{}
	pq := NewPersistenceQueueWithServerID(store, 1, 1, "s")
	pq.SetKillPostProcessor(legacyOnly{rec})
	pq.SetKillPersistedHook(func(ev *Event) { rec.note("card-queued") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)
	if err := waitReturns(t, "kill", func() error { return pq.EnqueueAndWait(ctx, stagedKillEvent("v1", time.Now())) }); err != nil {
		t.Fatal(err)
	}
	if got := rec.seen(); len(got) != 2 || got[0] != "legacy" || got[1] != "card-queued" {
		t.Fatalf("order %v", got)
	}
}
