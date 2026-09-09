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
	CurrentlyConnected          bool
}
type ActivityRepository struct{ pool *pgxpool.Pool }

func NewActivityRepository(pool *pgxpool.Pool) *ActivityRepository {
	return &ActivityRepository{pool: pool}
}
func (r *ActivityRepository) Connect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO player_server_activity(guild_id,server_id,player_id,first_seen_at,last_seen_at,current_session_started_at,currently_connected) VALUES($1,$2,$3,$4,$4,$4,TRUE) ON CONFLICT(guild_id,server_id,player_id) DO UPDATE SET last_seen_at=$4,current_session_started_at=CASE WHEN player_server_activity.currently_connected THEN player_server_activity.current_session_started_at ELSE $4 END,currently_connected=TRUE,updated_at=NOW()`, guildID, serverID, playerID, at)
	return err
}
func (r *ActivityRepository) Disconnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_server_activity SET total_observed_seconds=total_observed_seconds+CASE WHEN currently_connected AND current_session_started_at IS NOT NULL AND $4>=current_session_started_at THEN EXTRACT(EPOCH FROM ($4-current_session_started_at))::BIGINT ELSE 0 END,last_seen_at=$4,current_session_started_at=NULL,currently_connected=FALSE,updated_at=NOW() WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND currently_connected`, guildID, serverID, playerID, at)
	return err
}
func (r *ActivityRepository) Get(ctx context.Context, guildID, serverID, playerID int64) (*PlayerActivity, error) {
	var a PlayerActivity
	err := r.pool.QueryRow(ctx, `SELECT guild_id,server_id,player_id,first_seen_at,last_seen_at,total_observed_seconds,current_session_started_at,currently_connected FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`, guildID, serverID, playerID).Scan(&a.GuildID, &a.ServerID, &a.PlayerID, &a.FirstSeenAt, &a.LastSeenAt, &a.TotalObservedSeconds, &a.CurrentSessionStartedAt, &a.CurrentlyConnected)
	return &a, err
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
	if a.CurrentlyConnected && a.CurrentSessionStartedAt != nil && at.After(*a.CurrentSessionStartedAt) {
		total += at.Sub(*a.CurrentSessionStartedAt)
	}
	return total
}
