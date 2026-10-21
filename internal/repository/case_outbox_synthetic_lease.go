package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SyntheticOutboxClaim is an inert fixture lease. It is not permission to
// contact Discord and has no runtime caller.
type SyntheticOutboxClaim struct {
	FixtureOnly bool
	Scope       CaseReviewScope
	At          time.Time
	LeaseFor    time.Duration
}

type SyntheticOutboxLease struct {
	ID, CaseID, EventVersion int64
	DeliveryKey, LeaseToken  string
	LeaseUntil               time.Time
	Attempts                 int
}

// SyntheticResolvedLeaseSuppression is only a fixture cleanup request. Its
// token is a lease identity, not proof of any Discord delivery or permission.
type SyntheticResolvedLeaseSuppression struct {
	FixtureOnly bool
	Scope       CaseReviewScope
	OutboxID    int64
	LeaseToken  string
	At          time.Time
}

type CASEOutboxLeaseRepository struct{ pool *pgxpool.Pool }

func NewCASEOutboxLeaseRepository(pool *pgxpool.Pool) *CASEOutboxLeaseRepository {
	return &CASEOutboxLeaseRepository{pool: pool}
}

// ClaimDueSynthetic atomically leases at most one due fixture under exact
// guild/server/installation scope. A REVIEWED status alone cannot qualify:
// linked evidence and an audited PENDING_REVIEW -> REVIEWED transition are
// also required. SKIP LOCKED prevents two concurrent fixture claimants from
// taking the same row. No sender, route or receipt path calls this method.
func (r *CASEOutboxLeaseRepository) ClaimDueSynthetic(ctx context.Context, in SyntheticOutboxClaim) (SyntheticOutboxLease, bool, error) {
	if !in.FixtureOnly {
		return SyntheticOutboxLease{}, false, ErrCASEReviewFixtureDisabled
	}
	if r == nil || r.pool == nil {
		return SyntheticOutboxLease{}, false, errors.New("C.A.S.E. outbox database unavailable")
	}
	if in.Scope.GuildID <= 0 || in.Scope.ServerID <= 0 || in.Scope.InstallationID <= 0 ||
		in.At.IsZero() || in.LeaseFor < time.Second || in.LeaseFor > time.Minute {
		return SyntheticOutboxLease{}, false, errors.New("invalid synthetic outbox claim")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return SyntheticOutboxLease{}, false, errors.New("lease token unavailable")
	}
	token := hex.EncodeToString(tokenBytes)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SyntheticOutboxLease{}, false, err
	}
	defer tx.Rollback(ctx)
	var item SyntheticOutboxLease
	err = tx.QueryRow(ctx, `
 SELECT o.id,o.case_id,o.event_version,o.delivery_key,o.attempts
 FROM case_staff_outbox o
 JOIN case_review_cases c ON c.guild_id=o.guild_id AND c.server_id=o.server_id
  AND c.installation_id=o.installation_id AND c.id=o.case_id
 WHERE o.guild_id=$1 AND o.server_id=$2 AND o.installation_id=$3
  AND o.status IN ('PENDING','RETRY_WAIT') AND o.next_attempt_at<=$4
  AND o.attempts<5 AND c.status='REVIEWED'
  AND EXISTS (
   SELECT 1 FROM case_review_audit a
   WHERE a.guild_id=c.guild_id AND a.server_id=c.server_id
    AND a.installation_id=c.installation_id AND a.case_id=c.id
    AND a.from_status='PENDING_REVIEW' AND a.to_status='REVIEWED'
    AND a.reason_code='EVIDENCE_REVIEWED'
  )
  AND EXISTS (
   SELECT 1 FROM case_review_evidence e
   WHERE e.guild_id=c.guild_id AND e.server_id=c.server_id
    AND e.installation_id=c.installation_id AND e.case_id=c.id
  )
 ORDER BY o.next_attempt_at,o.id LIMIT 1 FOR UPDATE OF o,c SKIP LOCKED`,
		in.Scope.GuildID, in.Scope.ServerID, in.Scope.InstallationID, in.At.UTC()).
		Scan(&item.ID, &item.CaseID, &item.EventVersion, &item.DeliveryKey, &item.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyntheticOutboxLease{}, false, nil
	}
	if err != nil {
		return SyntheticOutboxLease{}, false, err
	}
	until := in.At.UTC().Add(in.LeaseFor)
	tag, err := tx.Exec(ctx, `UPDATE case_staff_outbox
 SET status='LEASED',attempts=attempts+1,lease_token=$1,lease_until=$2,updated_at=$3
 WHERE id=$4 AND guild_id=$5 AND server_id=$6 AND installation_id=$7
  AND status IN ('PENDING','RETRY_WAIT') AND attempts<5`,
		token, until, in.At.UTC(), item.ID, in.Scope.GuildID, in.Scope.ServerID, in.Scope.InstallationID)
	if err != nil {
		return SyntheticOutboxLease{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return SyntheticOutboxLease{}, false, errors.New("synthetic outbox claim changed")
	}
	if err = tx.Commit(ctx); err != nil {
		return SyntheticOutboxLease{}, false, err
	}
	item.LeaseToken = token
	item.LeaseUntil = until
	item.Attempts++
	return item, true, nil
}

