package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Death type classifications. Only values the real ADM data supports.
const (
	DeathTypePVP     = "PVP"
	DeathTypeSuicide = "SUICIDE"
	DeathTypeUnknown = "UNKNOWN"
)

// DeathRepository persists deaths with durable dedupe.
type DeathRepository struct {
	pool *pgxpool.Pool
}

// NewDeathRepository creates a death repository.
func NewDeathRepository(pool *pgxpool.Pool) *DeathRepository {
	return &DeathRepository{pool: pool}
}

// DeathRecord is one normalized, persisted death.
type DeathRecord struct {
	GuildID     int64
	SessionID   string
	Fingerprint string
	PlayerID    int64
	DeathType   string
	EventTime   *time.Time
}

// InsertDeath persists a death. Returns ErrDuplicate on (guild, fingerprint) conflict.
func (r *DeathRepository) InsertDeath(ctx context.Context, d DeathRecord) error {
	const q = `
INSERT INTO deaths (guild_id, session_id, event_fingerprint, player_id, death_type, event_time)
VALUES ($1,$2,$3,$4,$5,$6)`

	_, err := r.pool.Exec(ctx, q, d.GuildID, d.SessionID, d.Fingerprint, d.PlayerID, d.DeathType, d.EventTime)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("insert death: %w", err)
	}
	return nil
}
