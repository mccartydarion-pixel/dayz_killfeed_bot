package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	WarPending   = "PENDING"
	WarActive    = "ACTIVE"
	WarEnded     = "ENDED"
	WarCancelled = "CANCELLED"
)

type War struct {
	ID, GuildID, SeasonID, FactionAID, FactionBID int64
	Status                                        string
	StartedAt, EndedAt                            *time.Time
	ScoreA, ScoreB                                int64
	WinnerFactionID                               *int64
}

type WarRepository interface {
	Challenge(ctx context.Context, guildID, seasonID, factionA, factionB, creator int64) (*War, error)
	Accept(ctx context.Context, guildID, warID int64) error
	Decline(ctx context.Context, guildID, warID int64) error
	End(ctx context.Context, guildID, warID int64) error
	GetActivePair(ctx context.Context, guildID, factionA, factionB int64) (*War, error)
}

type PostgresWarRepository struct{ pool *pgxpool.Pool }

func NewPostgresWarRepository(pool *pgxpool.Pool) *PostgresWarRepository {
	return &PostgresWarRepository{pool: pool}
}

// CanonicalPair normalizes faction order to prevent A/B and B/A duplicates.
func CanonicalPair(a, b int64) (int64, int64, error) {
	if a <= 0 || b <= 0 || a == b {
		return 0, 0, fmt.Errorf("factions must be distinct")
	}
	if a > b {
		return b, a, nil
	}
	return a, b, nil
}

// WarClassification classifies faction combat from kill-time snapshots.
func WarClassification(killerFaction, victimFaction *int64) string {
	if killerFaction == nil || victimFaction == nil {
		return "NO_FACTION"
	}
	if *killerFaction == *victimFaction {
		return "TEAM_KILL"
	}
	return "ENEMY_FACTION_KILL"
}

func (r *PostgresWarRepository) Challenge(ctx context.Context, guildID, seasonID, factionA, factionB, creator int64) (*War, error) {
	a, b, err := CanonicalPair(factionA, factionB)
	if err != nil {
		return nil, err
	}
	var w War
	err = r.pool.QueryRow(ctx, `INSERT INTO faction_wars(guild_id,season_id,faction_a_id,faction_b_id,status,created_by_player_id) VALUES($1,NULLIF($2,0),$3,$4,$5,$6) RETURNING id,guild_id,COALESCE(season_id,0),faction_a_id,faction_b_id,status,started_at,ended_at,faction_a_score,faction_b_score,winner_faction_id`, guildID, seasonID, a, b, WarPending, creator).Scan(&w.ID, &w.GuildID, &w.SeasonID, &w.FactionAID, &w.FactionBID, &w.Status, &w.StartedAt, &w.EndedAt, &w.ScoreA, &w.ScoreB, &w.WinnerFactionID)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *PostgresWarRepository) Accept(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,started_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4`, WarActive, guildID, warID, WarPending)
	return err
}
func (r *PostgresWarRepository) Decline(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,ended_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4`, WarCancelled, guildID, warID, WarPending)
	return err
}
func (r *PostgresWarRepository) End(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,ended_at=NOW(),winner_faction_id=CASE WHEN faction_a_score>faction_b_score THEN faction_a_id WHEN faction_b_score>faction_a_score THEN faction_b_id ELSE NULL END WHERE guild_id=$2 AND id=$3 AND status=$4`, WarEnded, guildID, warID, WarActive)
	return err
}

func (r *PostgresWarRepository) GetActivePair(ctx context.Context, guildID, factionA, factionB int64) (*War, error) {
	a, b, err := CanonicalPair(factionA, factionB)
	if err != nil {
		return nil, err
	}
	var w War
	err = r.pool.QueryRow(ctx, `SELECT id,guild_id,COALESCE(season_id,0),faction_a_id,faction_b_id,status,started_at,ended_at,faction_a_score,faction_b_score,winner_faction_id FROM faction_wars WHERE guild_id=$1 AND faction_a_id=$2 AND faction_b_id=$3 AND status=$4`, guildID, a, b, WarActive).Scan(&w.ID, &w.GuildID, &w.SeasonID, &w.FactionAID, &w.FactionBID, &w.Status, &w.StartedAt, &w.EndedAt, &w.ScoreA, &w.ScoreB, &w.WinnerFactionID)
	if err != nil {
		return nil, err
	}
	return &w, nil
}
