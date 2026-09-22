package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the pgx connection pool. It never logs the connection string.
type DB struct {
	Pool   *pgxpool.Pool
	tracer *queryTracer
}

// Pool tuning defaults (Champion Performance Phase 1, docs/PERFORMANCE.md
// "Connection pool"). Unchanged from the values this package shipped with
// before they became configurable - overriding them is opt-in, never a
// silent behavior change.
const (
	DefaultMaxConns             = 10
	DefaultMinConns             = 1
	DefaultMaxConnLifetime      = 30 * time.Minute
	DefaultMaxConnIdleTime      = 5 * time.Minute
	DefaultHealthCheckPeriod    = time.Minute
	DefaultSlowQueryThresholdMS = 250
)

// Connect creates a connection pool and verifies connectivity with a bounded ping.
// Returns an error if the database is unreachable; callers decide whether to
// continue degraded or fail startup.
//
// Pool size and the slow-query log threshold are tunable via environment
// variables (DATABASE_MAX_CONNS, DATABASE_MIN_CONNS,
// DATABASE_MAX_CONN_LIFETIME, DATABASE_MAX_CONN_IDLE_TIME,
// DATABASE_HEALTH_CHECK_PERIOD, SLOW_QUERY_THRESHOLD_MS) - every one of them
// defaults to exactly what this package hardcoded before, so an operator who
// sets none of them sees no behavior change.
func Connect(ctx context.Context, databaseURL string) (*DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is not configured")
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	cfg.MaxConns = envInt32("DATABASE_MAX_CONNS", DefaultMaxConns)
	cfg.MinConns = envInt32("DATABASE_MIN_CONNS", DefaultMinConns)
	cfg.MaxConnLifetime = envDuration("DATABASE_MAX_CONN_LIFETIME", DefaultMaxConnLifetime)
	cfg.MaxConnIdleTime = envDuration("DATABASE_MAX_CONN_IDLE_TIME", DefaultMaxConnIdleTime)
	cfg.HealthCheckPeriod = envDuration("DATABASE_HEALTH_CHECK_PERIOD", DefaultHealthCheckPeriod)

	tracer := newQueryTracer(envInt("SLOW_QUERY_THRESHOLD_MS", DefaultSlowQueryThresholdMS))
	cfg.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database ping failed: %w", err)
	}

	slog.Info("component=database", "msg", "connected", "max_conns", cfg.MaxConns, "min_conns", cfg.MinConns,
		"slow_query_threshold_ms", tracer.slowThreshold.Milliseconds())
	return &DB{Pool: pool, tracer: tracer}, nil
}

// Close gracefully shuts down the pool.
func (d *DB) Close() {
	if d == nil || d.Pool == nil {
		return
	}
	d.Pool.Close()
	slog.Info("component=database", "msg", "connection pool closed")
}

// PoolStats returns pool metrics for the status endpoint (no secrets).
func (d *DB) PoolStats() (total, idle int32, ok bool) {
	if d == nil || d.Pool == nil {
		return 0, 0, false
	}
	s := d.Pool.Stat()
	return s.TotalConns(), s.IdleConns(), true
}

// ExtendedPoolStats is the fuller pool snapshot for the internal performance
// view (Champion Performance Phase 1, section 24/35) - acquire contention is
// the signal that actually indicates the pool is undersized, which total/idle
// alone can't show (a pool can be "full" of idle connections while every
// acquire still waits, if MaxConns itself is too low for peak concurrency).
type ExtendedPoolStats struct {
	TotalConns        int32
	IdleConns         int32
	MaxConns          int32
	AcquiredConns     int32
	AcquireCount      int64
	EmptyAcquireCount int64 // acquires that had to wait for a connection to free up
	AcquireDuration   time.Duration
}

