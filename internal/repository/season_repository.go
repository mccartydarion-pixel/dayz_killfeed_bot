package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	SeasonUpcoming = "UPCOMING"
	SeasonActive   = "ACTIVE"
	SeasonEnded    = "ENDED"
)

type Season struct {
	ID, GuildID  int64
	Name, Status string
	StartsAt     time.Time
	EndsAt       *time.Time
}
type SeasonRepository struct{ pool *pgxpool.Pool }

func NewSeasonRepository(pool *pgxpool.Pool) *SeasonRepository { return &SeasonRepository{pool: pool} }

// GetActiveSeason returns the single active season or nil.
func (r *SeasonRepository) GetActiveSeason(ctx context.Context, guildID int64) (*Season, error) {
	var s Season
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,name,status,starts_at,ends_at FROM seasons WHERE guild_id=$1 AND status=$2`, guildID, SeasonActive).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// Start creates an active season only after locking the guild's active-season
// check in a transaction. The partial unique index is the final race guard.
func (r *SeasonRepository) Start(ctx context.Context, guildID int64, name string, startsAt time.Time) (*Season, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var activeID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM seasons WHERE guild_id=$1 AND status=$2 FOR UPDATE`, guildID, SeasonActive).Scan(&activeID); err == nil {
		return nil, fmt.Errorf("active season %d already exists", activeID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var s Season
	err = tx.QueryRow(ctx, `INSERT INTO seasons(guild_id,name,starts_at,status) VALUES($1,$2,$3,$4) RETURNING id,guild_id,name,status,starts_at,ends_at`, guildID, name, startsAt, SeasonActive).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SeasonRepository) End(ctx context.Context, guildID, seasonID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE seasons SET status=$1,ends_at=$2,updated_at=NOW() WHERE guild_id=$3 AND id=$4 AND status=$5`, SeasonEnded, at, guildID, seasonID, SeasonActive)
	return err
}
