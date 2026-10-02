package servers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

// WorkerFactory runs one server's worker until ctx is cancelled. A nil return
// means the worker finished on purpose; an error (or a panic) means it failed
// and the manager will supervise a restart (see RestartPolicy).
type WorkerFactory func(context.Context, int64) error

// RestartGate is consulted before every supervised restart. Returning false
// retires the worker instead of restarting it (e.g. game_servers.active was
// cleared while the worker was failing).
type RestartGate func(ctx context.Context, serverID int64) bool

// RestartPolicy controls the supervised restart of a worker whose factory
// returned an error or panicked. Zero values take the defaults below.
type RestartPolicy struct {
	// Disabled turns supervision off: a failed worker is removed, never retried.
	Disabled bool
	// MinBackoff is the wait before the first restart (default 1s). Each
	// consecutive failure doubles it, with a random jitter of up to +50%.
	MinBackoff time.Duration
	// MaxBackoff caps the backoff (default 5m).
	MaxBackoff time.Duration
	// ResetAfter: a run that stayed up at least this long (default 10m) resets
	// the attempt counter, so a worker that fails after a healthy stretch
	// restarts quickly again instead of inheriting an old long backoff.
	ResetAfter time.Duration
}

const (
	defaultMinBackoff  = time.Second
	defaultMaxBackoff  = 5 * time.Minute
	defaultResetAfter  = 10 * time.Minute
	defaultStopTimeout = 30 * time.Second
)

// ErrStopTimeout is returned by StopContext when the worker goroutine did not
// exit within the stop timeout. The worker stays registered (still exiting);
// a later Start replaces it.
var ErrStopTimeout = errors.New("worker did not stop in time")

// WorkerStatus is a point-in-time view of one managed worker.
type WorkerStatus struct {
	// Running is true while the manager owns a worker for the server (its
	// factory is executing, or it is waiting to be restarted).
	Running bool
	// Restarting is true while the worker waits out a restart backoff.
	Restarting bool
	// Restarts counts supervised restarts since the last healthy run.
	Restarts int
	// LastError is the error (or panic) that caused the last restart.
	LastError string
	// NextRestartAt is when the pending restart fires (zero unless Restarting).
	NextRestartAt time.Time
}

type worker struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopping bool

	restarting    bool
	restarts      int
	lastError     string
	nextRestartAt time.Time
}

// WorkerManager is the sole owner of per-server worker goroutines. It isolates
// faults (a panic in one worker never reaches another), supervises restarts of
// failed workers, and makes Stop synchronous so a stop-then-start sequence
// (owner "restart worker", self-heal) really restarts the worker.
type WorkerManager struct {
	mu          sync.Mutex
	workers     map[int64]*worker
	factory     WorkerFactory
	policy      RestartPolicy
	stopTimeout time.Duration
	gate        RestartGate
}

func NewWorkerManager(factory WorkerFactory) *WorkerManager {
	return &WorkerManager{workers: make(map[int64]*worker), factory: factory, stopTimeout: defaultStopTimeout}
}

// SetRestartPolicy replaces the supervised-restart policy. Call before Start.
func (m *WorkerManager) SetRestartPolicy(p RestartPolicy) {
	m.mu.Lock()
	m.policy = p
	m.mu.Unlock()
}

// SetRestartGate installs the check consulted before every supervised restart.
func (m *WorkerManager) SetRestartGate(gate RestartGate) {
	m.mu.Lock()
	m.gate = gate
	m.mu.Unlock()
}

// SetStopTimeout bounds how long Stop/StopAll wait for a worker goroutine.
func (m *WorkerManager) SetStopTimeout(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	m.stopTimeout = d
	m.mu.Unlock()
}

