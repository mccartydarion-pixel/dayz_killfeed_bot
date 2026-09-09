package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Points struct{ Lifetime, Season int64 }
type PointsRepository struct{ pool *pgxpool.Pool }

func NewPointsRepository(pool *pgxpool.Pool) *PointsRepository { return &PointsRepository{pool: pool} }
func (r *PointsRepository) Award(ctx context.Context, guildID, seasonID, playerID int64, amount int, reason string, sourceID int64, sourceKey string) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO point_transactions(guild_id,season_id,player_id,amount,reason_type,source_id,source_key) VALUES($1,NULLIF($2,0),$3,$4,$5,NULLIF($6,0),$7) ON CONFLICT DO NOTHING`, guildID, seasonID, playerID, amount, reason, sourceID, sourceKey)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO player_points(guild_id,player_id,lifetime_points,season_points) VALUES($1,$2,$3,$3) ON CONFLICT(guild_id,player_id) DO UPDATE SET lifetime_points=player_points.lifetime_points+EXCLUDED.lifetime_points,season_points=player_points.season_points+EXCLUDED.season_points,updated_at=NOW()`, guildID, playerID, amount)
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func (r *PointsRepository) Get(ctx context.Context, guildID, playerID int64) (*Points, error) {
	var p Points
	err := r.pool.QueryRow(ctx, `SELECT lifetime_points,season_points FROM player_points WHERE guild_id=$1 AND player_id=$2`, guildID, playerID).Scan(&p.Lifetime, &p.Season)
	return &p, err
}
func (r *PointsRepository) ResetSeason(ctx context.Context, guildID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_points SET season_points=0,updated_at=NOW() WHERE guild_id=$1`, guildID)
	return err
}
