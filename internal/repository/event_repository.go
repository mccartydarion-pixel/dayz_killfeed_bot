package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CompetitiveEvent struct {
	ID, GuildID, SeasonID           int64
	Type, Name, Description, Status string
	StartsAt, EndsAt                *time.Time
	Config                          json.RawMessage
}
type EventScore struct {
	PlayerID, FactionID int64
	Score               float64
	Kills               int64
	BestDistance        *float64
	BestStreak          int
}
type EventResult struct {
	EventID, WinnerPlayerID, WinnerFactionID int64
	WinningScore                             float64
	FinalizedAt                              time.Time
}
type EventRepository struct{ pool *pgxpool.Pool }

func NewEventRepository(pool *pgxpool.Pool) *EventRepository { return &EventRepository{pool: pool} }

func (r *EventRepository) CreateEvent(ctx context.Context, e CompetitiveEvent, createdBy string) (*CompetitiveEvent, error) {
	var out CompetitiveEvent
	err := r.pool.QueryRow(ctx, `INSERT INTO competitive_events(guild_id,season_id,event_type,name,description,status,starts_at,ends_at,created_by_discord_user_id,config) VALUES($1,NULLIF($2,0),$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config`, e.GuildID, e.SeasonID, e.Type, e.Name, e.Description, e.Status, e.StartsAt, e.EndsAt, createdBy, e.Config).Scan(&out.ID, &out.GuildID, &out.SeasonID, &out.Type, &out.Name, &out.Description, &out.Status, &out.StartsAt, &out.EndsAt, &out.Config)
	return &out, err
}
func (r *EventRepository) GetEvent(ctx context.Context, guildID, eventID int64) (*CompetitiveEvent, error) {
	var e CompetitiveEvent
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config FROM competitive_events WHERE guild_id=$1 AND id=$2`, guildID, eventID).Scan(&e.ID, &e.GuildID, &e.SeasonID, &e.Type, &e.Name, &e.Description, &e.Status, &e.StartsAt, &e.EndsAt, &e.Config)
	if err != nil {
		return nil, err
	}
	return &e, nil
}
func (r *EventRepository) GetActiveEvents(ctx context.Context, guildID int64) ([]CompetitiveEvent, error) {
	return r.list(ctx, `SELECT id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config FROM competitive_events WHERE guild_id=$1 AND status='ACTIVE' ORDER BY id`, guildID)
}
func (r *EventRepository) GetScheduledEvents(ctx context.Context, guildID int64) ([]CompetitiveEvent, error) {
	return r.list(ctx, `SELECT id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config FROM competitive_events WHERE guild_id=$1 AND status='SCHEDULED' ORDER BY starts_at`, guildID)
}

func (r *EventRepository) GetRecentEvents(ctx context.Context, guildID int64, limit int) ([]CompetitiveEvent, error) {
	return r.list(ctx, `SELECT id,guild_id,COALESCE(season_id,0),event_type,name,COALESCE(description,''),status,starts_at,ends_at,config FROM competitive_events WHERE guild_id=$1 ORDER BY created_at DESC LIMIT $2`, guildID, limit)
}

func (r *EventRepository) GetEndedUnfinalized(ctx context.Context, guildID int64, limit int) ([]CompetitiveEvent, error) {
	return r.list(ctx, `SELECT e.id,e.guild_id,COALESCE(e.season_id,0),e.event_type,e.name,COALESCE(e.description,''),e.status,e.starts_at,e.ends_at,e.config FROM competitive_events e LEFT JOIN event_results r ON r.event_id=e.id WHERE e.guild_id=$1 AND e.status='ENDED' AND r.event_id IS NULL ORDER BY e.ends_at LIMIT $2`, guildID, limit)
}

func (r *EventRepository) GetEventLeaderboard(ctx context.Context, eventID int64, limit int) ([]EventScore, error) {
	return r.Leaderboard(ctx, eventID, limit)
}
func (r *EventRepository) list(ctx context.Context, q string, args ...any) ([]CompetitiveEvent, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompetitiveEvent
	for rows.Next() {
		var e CompetitiveEvent
		if err := rows.Scan(&e.ID, &e.GuildID, &e.SeasonID, &e.Type, &e.Name, &e.Description, &e.Status, &e.StartsAt, &e.EndsAt, &e.Config); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (r *EventRepository) transition(ctx context.Context, guildID, eventID, from int64, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET status=$1,updated_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4`, status, guildID, eventID, from)
	return err
}
func (r *EventRepository) StartEvent(ctx context.Context, guildID, eventID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET status='ACTIVE',starts_at=COALESCE(starts_at,$1),updated_at=NOW() WHERE guild_id=$2 AND id=$3 AND status IN ('DRAFT','SCHEDULED') AND (starts_at IS NULL OR starts_at<=$1)`, at, guildID, eventID)
	return err
}
func (r *EventRepository) EndEvent(ctx context.Context, guildID, eventID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET status='ENDED',ends_at=COALESCE(ends_at,$1),updated_at=NOW() WHERE guild_id=$2 AND id=$3 AND status='ACTIVE'`, at, guildID, eventID)
	return err
}
func (r *EventRepository) CancelEvent(ctx context.Context, guildID, eventID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET status='CANCELLED',updated_at=NOW() WHERE guild_id=$1 AND id=$2 AND status IN ('DRAFT','SCHEDULED','ACTIVE')`, guildID, eventID)
	return err
}

func (r *EventRepository) ActivateDue(ctx context.Context, now time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET status='ACTIVE',starts_at=COALESCE(starts_at,$1),updated_at=NOW() WHERE status='SCHEDULED' AND starts_at IS NOT NULL AND starts_at<=$1`, now)
	return err
}

