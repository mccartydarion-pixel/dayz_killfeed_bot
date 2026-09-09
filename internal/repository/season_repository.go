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
	SeasonUpcoming = "UPCOMING"
	SeasonActive   = "ACTIVE"
	SeasonEnded    = "ENDED"
)

type Season struct {
	ID, GuildID  int64
	Name, Status string
	StartsAt     time.Time
	EndsAt       *time.Time
}
type SeasonResult struct {
	SeasonID                        int64
	TopPlayerID, TopFactionID       int64
	TopPlayerKills, TopFactionKills int64
	LongestKillPlayerID             int64
	LongestKillValue                float64
	BestStreakPlayerID              int64
	BestStreakValue                 int
	FinalizedAt                     time.Time
}
type SeasonRepository struct{ pool *pgxpool.Pool }

func NewSeasonRepository(pool *pgxpool.Pool) *SeasonRepository { return &SeasonRepository{pool: pool} }

// GetActiveSeason returns the single active season or nil.
func (r *SeasonRepository) GetActiveSeason(ctx context.Context, guildID int64) (*Season, error) {
	var s Season
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,name,status,starts_at,ends_at FROM seasons WHERE guild_id=$1 AND status=$2`, guildID, SeasonActive).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

func (r *SeasonRepository) ResolveAt(ctx context.Context, guildID int64, at time.Time) (*Season, error) {
	var s Season
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,name,status,starts_at,ends_at FROM seasons WHERE guild_id=$1 AND status=$2 AND starts_at<=$3 AND (ends_at IS NULL OR $3<ends_at) ORDER BY starts_at DESC LIMIT 1`, guildID, SeasonActive, at).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

func (r *SeasonRepository) GetSeasonByID(ctx context.Context, guildID, seasonID int64) (*Season, error) {
	var s Season
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,name,status,starts_at,ends_at FROM seasons WHERE guild_id=$1 AND id=$2`, guildID, seasonID).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

func (r *SeasonRepository) EnsureDefaultSeason(ctx context.Context, guildID int64, now time.Time) (*Season, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM guilds WHERE id=$1 FOR UPDATE`, guildID); err != nil {
		return nil, err
	}
	var s Season
	err = tx.QueryRow(ctx, `SELECT id,guild_id,name,status,starts_at,ends_at FROM seasons WHERE guild_id=$1 ORDER BY id LIMIT 1`, guildID).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO seasons(guild_id,name,starts_at,status) VALUES($1,'Season 1',$2,$3) RETURNING id,guild_id,name,status,starts_at,ends_at`, guildID, now, SeasonActive).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SeasonRepository) GetSeasonHistory(ctx context.Context, guildID int64, limit int) ([]Season, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,guild_id,name,status,starts_at,ends_at FROM seasons WHERE guild_id=$1 ORDER BY id DESC LIMIT $2`, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Season
	for rows.Next() {
		var s Season
		if err := rows.Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Start creates an active season only after locking the guild's active-season
// check in a transaction. The partial unique index is the final race guard.
func (r *SeasonRepository) Start(ctx context.Context, guildID int64, name string, startsAt time.Time) (*Season, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM guilds WHERE id=$1 FOR UPDATE`, guildID); err != nil {
		return nil, err
	}
	var activeID int64
	if err := tx.QueryRow(ctx, `SELECT id FROM seasons WHERE guild_id=$1 AND status=$2 FOR UPDATE`, guildID, SeasonActive).Scan(&activeID); err == nil {
		return nil, fmt.Errorf("active season %d already exists", activeID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var s Season
	err = tx.QueryRow(ctx, `INSERT INTO seasons(guild_id,name,starts_at,status) VALUES($1,$2,$3,$4) RETURNING id,guild_id,name,status,starts_at,ends_at`, guildID, name, startsAt, SeasonActive).Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *SeasonRepository) End(ctx context.Context, guildID, seasonID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE seasons SET status=$1,ends_at=$2,updated_at=NOW() WHERE guild_id=$3 AND id=$4 AND status=$5`, SeasonEnded, at, guildID, seasonID, SeasonActive)
	return err
}

