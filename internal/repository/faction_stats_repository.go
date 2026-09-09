package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type FactionSeasonStats struct {
	FactionID                                      int64
	Kills, Deaths, EnemyKills, TeamKills, WarKills int64
	LongestKill                                    float64
	BestStreak                                     int
}
type RivalryStats struct {
	FactionAID, FactionBID               int64
	AKills, BKills, TotalKills, WarCount int64
	LastEncounter                        *time.Time
	LongestKill                          float64
	LongestKillerID                      int64
	TopKillerID                          int64
	TopKillerKills                       int64
}
type FactionStatsRepository struct{ pool *pgxpool.Pool }
type FactionLeaderboardEntry struct {
	FactionID               int64
	Name, Tag               string
	Kills, Deaths, WarKills int64
	LongestKill             float64
	BestStreak              int
}

func NewFactionStatsRepository(pool *pgxpool.Pool) *FactionStatsRepository {
	return &FactionStatsRepository{pool: pool}
}
func (r *FactionStatsRepository) GetFactionSeasonStats(ctx context.Context, guildID, seasonID, factionID int64) (*FactionSeasonStats, error) {
	var s FactionSeasonStats
	err := r.pool.QueryRow(ctx, `SELECT $3,COUNT(*) FILTER(WHERE killer_faction_id=$3),COUNT(*) FILTER(WHERE victim_faction_id=$3),COUNT(*) FILTER(WHERE killer_faction_id=$3 AND victim_faction_id IS NOT NULL AND killer_faction_id<>victim_faction_id),COUNT(*) FILTER(WHERE killer_faction_id=$3 AND victim_faction_id=$3),COUNT(*) FILTER(WHERE war_id IS NOT NULL AND killer_faction_id=$3),COALESCE(MAX(distance) FILTER(WHERE killer_faction_id=$3),0),0 FROM kills WHERE guild_id=$1 AND season_id=$2 AND (killer_faction_id=$3 OR victim_faction_id=$3)`, guildID, seasonID, factionID).Scan(&s.FactionID, &s.Kills, &s.Deaths, &s.EnemyKills, &s.TeamKills, &s.WarKills, &s.LongestKill, &s.BestStreak)
	if err != nil {
		return nil, fmt.Errorf("faction season stats: %w", err)
	}
	return &s, nil
}
func (r *FactionStatsRepository) Leaderboard(ctx context.Context, guildID, seasonID int64, scope, category string, limit int) ([]FactionLeaderboardEntry, error) {
	seasonClause := ""
	args := []any{guildID}
	if scope == "season" {
		seasonClause = " AND k.season_id=$2"
		args = append(args, seasonID)
	}
	order := "kills DESC"
	if category == "longest" {
		order = "longest_kill DESC"
	}
	if category == "war-kills" {
		order = "war_kills DESC"
	}
	q := `SELECT f.id,f.name,f.tag,COUNT(k.id) FILTER(WHERE k.killer_faction_id=f.id),COUNT(k.id) FILTER(WHERE k.victim_faction_id=f.id),COUNT(k.id) FILTER(WHERE k.war_id IS NOT NULL AND k.killer_faction_id=f.id),COALESCE(MAX(k.distance) FILTER(WHERE k.killer_faction_id=f.id),0),0 FROM factions f LEFT JOIN kills k ON k.guild_id=f.guild_id AND (k.killer_faction_id=f.id OR k.victim_faction_id=f.id)` + seasonClause + ` WHERE f.guild_id=$1 AND f.active GROUP BY f.id,f.name,f.tag`
	if category == "kd" {
		q += ` HAVING COUNT(k.id) FILTER(WHERE k.killer_faction_id=f.id)>=20`
	}
	q += ` ORDER BY ` + order + `,f.name LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactionLeaderboardEntry
	for rows.Next() {
		var e FactionLeaderboardEntry
		if err := rows.Scan(&e.FactionID, &e.Name, &e.Tag, &e.Kills, &e.Deaths, &e.WarKills, &e.LongestKill, &e.BestStreak); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (r *FactionStatsRepository) GetRivalry(ctx context.Context, guildID, factionA, factionB int64) (*RivalryStats, error) {
	a, b := factionA, factionB
	if a > b {
		a, b = b, a
	}
	var s RivalryStats
	s.FactionAID = a
	s.FactionBID = b
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FILTER(WHERE killer_faction_id=$2 AND victim_faction_id=$3),COUNT(*) FILTER(WHERE killer_faction_id=$3 AND victim_faction_id=$2),COUNT(*),COUNT(DISTINCT war_id) FILTER(WHERE war_id IS NOT NULL),MAX(event_time) FROM kills WHERE guild_id=$1 AND ((killer_faction_id=$2 AND victim_faction_id=$3) OR (killer_faction_id=$3 AND victim_faction_id=$2))`, guildID, a, b).Scan(&s.AKills, &s.BKills, &s.TotalKills, &s.WarCount, &s.LastEncounter)
	return &s, err
}

func (r *FactionStatsRepository) GetRivalryLongestKill(ctx context.Context, guildID, factionA, factionB int64) (int64, float64, error) {
	a, b := factionA, factionB
	if a > b {
		a, b = b, a
	}
	var player int64
	var distance float64
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,distance FROM kills WHERE guild_id=$1 AND distance IS NOT NULL AND ((killer_faction_id=$2 AND victim_faction_id=$3) OR (killer_faction_id=$3 AND victim_faction_id=$2)) ORDER BY distance DESC,id LIMIT 1`, guildID, a, b).Scan(&player, &distance)
	return player, distance, err
}
func (r *FactionStatsRepository) GetRivalryTopKiller(ctx context.Context, guildID, factionA, factionB int64) (int64, int64, error) {
	var player, count int64
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,COUNT(*) FROM kills WHERE guild_id=$1 AND ((killer_faction_id=$2 AND victim_faction_id=$3) OR (killer_faction_id=$3 AND victim_faction_id=$2)) GROUP BY killer_player_id ORDER BY COUNT(*) DESC,killer_player_id LIMIT 1`, guildID, factionA, factionB).Scan(&player, &count)
	return player, count, err
}
func (r *FactionStatsRepository) GetTopFactionRivals(ctx context.Context, guildID, factionID int64, limit int) ([]RivalryStats, error) {
	rows, err := r.pool.Query(ctx, `SELECT CASE WHEN killer_faction_id=$2 THEN victim_faction_id ELSE killer_faction_id END,COUNT(*) FROM kills WHERE guild_id=$1 AND (killer_faction_id=$2 OR victim_faction_id=$2) AND killer_faction_id IS NOT NULL AND victim_faction_id IS NOT NULL AND killer_faction_id<>victim_faction_id GROUP BY 1 ORDER BY COUNT(*) DESC LIMIT $3`, guildID, factionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RivalryStats
	for rows.Next() {
		var s RivalryStats
		if err := rows.Scan(&s.FactionBID, &s.TotalKills); err != nil {
			return nil, err
		}
		s.FactionAID = factionID
		out = append(out, s)
	}
	return out, rows.Err()
}
