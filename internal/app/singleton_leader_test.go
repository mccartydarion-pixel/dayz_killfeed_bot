package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/leader"
)

// gateLocker is a leader lock the test opens and closes.
type gateLocker struct {
	mu   sync.Mutex
	free bool
	held *gateLock
}

type gateLock struct {
	mu   sync.Mutex
	lost bool
}

func (g *gateLocker) TryAcquire(context.Context) (leader.Lock, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.free {
		return nil, nil // another process holds it
	}
	g.held = &gateLock{}
	return g.held, nil
}

func (g *gateLocker) set(free bool) {
	g.mu.Lock()
	g.free = free
	if !free && g.held != nil {
		g.held.mu.Lock()
		g.held.lost = true
		g.held.mu.Unlock()
	}
	g.mu.Unlock()
}

func (k *gateLock) Check(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.lost {
		return context.Canceled
	}
	return nil
}

func (k *gateLock) Release(context.Context) error { return nil }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSingletonWithoutElectionRunsTheWorkerDirectly(t *testing.T) {
	a := &App{} // no database: no election, exactly the old `go fn(ctx)` behaviour
	ran := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.singleton(ctx, "w", func(context.Context) { close(ran) })
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not start")
	}
	if !a.isSingletonLeader() {
		t.Fatal("a process without an election must act as the leader")
	}
	l := a.runtimeLeadership()
	if l.Enabled || !l.Leader || l.Since != nil {
		t.Fatalf("leadership without election: %+v", l)
	}
	if c := leaderHealthComponent(a.Leader.Status()); c.State != health.Healthy || !strings.Contains(c.Message, "no leader lock") {
		t.Fatalf("health component: %+v", c)
	}
}

func TestSingletonWorkerFollowsLeadership(t *testing.T) {
	locker := &gateLocker{}
	a := &App{Leader: leader.New(locker, leader.Options{RetryEvery: 5 * time.Millisecond, CheckEvery: 5 * time.Millisecond})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Leader.Run(ctx)

	var mu sync.Mutex
	starts, running := 0, 0
	go a.singleton(ctx, "w", func(ctx context.Context) {
		mu.Lock()
		starts++
		running++
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		running--
		mu.Unlock()
	})
	state := func() (int, int) { mu.Lock(); defer mu.Unlock(); return starts, running }

	// Standby: another process leads, nothing runs here, and that is healthy.
	time.Sleep(40 * time.Millisecond)
	if s, _ := state(); s != 0 || a.isSingletonLeader() {
		t.Fatalf("a standby process ran the worker: starts=%d leader=%v", s, a.isSingletonLeader())
	}
	if c := leaderHealthComponent(a.Leader.Status()); c.State != health.Healthy || !strings.HasPrefix(c.Message, "standby since ") {
		t.Fatalf("standby health: %+v", c)
	}
	if l := a.runtimeLeadership(); !l.Enabled || l.Leader || l.Since == nil {
		t.Fatalf("standby leadership: %+v", l)
	}

	locker.set(true) // the other process went away
	waitUntil(t, "worker to start", func() bool { _, r := state(); return r == 1 })
	l := a.runtimeLeadership()
	if !l.Leader || l.Since == nil || l.Acquisitions != 1 {
		t.Fatalf("leader leadership: %+v", l)
	}
	if c := leaderHealthComponent(a.Leader.Status()); c.State != health.Healthy || c.Message != "leader since "+*l.Since {
		t.Fatalf("leader health: %+v", c)
	}
	raw, _ := json.Marshal(RuntimeStatusResponse{OK: true, Leadership: l})
	if !strings.Contains(string(raw), `"leadership":{"enabled":true,"leader":true,"since":"`) {
		t.Fatalf("runtime status JSON: %s", raw)
	}

	locker.set(false) // the lock is lost: the work stops
	waitUntil(t, "worker to stop", func() bool { _, r := state(); return r == 0 && !a.isSingletonLeader() })
	locker.set(true)
	waitUntil(t, "worker to resume", func() bool { s, r := state(); return s == 2 && r == 1 })
}

func TestLeaderHealthIsDegradedWhenTheLockCannotBeReached(t *testing.T) {
	since := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c := leaderHealthComponent(leader.Status{Enabled: true, Since: since, LastError: "connection refused"})
	if c.State != health.Degraded || c.Critical || c.Message != "not leader since 2026-10-05T12:00:00Z: cannot take the leader lock (connection refused)" {
		t.Fatalf("component: %+v", c)
	}
}
