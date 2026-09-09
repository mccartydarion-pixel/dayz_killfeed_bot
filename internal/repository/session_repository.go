package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckpointRepository persists ADM byte-offset checkpoints for safe resume.
type CheckpointRepository struct {
	pool *pgxpool.Pool
}

// NewCheckpointRepository creates a checkpoint repository.
func NewCheckpointRepository(pool *pgxpool.Pool) *CheckpointRepository {
	return &CheckpointRepository{pool: pool}
}

// SaveCheckpoint upserts the byte offset for a guild+session. Called on a batched
// cadence (not per line) and on graceful shutdown.
func (r *CheckpointRepository) SaveCheckpoint(ctx context.Context, guildID int64, sessionID, filename string, offset int64) error {
	const q = `
INSERT INTO adm_checkpoints (guild_id, session_id, adm_filename, byte_offset)
VALUES ($1,$2,$3,$4)
ON CONFLICT (guild_id, session_id) DO UPDATE SET
    byte_offset = EXCLUDED.byte_offset,
    adm_filename = EXCLUDED.adm_filename,
    updated_at = NOW()`
	_, err := r.pool.Exec(ctx, q, guildID, sessionID, filename, offset)
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint returns the stored byte offset for a guild+session, or -1 if none.
func (r *CheckpointRepository) LoadCheckpoint(ctx context.Context, guildID int64, sessionID string) (int64, error) {
	const q = `SELECT byte_offset FROM adm_checkpoints WHERE guild_id=$1 AND session_id=$2`
	var offset int64
	err := r.pool.QueryRow(ctx, q, guildID, sessionID).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("load checkpoint: %w", err)
	}
	return offset, nil
}

// SessionRepository tracks ADM sessions (one per rotated log file).
type SessionRepository struct {
	pool *pgxpool.Pool
}

// NewSessionRepository creates a session repository.
func NewSessionRepository(pool *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{pool: pool}
}

// StartSession upserts a new ADM session for a guild, marking it active.
func (r *SessionRepository) StartSession(ctx context.Context, guildID int64, sessionID, filename string, startedAt *time.Time) error {
	const q = `
INSERT INTO adm_sessions (guild_id, session_id, adm_filename, started_at)
VALUES ($1,$2,$3,$4)
ON CONFLICT (guild_id, session_id) DO UPDATE SET
    adm_filename = EXCLUDED.adm_filename,
    ended_at = NULL`
	_, err := r.pool.Exec(ctx, q, guildID, sessionID, filename, startedAt)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	return nil
}

// EndSession marks a session ended. Called when rotation to a new session occurs.
func (r *SessionRepository) EndSession(ctx context.Context, guildID int64, sessionID string) error {
	const q = `UPDATE adm_sessions SET ended_at=NOW() WHERE guild_id=$1 AND session_id=$2 AND ended_at IS NULL`
	_, err := r.pool.Exec(ctx, q, guildID, sessionID)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}