func (r *EventRepository) EndDue(ctx context.Context, now time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE competitive_events SET status='ENDED',updated_at=NOW() WHERE status='ACTIVE' AND ends_at IS NOT NULL AND ends_at<=$1`, now)
	return err
}
func (r *EventRepository) ScoreKill(ctx context.Context, eventID, killID, playerID, factionID int64, points float64, distance *float64, streak int) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO event_kills(event_id,kill_id,points) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, eventID, killID, points)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if playerID > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO event_scores(event_id,player_id,score,kills,best_distance,best_streak) VALUES($1,$2,$3,1,$4,$5) ON CONFLICT(event_id,player_id) DO UPDATE SET score=event_scores.score+EXCLUDED.score,kills=event_scores.kills+1,best_distance=GREATEST(event_scores.best_distance,EXCLUDED.best_distance),best_streak=GREATEST(event_scores.best_streak,EXCLUDED.best_streak),updated_at=NOW()`, eventID, playerID, points, distance, streak)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO event_scores(event_id,faction_id,score,kills,best_distance,best_streak) VALUES($1,$2,$3,1,$4,$5) ON CONFLICT(event_id,faction_id) DO UPDATE SET score=event_scores.score+EXCLUDED.score,kills=event_scores.kills+1,best_distance=GREATEST(event_scores.best_distance,EXCLUDED.best_distance),best_streak=GREATEST(event_scores.best_streak,EXCLUDED.best_streak),updated_at=NOW()`, eventID, factionID, points, distance, streak)
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
func (r *EventRepository) Leaderboard(ctx context.Context, eventID int64, limit int) ([]EventScore, error) {
	rows, err := r.pool.Query(ctx, `SELECT COALESCE(player_id,0),COALESCE(faction_id,0),score,kills,best_distance,COALESCE(best_streak,0) FROM event_scores WHERE event_id=$1 ORDER BY score DESC,kills DESC,id LIMIT $2`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventScore
	for rows.Next() {
		var s EventScore
		if err := rows.Scan(&s.PlayerID, &s.FactionID, &s.Score, &s.Kills, &s.BestDistance, &s.BestStreak); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (r *EventRepository) FinalizeEvent(ctx context.Context, eventID int64, result EventResult) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO event_results(event_id,winner_player_id,winner_faction_id,winning_score,finalized_at) VALUES($1,NULLIF($2,0),NULLIF($3,0),$4,$5) ON CONFLICT(event_id) DO NOTHING`, eventID, result.WinnerPlayerID, result.WinnerFactionID, result.WinningScore, result.FinalizedAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE competitive_events SET status='ENDED',updated_at=NOW() WHERE id=$1 AND status<>'CANCELLED'`, eventID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ = errors.Is
var _ = fmt.Sprintf
var _ = pgx.ErrNoRows
