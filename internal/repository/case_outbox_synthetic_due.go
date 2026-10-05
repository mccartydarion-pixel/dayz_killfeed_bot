package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SyntheticDueInspection is a bounded, read-only diagnostic, not delivery
// eligibility or permission to contact Discord. No runtime caller exists.
type SyntheticDueInspection struct {
	FixtureOnly bool
	Scope       CaseReviewScope
	At          time.Time
	Limit       int
}

type SyntheticDueEntry struct {
	ID           int64
	CaseID       int64
	EventVersion int64
	DeliveryKey  string
	Status       string
	Attempts     int
}

// CASEOutboxInspector cannot claim, acknowledge, send, or mutate anything.
type CASEOutboxInspector struct{ pool *pgxpool.Pool }

func NewCASEOutboxInspector(pool *pgxpool.Pool) *CASEOutboxInspector {
	return &CASEOutboxInspector{pool: pool}
}

// InspectDueSynthetic exposes only scoped opaque delivery identities. The
// SQL excludes terminal/leased items and requires a reviewed case with at
// least one exact linked evidence row. Those conditions are necessary, NOT
// sufficient for actual delivery: live source, detector, reviewer, opt-in,
// destination and recipient checks remain independently unimplemented.
func (r *CASEOutboxInspector) InspectDueSynthetic(ctx context.Context, in SyntheticDueInspection) ([]SyntheticDueEntry, error) {
	if !in.FixtureOnly {
		return nil, ErrCASEReviewFixtureDisabled
	}
	if r == nil || r.pool == nil {
		return nil, errors.New("C.A.S.E. outbox database unavailable")
	}
	if in.Scope.GuildID <= 0 || in.Scope.ServerID <= 0 || in.Scope.InstallationID <= 0 ||
		in.At.IsZero() || in.Limit < 1 || in.Limit > 50 {
		return nil, errors.New("invalid synthetic due inspection")
	}
	rows, err := r.pool.Query(ctx, `
 SELECT o.id,o.case_id,o.event_version,o.delivery_key,o.status,o.attempts
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
 ORDER BY o.next_attempt_at,o.id LIMIT $5`,
		in.Scope.GuildID, in.Scope.ServerID, in.Scope.InstallationID, in.At.UTC(), in.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SyntheticDueEntry, 0, in.Limit)
	for rows.Next() {
		var v SyntheticDueEntry
		if err = rows.Scan(&v.ID, &v.CaseID, &v.EventVersion, &v.DeliveryKey, &v.Status, &v.Attempts); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
