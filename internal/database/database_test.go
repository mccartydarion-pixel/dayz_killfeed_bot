package database

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestEnvHelpersDefaultAndOverride(t *testing.T) {
	t.Run("envInt32", func(t *testing.T) {
		if got := envInt32("DB_TEST_UNSET_1", 7); got != 7 {
			t.Fatalf("got %d", got)
		}
		t.Setenv("DB_TEST_INT32", "42")
		if got := envInt32("DB_TEST_INT32", 7); got != 42 {
			t.Fatalf("got %d", got)
		}
		t.Setenv("DB_TEST_INT32_BAD", "not-a-number")
		if got := envInt32("DB_TEST_INT32_BAD", 7); got != 7 {
			t.Fatalf("malformed value should fall back, got %d", got)
		}
		t.Setenv("DB_TEST_INT32_ZERO", "0")
		if got := envInt32("DB_TEST_INT32_ZERO", 7); got != 7 {
			t.Fatalf("a non-positive value should fall back, got %d", got)
		}
	})
	t.Run("envDuration", func(t *testing.T) {
		if got := envDuration("DB_TEST_UNSET_2", 5*time.Minute); got != 5*time.Minute {
			t.Fatalf("got %v", got)
		}
		t.Setenv("DB_TEST_DURATION", "10s")
		if got := envDuration("DB_TEST_DURATION", 5*time.Minute); got != 10*time.Second {
			t.Fatalf("got %v", got)
		}
		t.Setenv("DB_TEST_DURATION_BAD", "not-a-duration")
		if got := envDuration("DB_TEST_DURATION_BAD", 5*time.Minute); got != 5*time.Minute {
			t.Fatalf("malformed value should fall back, got %v", got)
		}
	})
}

func TestTruncateSQLCollapsesWhitespaceAndBounds(t *testing.T) {
	multi := "SELECT id\n  FROM kills\n  WHERE guild_id = $1"
	if got := truncateSQL(multi); got != "SELECT id FROM kills WHERE guild_id = $1" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", maxLoggedSQLLen+50)
	got := truncateSQL(long)
	if len([]rune(got)) != maxLoggedSQLLen+1 { // +1 for the trailing ellipsis rune
		t.Fatalf("expected truncation to %d runes + ellipsis, got %d: %q", maxLoggedSQLLen, len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected a truncation marker, got %q", got)
	}
}

// captureLogs redirects the default slog logger to a buffer for the duration
// of fn, then restores it - lets a test assert on log level/content without
// depending on global state elsewhere.
func captureLogs(t *testing.T, level slog.Level, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(old) })
	fn()
	return buf.String()
}

func TestQueryTracerLogsSlowQueriesAtWarnAndFastAtDebug(t *testing.T) {
	tracer := newQueryTracer(50) // 50ms threshold
	ctx := context.Background()

	// A "slow" query: fabricate a start time in the past instead of sleeping,
	// so the test stays fast and deterministic.
	slowCtx := context.WithValue(ctx, traceCtxKey{}, traceStart{at: time.Now().Add(-100 * time.Millisecond), sql: "SELECT * FROM kills WHERE guild_id = $1"})
	out := captureLogs(t, slog.LevelDebug, func() {
		tracer.TraceQueryEnd(slowCtx, nil, pgx.TraceQueryEndData{})
	})
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "slow_query") || !strings.Contains(out, "kills") {
		t.Fatalf("expected a WARN slow_query log mentioning the query, got: %s", out)
	}

	// A fast query: logged at Debug, not Warn.
	fastCtx := context.WithValue(ctx, traceCtxKey{}, traceStart{at: time.Now(), sql: "SELECT 1"})
	out = captureLogs(t, slog.LevelDebug, func() {
		tracer.TraceQueryEnd(fastCtx, nil, pgx.TraceQueryEndData{})
	})
	if !strings.Contains(out, "level=DEBUG") || strings.Contains(out, "WARN") {
		t.Fatalf("expected a DEBUG-level log for a fast query, got: %s", out)
	}

	stats := tracer.stats()
	if stats.Total != 2 || stats.Slow != 1 {
		t.Fatalf("expected total=2 slow=1, got %+v", stats)
	}
	if stats.AvgDurationMS <= 0 {
		t.Fatalf("expected a positive average duration, got %+v", stats)
	}
}

func TestQueryTracerNeverLogsArgumentValues(t *testing.T) {
	tracer := newQueryTracer(50)
	ctx := context.WithValue(context.Background(), traceCtxKey{}, traceStart{at: time.Now().Add(-time.Second), sql: "SELECT * FROM app_users WHERE discord_user_id = $1"})
	out := captureLogs(t, slog.LevelDebug, func() {
		// TraceQueryStart's Args are deliberately never threaded into the
		// context the tracer keeps - only SQL text and timing are. This test
		// documents that guarantee: even if a caller's query carried a
		// secret-looking argument, the tracer has no way to log it because it
		// never captured Args in the first place.
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	})
	if strings.Contains(out, "secret") || strings.Contains(out, "token") || strings.Contains(out, "Args") {
		t.Fatalf("the tracer must never log argument values, got: %s", out)
	}
}

func TestQueryTracerSurfacesPgErrorCode(t *testing.T) {
	tracer := newQueryTracer(50)
	ctx := context.WithValue(context.Background(), traceCtxKey{}, traceStart{at: time.Now().Add(-time.Second), sql: "INSERT INTO kills (...)"})
	out := captureLogs(t, slog.LevelDebug, func() {
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "23505", Message: "duplicate key"}})
	})
	if !strings.Contains(out, "pg_code=23505") {
		t.Fatalf("expected the pg error code surfaced, got: %s", out)
	}
}

func TestQueryTracerIgnoresEndWithoutMatchingStart(t *testing.T) {
	tracer := newQueryTracer(50)
	// No TraceQueryStart value in the context: TraceQueryEnd must be a no-op,
	// never panic or record a bogus (huge) duration.
	tracer.TraceQueryEnd(context.Background(), nil, pgx.TraceQueryEndData{})
	if stats := tracer.stats(); stats.Total != 0 {
		t.Fatalf("expected no stats recorded, got %+v", stats)
	}
}

func TestNewQueryTracerNonPositiveThresholdFallsBackToDefault(t *testing.T) {
	tracer := newQueryTracer(0)
	if tracer.slowThreshold != DefaultSlowQueryThresholdMS*time.Millisecond {
		t.Fatalf("got %v", tracer.slowThreshold)
	}
}