func (r *SeasonRepository) FinalizeSeason(ctx context.Context, guildID, seasonID int64, at time.Time) (*SeasonResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var result SeasonResult
	err = tx.QueryRow(ctx, `SELECT id FROM seasons WHERE guild_id=$1 AND id=$2 FOR UPDATE`, guildID, seasonID).Scan(&result.SeasonID)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE seasons SET status=$1,ends_at=COALESCE(ends_at,$2),updated_at=NOW() WHERE guild_id=$3 AND id=$4 AND status<>$5`, SeasonEnded, at, guildID, seasonID, SeasonEnded)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(top_player_id,0),COALESCE(top_faction_id,0),COALESCE(top_player_kills,0),COALESCE(top_faction_kills,0),COALESCE(longest_kill_player_id,0),COALESCE(longest_kill_value,0),COALESCE(best_streak_player_id,0),COALESCE(best_streak_value,0),finalized_at FROM season_results WHERE season_id=$1`, seasonID).Scan(&result.TopPlayerID, &result.TopFactionID, &result.TopPlayerKills, &result.TopFactionKills, &result.LongestKillPlayerID, &result.LongestKillValue, &result.BestStreakPlayerID, &result.BestStreakValue, &result.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `WITH player_kills AS (SELECT killer_player_id,COUNT(*) kills FROM kills WHERE guild_id=$1 AND season_id=$2 GROUP BY killer_player_id), faction_kills AS (SELECT killer_faction_id,COUNT(*) kills FROM kills WHERE guild_id=$1 AND season_id=$2 AND killer_faction_id IS NOT NULL GROUP BY killer_faction_id), longest AS (SELECT killer_player_id,distance FROM kills WHERE guild_id=$1 AND season_id=$2 AND distance IS NOT NULL ORDER BY distance DESC LIMIT 1), streak AS (SELECT player_id,best_streak FROM player_combat_stats WHERE guild_id=$1 ORDER BY best_streak DESC LIMIT 1) SELECT COALESCE((SELECT killer_player_id FROM player_kills ORDER BY kills DESC,killer_player_id LIMIT 1),0),COALESCE((SELECT killer_faction_id FROM faction_kills ORDER BY kills DESC,killer_faction_id LIMIT 1),0),COALESCE((SELECT kills FROM player_kills ORDER BY kills DESC,killer_player_id LIMIT 1),0),COALESCE((SELECT kills FROM faction_kills ORDER BY kills DESC,killer_faction_id LIMIT 1),0),COALESCE((SELECT killer_player_id FROM longest),0),COALESCE((SELECT distance FROM longest),0),COALESCE((SELECT player_id FROM streak),0),COALESCE((SELECT best_streak FROM streak),0)`, guildID, seasonID).Scan(&result.TopPlayerID, &result.TopFactionID, &result.TopPlayerKills, &result.TopFactionKills, &result.LongestKillPlayerID, &result.LongestKillValue, &result.BestStreakPlayerID, &result.BestStreakValue)
		if err != nil {
			return nil, err
		}
		result.FinalizedAt = at
		_, err = tx.Exec(ctx, `INSERT INTO season_results(season_id,top_player_id,top_faction_id,top_player_kills,top_faction_kills,longest_kill_player_id,longest_kill_value,best_streak_player_id,best_streak_value,finalized_at) VALUES($1,NULLIF($2,0),NULLIF($3,0),$4,$5,NULLIF($6,0),$7,NULLIF($8,0),$9,$10) ON CONFLICT(season_id) DO NOTHING`, seasonID, result.TopPlayerID, result.TopFactionID, result.TopPlayerKills, result.TopFactionKills, result.LongestKillPlayerID, result.LongestKillValue, result.BestStreakPlayerID, result.BestStreakValue, result.FinalizedAt)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *SeasonRepository) GetSeasonResults(ctx context.Context, guildID, seasonID int64) (*SeasonResult, error) {
	var out SeasonResult
	err := r.pool.QueryRow(ctx, `SELECT sr.season_id,COALESCE(sr.top_player_id,0),COALESCE(sr.top_faction_id,0),COALESCE(sr.top_player_kills,0),COALESCE(sr.top_faction_kills,0),COALESCE(sr.longest_kill_player_id,0),COALESCE(sr.longest_kill_value,0),COALESCE(sr.best_streak_player_id,0),COALESCE(sr.best_streak_value,0),sr.finalized_at FROM season_results sr JOIN seasons s ON s.id=sr.season_id WHERE s.guild_id=$1 AND sr.season_id=$2`, guildID, seasonID).Scan(&out.SeasonID, &out.TopPlayerID, &out.TopFactionID, &out.TopPlayerKills, &out.TopFactionKills, &out.LongestKillPlayerID, &out.LongestKillValue, &out.BestStreakPlayerID, &out.BestStreakValue, &out.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &out, err
}

func (r *SeasonRepository) GetEndedSeasons(ctx context.Context, guildID int64, limit int) ([]Season, error) {
	rows, err := r.pool.Query(ctx, `SELECT s.id,s.guild_id,s.name,s.status,s.starts_at,s.ends_at FROM seasons s JOIN season_results r ON r.season_id=s.id WHERE s.guild_id=$1 AND s.status=$2 ORDER BY s.ends_at DESC LIMIT $3`, guildID, SeasonEnded, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Season
	for rows.Next() {
		var s Season
		if err := rows.Scan(&s.ID, &s.GuildID, &s.Name, &s.Status, &s.StartsAt, &s.EndsAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
