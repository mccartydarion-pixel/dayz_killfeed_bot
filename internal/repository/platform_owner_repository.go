package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlatformAuditEntry is one platform-owner action (docs/ADMIN_API.md "Owner controls"). Unlike
// AuditEntry it is not scoped to an organization: the owner acts across tenants, so every entry
// names its target explicitly and OrganizationID is only set when the target belongs to one.
// BeforeState/AfterState are small sanitized snapshots; never a secret.
type PlatformAuditEntry struct {
	ID             int64
	ActorDiscordID string
	Action         string
	TargetType     string // "organization" | "installation" | "user" | "platform"
	TargetID       *int64
	OrganizationID *int64
	Reason         string
	BeforeState    json.RawMessage
	AfterState     json.RawMessage
	Result         string
	CreatedAt      time.Time
}

// PlatformAuditFilter narrows ListAudit. Zero values mean "any".
type PlatformAuditFilter struct {
	OrganizationID int64
	TargetType     string
	TargetID       int64
	Action         string
	BeforeID       int64
	Limit          int
}

// ErrOwnerStripeManaged is returned by the subscription mutations when the organization has a
// live Stripe subscription: those are managed through Stripe (cancel / reconcile), never by
// overwriting the row the webhooks own.
var ErrOwnerStripeManaged = errors.New("subscription is managed by stripe")

// ErrOwnerNoChange is returned when the requested transition is already the current state
// (suspending a suspended installation, banning a banned user).
var ErrOwnerNoChange = errors.New("already in the requested state")

// PlatformOwnerRepository is the platform owner's write model: every method here is reachable
// only through /api/admin (requirePlatformAdmin) and is recorded in platform_audit_log by the
// caller.
type PlatformOwnerRepository struct{ pool *pgxpool.Pool }

func NewPlatformOwnerRepository(pool *pgxpool.Pool) *PlatformOwnerRepository {
	return &PlatformOwnerRepository{pool: pool}
}

// --- audit ----------------------------------------------------------------------------------------

