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
}
type FactionStatsRepository struct{ pool *pgxpool.Pool }

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
