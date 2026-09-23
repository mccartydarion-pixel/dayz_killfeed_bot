package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditEntry is one tenant-admin action record (Client Admin Control Plane Phase 1,
// docs/CLIENT_ADMIN.md "Admin audit log"). BeforeState/AfterState are small, already-sanitized
// JSON snapshots - callers must never marshal a secret (a Nitrado token, credential ciphertext,
// etc.) into either field.
type AuditEntry struct {
	ID             int64
	OrganizationID int64
	InstallationID *int64
	ActorUserID    *int64
	ActorDiscordID string
	Action         string
	Target         string
	Reason         string
	BeforeState    json.RawMessage
	AfterState     json.RawMessage
	Result         string
	CreatedAt      time.Time
}

type AuditRepository struct{ pool *pgxpool.Pool }

func NewAuditRepository(pool *pgxpool.Pool) *AuditRepository { return &AuditRepository{pool: pool} }

// Record persists one audit entry. A failure here is logged by the caller but must never block
// the underlying admin action from completing - the action already happened; losing its audit
// record is a real problem to alert on, but re-attempting or rolling back the action itself would
// be worse.
func (r *AuditRepository) Record(ctx context.Context, e AuditEntry) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO admin_audit_log(organization_id,installation_id,actor_user_id,actor_discord_id,action,target,reason,before_state,after_state,result)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.OrganizationID, e.InstallationID, e.ActorUserID, e.ActorDiscordID, e.Action, e.Target, e.Reason, e.BeforeState, e.AfterState, e.Result)
	return err
}

// List returns audit entries for an organization, optionally scoped to one installation, newest
// first, cursor-paginated on id (matching admin_api.go's existing cursor convention).
func (r *AuditRepository) List(ctx context.Context, organizationID int64, installationID *int64, beforeID int64, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id,organization_id,installation_id,actor_user_id,actor_discord_id,action,target,reason,before_state,after_state,result,created_at
FROM admin_audit_log WHERE organization_id=$1`
	args := []any{organizationID}
	if installationID != nil {
		args = append(args, *installationID)
		query += ` AND installation_id=$2`
	}
	if beforeID > 0 {
		args = append(args, beforeID)
		query += ` AND id<$` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.OrganizationID, &e.InstallationID, &e.ActorUserID, &e.ActorDiscordID, &e.Action, &e.Target, &e.Reason, &e.BeforeState, &e.AfterState, &e.Result, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
