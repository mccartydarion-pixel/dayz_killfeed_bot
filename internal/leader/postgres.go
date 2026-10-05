package leader

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
)

// SingletonLockKey is the advisory lock that elects the process running the singleton background
// workers. It is unrelated to the migration lock (database.migrationLockKey), which is a
// transaction-level lock on another key.
const SingletonLockKey int64 = 0x43484d504c454144 // "CHMPLEAD"

// PostgresLocker takes a PostgreSQL session-level advisory lock on a dedicated connection (never a
// pooled one: a session lock belongs to its connection, and a pool would hand that connection to
// other work or close it when idle). PostgreSQL drops the lock when the connection ends, however
// it ends, so a crashed or partitioned leader frees it without any timeout to tune.
//
// It needs a direct session to PostgreSQL. Behind a pooler in transaction mode a session lock is
// meaningless; Check then fails (the lock is not found on the backend answering) and leadership is
// reported lost rather than silently shared.
type PostgresLocker struct {
	cfg *pgx.ConnConfig
	key int64

	mu   sync.Mutex
	conn *pgx.Conn // kept between attempts while standing by
}

// NewPostgresLocker builds a locker that connects with a copy of cfg.
func NewPostgresLocker(cfg *pgx.ConnConfig, key int64) *PostgresLocker {
	c := cfg.Copy()
	if c.RuntimeParams == nil {
		c.RuntimeParams = map[string]string{}
	}
	c.RuntimeParams["application_name"] = "champion-leader-lock"
	return &PostgresLocker{cfg: c, key: key}
}

// TryAcquire implements Locker.
func (l *PostgresLocker) TryAcquire(ctx context.Context) (Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil || l.conn.IsClosed() {
		conn, err := pgx.ConnectConfig(ctx, l.cfg)
		if err != nil {
			return nil, err
		}
		l.conn = conn
	}
	var got bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, l.key).Scan(&got); err != nil {
		_ = l.conn.Close(context.WithoutCancel(ctx))
		l.conn = nil
		return nil, err
	}
	if !got {
		return nil, nil
	}
	held := &postgresLock{conn: l.conn, key: l.key}
	l.conn = nil
	return held, nil
}

// Close ends the standby connection, if any. A held lock is ended by its own Release.
func (l *PostgresLocker) Close(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		_ = l.conn.Close(ctx)
		l.conn = nil
	}
}

type postgresLock struct {
	conn *pgx.Conn
	key  int64
}

// errLockNotHeld: the session answered but no longer holds the advisory lock.
var errLockNotHeld = errors.New("leader lock is not held by this session")

// Check asks the session itself whether it still holds the lock. A dead connection fails the
// query; a live session that lost the lock (or a pooler answering from another backend) finds no
// row.
func (p *postgresLock) Check(ctx context.Context) error {
	var held bool
	err := p.conn.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_locks
   WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted AND objsubid = 1
     AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid)`, p.key).Scan(&held)
	if err != nil {
		return err
	}
	if !held {
		return errLockNotHeld
	}
	return nil
}

// Release unlocks and closes the connection. Closing alone already frees the lock; the explicit
// unlock just makes it immediate.
func (p *postgresLock) Release(ctx context.Context) error {
	var unlocked bool
	err := p.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, p.key).Scan(&unlocked)
	if cerr := p.conn.Close(ctx); err == nil {
		err = cerr
	}
	return err
}
