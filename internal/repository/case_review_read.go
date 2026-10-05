package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CaseReviewReader is a read-only, tenant-scoped repository. It creates no
// findings and performs no mutations, Discord sends or detector evaluation.
// The future HTTP endpoint must authenticate the caller and authorize this
// exact installation and capability independently before invoking it.
type CaseReviewReader struct{ pool *pgxpool.Pool }

type CaseReviewScope struct {
	GuildID        int64
	InstallationID int64
	ServerID       int64
}

type CaseReviewSummary struct {
	ID              int64
	DetectorID      string
	DetectorVersion string
	Status          string
	EvidenceCount   int64
	AuditCount      int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func NewCaseReviewReader(pool *pgxpool.Pool) *CaseReviewReader {
	return &CaseReviewReader{pool: pool}
}

// List fetches at most 50 neutral case summaries by exact guild, server and
// installation scope. It deliberately omits player IDs, positions, raw ADM
// source addresses and any ineligible detector payloads. A missing or foreign
// scope returns no records rather than falling back to a guild-wide queue.
func (r *CaseReviewReader) List(ctx context.Context, scope CaseReviewScope, before *int64, limit int) ([]CaseReviewSummary, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("C.A.S.E. review database unavailable")
	}
	if scope.GuildID <= 0 || scope.ServerID <= 0 || scope.InstallationID <= 0 ||
		limit < 1 || limit > 50 {
		return nil, errors.New("invalid case review scope or limit")
	}
	if before != nil && *before <= 0 {
		return nil, errors.New("invalid case review cursor")
	}
	rows, err := r.pool.Query(ctx, `
 SELECT c.id,c.detector_id,c.detector_version,c.status,
 (SELECT COUNT(*) FROM case_review_evidence ev
  WHERE ev.guild_id=c.guild_id AND ev.server_id=c.server_id
   AND ev.installation_id=c.installation_id AND ev.case_id=c.id),
 (SELECT COUNT(*) FROM case_review_audit a
  WHERE a.guild_id=c.guild_id AND a.server_id=c.server_id
   AND a.installation_id=c.installation_id AND a.case_id=c.id),
 c.created_at,c.updated_at
 FROM case_review_cases c
 WHERE c.guild_id=$1 AND c.server_id=$2 AND c.installation_id=$3
  AND ($4::bigint IS NULL OR c.id<$4)
 ORDER BY c.id DESC LIMIT $5`,
		scope.GuildID, scope.ServerID, scope.InstallationID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CaseReviewSummary, 0, limit)
	for rows.Next() {
		var v CaseReviewSummary
		if err = rows.Scan(&v.ID, &v.DetectorID, &v.DetectorVersion, &v.Status,
			&v.EvidenceCount, &v.AuditCount, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
