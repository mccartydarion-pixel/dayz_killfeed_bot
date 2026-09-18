package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Organization member roles. Enforced at the application layer, matching
// this schema's existing convention for status-like text columns (e.g.
// GameServer.Status, GuildRecord) - no SQL CHECK constraint is used for
// these anywhere in the schema, so adding a role later needs no migration.
const (
	RoleOwner  = "OWNER"
	RoleAdmin  = "ADMIN"
	RoleMember = "MEMBER"
)

// Organization is the customer/account tenant boundary - the primary scope
// every SaaS repository method below requires (section 15).
type Organization struct {
	ID                   int64
	Name, Slug           string
	OwnerUserID          int64
	CreatedAt, UpdatedAt time.Time
}

type OrganizationMember struct {
	ID             int64
	OrganizationID int64
	UserID         int64
	Role           string
	CreatedAt      time.Time
}

type OrganizationRepository struct{ pool *pgxpool.Pool }

func NewOrganizationRepository(pool *pgxpool.Pool) *OrganizationRepository {
	return &OrganizationRepository{pool: pool}
}

// Create inserts a new organization and its owner membership atomically - an
// organization must never exist without its OWNER membership row, or vice
// versa (section 17: half-created state is not acceptable here).
func (r *OrganizationRepository) Create(ctx context.Context, name, slug string, ownerUserID int64) (*Organization, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create organization: %w", err)
	}
	defer tx.Rollback(ctx)

	var out Organization
	err = tx.QueryRow(ctx, `INSERT INTO organizations(name, slug, owner_user_id) VALUES($1,$2,$3) RETURNING id, name, slug, owner_user_id, created_at, updated_at`,
		name, slug, ownerUserID,
	).Scan(&out.ID, &out.Name, &out.Slug, &out.OwnerUserID, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, fmt.Errorf("create organization: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,$3)`, out.ID, ownerUserID, RoleOwner); err != nil {
		return nil, fmt.Errorf("create owner membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create organization: %w", err)
	}
	return &out, nil
}

// GetByID returns one organization by ID, or nil if it doesn't exist. This
// is intentionally NOT tenant-scoped by itself (there is no "tenant" above
// an organization) - callers must authorize the caller against
// organizationID (e.g. via VerifyMembership) before calling this, exactly
// like every GetScoped elsewhere in this package does internally.
func (r *OrganizationRepository) GetByID(ctx context.Context, organizationID int64) (*Organization, error) {
	const q = `SELECT id, name, slug, owner_user_id, created_at, updated_at FROM organizations WHERE id=$1`
	var o Organization
	err := r.pool.QueryRow(ctx, q, organizationID).Scan(&o.ID, &o.Name, &o.Slug, &o.OwnerUserID, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get organization: %w", err)
	}
	return &o, nil
}

// ListForUser returns every organization userID is a member of.
func (r *OrganizationRepository) ListForUser(ctx context.Context, userID int64) ([]Organization, error) {
	rows, err := r.pool.Query(ctx, `
SELECT o.id, o.name, o.slug, o.owner_user_id, o.created_at, o.updated_at
FROM organizations o
JOIN organization_members m ON m.organization_id = o.id
WHERE m.user_id = $1
ORDER BY o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list organizations for user: %w", err)
	}
	defer rows.Close()
	var out []Organization
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &o.OwnerUserID, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// VerifyMembership reports whether userID belongs to organizationID, and
// their role if so. This is the tenant-isolation check every SaaS handler
// must call before trusting an organization-scoped request (section 15) -
// Customer A must never reach Customer B's data through ID guessing.
func (r *OrganizationRepository) VerifyMembership(ctx context.Context, organizationID, userID int64) (role string, ok bool, err error) {
	err = r.pool.QueryRow(ctx, `SELECT role FROM organization_members WHERE organization_id=$1 AND user_id=$2`, organizationID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("verify membership: %w", err)
	}
	return role, true, nil
}
