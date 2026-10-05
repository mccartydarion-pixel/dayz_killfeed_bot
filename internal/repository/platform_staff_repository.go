package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PlatformStaffMember is one platform staff account: a Discord user who may read the Owner Hub
// and change nothing (docs/ADMIN_API.md "Roles").
type PlatformStaffMember struct {
	DiscordID string
	Note      string
	AddedBy   string
	AddedAt   time.Time
}

// ErrPlatformStaffNotFound is returned when removing a Discord id that is not on the staff list.
var ErrPlatformStaffNotFound = errors.New("platform staff member not found")

// ListPlatformStaff returns the staff list, oldest first.
func (r *PlatformOwnerRepository) ListPlatformStaff(ctx context.Context) ([]PlatformStaffMember, error) {
	rows, err := r.pool.Query(ctx, `SELECT discord_user_id, note, added_by, added_at FROM platform_staff ORDER BY added_at, discord_user_id`)
	if err != nil {
		return nil, fmt.Errorf("list platform staff: %w", err)
	}
	defer rows.Close()
	out := []PlatformStaffMember{}
	for rows.Next() {
		var m PlatformStaffMember
		if err := rows.Scan(&m.DiscordID, &m.Note, &m.AddedBy, &m.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetPlatformStaff returns one staff member, or nil when the id is not on the list.
func (r *PlatformOwnerRepository) GetPlatformStaff(ctx context.Context, discordID string) (*PlatformStaffMember, error) {
	var m PlatformStaffMember
	err := r.pool.QueryRow(ctx, `SELECT discord_user_id, note, added_by, added_at FROM platform_staff WHERE discord_user_id=$1`, discordID).
		Scan(&m.DiscordID, &m.Note, &m.AddedBy, &m.AddedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get platform staff: %w", err)
	}
	return &m, nil
}

// UpsertPlatformStaff adds discordID to the staff list. Someone already on it keeps who added
// them and when; only the note changes. before is the row as it was (nil when newly added).
func (r *PlatformOwnerRepository) UpsertPlatformStaff(ctx context.Context, discordID, note, addedBy string) (before *PlatformStaffMember, after PlatformStaffMember, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, after, fmt.Errorf("begin platform staff upsert: %w", err)
	}
	defer tx.Rollback(ctx)
	var b PlatformStaffMember
	err = tx.QueryRow(ctx, `SELECT discord_user_id, note, added_by, added_at FROM platform_staff WHERE discord_user_id=$1 FOR UPDATE`, discordID).
		Scan(&b.DiscordID, &b.Note, &b.AddedBy, &b.AddedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, after, fmt.Errorf("lock platform staff: %w", err)
	default:
		before = &b
	}
	err = tx.QueryRow(ctx, `
INSERT INTO platform_staff(discord_user_id, note, added_by) VALUES($1,$2,$3)
ON CONFLICT(discord_user_id) DO UPDATE SET note=EXCLUDED.note
RETURNING discord_user_id, note, added_by, added_at`, discordID, note, addedBy).
		Scan(&after.DiscordID, &after.Note, &after.AddedBy, &after.AddedAt)
	if err != nil {
		return nil, after, fmt.Errorf("upsert platform staff: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, after, fmt.Errorf("commit platform staff upsert: %w", err)
	}
	return before, after, nil
}

// RemovePlatformStaff deletes discordID from the staff list and returns the removed row;
// ErrPlatformStaffNotFound when it was not on it.
func (r *PlatformOwnerRepository) RemovePlatformStaff(ctx context.Context, discordID string) (PlatformStaffMember, error) {
	var m PlatformStaffMember
	err := r.pool.QueryRow(ctx, `DELETE FROM platform_staff WHERE discord_user_id=$1 RETURNING discord_user_id, note, added_by, added_at`, discordID).
		Scan(&m.DiscordID, &m.Note, &m.AddedBy, &m.AddedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrPlatformStaffNotFound
	}
	if err != nil {
		return m, fmt.Errorf("remove platform staff: %w", err)
	}
	return m, nil
}

// PlatformOwnerScope returns every organization whose OWNER (organizations.owner_user_id) has
// one of ownerDiscordIDs, and every installation of those organizations (owneraccess.Store).
// Being an ADMIN or MEMBER of somebody else's organization never puts it in the result.
func (r *PlatformOwnerRepository) PlatformOwnerScope(ctx context.Context, ownerDiscordIDs []string) (organizationIDs, installationIDs []int64, err error) {
	if len(ownerDiscordIDs) == 0 {
		return nil, nil, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT o.id, i.id
FROM organizations o
JOIN app_users u ON u.id = o.owner_user_id
LEFT JOIN installations i ON i.organization_id = o.id
WHERE u.discord_user_id = ANY($1)
ORDER BY o.id, i.id`, ownerDiscordIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("platform owner scope: %w", err)
	}
	defer rows.Close()
	var lastOrg int64
	for rows.Next() {
		var org int64
		var inst *int64
		if err := rows.Scan(&org, &inst); err != nil {
			return nil, nil, err
		}
		if org != lastOrg {
			organizationIDs = append(organizationIDs, org)
			lastOrg = org
		}
		if inst != nil {
			installationIDs = append(installationIDs, *inst)
		}
	}
	return organizationIDs, installationIDs, rows.Err()
}
