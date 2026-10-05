package leader

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeArbiter is the lock itself: at most one holder at a time, like the database.
type fakeArbiter struct {
	mu     sync.Mutex
	holder *fakeLock
}

type fakeLocker struct {
	arbiter *fakeArbiter
	mu      sync.Mutex
	err     error // TryAcquire fails with this while set (the database is unreachable)
	tries   int
	closed  bool
}

type fakeLock struct {
	arbiter  *fakeArbiter
	broken   atomic.Bool // the session died: Check fails
	released atomic.Int32
}

func (l *fakeLocker) TryAcquire(context.Context) (Lock, error) {
	l.mu.Lock()
	l.tries++
	err := l.err
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	l.arbiter.mu.Lock()
	defer l.arbiter.mu.Unlock()
	if l.arbiter.holder != nil {
		return nil, nil
	}
	lock := &fakeLock{arbiter: l.arbiter}
	l.arbiter.holder = lock
	return lock, nil
}

func (l *fakeLocker) Close(context.Context) {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

func (l *fakeLocker) setErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

func (k *fakeLock) Check(context.Context) error {
	if k.broken.Load() {
		return errors.New("connection reset")
	}
	return nil
}

// Release frees the lock, as closing the connection does in PostgreSQL.
func (k *fakeLock) Release(context.Context) error {
	k.released.Add(1)
	k.arbiter.mu.Lock()
	if k.arbiter.holder == k {
		k.arbiter.holder = nil
	}
	k.arbiter.mu.Unlock()
	return nil
}

// drop simulates the database ending the holder's session: the lock is free for others at once,
// and the old holder finds out at its next check.
func (a *fakeArbiter) drop() {
	a.mu.Lock()
	if a.holder != nil {
		a.holder.broken.Store(true)
		a.holder = nil
	}
	a.mu.Unlock()
}

func (a *fakeArbiter) current() *fakeLock {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holder
}

func fastOptions() Options {
	return Options{RetryEvery: 10 * time.Millisecond, CheckEvery: 5 * time.Millisecond, OpTimeout: time.Second}
}

func startElector(t *testing.T, e *Elector) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
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

// worker counts how often it ran and how many copies run right now.
type worker struct {
	starts, running atomic.Int32
}

func (w *worker) run(ctx context.Context) {
	w.starts.Add(1)
	w.running.Add(1)
	<-ctx.Done()
	w.running.Add(-1)
}

func TestSingleProcessBecomesLeaderAtOnceAndReleasesOnStop(t *testing.T) {
	arb := &fakeArbiter{}
	locker := &fakeLocker{arbiter: arb}
	opts := fastOptions()
	opts.RetryEvery = time.Hour // only the first attempt may be needed
	e := New(locker, opts)
	if st := e.Status(); !st.Enabled || st.Leader {
		t.Fatalf("before Run: %+v", st)
	}
	stop := startElector(t, e)
	started := time.Now()
	eventually(t, "leadership", e.IsLeader)
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("a lone process took %s to become leader", waited)
	}
	st := e.Status()
	if !st.Leader || st.Acquisitions != 1 || st.Since.IsZero() || st.LastError != "" {
		t.Fatalf("status: %+v", st)
	}
	held := arb.current()
	stop()
	if e.IsLeader() || arb.current() != nil || held.released.Load() != 1 || !locker.closed {
		t.Fatalf("after stop: leader=%v holder=%v released=%d closed=%v", e.IsLeader(), arb.current(), held.released.Load(), locker.closed)
	}
}

func TestOnlyOneOfTwoRunsAndTheOtherTakesOver(t *testing.T) {
	arb := &fakeArbiter{}
	a, b := New(&fakeLocker{arbiter: arb}, fastOptions()), New(&fakeLocker{arbiter: arb}, fastOptions())
	var wa, wb worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWhileLeader(ctx, "w", wa.run)
	go b.RunWhileLeader(ctx, "w", wb.run)

	stopA := startElector(t, a)
	eventually(t, "a leads and its worker runs", func() bool { return a.IsLeader() && wa.running.Load() == 1 })
	startElector(t, b)
	time.Sleep(60 * time.Millisecond) // several retry periods
	if b.IsLeader() || wb.starts.Load() != 0 || wa.running.Load() != 1 {
		t.Fatalf("two leaders: b=%v b.starts=%d a.running=%d", b.IsLeader(), wb.starts.Load(), wa.running.Load())
	}
	if st := b.Status(); st.LastError != "" {
		t.Fatalf("standing by is not an error: %+v", st)
	}

	stopA() // a shuts down and releases: b takes over
	eventually(t, "b takes over", func() bool { return b.IsLeader() && wb.running.Load() == 1 })
	if a.IsLeader() || wa.running.Load() != 0 {
		t.Fatalf("a still works after stopping: leader=%v running=%d", a.IsLeader(), wa.running.Load())
	}
}

