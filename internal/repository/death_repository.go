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
	ServerID    int64
	SessionID   string
	Fingerprint string
	PlayerID    int64
	SeasonID    *int64
	DeathType   string
	EventTime   *time.Time
	// Physical ADM source of the death line (Live Sync phase 2); empty for legacy callers.
	SourceFile      string
	SourceOffset    int64
	SourceLocalTime *time.Time
}

// InsertDeath persists a death. Returns ErrDuplicate on (guild, fingerprint) conflict.
func (r *DeathRepository) InsertDeath(ctx context.Context, d DeathRecord) error {
	const q = `
	INSERT INTO deaths (guild_id, server_id, session_id, event_fingerprint, player_id, season_id, death_type, event_time, source_file, source_offset, source_local_time)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`

	_, err := r.pool.Exec(ctx, q, d.GuildID, nilIfZero(d.ServerID), d.SessionID, d.Fingerprint, d.PlayerID, d.SeasonID, d.DeathType, d.EventTime,
		nilIfEmpty(d.SourceFile), sourceOffsetArg(d.SourceFile, d.SourceOffset), d.SourceLocalTime)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("insert death: %w", err)
	}
	return nil
}
