package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/achievements"
)

type AchievementRepository struct{ pool *pgxpool.Pool }

func NewAchievementRepository(pool *pgxpool.Pool) *AchievementRepository {
	return &AchievementRepository{pool: pool}
}

// EnsureDefinitions seeds code-defined achievements idempotently.
func (r *AchievementRepository) EnsureDefinitions(ctx context.Context) error {
	for _, d := range achievements.Definitions {
		if _, err := r.pool.Exec(ctx, `INSERT INTO achievements(key,name,description,emoji,hidden) VALUES($1,$2,$3,$4,$5) ON CONFLICT(key) DO NOTHING`, d.Key, d.Name, d.Description, d.Emoji, d.Hidden); err != nil {
			return err
		}
	}
	return nil
}

// Unlock inserts once and returns true only for a newly unlocked achievement.
func (r *AchievementRepository) Unlock(ctx context.Context, guildID, playerID int64, key string, killID *int64) (bool, error) {
	command := `INSERT INTO player_achievements(guild_id,player_id,achievement_key,source_kill_id) VALUES($1,$2,$3,$4) ON CONFLICT(guild_id,player_id,achievement_key) DO NOTHING`
	ct, err := r.pool.Exec(ctx, command, guildID, playerID, key, killID)
	if err != nil {
		return false, fmt.Errorf("unlock achievement: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

func (r *AchievementRepository) Count(ctx context.Context, guildID, playerID int64) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM player_achievements WHERE guild_id=$1 AND player_id=$2`, guildID, playerID).Scan(&n)
	return n, err
}
