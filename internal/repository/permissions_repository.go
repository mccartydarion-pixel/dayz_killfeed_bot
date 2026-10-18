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

// InstallationRoleLevel is one installation of a Discord server with the Champion level a set
// of Discord roles maps to there (empty when none of them is mapped on it).
type InstallationRoleLevel struct {
	InstallationID int64
	GameServerID   int64
	Level          string
}

// InstallationLevelsForGuildRoles lists every installation whose Discord server is discordGuildID
// (the Discord snowflake) with the levels the given Discord roles map to on each: one row per
// installation and mapped level, and one row with an empty level for an installation none of the
// roles is mapped on. A Discord server can hold one installation per DayZ server, so the caller
// gates by the installation a command targets rather than by the Discord server alone.
func (r *PermissionsRepository) InstallationLevelsForGuildRoles(ctx context.Context, discordGuildID string, discordRoleIDs []string) ([]InstallationRoleLevel, error) {
	if discordGuildID == "" {
		return nil, nil
	}
	if discordRoleIDs == nil {
		discordRoleIDs = []string{}
	}
	rows, err := r.pool.Query(ctx, `SELECT i.id, COALESCE(i.game_server_id, 0), COALESCE(p.permission_level, '')
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
LEFT JOIN installation_role_permissions p ON p.installation_id = i.id AND p.discord_role_id = ANY($2)
WHERE g.discord_guild_id = $1`, discordGuildID, discordRoleIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InstallationRoleLevel
	for rows.Next() {
		var row InstallationRoleLevel
		if err := rows.Scan(&row.InstallationID, &row.GameServerID, &row.Level); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
