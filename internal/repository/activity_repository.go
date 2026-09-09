package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const LinkMinimumObservedPlaytime = 5 * time.Minute

type PlayerActivity struct {
	GuildID, ServerID, PlayerID int64
	FirstSeenAt, LastSeenAt     time.Time
	TotalObservedSeconds        int64
	CurrentSessionStartedAt     *time.Time
	LastObservedAt              *time.Time
	CurrentlyConnected          bool
}
type ActivityRepository struct{ pool *pgxpool.Pool }

func NewActivityRepository(pool *pgxpool.Pool) *ActivityRepository {
	return &ActivityRepository{pool: pool}
}
func (r *ActivityRepository) Connect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO player_server_activity(guild_id,server_id,player_id,first_seen_at,last_seen_at,current_session_started_at,last_observed_at,currently_connected) VALUES($1,$2,$3,$4,$4,$4,$4,TRUE) ON CONFLICT(guild_id,server_id,player_id) DO UPDATE SET last_seen_at=$4,current_session_started_at=CASE WHEN player_server_activity.currently_connected THEN player_server_activity.current_session_started_at ELSE $4 END,last_observed_at=CASE WHEN player_server_activity.currently_connected THEN player_server_activity.last_observed_at ELSE $4 END,currently_connected=TRUE,updated_at=NOW()`, guildID, serverID, playerID, at)
	return err
}
func (r *ActivityRepository) Disconnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_server_activity SET total_observed_seconds=total_observed_seconds+CASE WHEN currently_connected AND last_observed_at IS NOT NULL AND $4>=last_observed_at THEN EXTRACT(EPOCH FROM ($4-last_observed_at))::BIGINT ELSE 0 END,last_seen_at=$4,last_observed_at=NULL,current_session_started_at=NULL,currently_connected=FALSE,updated_at=NOW() WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND currently_connected`, guildID, serverID, playerID, at)
	return err
}
func (r *ActivityRepository) Get(ctx context.Context, guildID, serverID, playerID int64) (*PlayerActivity, error) {
	var a PlayerActivity
	err := r.pool.QueryRow(ctx, `SELECT guild_id,server_id,player_id,first_seen_at,last_seen_at,total_observed_seconds,current_session_started_at,last_observed_at,currently_connected FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`, guildID, serverID, playerID).Scan(&a.GuildID, &a.ServerID, &a.PlayerID, &a.FirstSeenAt, &a.LastSeenAt, &a.TotalObservedSeconds, &a.CurrentSessionStartedAt, &a.LastObservedAt, &a.CurrentlyConnected)
	return &a, err
}
func (r *ActivityRepository) Checkpoint(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_server_activity SET total_observed_seconds=total_observed_seconds+CASE WHEN currently_connected AND last_observed_at IS NOT NULL AND $4>=last_observed_at AND EXTRACT(EPOCH FROM ($4-last_observed_at)) BETWEEN 0 AND 300 THEN EXTRACT(EPOCH FROM ($4-last_observed_at))::BIGINT ELSE 0 END,last_observed_at=CASE WHEN currently_connected THEN $4 ELSE last_observed_at END,last_seen_at=CASE WHEN currently_connected THEN $4 ELSE last_seen_at END,updated_at=NOW() WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND currently_connected`, guildID, serverID, playerID, at)
	return err
}
func (r *ActivityRepository) CheckpointConnected(ctx context.Context, guildID, serverID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_server_activity SET total_observed_seconds=total_observed_seconds+CASE WHEN last_observed_at IS NOT NULL AND $3>=last_observed_at AND EXTRACT(EPOCH FROM ($3-last_observed_at)) BETWEEN 0 AND 300 THEN EXTRACT(EPOCH FROM ($3-last_observed_at))::BIGINT ELSE 0 END,last_observed_at=$3,last_seen_at=$3,updated_at=NOW() WHERE guild_id=$1 AND server_id=$2 AND currently_connected`, guildID, serverID, at)
	return err
}
func (r *ActivityRepository) GetObservedPlaytime(ctx context.Context, guildID, serverID, playerID int64, at time.Time) (time.Duration, error) {
	a, err := r.Get(ctx, guildID, serverID, playerID)
	if err != nil {
		return 0, err
	}
	return a.Effective(at), nil
}
func (a *PlayerActivity) Effective(at time.Time) time.Duration {
	if a == nil {
		return 0
	}
	total := time.Duration(a.TotalObservedSeconds) * time.Second
	if a.CurrentlyConnected && a.LastObservedAt != nil && at.After(*a.LastObservedAt) && at.Sub(*a.LastObservedAt) <= 5*time.Minute {
		total += at.Sub(*a.LastObservedAt)
	}
	return total
}
