package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/leader"
)

// Singleton workers (docs/MULTI_PROCESS.md).
//
// Some background workers must run in one process only when several bot processes share a
// database (a deploy overlap, a second replica): they post to Discord, call Nitrado or send a DM
// from a plain "list what is due, act, mark done" loop with no per-row claim. They are started
// through App.singleton, which runs them only in the process holding the leader lock
// (internal/leader: a PostgreSQL session-level advisory lock on a dedicated connection).
//
// Workers that already claim their work row by row (a lease, FOR UPDATE SKIP LOCKED, a conditional
// UPDATE, a unique index), and everything fed by this process's own events or serving its own HTTP
// requests, are NOT started this way and run in every process.

// singletonLeaderLockEnabled: the lock is on unless SINGLETON_LEADER_LOCK=off. Switch it off only
// when exactly one process runs and the database cannot give a session lock (a pooler in
// transaction mode); every process then behaves as the leader, as before the lock existed.
func singletonLeaderLockEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("SINGLETON_LEADER_LOCK")), "off")
}

// startLeaderElection starts the election. Without a database (or switched off) a.Leader stays
// nil, which always leads. It does not wait: a lone process takes the lock within milliseconds and
// the workers waiting in App.singleton start then.
func (a *App) startLeaderElection() {
	if a.DB == nil || a.DB.Pool == nil {
		return
	}
	if !singletonLeaderLockEnabled() {
		slog.Warn("component=leader", "event", "disabled", "msg", "SINGLETON_LEADER_LOCK=off: this process runs every singleton worker; run only one process")
		return
	}
	a.Leader = leader.New(leader.NewPostgresLocker(a.DB.Pool.Config().ConnConfig, leader.SingletonLockKey), leader.Options{})
	// Its own context: the lock is released by shutdown(), before the database closes.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Leader.Run(ctx)
	}()
	a.stopLeader = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			slog.Warn("component=leader", "event", "release_timeout")
		}
	}
}

// singleton runs fn only while this process is the leader, until ctx ends: fn's context is
// cancelled when leadership is lost and fn is called again when it is regained. It blocks, so it
// is used as `go a.singleton(ctx, "name", fn)` where `go fn(ctx)` stood before.
func (a *App) singleton(ctx context.Context, name string, fn func(ctx context.Context)) {
	a.Leader.RunWhileLeader(ctx, name, fn)
}

// isSingletonLeader reports whether this process may do singleton work right now.
func (a *App) isSingletonLeader() bool { return a.Leader.IsLeader() }

// RuntimeLeadership is the leadership block of GET /api/runtime/status: whether THIS process runs
// the singleton workers, and since when.
type RuntimeLeadership struct {
	// Enabled is false when there is no election (no database, or SINGLETON_LEADER_LOCK=off): the
	// process then always runs the singleton workers.
	Enabled bool `json:"enabled"`
	Leader  bool `json:"leader"`
	// Since is when this process last became leader (leader true) or last stopped being / started
	// without being leader (leader false). Null when there is no election.
	Since        *string `json:"since"`
	Acquisitions int     `json:"acquisitions"`
	LastError    string  `json:"lastError,omitempty"`
}

func (a *App) runtimeLeadership() *RuntimeLeadership {
	st := a.Leader.Status()
	return &RuntimeLeadership{Enabled: st.Enabled, Leader: st.Leader, Since: nullableTime(st.Since), Acquisitions: st.Acquisitions, LastError: st.LastError}
}

// leaderHealthComponent is the "singleton_leader" health component. Standing by because another
// process leads is healthy; failing to reach the lock is degraded (nothing singleton runs here,
// and maybe nowhere).
func leaderHealthComponent(st leader.Status) health.Component {
	c := health.Component{Name: "singleton_leader", State: health.Healthy}
	since := st.Since.UTC().Format(time.RFC3339)
	switch {
	case !st.Enabled:
		c.Message = "no leader lock: this process runs every singleton worker"
	case st.Leader:
		c.Message = "leader since " + since
	case st.LastError != "":
		c.State = health.Degraded
		c.Message = fmt.Sprintf("not leader since %s: cannot take the leader lock (%s)", since, st.LastError)
	default:
		c.Message = "standby since " + since + ": another process runs the singleton workers"
	}
	return c
}