// Start launches the worker for serverID. It fails if a worker is already
// running. If the previous instance was stopped but is still exiting, Start
// waits for it (bounded by the stop timeout and parent) before starting the
// replacement, so the new worker never shares a server with the old one.
func (m *WorkerManager) Start(parent context.Context, serverID int64) error {
	m.mu.Lock()
	if existing, ok := m.workers[serverID]; ok {
		if !existing.stopping {
			m.mu.Unlock()
			return fmt.Errorf("worker %d already running", serverID)
		}
		timeout := m.stopTimeout
		m.mu.Unlock()
		if err := waitDone(parent, existing.done, timeout); err != nil {
			if errors.Is(err, ErrStopTimeout) {
				slog.Warn("component=servers", "msg", "previous worker instance still exiting; starting replacement", "server_id", serverID)
			} else {
				return fmt.Errorf("worker %d: waiting for previous instance: %w", serverID, err)
			}
		}
		m.mu.Lock()
		if current, ok := m.workers[serverID]; ok && current != existing {
			m.mu.Unlock()
			return fmt.Errorf("worker %d already running", serverID)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	w := &worker{cancel: cancel, done: make(chan struct{})}
	m.workers[serverID] = w
	m.mu.Unlock()
	go m.run(ctx, serverID, w)
	return nil
}

// run executes the worker factory inside a panic-recovery boundary so a fault on
// one server (panic, or any other failure) can never take down other workers or
// the process. This is what makes WorkerManager the safe, sole owner of workers.
// A failed run is restarted with jittered exponential backoff until it succeeds,
// the worker is stopped, or the restart gate retires it.
func (m *WorkerManager) run(ctx context.Context, serverID int64, w *worker) {
	defer close(w.done)
	defer func() {
		m.mu.Lock()
		if m.workers[serverID] == w {
			delete(m.workers, serverID)
		}
		m.mu.Unlock()
	}()
	for {
		started := time.Now()
		failure := m.runOnce(ctx, serverID)
		if ctx.Err() != nil {
			return // intentional stop (Stop/StopAll/parent cancelled): never restart
		}
		if failure == "" {
			slog.Info("component=servers", "msg", "worker finished", "server_id", serverID)
			return
		}
		m.mu.Lock()
		policy := m.policy
		gate := m.gate
		if policy.ResetAfter <= 0 {
			policy.ResetAfter = defaultResetAfter
		}
		if time.Since(started) >= policy.ResetAfter {
			w.restarts = 0
		}
		w.restarts++
		attempt := w.restarts
		w.lastError = failure
		disabled := policy.Disabled
		m.mu.Unlock()
		if disabled {
			slog.Error("component=servers", "msg", "worker failed; supervision disabled, not restarting", "server_id", serverID, "err", failure)
			return
		}
		if gate != nil && !gate(ctx, serverID) {
			slog.Warn("component=servers", "msg", "worker failed; restart refused by gate (server inactive), retiring", "server_id", serverID, "attempt", attempt, "err", failure)
			return
		}
		delay := restartBackoff(policy, attempt)
		m.mu.Lock()
		w.restarting = true
		w.nextRestartAt = time.Now().Add(delay)
		m.mu.Unlock()
		slog.Warn("component=servers", "msg", "worker failed; scheduling supervised restart", "server_id", serverID, "attempt", attempt, "backoff_ms", delay.Milliseconds(), "err", failure)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		m.mu.Lock()
		w.restarting = false
		w.nextRestartAt = time.Time{}
		m.mu.Unlock()
		slog.Info("component=servers", "msg", "worker restarting", "server_id", serverID, "attempt", attempt)
	}
}

// runOnce runs the factory once and returns "" on a clean return, otherwise the
// failure description (error text or recovered panic).
func (m *WorkerManager) runOnce(ctx context.Context, serverID int64) (failure string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=servers", "msg", "worker panic recovered; other servers unaffected", "server_id", serverID, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			failure = fmt.Sprintf("panic: %v", r)
		}
	}()
	if m.factory == nil {
		return ""
	}
	if err := m.factory(ctx, serverID); err != nil {
		slog.Error("component=servers", "msg", "worker exited with error", "server_id", serverID, "err", err.Error())
		return err.Error()
	}
	return ""
}

// restartBackoff is MinBackoff doubled per attempt, jittered by up to +50% and
// capped at MaxBackoff.
func restartBackoff(p RestartPolicy, attempt int) time.Duration {
	minBackoff, maxBackoff := p.MinBackoff, p.MaxBackoff
	if minBackoff <= 0 {
		minBackoff = defaultMinBackoff
	}
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	if maxBackoff < minBackoff {
		maxBackoff = minBackoff
	}
	delay := minBackoff
	for i := 1; i < attempt && delay < maxBackoff; i++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	delay = Jitter(delay, time.Duration(rand.Int64N(int64(delay)/2+1)))
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

// Stop cancels the worker for serverID and waits for its goroutine to exit,
// bounded by the stop timeout. It returns once the worker is gone (or the
// timeout elapsed, in which case the worker is logged and left to finish).
func (m *WorkerManager) Stop(serverID int64) {
	if err := m.StopContext(context.Background(), serverID); err != nil {
		slog.Warn("component=servers", "msg", "worker stop did not complete", "server_id", serverID, "err", err.Error())
	}
}

// StopContext is Stop with a caller-supplied context: it returns ctx.Err() if
// the context ends first, ErrStopTimeout if the worker outlives the stop
// timeout, and nil once the worker goroutine has exited. Stopping a server
// with no worker is a no-op.
func (m *WorkerManager) StopContext(ctx context.Context, serverID int64) error {
	m.mu.Lock()
	w := m.workers[serverID]
	timeout := m.stopTimeout
	if w != nil {
		w.stopping = true
		w.cancel()
	}
	m.mu.Unlock()
	if w == nil {
		return nil
	}
	return waitDone(ctx, w.done, timeout)
}

// StopAll cancels every worker and waits (bounded by the stop timeout, shared
// across all workers) for them to exit.
func (m *WorkerManager) StopAll() {
	m.mu.Lock()
	timeout := m.stopTimeout
	pending := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		w.stopping = true
		w.cancel()
		pending = append(pending, w)
	}
	m.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for _, w := range pending {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		if err := waitDone(context.Background(), w.done, remaining); err != nil {
			slog.Warn("component=servers", "msg", "worker did not stop before shutdown deadline", "err", err.Error())
			break
		}
	}
	m.mu.Lock()
	for id, w := range m.workers {
		if w.stopping {
			delete(m.workers, id)
		}
	}
	m.mu.Unlock()
}

func waitDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrStopTimeout
	}
}

// Running reports whether the manager owns a worker for serverID: its factory
// is executing or it is waiting for a supervised restart. A stopped worker
// still exiting after a stop timeout does not count.
func (m *WorkerManager) Running(serverID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[serverID]
	return ok && !w.stopping
}

// Status returns the worker's point-in-time status (zero value when none).
func (m *WorkerManager) Status(serverID int64) WorkerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[serverID]
	if !ok || w.stopping {
		return WorkerStatus{}
	}
	return WorkerStatus{Running: true, Restarting: w.restarting, Restarts: w.restarts, LastError: w.lastError, NextRestartAt: w.nextRestartAt}
}

func Jitter(base time.Duration, offset time.Duration) time.Duration {
	if offset < 0 {
		offset = -offset
	}
	return base + offset
}
