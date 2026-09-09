package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	AnnouncementPending   = "PENDING"
	AnnouncementClaimed   = "CLAIMED"
	AnnouncementAnnounced = "ANNOUNCED"
)

type AnnouncementRepository struct{ pool *pgxpool.Pool }

func NewAnnouncementRepository(pool *pgxpool.Pool) *AnnouncementRepository {
	return &AnnouncementRepository{pool: pool}
}

// Claim atomically acquires a publication lease. Expired leases become retryable.
func (r *AnnouncementRepository) Claim(ctx context.Context, kind string, objectID int64, now time.Time, lease time.Duration) (bool, error) {
	_, err := r.pool.Exec(ctx, `INSERT INTO completion_announcements(kind,object_id,status) VALUES($1,$2,'PENDING') ON CONFLICT(kind,object_id) DO NOTHING`, kind, objectID)
	if err != nil {
		return false, err
	}
	var claimed bool
	err = r.pool.QueryRow(ctx, `UPDATE completion_announcements SET status='CLAIMED',claimed_at=$3,updated_at=NOW() WHERE kind=$1 AND object_id=$2 AND (status='PENDING' OR (status='CLAIMED' AND claimed_at<$3-$4::interval)) RETURNING TRUE`, kind, objectID, now, fmt.Sprintf("%d seconds", int(lease.Seconds()))).Scan(&claimed)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return claimed, err
}
func (r *AnnouncementRepository) MarkAnnounced(ctx context.Context, kind string, objectID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE completion_announcements SET status='ANNOUNCED',announced_at=$3,updated_at=NOW() WHERE kind=$1 AND object_id=$2 AND status='CLAIMED'`, kind, objectID, at)
	return err
}
func (r *AnnouncementRepository) Release(ctx context.Context, kind string, objectID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE completion_announcements SET status='PENDING',claimed_at=NULL,updated_at=NOW() WHERE kind=$1 AND object_id=$2 AND status='CLAIMED'`, kind, objectID)
	return err
}
func (r *AnnouncementRepository) IsAnnounced(ctx context.Context, kind string, objectID int64) (bool, error) {
	var yes bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM completion_announcements WHERE kind=$1 AND object_id=$2 AND status='ANNOUNCED')`, kind, objectID).Scan(&yes)
	return yes, err
}
