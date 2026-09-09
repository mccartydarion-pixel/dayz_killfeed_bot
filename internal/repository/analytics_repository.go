package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/analytics"
)

type AnalyticsRepository struct{ pool *pgxpool.Pool }

func NewAnalyticsRepository(pool *pgxpool.Pool) *AnalyticsRepository {
	return &AnalyticsRepository{pool: pool}
}
func scoped(scope analytics.Scope, seasonID int64, placeholder int) (string, []any) {
	if scope == analytics.ScopeSeason {
		return fmt.Sprintf(" AND k.season_id=$%d", placeholder), []any{seasonID}
	}
	return "", nil
}
func (r *AnalyticsRepository) GetPlayer(ctx context.Context, guildID int64, name string, scope analytics.Scope, seasonID int64) (*analytics.PlayerAnalytics, error) {
	var id int64
	var out analytics.PlayerAnalytics
	if err := r.pool.QueryRow(ctx, `SELECT id,display_name FROM players WHERE guild_id=$1 AND LOWER(display_name)=LOWER($2)`, guildID, name).Scan(&id, &out.Name); err != nil {
		return nil, err
	}
	cl, extra := scoped(scope, seasonID, 2)
	killerPlaceholder := "$2"
	if scope == analytics.ScopeSeason {
		killerPlaceholder = "$3"
	}
	args := []any{guildID}
	args = append(args, extra...)
	args = append(args, id)
	q := `SELECT COUNT(*),AVG(k.distance) FILTER(WHERE k.distance IS NOT NULL),percentile_cont(.5) WITHIN GROUP(ORDER BY k.distance) FILTER(WHERE k.distance IS NOT NULL),MAX(k.distance) FILTER(WHERE k.distance IS NOT NULL),COUNT(*) FILTER(WHERE k.headshot),COUNT(DISTINCT k.victim_player_id),COUNT(*) FILTER(WHERE k.distance<50),COUNT(*) FILTER(WHERE k.distance>=50 AND k.distance<100),COUNT(*) FILTER(WHERE k.distance>=100 AND k.distance<200),COUNT(*) FILTER(WHERE k.distance>=200) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=` + killerPlaceholder + cl
	if err := r.pool.QueryRow(ctx, q, args...).Scan(&out.Kills, &out.AverageDistance, &out.MedianDistance, &out.LongestKill, &out.Headshots, &out.UniqueVictims, &out.CloseKills, &out.MediumKills, &out.LongKills, &out.ExtremeKills); err != nil {
		return nil, err
	}
	var err error
	if scope == analytics.ScopeSeason {
		err = r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND player_id=$2 AND season_id=$3`, guildID, id, seasonID).Scan(&out.Deaths)
	} else {
		err = r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND player_id=$2`, guildID, id).Scan(&out.Deaths)
	}
	if err != nil {
		return nil, err
	}
	out.KD = float64(out.Kills)
	if out.Deaths > 0 {
		out.KD /= float64(out.Deaths)
	}
	out.HeadshotRate = analytics.Rate(out.Headshots, out.Kills)
	out.RecentForm = analytics.Form(out.Kills, out.Deaths)
	return &out, nil
}
func (r *AnalyticsRepository) Matchup(ctx context.Context, guildID int64, a, b string, scope analytics.Scope, seasonID int64) (*analytics.Matchup, error) {
	var out analytics.Matchup
	out.PlayerA = a
	out.PlayerB = b
	var ids [2]int64
	if err := r.pool.QueryRow(ctx, `SELECT id FROM players WHERE guild_id=$1 AND LOWER(display_name)=LOWER($2)`, guildID, a).Scan(&ids[0]); err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT id FROM players WHERE guild_id=$1 AND LOWER(display_name)=LOWER($2)`, guildID, b).Scan(&ids[1]); err != nil {
		return nil, err
	}
	cl, extra := scoped(scope, seasonID, 4)
	args := []any{guildID, ids[0], ids[1]}
	args = append(args, extra...)
	q := `SELECT COUNT(*) FILTER(WHERE killer_player_id=$2 AND victim_player_id=$3),COUNT(*) FILTER(WHERE killer_player_id=$3 AND victim_player_id=$2),COUNT(*),MAX(event_time),MAX(distance) FROM kills k WHERE guild_id=$1 AND ((killer_player_id=$2 AND victim_player_id=$3) OR (killer_player_id=$3 AND victim_player_id=$2))` + cl
	if err := r.pool.QueryRow(ctx, q, args...).Scan(&out.AKills, &out.BKills, &out.Total, &out.LastEncounter, &out.Longest); err != nil {
		return nil, err
	}
	return &out, nil
}
func (r *AnalyticsRepository) Weapon(ctx context.Context, guildID int64, weapon string, scope analytics.Scope, seasonID int64) (*analytics.WeaponStats, error) {
	var out analytics.WeaponStats
	out.Weapon = weapon
	cl, extra := scoped(scope, seasonID, 3)
	args := []any{guildID, weapon}
	args = append(args, extra...)
	q := `SELECT COUNT(*),COUNT(DISTINCT killer_player_id),COUNT(*) FILTER(WHERE headshot),AVG(distance) FILTER(WHERE distance IS NOT NULL),MAX(distance) FILTER(WHERE distance IS NOT NULL) FROM kills k WHERE guild_id=$1 AND LOWER(weapon_display)=LOWER($2)` + cl
	if err := r.pool.QueryRow(ctx, q, args...).Scan(&out.Kills, &out.UniqueUsers, &out.Headshots, &out.AverageDistance, &out.LongestDistance); err != nil {
		return nil, err
	}
	out.HeadshotRate = analytics.Rate(out.Headshots, out.Kills)
	return &out, nil
}
