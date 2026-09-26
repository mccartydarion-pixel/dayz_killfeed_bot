package killfeed

import (
	"context"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/nitrado/nitradofixture"
)

// fixturePublisher records kills and deaths published after persistence.
type fixturePublisher struct {
	mu            sync.Mutex
	kills, deaths []*Event
}

func (r *fixturePublisher) PublishKill(ev *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills = append(r.kills, ev)
	return nil
}

func (r *fixturePublisher) PublishDeath(ev *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deaths = append(r.deaths, ev)
	return nil
}

func (r *fixturePublisher) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.kills), len(r.deaths)
}

// TestEngineReadsNitradoFixture runs the production ADM engine and the real
// Nitrado client against the staging Nitrado fixture: discovery through
// game_specific.path, stat, delta reads, parsing and dedupe all work, every
// synthetic kill is published exactly once, deaths stay separate, a fixture
// restart (new ADM) is followed, and a write is refused.
func TestEngineReadsNitradoFixture(t *testing.T) {
	fx := nitradofixture.New(90000001, "")
	srv := httptest.NewServer(fx)
	defer srv.Close()
	svc := strconv.FormatInt(90000001, 10)
	client := nitrado.NewClient(srv.URL, "fixture-token-not-a-credential", nil)
	ctx := context.Background()

	if services, err := client.GetServices(ctx); err != nil || len(nitrado.FindDayZServices(services)) != 1 {
		t.Fatalf("service discovery: %v %+v", err, services)
	}
	e := NewEngine(client, svc, NewADMParser())
	pub := &fixturePublisher{}
	e.SetKillPublisher(pub)
	e.SetDeathPublisher(pub)
	pq := NewPersistenceQueue(newFakePersistenceStore(), 1, "fixture")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go pq.Run(runCtx)
	e.SetPersistence(pq)
	// pollUntil polls like the worker does until the published counts match.
	pollUntil := func(what string, kills, deaths int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := e.PollOnce(ctx); err != nil {
				t.Fatal(err)
			}
			k, d := pub.counts()
			if k == kills && d == deaths {
				return
			}
			if k > kills || d > deaths || time.Now().After(deadline) {
				t.Fatalf("%s: published %d kills / %d deaths, want %d / %d", what, k, d, kills, deaths)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	pollUntil("initial discovery", 0, 0)
	pollUntil("initial read", 0, 0)

	victims := fx.AddKills(15)
	fx.AddDeaths(2)
	pollUntil("15 kills + 2 deaths", 15, 2)
	for i := 0; i < 3; i++ { // polls with no new bytes must not republish
		pollUntil("no republish", 15, 2)
	}
	pub.mu.Lock()
	for i, ev := range pub.kills {
		if ev.Victim == nil || ev.Victim.Name != victims[i] {
			pub.mu.Unlock()
			t.Fatalf("kill %d out of order: %+v", i, ev.Victim)
		}
	}
	pub.mu.Unlock()

	// Server restart: a new ADM file (a later boot); the engine follows it.
	time.Sleep(1100 * time.Millisecond) // ADM names and boot headers have 1s resolution
	fx.Restart()
	fx.AddKills(1)
	pollUntil("kill after restart", 16, 2)

	// Read-only: a restart request is refused and counted, never performed.
	if err := client.Restart(ctx, svc, "staging must not restart servers"); err == nil {
		t.Fatal("fixture accepted a write")
	}
	if fx.Snapshot().RefusedWrites != 1 {
		t.Fatalf("refused writes %d, want 1", fx.Snapshot().RefusedWrites)
	}
}
