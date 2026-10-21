// Package leader elects one "leader" among the bot processes that share a database, so that the
// background workers that must run exactly once (a Discord board refresher, a reminder loop, the
// Nitrado name sync) run in one process only.
//
// The mechanism is one lock (in production a PostgreSQL session-level advisory lock held on a
// dedicated connection, postgres.go). The process that holds it is the leader. A process that does
// not hold it keeps trying every Options.RetryEvery and takes over when the holder's connection
// ends. A leader that can no longer prove it holds the lock stops being leader at once: every
// worker started with RunWhileLeader has its context cancelled, and is started again when the lock
// is taken again.
//
// A nil *Elector means "no election" (no database, or switched off): it always reports leader and
// RunWhileLeader simply runs the worker. Callers therefore never need a nil check.
package leader

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Lock is a held leader lock.
type Lock interface {
	// Check returns nil only while the lock is provably still held. Any error means it is lost.
	Check(ctx context.Context) error
	// Release gives the lock up and frees whatever holds it. It is called exactly once.
	Release(ctx context.Context) error
}

// Locker tries to take the lock without waiting. It returns (nil, nil) when another holder has it.
type Locker interface {
	TryAcquire(ctx context.Context) (Lock, error)
}

// Options tunes an Elector. Zero values take the defaults.
type Options struct {
	RetryEvery time.Duration // how often a process without the lock tries to take it (default 15s)
	CheckEvery time.Duration // how often the leader proves it still holds the lock (default 5s)
	OpTimeout  time.Duration // limit for one acquire, check or release (default 5s)
	Now        func() time.Time
}

func (o *Options) defaults() {
	if o.RetryEvery <= 0 {
		o.RetryEvery = 15 * time.Second
	}
	if o.CheckEvery <= 0 {
		o.CheckEvery = 5 * time.Second
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = 5 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Status is the leadership view for health and diagnostics output.
type Status struct {
	// Enabled is false when there is no election (nil Elector): this process then always acts as
	// the leader.
	Enabled bool `json:"enabled"`
	Leader  bool `json:"leader"`
	// Since is when this process last became leader (Leader true) or last stopped being leader /
	// started (Leader false). Zero when there is no election.
	Since time.Time `json:"since"`
	// Acquisitions counts how often this process became leader.
	Acquisitions int `json:"acquisitions"`
	// LastError is the latest acquire or check failure; empty after a clean attempt. "Another
	// process holds the lock" is not an error.
	LastError string `json:"lastError,omitempty"`
}

// Elector runs the election for one lock.
type Elector struct {
	locker Locker
	opts   Options

	mu       sync.Mutex
	status   Status
	leadCtx  context.Context    // non-nil while leader; cancelled when leadership ends
	leadStop context.CancelFunc // cancels leadCtx
	changed  chan struct{}      // closed and replaced on every leadership change
}

// New builds an Elector. Run must be called for it to ever become leader.
func New(locker Locker, opts Options) *Elector {
	opts.defaults()
	return &Elector{locker: locker, opts: opts, status: Status{Enabled: true, Since: opts.Now()}, changed: make(chan struct{})}
}

// Run takes part in the election until ctx ends, then releases the lock if held. It blocks.
func (e *Elector) Run(ctx context.Context) {
	if e == nil || e.locker == nil {
		return
	}
	defer func() {
		// A locker may keep a standby connection between attempts.
		if c, ok := e.locker.(interface{ Close(context.Context) }); ok {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.opts.OpTimeout)
			c.Close(closeCtx)
			cancel()
		}
	}()
	standbyLogged := false
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, e.opts.OpTimeout)
		lock, err := e.locker.TryAcquire(opCtx)
		cancel()
		if err == nil && lock != nil {
			standbyLogged = false
			e.become(true, "")
			lostErr := e.hold(ctx, lock)
			reason := ""
			if lostErr != nil {
				reason = lostErr.Error()
			}
			e.become(false, reason)
			// Always release: it frees the connection, and after a failed check it is the only
			// way to be sure the lock is not left behind on a half-dead session.
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), e.opts.OpTimeout)
			if rerr := lock.Release(relCtx); rerr != nil && lostErr == nil {
				slog.Warn("component=leader", "event", "release_failed", "err", rerr.Error())
			}
			relCancel()
			if lostErr != nil {
				continue // lost, not stopped: try to take it back straight away
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		msg := ""
		if err != nil {
			msg = err.Error()
			slog.Warn("component=leader", "event", "acquire_failed", "err", msg, "retry_in", e.opts.RetryEvery.String())
		} else if !standbyLogged {
			standbyLogged = true
			slog.Info("component=leader", "event", "standby", "msg", "another process holds the leader lock; singleton workers stay off here",
				"retry_every", e.opts.RetryEvery.String())
		}
		e.mu.Lock()
		e.status.LastError = msg
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.opts.RetryEvery):
		}
	}
}