// ExtendedPoolStats returns the fuller pgxpool.Stat() snapshot. ok is false
// only when d or its pool is nil.
func (d *DB) ExtendedPoolStats() (ExtendedPoolStats, bool) {
	if d == nil || d.Pool == nil {
		return ExtendedPoolStats{}, false
	}
	s := d.Pool.Stat()
	return ExtendedPoolStats{
		TotalConns: s.TotalConns(), IdleConns: s.IdleConns(), MaxConns: s.MaxConns(), AcquiredConns: s.AcquiredConns(),
		AcquireCount: s.AcquireCount(), EmptyAcquireCount: s.EmptyAcquireCount(), AcquireDuration: s.AcquireDuration(),
	}, true
}

// QueryStats is a point-in-time snapshot of query latency observed by the
// slow-query tracer since the pool was created (Champion Performance Phase
// 1, section 22/35). Counts are cumulative and never reset.
type QueryStats struct {
	Total         int64
	Slow          int64 // Total queries at or above the configured threshold
	AvgDurationMS float64
}

// QueryStats returns the current query-latency counters, or a zero value if
// d is nil.
func (d *DB) QueryStats() QueryStats {
	if d == nil || d.tracer == nil {
		return QueryStats{}
	}
	return d.tracer.stats()
}

// --- slow-query tracing (Champion Performance Phase 1, section 22/23/31/32) -----------------------
//
// There was previously zero query-timing instrumentation anywhere in this
// codebase (confirmed by a full repo audit before this change) - every kill/
// death/route/installation/player/bounty/economy/embed-template query was
// invisible to observability. A pgx.QueryTracer wraps every Query/QueryRow/
// Exec call transparently, with NO call-site changes anywhere in
// internal/repository: duration is measured, a query at or above the
// threshold is logged at WARN (the "recoverable degradation" level, section
// 32), everything else at DEBUG (not INFO - this is exactly the
// "high-frequency diagnostics" section 32 says must stay off INFO). Only the
// static SQL text is ever logged, truncated to a bounded length; query
// arguments are never logged, so no parameter value (including anything
// that happens to carry a credential-adjacent column) can leak through this
// path.

const maxLoggedSQLLen = 300

type queryTracer struct {
	slowThreshold time.Duration

	total, slow   atomic.Int64
	totalDuration atomic.Int64 // nanoseconds; QueryStats derives the average from this
}

func newQueryTracer(thresholdMS int) *queryTracer {
	if thresholdMS <= 0 {
		thresholdMS = DefaultSlowQueryThresholdMS
	}
	return &queryTracer{slowThreshold: time.Duration(thresholdMS) * time.Millisecond}
}

type traceCtxKey struct{}

type traceStart struct {
	at  time.Time
	sql string
}

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, traceStart{at: time.Now(), sql: data.SQL})
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(traceCtxKey{}).(traceStart)
	if !ok {
		return
	}
	duration := time.Since(start.at)
	t.total.Add(1)
	t.totalDuration.Add(int64(duration))

	if duration >= t.slowThreshold {
		t.slow.Add(1)
		attrs := []any{"event", "slow_query", "duration_ms", duration.Milliseconds(),
			"threshold_ms", t.slowThreshold.Milliseconds(), "sql", truncateSQL(start.sql)}
		var pgErr *pgconn.PgError
		if errors.As(data.Err, &pgErr) {
			attrs = append(attrs, "pg_code", pgErr.Code)
		}
		slog.Warn("component=database", attrs...)
		return
	}
	slog.Debug("component=database", "event", "query", "duration_ms", duration.Milliseconds())
}

func truncateSQL(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ") // collapse whitespace/newlines for a single log line
	if len(sql) > maxLoggedSQLLen {
		return sql[:maxLoggedSQLLen] + "…"
	}
	return sql
}

func (t *queryTracer) stats() QueryStats {
	total := t.total.Load()
	qs := QueryStats{Total: total, Slow: t.slow.Load()}
	if total > 0 {
		qs.AvgDurationMS = float64(t.totalDuration.Load()) / float64(total) / float64(time.Millisecond)
	}
	return qs
}

// --- env parsing helpers -------------------------------------------------------------------------

func envInt32(name string, fallback int32) int32 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return int32(v)
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
