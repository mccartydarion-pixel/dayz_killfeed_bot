package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LeaderboardEntry is one ranked row.
type LeaderboardEntry struct {
	DisplayName string
	Value       string
}

type SeasonPlayerStats struct {
	DisplayName   string
	Kills, Deaths int64
	LongestKill   *float64
	BestStreak    int
}

// StatsRepository answers player stat and leaderboard queries from kills/deaths.
// It queries the source tables directly (no fragile manually-maintained stats table).
type StatsRepository struct {
	pool *pgxpool.Pool
}

// NewStatsRepository creates a stats repository.
func NewStatsRepository(pool *pgxpool.Pool) *StatsRepository {
	return &StatsRepository{pool: pool}
}

// GetPlayerProfile returns a player's persistent stats for one guild by display
// name (case-insensitive). Returns nil if not found.
func (r *StatsRepository) GetPlayerProfile(ctx context.Context, guildID int64, displayName string) (*PlayerProfile, error) {
	const q = `
SELECT p.display_name,
       (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=p.id) AS kills,
       (SELECT COUNT(*) FROM deaths d WHERE d.guild_id=$1 AND d.player_id=p.id) AS deaths,
       (SELECT MAX(k.distance) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=p.id) AS longest,
       p.last_seen_at
FROM players p
WHERE p.guild_id=$1 AND LOWER(p.display_name)=LOWER($2)`

	var prof PlayerProfile
	err := r.pool.QueryRow(ctx, q, guildID, displayName).Scan(
		&prof.DisplayName, &prof.Kills, &prof.Deaths, &prof.LongestKill, &prof.LastSeen,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("get player profile: %w", err)
	}
	return &prof, nil
}

// TopByKills returns the top players by kill count.
func (r *StatsRepository) TopByKills(ctx context.Context, guildID int64, limit int) ([]LeaderboardEntry, error) {
	const q = `
SELECT p.display_name, COUNT(k.id) AS kills
FROM players p
JOIN kills k ON k.killer_player_id=p.id AND k.guild_id=p.guild_id
WHERE p.guild_id=$1
GROUP BY p.id, p.display_name
ORDER BY kills DESC, p.display_name ASC
LIMIT $2`
	return r.queryLeaderboard(ctx, q, guildID, limit)
}