// SuppressResolvedSynthetic clears an outstanding fixture lease only after
// the linked case has an audited REVIEWED -> RESOLVED staff transition. A
// stale lease token, foreign installation or merely asserted status cannot
// suppress another delivery. No runtime caller, route, or Discord sender uses
// this transaction.
func (r *CASEOutboxLeaseRepository) SuppressResolvedSynthetic(ctx context.Context, in SyntheticResolvedLeaseSuppression) (bool, error) {
	if !in.FixtureOnly {
		return false, ErrCASEReviewFixtureDisabled
	}
	if r == nil || r.pool == nil {
		return false, errors.New("C.A.S.E. outbox database unavailable")
	}
	if in.Scope.GuildID <= 0 || in.Scope.ServerID <= 0 || in.Scope.InstallationID <= 0 ||
		in.OutboxID <= 0 || !reviewValidKey(in.LeaseToken) || in.At.IsZero() {
		return false, errors.New("invalid synthetic lease suppression")
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var caseID int64
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `SELECT c.id,o.updated_at
 FROM case_staff_outbox o
 JOIN case_review_cases c ON c.guild_id=o.guild_id AND c.server_id=o.server_id
  AND c.installation_id=o.installation_id AND c.id=o.case_id
 WHERE o.id=$1 AND o.guild_id=$2 AND o.server_id=$3 AND o.installation_id=$4
  AND o.status='LEASED' AND o.lease_token=$5 AND c.status='RESOLVED'
  AND EXISTS (SELECT 1 FROM case_review_audit a
   WHERE a.guild_id=c.guild_id AND a.server_id=c.server_id
    AND a.installation_id=c.installation_id AND a.case_id=c.id
    AND a.from_status='REVIEWED' AND a.to_status='RESOLVED'
    AND a.reason_code='STAFF_CLOSED')
 FOR UPDATE OF c,o`, in.OutboxID, in.Scope.GuildID, in.Scope.ServerID,
		in.Scope.InstallationID, in.LeaseToken).Scan(&caseID, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if in.At.UTC().Before(updatedAt) {
		return false, errors.New("synthetic suppression precedes lease history")
	}
	tag, err := tx.Exec(ctx, `UPDATE case_staff_outbox
 SET status='SUPPRESSED',lease_token=NULL,lease_until=NULL,updated_at=$1
 WHERE id=$2 AND guild_id=$3 AND server_id=$4 AND installation_id=$5
  AND case_id=$6 AND status='LEASED' AND lease_token=$7`,
		in.At.UTC(), in.OutboxID, in.Scope.GuildID, in.Scope.ServerID,
		in.Scope.InstallationID, caseID, in.LeaseToken)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("synthetic lease suppression changed")
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
