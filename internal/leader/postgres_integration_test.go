//go:build integration

package leader

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Two lock holders against a real PostgreSQL: the session-level advisory lock is what makes
// "only one process runs the singleton workers" true, so it is tested for real.

// integrationLockKey is a key of the test's own, so a run never meets the production key.
const integrationLockKey int64 = 0x43484d5054455354 // "CHMPTEST"

func integrationConfig(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL required")
		}
		t.Skip("TEST_DATABASE_URL not configured")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("explicit ALLOW_INTEGRATION_DB_TESTS=true required")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal("TEST_DATABASE_URL is not a valid connection string")
	}
	return cfg
}

func integrationOptions() Options {
	return Options{RetryEvery: 50 * time.Millisecond, CheckEvery: 25 * time.Millisecond, OpTimeout: 5 * time.Second}
}

// lockHolders returns the backend pids holding the test lock right now.
func lockHolders(t *testing.T, cfg *pgx.ConnConfig) []int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND objsubid=1
 AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid`, integrationLockKey)
	if err != nil {
		t.Fatal(err)
	}
	pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatal(err)
	}
	return pids
}

func TestPostgresOnlyOneHolderRunsAndTheOtherTakesOverOnRelease(t *testing.T) {
	cfg := integrationConfig(t)
	a := New(NewPostgresLocker(cfg, integrationLockKey), integrationOptions())
	b := New(NewPostgresLocker(cfg, integrationLockKey), integrationOptions())
	var wa, wb worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWhileLeader(ctx, "w", wa.run)
	go b.RunWhileLeader(ctx, "w", wb.run)

	stopA := startElector(t, a)
	started := time.Now()
	eventually(t, "a leads", func() bool { return a.IsLeader() && wa.running.Load() == 1 })
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("a lone process took %s to become leader", waited)
	}
	startElector(t, b)
	time.Sleep(400 * time.Millisecond) // eight retry periods
	if b.IsLeader() || wb.starts.Load() != 0 || wa.running.Load() != 1 || len(lockHolders(t, cfg)) != 1 {
		t.Fatalf("two holders: b=%v b.starts=%d a.running=%d holders=%v", b.IsLeader(), wb.starts.Load(), wa.running.Load(), lockHolders(t, cfg))
	}
	if st := b.Status(); st.LastError != "" {
		t.Fatalf("standing by is not an error: %+v", st)
	}

	stopA() // the first releases (a clean shutdown)
	eventually(t, "b takes over", func() bool { return b.IsLeader() && wb.running.Load() == 1 })
	if a.IsLeader() || wa.running.Load() != 0 || len(lockHolders(t, cfg)) != 1 {
		t.Fatalf("after release: a=%v a.running=%d holders=%v", a.IsLeader(), wa.running.Load(), lockHolders(t, cfg))
	}
}

func TestPostgresKilledSessionStopsTheLeaderAndAnotherTakesOver(t *testing.T) {
	cfg := integrationConfig(t)
	a := New(NewPostgresLocker(cfg, integrationLockKey), integrationOptions())
	b := New(NewPostgresLocker(cfg, integrationLockKey), integrationOptions())
	var wa, wb worker
	var both atomic.Int32 // how often both workers were seen running together
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunWhileLeader(ctx, "w", wa.run)
	go b.RunWhileLeader(ctx, "w", wb.run)
	startElector(t, a)
	eventually(t, "a leads", func() bool { return wa.running.Load() == 1 })
	startElector(t, b)

	holders := lockHolders(t, cfg)
	if len(holders) != 1 {
		t.Fatalf("holders: %v", holders)
	}
	// The database ends the leader's session (what a network cut or a failover does).
	kctx, kcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer kcancel()
	killer, err := pgx.ConnectConfig(kctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer killer.Close(kctx)
	if _, err := killer.Exec(kctx, `SELECT pg_terminate_backend($1)`, holders[0]); err != nil {
		t.Fatal(err)
	}
	killedAt := time.Now()
	eventually(t, "a stops its work", func() bool { return wa.starts.Load() >= 1 && (wa.running.Load() == 0 || wa.starts.Load() >= 2) })
	if took := time.Since(killedAt); took > 2*time.Second {
		t.Fatalf("the old leader kept working for %s after its session ended", took)
	}
	// Exactly one of the two leads afterwards (either may win the race), never both.
	eventually(t, "one leader again", func() bool { return a.IsLeader() != b.IsLeader() })
	for i := 0; i < 40; i++ {
		if wa.running.Load()+wb.running.Load() > 1 && a.IsLeader() && b.IsLeader() {
			both.Add(1)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if both.Load() != 0 || len(lockHolders(t, cfg)) != 1 || a.IsLeader() == b.IsLeader() {
		t.Fatalf("after the kill: a=%v b=%v both=%d holders=%v", a.IsLeader(), b.IsLeader(), both.Load(), lockHolders(t, cfg))
	}
}
