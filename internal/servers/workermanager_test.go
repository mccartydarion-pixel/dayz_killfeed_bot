package servers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWorkerManagerIsolation(t *testing.T) {
	m := NewWorkerManager(func(ctx context.Context, id int64) error { <-ctx.Done(); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, 1); err == nil {
		t.Fatal("duplicate worker")
	}
	if err := m.Start(ctx, 2); err != nil {
		t.Fatal(err)
	}
	m.Stop(1)
	m.StopAll()
	if Jitter(time.Second, -time.Millisecond) != time.Second+time.Millisecond {
		t.Fatal("jitter")
	}
}

// TestWorkerManagerMultiServerIndependentOperation starts 3+ mock "servers"
// behind one WorkerManager and asserts they operate independently: each
// worker maintains its own isolated counter (simulating a per-server
// PlayerTracker/checkpoint), and none observes another server's state.
func TestWorkerManagerMultiServerIndependentOperation(t *testing.T) {
	var mu sync.Mutex
	ticks := make(map[int64]int)

	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		// Simulate an isolated per-server worker loop (like Engine.Start):
		// each worker only ever touches its own map entry, never another's.
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				mu.Lock()
				ticks[id]++
				mu.Unlock()
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverIDs := []int64{101, 102, 103}
	for _, id := range serverIDs {
		if err := m.Start(ctx, id); err != nil {
			t.Fatalf("start worker %d: %v", id, err)
		}
	}
	for _, id := range serverIDs {
		if !m.Running(id) {
			t.Fatalf("worker %d not running", id)
		}
	}

	time.Sleep(30 * time.Millisecond)

	// Stop one server; the others must keep running and keep advancing
	// independently (no shared mutable state / no cross-server coupling).
	m.Stop(serverIDs[0])
	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	stoppedCount := ticks[serverIDs[0]]
	mu.Unlock()

	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if ticks[serverIDs[0]] != stoppedCount {
		t.Fatalf("stopped server %d kept advancing: %d -> %d", serverIDs[0], stoppedCount, ticks[serverIDs[0]])
	}
	for _, id := range serverIDs[1:] {
		if ticks[id] == 0 {
			t.Fatalf("server %d made no independent progress", id)
		}
	}
	if m.Running(serverIDs[0]) {
		t.Fatalf("server %d should have stopped", serverIDs[0])
	}
	for _, id := range serverIDs[1:] {
		if !m.Running(id) {
			t.Fatalf("server %d should still be running", id)
		}
	}
	m.StopAll()
}

// TestWorkerManagerPanicIsolation proves a panic in one server's worker is
// recovered and never affects, crashes, or stops any other server's worker
// or the test process itself.
func TestWorkerManagerPanicIsolation(t *testing.T) {
	var mu sync.Mutex
	healthyTicks := 0
	recovered := make(chan struct{}, 1)

	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		if id == 1 {
			// Simulate a Nitrado-outage-triggered panic on server 1.
			panic(fmt.Sprintf("simulated fault on server %d", id))
		}
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				mu.Lock()
				healthyTicks++
				mu.Unlock()
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.Start(ctx, 1); err != nil {
		t.Fatalf("start faulty worker: %v", err)
	}
	if err := m.Start(ctx, 2); err != nil {
		t.Fatalf("start healthy worker: %v", err)
	}

	// Poll until the panicking worker has been removed by the recover()
	// boundary (proves the panic did not propagate out of WorkerManager).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !m.Running(1) {
			select {
			case recovered <- struct{}{}:
			default:
			}
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case <-recovered:
	default:
		t.Fatal("panicking worker was never cleaned up; recover() boundary did not fire")
	}

	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	progressed := healthyTicks > 0
	mu.Unlock()
	if !progressed {
		t.Fatal("healthy worker made no progress after sibling panic; isolation failed")
	}
	if !m.Running(2) {
		t.Fatal("healthy worker was incorrectly stopped by sibling's panic")
	}
	m.StopAll()
}