// TopByKD returns players ranked by K/D, requiring a minimum kill count so that
// a 1/0 player doesn't top the board. minKills is configurable by callers.
func (r *StatsRepository) TopByKD(ctx context.Context, guildID int64, limit, minKills int) ([]LeaderboardEntry, error) {
	const q = `
SELECT p.display_name,
       (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=p.id) AS kills,
       (SELECT COUNT(*) FROM deaths d WHERE d.guild_id=$1 AND d.player_id=p.id) AS deaths
FROM players p
WHERE p.guild_id=$1
  AND (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=p.id) >= $3
ORDER BY (kills::float / GREATEST(deaths,1)) DESC, kills DESC, p.display_name ASC
LIMIT $2`

	rows, err := r.pool.Query(ctx, q, guildID, limit, minKills)
	if err != nil {
		return nil, fmt.Errorf("top by KD: %w", err)
	}
	defer rows.Close()

	var out []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		var kills, deaths int64
		if err := rows.Scan(&e.DisplayName, &kills, &deaths); err != nil {
			return nil, err
		}
		kd := float64(kills)
		if deaths > 0 {
			kd = float64(kills) / float64(deaths)
		}
		e.Value = fmt.Sprintf("%.2f", kd)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TopLongestKill returns players ranked by their longest kill distance.
func (r *StatsRepository) TopLongestKill(ctx context.Context, guildID int64, limit int) ([]LeaderboardEntry, error) {
	const q = `
SELECT p.display_name, MAX(k.distance) AS longest
FROM players p
JOIN kills k ON k.killer_player_id=p.id AND k.guild_id=p.guild_id
WHERE p.guild_id=$1 AND k.distance IS NOT NULL
GROUP BY p.id, p.display_name
ORDER BY longest DESC, p.display_name ASC
LIMIT $2`

	rows, err := r.pool.Query(ctx, q, guildID, limit)
	if err != nil {
		return nil, fmt.Errorf("top longest kill: %w", err)
	}
	defer rows.Close()

	var out []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		var dist float64
		if err := rows.Scan(&e.DisplayName, &dist); err != nil {
			return nil, err
		}
		e.Value = fmt.Sprintf("%.1fm", dist)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *StatsRepository) GetPlayerSeasonStats(ctx context.Context, guildID, seasonID int64, displayName string) (*SeasonPlayerStats, error) {
	var s SeasonPlayerStats
	err := r.pool.QueryRow(ctx, `SELECT p.display_name,COUNT(k.id),COUNT(d.id),MAX(k.distance),0 FROM players p LEFT JOIN kills k ON k.killer_player_id=p.id AND k.guild_id=$1 AND k.season_id=$2 LEFT JOIN deaths d ON d.player_id=p.id AND d.guild_id=$1 AND d.season_id=$2 WHERE p.guild_id=$1 AND LOWER(p.display_name)=LOWER($3) GROUP BY p.id,p.display_name`, guildID, seasonID, displayName).Scan(&s.DisplayName, &s.Kills, &s.Deaths, &s.LongestKill, &s.BestStreak)
	if err != nil {
		return nil, fmt.Errorf("season player stats: %w", err)
	}
	return &s, nil
}
func (r *StatsRepository) GetPlayerSeasonKillRank(ctx context.Context, guildID, seasonID, playerID int64) (int64, error) {
	var rank int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*)+1 FROM (SELECT killer_player_id,COUNT(*) kills FROM kills WHERE guild_id=$1 AND season_id=$2 GROUP BY killer_player_id) q WHERE q.kills>(SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND season_id=$2 AND killer_player_id=$3)`, guildID, seasonID, playerID).Scan(&rank)
	return rank, err
}
func (r *StatsRepository) GetPlayerSeasonKDRank(ctx context.Context, guildID, seasonID, playerID int64) (int64, error) {
	var rank int64
	err := r.pool.QueryRow(ctx, `WITH scores AS (SELECT p.id,COUNT(k.id)::float/GREATEST(COUNT(d.id),1) kd FROM players p LEFT JOIN kills k ON k.killer_player_id=p.id AND k.guild_id=$1 AND k.season_id=$2 LEFT JOIN deaths d ON d.player_id=p.id AND d.guild_id=$1 AND d.season_id=$2 WHERE p.guild_id=$1 GROUP BY p.id) SELECT COUNT(*)+1 FROM scores WHERE kd>(SELECT kd FROM scores WHERE id=$3)`, guildID, seasonID, playerID).Scan(&rank)
	return rank, err
}
func (r *StatsRepository) GetPlayerSeasonLongestRank(ctx context.Context, guildID, seasonID, playerID int64) (int64, error) {
	var rank int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*)+1 FROM (SELECT killer_player_id,MAX(distance) longest FROM kills WHERE guild_id=$1 AND season_id=$2 GROUP BY killer_player_id) q WHERE q.longest>(SELECT COALESCE(MAX(distance),0) FROM kills WHERE guild_id=$1 AND season_id=$2 AND killer_player_id=$3)`, guildID, seasonID, playerID).Scan(&rank)
	return rank, err
}
func (r *StatsRepository) GetPlayerSeasonBestStreak(ctx context.Context, guildID, seasonID, playerID int64) (int, error) {
	var streak int
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(best_streak,0) FROM player_combat_stats WHERE guild_id=$1 AND player_id=$2`, guildID, playerID).Scan(&streak)
	return streak, err
}

func (r *StatsRepository) queryLeaderboard(ctx context.Context, q string, guildID int64, limit int) ([]LeaderboardEntry, error) {
	rows, err := r.pool.Query(ctx, q, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LeaderboardEntry
	for rows.Next() {
		var e LeaderboardEntry
		var n int64
		if err := rows.Scan(&e.DisplayName, &n); err != nil {
			return nil, err
		}
		e.Value = fmt.Sprintf("%d", n)
		out = append(out, e)
	}
	return out, rows.Err()
}
