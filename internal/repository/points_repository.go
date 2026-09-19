package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Points struct{ Lifetime, Season int64 }
type PointsRepository struct{ pool *pgxpool.Pool }

func NewPointsRepository(pool *pgxpool.Pool) *PointsRepository { return &PointsRepository{pool: pool} }
// Award credits earned Champion Points through the economy ledger (the single
// implementation of every point credit). It returns false, nil when the same
// (guild, player, reason, sourceKey) was already awarded.
func (r *PointsRepository) Award(ctx context.Context, guildID, seasonID, playerID int64, amount int, reason string, sourceID int64, sourceKey string) (bool, error) {
	entry, err := NewEconomyRepository(r.pool).Credit(ctx, LedgerParams{
		GuildID: guildID, PlayerID: playerID, SeasonID: seasonID, Type: reason, Amount: int64(amount),
		Earned: true, ReferenceID: sourceKey, SourceID: sourceID, CreatedBy: "SYSTEM",
	})
	if err != nil {
		return false, err
	}
	return !entry.Duplicate, nil
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

func (r *PointsRepository) Leaderboard(ctx context.Context, guildID int64, season bool, limit int) ([]LeaderboardEntry, error) {
	column := "lifetime_points"
	if season {
		column = "season_points"
	}
	rows, err := r.pool.Query(ctx, `SELECT p.display_name,pp.`+column+` FROM player_points pp JOIN players p ON p.id=pp.player_id WHERE pp.guild_id=$1 ORDER BY pp.`+column+` DESC,p.display_name LIMIT $2`, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		var points int64
		if err := rows.Scan(&e.DisplayName, &points); err != nil {
			return nil, err
		}
		e.Value = fmt.Sprintf("%d", points)
		out = append(out, e)
	}
	return out, rows.Err()
}
