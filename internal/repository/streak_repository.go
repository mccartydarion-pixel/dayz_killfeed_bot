package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StreakState is durable current/best streak state.
type StreakState struct {
	Current int
	Best    int
}

type StreakRepository struct{ pool *pgxpool.Pool }

func NewStreakRepository(pool *pgxpool.Pool) *StreakRepository { return &StreakRepository{pool: pool} }

// Increment atomically increments current streak and preserves the best streak.
func (r *StreakRepository) Increment(ctx context.Context, guildID, playerID int64) (StreakState, error) {
	var s StreakState
	err := r.pool.QueryRow(ctx, `
INSERT INTO player_combat_stats(guild_id,player_id,current_streak,best_streak)
VALUES($1,$2,1,1)
ON CONFLICT(guild_id,player_id) DO UPDATE SET
 current_streak=player_combat_stats.current_streak+1,
 best_streak=GREATEST(player_combat_stats.best_streak,player_combat_stats.current_streak+1),
 updated_at=NOW()
RETURNING current_streak,best_streak`, guildID, playerID).Scan(&s.Current, &s.Best)
	if err != nil {
		return s, fmt.Errorf("increment streak: %w", err)
	}
	return s, nil
}

func (r *StreakRepository) Reset(ctx context.Context, guildID, playerID int64) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO player_combat_stats(guild_id,player_id,current_streak) VALUES($1,$2,0)
ON CONFLICT(guild_id,player_id) DO UPDATE SET current_streak=0,updated_at=NOW()`, guildID, playerID)
	return err
}

func (r *StreakRepository) Get(ctx context.Context, guildID, playerID int64) (StreakState, error) {
	var s StreakState
	err := r.pool.QueryRow(ctx, `SELECT current_streak,best_streak FROM player_combat_stats WHERE guild_id=$1 AND player_id=$2`, guildID, playerID).Scan(&s.Current, &s.Best)
	return s, err
}
