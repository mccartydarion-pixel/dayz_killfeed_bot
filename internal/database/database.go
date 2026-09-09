package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the pgx connection pool. It never logs the connection string.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect creates a connection pool and verifies connectivity with a bounded ping.
// Returns an error if the database is unreachable; callers decide whether to
// continue degraded or fail startup.
func Connect(ctx context.Context, databaseURL string) (*DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is not configured")
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

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

	slog.Info("component=database", "msg", "connected", "max_conns", cfg.MaxConns)
	return &DB{Pool: pool}, nil
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
