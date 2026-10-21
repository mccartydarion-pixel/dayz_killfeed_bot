package servers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
// or the test process itself - and that the panicking worker is restarted
// under supervision rather than silently dropped.
func TestWorkerManagerPanicIsolation(t *testing.T) {
	var mu sync.Mutex
	healthyTicks := 0
	var faultyStarts atomic.Int32
	faultyRecovered := make(chan struct{})

	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		if id == 1 {
			if faultyStarts.Add(1) == 1 {
				// Simulate a Nitrado-outage-triggered panic on server 1.
				panic(fmt.Sprintf("simulated fault on server %d", id))
			}
			close(faultyRecovered)
			<-ctx.Done()
			return nil
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
	m.SetRestartPolicy(RestartPolicy{MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := m.Start(ctx, 1); err != nil {
		t.Fatalf("start faulty worker: %v", err)
	}
	if err := m.Start(ctx, 2); err != nil {
		t.Fatalf("start healthy worker: %v", err)
	}

	// The panic must be recovered inside WorkerManager (it never propagates
	// here) and the worker must come back under supervision.
	select {
	case <-faultyRecovered:
	case <-time.After(2 * time.Second):
		t.Fatal("panicking worker was never restarted; recover()/supervision did not fire")
	}
	if got := m.Status(1); !got.Running || got.Restarts != 1 || got.LastError == "" {
		t.Fatalf("unexpected status after recovered panic: %+v", got)
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
	if m.Running(1) || m.Running(2) {
		t.Fatal("StopAll left workers registered")
	}
}

// TestWorkerManagerStopThenStartRestarts reproduces the restart race: a worker
// whose factory takes a moment to unwind after cancellation (draining queues,
// like the real ADM worker) is stopped and immediately started again. Before
// Stop blocked on the goroutine, the second Start saw the stale entry and the
// "restart" silently did nothing.
func TestWorkerManagerStopThenStartRestarts(t *testing.T) {
	var starts atomic.Int32
	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		starts.Add(1)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // slow teardown after cancellation
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for starts.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	stopped := time.Now()
	m.Stop(1)
	if time.Since(stopped) < 40*time.Millisecond {
		t.Fatal("Stop returned before the worker goroutine finished unwinding")
	}
	if m.Running(1) {
		t.Fatal("worker still reported running after Stop returned")
	}
	if err := m.Start(ctx, 1); err != nil {
		t.Fatalf("restart after Stop must succeed: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for starts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("expected the second Start to really run the worker, factory invocations=%d", got)
	}
	if !m.Running(1) {
		t.Fatal("restarted worker not running")
	}
	m.StopAll()
}

// TestWorkerManagerStopContextTimeout: a worker that ignores cancellation
// cannot hang its caller - StopContext gives up after the stop timeout (or
// the caller's context) and a later Start replaces the stuck instance.
func TestWorkerManagerStopContextTimeout(t *testing.T) {
	release := make(chan struct{})
	var starts atomic.Int32
	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		starts.Add(1)
		<-release // ignores ctx on purpose
		return nil
	})
	m.SetStopTimeout(20 * time.Millisecond)
	defer close(release)
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := m.StopContext(context.Background(), 1); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("expected ErrStopTimeout, got %v", err)
	}
	if m.Running(1) {
		t.Fatal("a worker that was told to stop must not report running")
	}
	callerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := m.StopContext(callerCtx, 1); err == nil {
		t.Fatal("expected an error from a second StopContext on the stuck worker")
	}
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatalf("Start must replace a stuck instance: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for starts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 2 {
		t.Fatal("replacement worker never ran")
	}
	if !m.Running(1) {
		t.Fatal("replacement worker not running")
	}
}

// TestWorkerManagerSupervisedRestart: a factory that fails twice and then runs
// is restarted with backoff until it succeeds, and stays running afterwards.
func TestWorkerManagerSupervisedRestart(t *testing.T) {
	var starts atomic.Int32
	healthy := make(chan struct{})
	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		n := starts.Add(1)
		if n <= 2 {
			return fmt.Errorf("transient failure %d", n)
		}
		close(healthy)
		<-ctx.Done()
		return nil
	})
	m.SetRestartPolicy(RestartPolicy{MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin := time.Now()
	if err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-healthy:
	case <-time.After(2 * time.Second):
		t.Fatalf("worker never recovered; factory invocations=%d", starts.Load())
	}
	// Two backoffs (>=5ms and >=10ms) must have elapsed: restarts are delayed, not hot loops.
	if time.Since(begin) < 15*time.Millisecond {
		t.Fatal("restarts happened without backoff")
	}
	if got := starts.Load(); got != 3 {
		t.Fatalf("expected exactly 3 factory invocations, got %d", got)
	}
	st := m.Status(1)
	if !st.Running || st.Restarting || st.Restarts != 2 || st.LastError != "transient failure 2" {
		t.Fatalf("unexpected status: %+v", st)
	}
	m.Stop(1)
	if m.Running(1) {
		t.Fatal("worker still running after Stop")
	}
	if got := starts.Load(); got != 3 {
		t.Fatalf("an intentional stop must not restart the worker, invocations=%d", got)
	}
}

// TestWorkerManagerNoRestartAfterIntentionalStop: an error returned because
// the worker was cancelled is not a failure to supervise.
func TestWorkerManagerNoRestartAfterIntentionalStop(t *testing.T) {
	var starts atomic.Int32
	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		starts.Add(1)
		<-ctx.Done()
		return ctx.Err()
	})
	m.SetRestartPolicy(RestartPolicy{MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	m.Stop(1)
	time.Sleep(20 * time.Millisecond)
	if got := starts.Load(); got != 1 {
		t.Fatalf("worker restarted after an intentional stop: invocations=%d", got)
	}
	if m.Running(1) {
		t.Fatal("stopped worker still registered")
	}
}

// TestWorkerManagerRestartGateRetires: when the gate says no (the server was
// deactivated while failing), the worker is retired instead of restarted.
func TestWorkerManagerRestartGateRetires(t *testing.T) {
	var starts atomic.Int32
	var gateCalls atomic.Int32
	m := NewWorkerManager(func(ctx context.Context, id int64) error {
		starts.Add(1)
		return errors.New("boom")
	})
	m.SetRestartPolicy(RestartPolicy{MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	m.SetRestartGate(func(ctx context.Context, id int64) bool { gateCalls.Add(1); return false })
	if err := m.Start(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for m.Running(1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Running(1) {
		t.Fatal("gated worker was not retired")
	}
	if starts.Load() != 1 || gateCalls.Load() != 1 {
		t.Fatalf("expected one run and one gate check, got runs=%d gate=%d", starts.Load(), gateCalls.Load())
	}
}

func TestRestartBackoff(t *testing.T) {
	p := RestartPolicy{MinBackoff: time.Second, MaxBackoff: 5 * time.Minute}
	prevMax := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		base := time.Second << (attempt - 1)
		if base > 5*time.Minute {
			base = 5 * time.Minute
		}
		for i := 0; i < 20; i++ {
			d := restartBackoff(p, attempt)
			if d < base || d > 5*time.Minute || d > base+base/2 {
				t.Fatalf("attempt %d: backoff %s outside [%s, min(%s, 5m)]", attempt, d, base, base+base/2)
			}
		}
		if base < prevMax {
			t.Fatalf("backoff not monotonic at attempt %d", attempt)
		}
		prevMax = base
	}
	if d := restartBackoff(RestartPolicy{}, 1); d < time.Second || d > 1500*time.Millisecond {
		t.Fatalf("default first backoff %s", d)
	}
}
