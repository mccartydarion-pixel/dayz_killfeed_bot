package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PlayerRepository persists players keyed by (guild, DayZ player ID).
type PlayerRepository struct {
	pool *pgxpool.Pool
}

// NewPlayerRepository creates a player repository.
func NewPlayerRepository(pool *pgxpool.Pool) *PlayerRepository {
	return &PlayerRepository{pool: pool}
}

// UpsertPlayer inserts or updates a player by (guild, DayZ ID), updating the
// display name and last_seen_at. first_seen_at is set only on first insert.
// Returns the player row ID.
func (r *PlayerRepository) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	if dayzID == "" {
		return 0, fmt.Errorf("dayz_player_id is required")
	}
	const q = `
INSERT INTO players (guild_id, dayz_player_id, display_name, first_seen_at, last_seen_at)
VALUES ($1,$2,$3,$4,$4)
ON CONFLICT (guild_id, dayz_player_id) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at = NOW()
RETURNING id`

	var id int64
	err := r.pool.QueryRow(ctx, q, guildID, dayzID, displayName, seenAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert player: %w", err)
	}
	return id, nil
}

func (r *PlayerRepository) FindByDisplayName(ctx context.Context, guildID int64, displayName string) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM players WHERE guild_id=$1 AND LOWER(display_name)=LOWER($2) ORDER BY last_seen_at DESC LIMIT 1`, guildID, displayName).Scan(&id)
	return id, err
}

// PlayerProfile is a player's persistent stats snapshot.
type PlayerProfile struct {
	DisplayName string
	Kills       int64
	Deaths      int64
	LongestKill *float64
	LastSeen    time.Time
}

// KD returns kills/deaths. If deaths is zero, returns kills (no divide-by-zero).
func (p PlayerProfile) KD() float64 {
	if p.Deaths == 0 {
		return float64(p.Kills)
	}
	return float64(p.Kills) / float64(p.Deaths)
}
