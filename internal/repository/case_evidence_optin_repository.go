package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CaseEvidenceOptinRepository stores server owners' own choice to collect
// C.A.S.E. evidence. It is only one input: the platform's self-serve setting
// and the Owner Hub flag decide whether it counts.
type CaseEvidenceOptinRepository struct{ pool *pgxpool.Pool }

func NewCaseEvidenceOptinRepository(pool *pgxpool.Pool) *CaseEvidenceOptinRepository {
	return &CaseEvidenceOptinRepository{pool: pool}
}

var ErrInvalidCaseEvidenceOptin = errors.New("invalid C.A.S.E. evidence choice")

type CaseEvidenceOptin struct {
	Enabled   bool       `json:"enabled"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

func (r *CaseEvidenceOptinRepository) ready() bool { return r != nil && r.pool != nil }

// Get returns the owner's choice for one installation. No row means off.
func (r *CaseEvidenceOptinRepository) Get(ctx context.Context, installationID int64) (CaseEvidenceOptin, error) {
	var out CaseEvidenceOptin
	if !r.ready() || installationID <= 0 {
		return out, ErrInvalidCaseEvidenceOptin
	}
	var at time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,updated_at FROM case_evidence_optins WHERE installation_id=$1`, installationID).Scan(&out.Enabled, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.UpdatedAt = &at
	return out, nil
}

// Set stores the owner's choice; the foreign key rejects a mismatched server.
func (r *CaseEvidenceOptinRepository) Set(ctx context.Context, installationID, serverID int64, enabled bool, actorUserID *int64) (CaseEvidenceOptin, error) {
	if !r.ready() || installationID <= 0 || serverID <= 0 {
		return CaseEvidenceOptin{}, ErrInvalidCaseEvidenceOptin
	}
	out := CaseEvidenceOptin{}
	var at time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO case_evidence_optins(installation_id,server_id,enabled,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,NOW())
 ON CONFLICT (installation_id) DO UPDATE SET server_id=EXCLUDED.server_id,enabled=EXCLUDED.enabled,
  updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 RETURNING enabled,updated_at`, installationID, serverID, enabled, actorUserID).Scan(&out.Enabled, &at)
	if err != nil {
		return CaseEvidenceOptin{}, err
	}
	out.UpdatedAt = &at
	return out, nil
}

// EnabledServerIDs lists game servers whose owners turned evidence on.
func (r *CaseEvidenceOptinRepository) EnabledServerIDs(ctx context.Context) ([]int64, error) {
	if !r.ready() {
		return nil, ErrInvalidCaseEvidenceOptin
	}
	rows, err := r.pool.Query(ctx, `SELECT server_id FROM case_evidence_optins WHERE enabled ORDER BY server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