func TestLosingTheLockStopsWorkPromptlyAndItResumesOnReacquire(t *testing.T) {
	arb := &fakeArbiter{}
	locker := &fakeLocker{arbiter: arb}
	e := New(locker, fastOptions())
	var w worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.RunWhileLeader(ctx, "w", w.run)
	startElector(t, e)
	eventually(t, "worker running", func() bool { return w.running.Load() == 1 })

	// The database goes away: the session is dropped and new connections fail.
	locker.setErr(errors.New("connection refused"))
	arb.drop()
	lostAt := time.Now()
	eventually(t, "work stops", func() bool { return !e.IsLeader() && w.running.Load() == 0 })
	if took := time.Since(lostAt); took > time.Second {
		t.Fatalf("work stopped only after %s", took)
	}
	eventually(t, "acquire error is reported", func() bool { return e.Status().LastError == "connection refused" })
	time.Sleep(40 * time.Millisecond)
	if w.starts.Load() != 1 {
		t.Fatalf("worker restarted without the lock: %d starts", w.starts.Load())
	}

	// The database is back: leadership and the work resume without a restart of the process.
	locker.setErr(nil)
	eventually(t, "work resumes", func() bool { return e.IsLeader() && w.running.Load() == 1 })
	if st := e.Status(); w.starts.Load() != 2 || st.Acquisitions != 2 || st.LastError != "" {
		t.Fatalf("after resume: starts=%d status=%+v", w.starts.Load(), st)
	}
}

func TestStandbyTakesOverWhenTheLeadersSessionDrops(t *testing.T) {
	arb := &fakeArbiter{}
	lockerA := &fakeLocker{arbiter: arb}
	a, b := New(lockerA, fastOptions()), New(&fakeLocker{arbiter: arb}, fastOptions())
	var wa, wb worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWhileLeader(ctx, "w", wa.run)
	go b.RunWhileLeader(ctx, "w", wb.run)
	startElector(t, a)
	eventually(t, "a leads", func() bool { return wa.running.Load() == 1 })
	startElector(t, b)

	lockerA.setErr(errors.New("network unreachable")) // a is cut off from the database
	arb.drop()
	eventually(t, "b leads and a stopped", func() bool { return wb.running.Load() == 1 && wa.running.Load() == 0 })
	if a.IsLeader() || !b.IsLeader() {
		t.Fatalf("leaders: a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
}

func TestWorkerNeverRunsTwiceAtOnceAndIsNotRespunWhileLeader(t *testing.T) {
	arb := &fakeArbiter{}
	e := New(&fakeLocker{arbiter: arb}, fastOptions())
	var calls, concurrent, maxConcurrent atomic.Int32
	slow := func(ctx context.Context) {
		calls.Add(1)
		if n := concurrent.Add(1); n > maxConcurrent.Load() {
			maxConcurrent.Store(n)
		}
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond) // slow to stop: leadership may come back meanwhile
		concurrent.Add(-1)
	}
	var returns atomic.Int32
	returning := func(context.Context) { returns.Add(1) } // returns at once, e.g. nothing to do
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { e.RunWhileLeader(ctx, "slow", slow); done <- struct{}{} }()
	go func() { e.RunWhileLeader(ctx, "returning", returning); done <- struct{}{} }()
	startElector(t, e)
	eventually(t, "first run", func() bool { return calls.Load() == 1 && returns.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if returns.Load() != 1 {
		t.Fatalf("a worker that returned was called again while still leader: %d", returns.Load())
	}
	arb.drop() // lost and taken back within milliseconds
	eventually(t, "second run", func() bool { return calls.Load() == 2 && returns.Load() == 2 })
	if maxConcurrent.Load() != 1 {
		t.Fatalf("worker ran %d times at once", maxConcurrent.Load())
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunWhileLeader did not return when its context ended")
		}
	}
}

func TestNilElectorAlwaysLeads(t *testing.T) {
	var e *Elector
	if st := e.Status(); st.Enabled || !st.Leader || !e.IsLeader() {
		t.Fatalf("nil elector status: %+v", st)
	}
	e.Run(context.Background()) // returns at once
	ran := false
	e.RunWhileLeader(context.Background(), "w", func(context.Context) { ran = true })
	if !ran {
		t.Fatal("a nil elector must run the worker")
	}
}
