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

func (r *PostgresWarRepository) CreateChallenge(ctx context.Context, guildID, seasonID, factionA, factionB, creator int64) (*War, error) {
	return r.Challenge(ctx, guildID, seasonID, factionA, factionB, creator)
}

func (r *PostgresWarRepository) Accept(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,started_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4`, WarActive, guildID, warID, WarPending)
	return err
}
func (r *PostgresWarRepository) AcceptWar(ctx context.Context, guildID, warID int64) error {
	return r.Accept(ctx, guildID, warID)
}
func (r *PostgresWarRepository) Decline(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,ended_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4`, WarCancelled, guildID, warID, WarPending)
	return err
}
func (r *PostgresWarRepository) DeclineWar(ctx context.Context, guildID, warID int64) error {
	return r.Decline(ctx, guildID, warID)
}
func (r *PostgresWarRepository) End(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `WITH scores AS (SELECT COUNT(*) FILTER (WHERE k.killer_faction_id=w.faction_a_id) AS a, COUNT(*) FILTER (WHERE k.killer_faction_id=w.faction_b_id) AS b FROM faction_wars w LEFT JOIN kills k ON k.war_id=w.id WHERE w.guild_id=$1 AND w.id=$2) UPDATE faction_wars w SET status=$3,ended_at=NOW(),faction_a_score=s.a,faction_b_score=s.b,winner_faction_id=CASE WHEN s.a>s.b THEN w.faction_a_id WHEN s.b>s.a THEN w.faction_b_id ELSE NULL END FROM scores s WHERE w.guild_id=$1 AND w.id=$2 AND w.status=$4`, guildID, warID, WarEnded, WarActive)
	return err
}
func (r *PostgresWarRepository) EndWar(ctx context.Context, guildID, warID int64) error {
	return r.End(ctx, guildID, warID)
}
func (r *PostgresWarRepository) CancelWar(ctx context.Context, guildID, warID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,ended_at=NOW() WHERE guild_id=$2 AND id=$3 AND status IN ($4,$5)`, WarCancelled, guildID, warID, WarPending, WarActive)
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
func (r *PostgresWarRepository) GetActiveWarForPair(ctx context.Context, guildID, factionA, factionB int64) (*War, error) {
	return r.GetActivePair(ctx, guildID, factionA, factionB)
}

func (r *PostgresWarRepository) GetWar(ctx context.Context, guildID, warID int64) (*War, error) {
	var w War
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,COALESCE(season_id,0),faction_a_id,faction_b_id,status,started_at,ended_at,faction_a_score,faction_b_score,winner_faction_id FROM faction_wars WHERE guild_id=$1 AND id=$2`, guildID, warID).Scan(&w.ID, &w.GuildID, &w.SeasonID, &w.FactionAID, &w.FactionBID, &w.Status, &w.StartedAt, &w.EndedAt, &w.ScoreA, &w.ScoreB, &w.WinnerFactionID)
	return &w, err
}
func (r *PostgresWarRepository) GetFactionActiveWars(ctx context.Context, guildID, factionID int64) ([]War, error) {
	return r.listWars(ctx, `SELECT id,guild_id,COALESCE(season_id,0),faction_a_id,faction_b_id,status,started_at,ended_at,faction_a_score,faction_b_score,winner_faction_id FROM faction_wars WHERE guild_id=$1 AND (faction_a_id=$2 OR faction_b_id=$2) AND status='ACTIVE' ORDER BY started_at`, guildID, factionID)
}
func (r *PostgresWarRepository) GetWarHistory(ctx context.Context, guildID int64, limit int) ([]War, error) {
	return r.listWars(ctx, `SELECT id,guild_id,COALESCE(season_id,0),faction_a_id,faction_b_id,status,started_at,ended_at,faction_a_score,faction_b_score,winner_faction_id FROM faction_wars WHERE guild_id=$1 ORDER BY created_at DESC LIMIT $2`, guildID, limit)
}
func (r *PostgresWarRepository) listWars(ctx context.Context, q string, args ...any) ([]War, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []War
	for rows.Next() {
		var w War
		if err := rows.Scan(&w.ID, &w.GuildID, &w.SeasonID, &w.FactionAID, &w.FactionBID, &w.Status, &w.StartedAt, &w.EndedAt, &w.ScoreA, &w.ScoreB, &w.WinnerFactionID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (r *PostgresWarRepository) GetWarScore(ctx context.Context, guildID, warID int64) (int64, int64, error) {
	var a, b int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FILTER(WHERE killer_faction_id=faction_a_id),COUNT(*) FILTER(WHERE killer_faction_id=faction_b_id) FROM faction_wars w LEFT JOIN kills k ON k.war_id=w.id WHERE w.guild_id=$1 AND w.id=$2 GROUP BY w.id`, guildID, warID).Scan(&a, &b)
	return a, b, err
}
func (r *PostgresWarRepository) GetWarTopKiller(ctx context.Context, guildID, warID int64) (int64, int64, error) {
	var player, count int64
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,COUNT(*) FROM kills WHERE guild_id=$1 AND war_id=$2 GROUP BY killer_player_id ORDER BY COUNT(*) DESC,killer_player_id LIMIT 1`, guildID, warID).Scan(&player, &count)
	return player, count, err
}
func (r *PostgresWarRepository) GetWarLongestKill(ctx context.Context, guildID, warID int64) (int64, float64, error) {
	var player int64
	var distance float64
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,distance FROM kills WHERE guild_id=$1 AND war_id=$2 AND distance IS NOT NULL ORDER BY distance DESC,id LIMIT 1`, guildID, warID).Scan(&player, &distance)
	return player, distance, err
}

var _ = errors.Is
var _ = pgx.ErrNoRows
