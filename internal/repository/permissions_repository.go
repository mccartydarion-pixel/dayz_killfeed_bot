package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RolePermission is one Discord role -> Champion permission level mapping, scoped to a single
// installation (Client Admin Control Plane Phase 1, docs/CLIENT_ADMIN.md).
type RolePermission struct {
	ID              int64
	InstallationID  int64
	DiscordRoleID   string
	Level           string
	CreatedByUserID *int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type PermissionsRepository struct{ pool *pgxpool.Pool }

func NewPermissionsRepository(pool *pgxpool.Pool) *PermissionsRepository {
	return &PermissionsRepository{pool: pool}
}

const rolePermissionCols = "id,installation_id,discord_role_id,permission_level,created_by_user_id,created_at,updated_at"

func scanRolePermission(row pgx.Row) (RolePermission, error) {
	var p RolePermission
	err := row.Scan(&p.ID, &p.InstallationID, &p.DiscordRoleID, &p.Level, &p.CreatedByUserID, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// List returns every role->level mapping configured for an installation, newest first.
func (r *PermissionsRepository) List(ctx context.Context, installationID int64) ([]RolePermission, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+rolePermissionCols+` FROM installation_role_permissions WHERE installation_id=$1 ORDER BY created_at DESC`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RolePermission
	for rows.Next() {
		p, err := scanRolePermission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Set creates or updates the mapping for one Discord role (upsert on the (installation_id,
// discord_role_id) uniqueness). The caller must have already checked permissions.CanGrant(actor,
// target) before calling this - this method has no notion of "actor" and enforces nothing itself.
func (r *PermissionsRepository) Set(ctx context.Context, installationID int64, discordRoleID, level string, createdByUserID int64) (RolePermission, error) {
	row := r.pool.QueryRow(ctx, `
INSERT INTO installation_role_permissions(installation_id,discord_role_id,permission_level,created_by_user_id)
VALUES($1,$2,$3,$4)
ON CONFLICT(installation_id,discord_role_id) DO UPDATE SET permission_level=$3,updated_at=NOW()
RETURNING `+rolePermissionCols, installationID, discordRoleID, level, createdByUserID)
	return scanRolePermission(row)
}

// Get returns one mapping by id, scoped to installationID so a caller can't reach another
// installation's row by guessing an id.
func (r *PermissionsRepository) Get(ctx context.Context, installationID, mappingID int64) (*RolePermission, error) {
	p, err := scanRolePermission(r.pool.QueryRow(ctx, `SELECT `+rolePermissionCols+` FROM installation_role_permissions WHERE installation_id=$1 AND id=$2`, installationID, mappingID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Delete removes one mapping, scoped to installationID.
func (r *PermissionsRepository) Delete(ctx context.Context, installationID, mappingID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM installation_role_permissions WHERE installation_id=$1 AND id=$2`, installationID, mappingID)
	return err
}

// LevelsForRoles returns the distinct permission levels mapped to any of discordRoleIDs within
// installationID - the core lookup behind resolving an actor's effective level from their current
// Discord guild roles.
func (r *PermissionsRepository) LevelsForRoles(ctx context.Context, installationID int64, discordRoleIDs []string) ([]string, error) {
	if len(discordRoleIDs) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT permission_level FROM installation_role_permissions WHERE installation_id=$1 AND discord_role_id = ANY($2)`, installationID, discordRoleIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var level string
		if err := rows.Scan(&level); err != nil {
			return nil, err
		}
		out = append(out, level)
	}
	return out, rows.Err()
}