func (r *PlatformOwnerRepository) RecordAudit(ctx context.Context, e PlatformAuditEntry) error {
	if e.Result == "" {
		e.Result = "OK"
	}
	_, err := r.pool.Exec(ctx, `
INSERT INTO platform_audit_log(actor_discord_id,action,target_type,target_id,organization_id,reason,before_state,after_state,result)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ActorDiscordID, e.Action, e.TargetType, e.TargetID, e.OrganizationID, e.Reason, e.BeforeState, e.AfterState, e.Result)
	return err
}

// ListAudit returns owner actions newest first, cursor-paginated on id.
func (r *PlatformOwnerRepository) ListAudit(ctx context.Context, f PlatformAuditFilter) ([]PlatformAuditEntry, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if f.OrganizationID > 0 {
		where = append(where, "organization_id="+arg(f.OrganizationID))
	}
	if f.TargetType != "" {
		where = append(where, "target_type="+arg(f.TargetType))
	}
	if f.TargetID > 0 {
		where = append(where, "target_id="+arg(f.TargetID))
	}
	if f.Action != "" {
		where = append(where, "action="+arg(f.Action))
	}
	if f.BeforeID > 0 {
		where = append(where, "id<"+arg(f.BeforeID))
	}
	q := `SELECT id,actor_discord_id,action,target_type,target_id,organization_id,reason,before_state,after_state,result,created_at FROM platform_audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT " + arg(f.Limit)
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlatformAuditEntry{}
	for rows.Next() {
		var e PlatformAuditEntry
		if err := rows.Scan(&e.ID, &e.ActorDiscordID, &e.Action, &e.TargetType, &e.TargetID, &e.OrganizationID, &e.Reason, &e.BeforeState, &e.AfterState, &e.Result, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- subscriptions --------------------------------------------------------------------------------

// SetTrial puts the organization on (or extends) a trial ending at until. Refused for a
// Stripe-managed subscription. Creates the row when the organization has none yet.
func (r *PlatformOwnerRepository) SetTrial(ctx context.Context, organizationID int64, until time.Time, reason string) (*Subscription, error) {
	const q = `
INSERT INTO subscriptions(organization_id, plan, status, trial_ends_at, owner_grant_reason)
VALUES($1, $2, $3, $4, $5)
ON CONFLICT(organization_id) DO UPDATE SET
  plan=$2, status=$3, trial_ends_at=$4, owner_grant_until=NULL, owner_grant_reason=$5,
  cancel_at_period_end=FALSE, updated_at=NOW()
WHERE COALESCE(subscriptions.provider_subscription_id,'')=''
RETURNING ` + subscriptionCols
	out, err := scanSubscription(r.pool.QueryRow(ctx, q, organizationID, SubscriptionTrial, SubscriptionTrial, until.UTC(), emptyToNil(reason)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerStripeManaged
	}
	if err != nil {
		return nil, fmt.Errorf("owner set trial: %w", err)
	}
	return &out, nil
}

// GrantPlan activates plan for the organization without Stripe, until `until` (nil = until
// revoked). Refused for a Stripe-managed subscription.
func (r *PlatformOwnerRepository) GrantPlan(ctx context.Context, organizationID int64, plan string, until *time.Time, reason string) (*Subscription, error) {
	const q = `
INSERT INTO subscriptions(organization_id, plan, status, owner_grant_until, owner_grant_reason)
VALUES($1, $2, $3, $4, $5)
ON CONFLICT(organization_id) DO UPDATE SET
  plan=$2, status=$3, trial_ends_at=NULL, owner_grant_until=$4, owner_grant_reason=$5,
  cancel_at_period_end=FALSE, canceled_at=NULL, updated_at=NOW()
WHERE COALESCE(subscriptions.provider_subscription_id,'')=''
RETURNING ` + subscriptionCols
	out, err := scanSubscription(r.pool.QueryRow(ctx, q, organizationID, plan, SubscriptionActive, until, emptyToNil(reason)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerStripeManaged
	}
	if err != nil {
		return nil, fmt.Errorf("owner grant plan: %w", err)
	}
	return &out, nil
}

// RevokeAccess ends an owner grant or trial: the organization becomes INACTIVE / NONE and must
// pay before service resumes. Refused for a Stripe-managed subscription.
func (r *PlatformOwnerRepository) RevokeAccess(ctx context.Context, organizationID int64, reason string) (*Subscription, error) {
	const q = `
UPDATE subscriptions SET plan=$2, status=$3, owner_grant_until=NULL, owner_grant_reason=$4, updated_at=NOW()
WHERE organization_id=$1 AND COALESCE(provider_subscription_id,'')=''
RETURNING ` + subscriptionCols
	out, err := scanSubscription(r.pool.QueryRow(ctx, q, organizationID, PlanNone, SubscriptionInactive, emptyToNil(reason)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerStripeManaged
	}
	if err != nil {
		return nil, fmt.Errorf("owner revoke access: %w", err)
	}
	return &out, nil
}

// --- installations --------------------------------------------------------------------------------

// InstallationSuspension is the suspension state of one installation.
type InstallationSuspension struct {
	InstallationID      int64
	OrganizationID      int64
	GameServerID        *int64
	Status              string
	SuspendedAt         *time.Time
	SuspendedReason     string
	StatusBeforeSuspend string
}

const installationSuspensionCols = `id, organization_id, game_server_id, status, suspended_at, COALESCE(suspended_reason,''), COALESCE(status_before_suspend,'')`

func scanInstallationSuspension(row pgx.Row) (InstallationSuspension, error) {
	var s InstallationSuspension
	err := row.Scan(&s.InstallationID, &s.OrganizationID, &s.GameServerID, &s.Status, &s.SuspendedAt, &s.SuspendedReason, &s.StatusBeforeSuspend)
	return s, err
}

// GetInstallation reads the suspension view of any installation (cross-tenant; owner only).
func (r *PlatformOwnerRepository) GetInstallation(ctx context.Context, installationID int64) (*InstallationSuspension, error) {
	out, err := scanInstallationSuspension(r.pool.QueryRow(ctx, `SELECT `+installationSuspensionCols+` FROM installations WHERE id=$1`, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("owner get installation: %w", err)
	}
	return &out, nil
}

// SuspendInstallation parks the installation in SUSPENDED, remembering the status it came from.
func (r *PlatformOwnerRepository) SuspendInstallation(ctx context.Context, installationID int64, reason string) (*InstallationSuspension, error) {
	const q = `
UPDATE installations SET status_before_suspend=status, status=$2, suspended_at=NOW(), suspended_reason=$3, updated_at=NOW()
WHERE id=$1 AND status<>$2
RETURNING ` + installationSuspensionCols
	out, err := scanInstallationSuspension(r.pool.QueryRow(ctx, q, installationID, InstallationSuspended, emptyToNil(reason)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerNoChange
	}
	if err != nil {
		return nil, fmt.Errorf("owner suspend installation: %w", err)
	}
	return &out, nil
}

// ReinstateInstallation restores the status the installation had before it was suspended
// (READY when unknown).
func (r *PlatformOwnerRepository) ReinstateInstallation(ctx context.Context, installationID int64) (*InstallationSuspension, error) {
	const q = `
UPDATE installations SET status=COALESCE(NULLIF(status_before_suspend,''), $3), status_before_suspend=NULL, suspended_at=NULL, suspended_reason=NULL, updated_at=NOW()
WHERE id=$1 AND status=$2
RETURNING ` + installationSuspensionCols
	out, err := scanInstallationSuspension(r.pool.QueryRow(ctx, q, installationID, InstallationSuspended, InstallationReady))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerNoChange
	}
	if err != nil {
		return nil, fmt.Errorf("owner reinstate installation: %w", err)
	}
	return &out, nil
}

// --- users ----------------------------------------------------------------------------------------

const ownerUserCols = `id, discord_user_id, discord_username, COALESCE(discord_global_name,''), COALESCE(avatar,''), created_at, updated_at, last_login_at, banned_at, COALESCE(ban_reason,'')`

func scanOwnerUser(row pgx.Row) (AppUser, error) {
	var u AppUser
	err := row.Scan(&u.ID, &u.DiscordUserID, &u.DiscordUsername, &u.DiscordGlobalName, &u.Avatar, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &u.BannedAt, &u.BanReason)
	return u, err
}

// GetUser reads any website user by id (owner only).
func (r *PlatformOwnerRepository) GetUser(ctx context.Context, userID int64) (*AppUser, error) {
	out, err := scanOwnerUser(r.pool.QueryRow(ctx, `SELECT `+ownerUserCols+` FROM app_users WHERE id=$1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("owner get user: %w", err)
	}
	return &out, nil
}

// BanUser marks the account banned; resolveActingUser refuses it from then on.
func (r *PlatformOwnerRepository) BanUser(ctx context.Context, userID int64, reason string) (*AppUser, error) {
	out, err := scanOwnerUser(r.pool.QueryRow(ctx, `UPDATE app_users SET banned_at=NOW(), ban_reason=$2, updated_at=NOW() WHERE id=$1 AND banned_at IS NULL RETURNING `+ownerUserCols, userID, emptyToNil(reason)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerNoChange
	}
	if err != nil {
		return nil, fmt.Errorf("owner ban user: %w", err)
	}
	return &out, nil
}

func (r *PlatformOwnerRepository) UnbanUser(ctx context.Context, userID int64) (*AppUser, error) {
	out, err := scanOwnerUser(r.pool.QueryRow(ctx, `UPDATE app_users SET banned_at=NULL, ban_reason=NULL, updated_at=NOW() WHERE id=$1 AND banned_at IS NOT NULL RETURNING `+ownerUserCols, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOwnerNoChange
	}
	if err != nil {
		return nil, fmt.Errorf("owner unban user: %w", err)
	}
	return &out, nil
}