// hold blocks while the lock is held. It returns nil when ctx ended and the error of the failed
// check when the lock was lost.
func (e *Elector) hold(ctx context.Context, lock Lock) error {
	t := time.NewTicker(e.opts.CheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		opCtx, cancel := context.WithTimeout(ctx, e.opts.OpTimeout)
		err := lock.Check(opCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (e *Elector) become(leader bool, lostReason string) {
	now := e.opts.Now()
	e.mu.Lock()
	held := now.Sub(e.status.Since)
	e.status.Leader, e.status.Since, e.status.LastError = leader, now, lostReason
	if leader {
		e.status.Acquisitions++
		e.leadCtx, e.leadStop = context.WithCancel(context.Background())
	} else if e.leadStop != nil {
		e.leadStop()
		e.leadCtx, e.leadStop = nil, nil
	}
	n := e.status.Acquisitions
	close(e.changed)
	e.changed = make(chan struct{})
	e.mu.Unlock()
	switch {
	case leader:
		slog.Info("component=leader", "event", "leadership_acquired", "acquisitions", n, "waited", held.Round(time.Millisecond).String())
	case lostReason != "":
		slog.Warn("component=leader", "event", "leadership_lost", "err", lostReason, "held", held.Round(time.Millisecond).String())
	default:
		slog.Info("component=leader", "event", "leadership_released", "held", held.Round(time.Millisecond).String())
	}
}

// Status returns the current leadership view. A nil Elector reports an always-on leader.
func (e *Elector) Status() Status {
	if e == nil {
		return Status{Leader: true}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// IsLeader reports whether this process is the leader right now (always true for a nil Elector).
func (e *Elector) IsLeader() bool { return e.Status().Leader }

// RunWhileLeader runs fn only while this process is the leader, until ctx ends. fn gets a context
// that is cancelled when ctx ends or leadership is lost, and must return when it is; fn is called
// again (never concurrently with itself) each time leadership is gained. If fn returns by itself
// while still leader it is not called again until leadership has been lost and regained. It blocks.
// On a nil Elector it is exactly fn(ctx).
func (e *Elector) RunWhileLeader(ctx context.Context, name string, fn func(ctx context.Context)) {
	if e == nil {
		fn(ctx)
		return
	}
	for ctx.Err() == nil {
		e.mu.Lock()
		lead, changed := e.leadCtx, e.changed
		e.mu.Unlock()
		if lead == nil {
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
			continue
		}
		runCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(lead, cancel)
		slog.Debug("component=leader", "event", "worker_started", "worker", name)
		fn(runCtx)
		stop()
		cancel()
		if ctx.Err() != nil {
			return
		}
		slog.Info("component=leader", "event", "worker_stopped", "worker", name, "still_leader", lead.Err() == nil)
		select {
		case <-ctx.Done():
			return
		case <-lead.Done():
		}
	}
}
