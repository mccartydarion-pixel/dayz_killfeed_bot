package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	BountyActive    = "ACTIVE"
	BountyClaimed   = "CLAIMED"
	BountyExpired   = "EXPIRED"
	BountyCancelled = "CANCELLED"
	BountyAdmin     = "ADMIN"
	BountyAutomatic = "AUTOMATIC"
)

type Bounty struct {
	ID, GuildID, SeasonID, TargetPlayerID, RewardPoints int64
	CreatedByType, Status, Reason                       string
	StartsAt, ExpiresAt, ClaimedAt                      *time.Time
	ClaimedByPlayerID, ClaimedKillID                    *int64
}
type BountyRepository struct{ pool *pgxpool.Pool }

func NewBountyRepository(pool *pgxpool.Pool) *BountyRepository { return &BountyRepository{pool: pool} }
func (r *BountyRepository) Create(ctx context.Context, b Bounty, creator string) (*Bounty, error) {
	var out Bounty
	err := r.pool.QueryRow(ctx, `INSERT INTO bounties(guild_id,season_id,target_player_id,created_by_type,created_by_discord_user_id,status,reward_points,reason,starts_at,expires_at) VALUES($1,NULLIF($2,0),$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id,guild_id,COALESCE(season_id,0),target_player_id,reward_points,created_by_type,status,COALESCE(reason,''),starts_at,expires_at,claimed_by_player_id,claimed_kill_id,claimed_at`, b.GuildID, b.SeasonID, b.TargetPlayerID, b.CreatedByType, creator, BountyActive, b.RewardPoints, b.Reason, b.StartsAt, b.ExpiresAt).Scan(&out.ID, &out.GuildID, &out.SeasonID, &out.TargetPlayerID, &out.RewardPoints, &out.CreatedByType, &out.Status, &out.Reason, &out.StartsAt, &out.ExpiresAt, &out.ClaimedByPlayerID, &out.ClaimedKillID, &out.ClaimedAt)
	return &out, err
}
func (r *BountyRepository) GetActive(ctx context.Context, guildID, targetID int64) (*Bounty, error) {
	var b Bounty
	err := r.pool.QueryRow(ctx, `SELECT id,guild_id,COALESCE(season_id,0),target_player_id,reward_points,created_by_type,status,COALESCE(reason,''),starts_at,expires_at,claimed_by_player_id,claimed_kill_id,claimed_at FROM bounties WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE' AND starts_at<=NOW() AND (expires_at IS NULL OR expires_at>NOW())`, guildID, targetID).Scan(&b.ID, &b.GuildID, &b.SeasonID, &b.TargetPlayerID, &b.RewardPoints, &b.CreatedByType, &b.Status, &b.Reason, &b.StartsAt, &b.ExpiresAt, &b.ClaimedByPlayerID, &b.ClaimedKillID, &b.ClaimedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}
func (r *BountyRepository) Claim(ctx context.Context, guildID, bountyID, killerID, killID int64, at time.Time) (*Bounty, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var b Bounty
	err = tx.QueryRow(ctx, `SELECT id,guild_id,COALESCE(season_id,0),target_player_id,reward_points,created_by_type,status,COALESCE(reason,''),starts_at,expires_at FROM bounties WHERE guild_id=$1 AND id=$2 AND status='ACTIVE' FOR UPDATE`, guildID, bountyID).Scan(&b.ID, &b.GuildID, &b.SeasonID, &b.TargetPlayerID, &b.RewardPoints, &b.CreatedByType, &b.Status, &b.Reason, &b.StartsAt, &b.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if killerID == b.TargetPlayerID || b.StartsAt == nil || at.Before(*b.StartsAt) || (b.ExpiresAt != nil && !at.Before(*b.ExpiresAt)) {
		return nil, context.Canceled
	}
	_, err = tx.Exec(ctx, `UPDATE bounties SET status='CLAIMED',claimed_by_player_id=$1,claimed_kill_id=$2,claimed_at=$3 WHERE id=$4`, killerID, killID, at, bountyID)
	if err != nil {
		return nil, err
	}
	b.ClaimedByPlayerID = &killerID
	b.ClaimedKillID = &killID
	b.ClaimedAt = &at
	b.Status = BountyClaimed
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &b, nil
}
func (r *BountyRepository) Expire(ctx context.Context, now time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE bounties SET status='EXPIRED' WHERE status='ACTIVE' AND expires_at IS NOT NULL AND expires_at<=$1`, now)
	return err
}
func (r *BountyRepository) Cancel(ctx context.Context, guildID, bountyID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE bounties SET status='CANCELLED' WHERE guild_id=$1 AND id=$2 AND status='ACTIVE'`, guildID, bountyID)
	return err
}
