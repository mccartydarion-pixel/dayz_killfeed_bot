package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StaffActivityRepository reads the admin audit log for the staff activity view: who did what,
// when. It never returns before/after snapshots; the full audit log stays the place for those.
type StaffActivityRepository struct{ pool *pgxpool.Pool }

func NewStaffActivityRepository(pool *pgxpool.Pool) *StaffActivityRepository {
	return &StaffActivityRepository{pool: pool}
}

// StaffActionCount is how often one staff member took one action in the window.
type StaffActionCount struct {
	ActorDiscordID string
	ActorName      string
	Action         string
	Count, Failed  int64
	LastAt         time.Time
}

func (r *StaffActivityRepository) ActionCounts(ctx context.Context, organizationID, installationID int64, since time.Time) ([]StaffActionCount, error) {
	rows, err := r.pool.Query(ctx, `
SELECT COALESCE(l.actor_discord_id,''), COALESCE(MAX(COALESCE(u.discord_global_name,u.discord_username)),''), l.action,
       COUNT(*), COUNT(*) FILTER (WHERE l.result<>'success'), MAX(l.created_at)
FROM admin_audit_log l LEFT JOIN app_users u ON u.discord_user_id=l.actor_discord_id
WHERE l.organization_id=$1 AND l.installation_id=$2 AND l.created_at >= $3
GROUP BY l.actor_discord_id, l.action`, organizationID, installationID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaffActionCount{}
	for rows.Next() {
		var c StaffActionCount
		if err := rows.Scan(&c.ActorDiscordID, &c.ActorName, &c.Action, &c.Count, &c.Failed, &c.LastAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StaffDay is the number of staff actions on one UTC day.
type StaffDay struct {
	Day     string `json:"day"`
	Actions int64  `json:"actions"`
}

func (r *StaffActivityRepository) Daily(ctx context.Context, organizationID, installationID int64, since time.Time) ([]StaffDay, error) {
	rows, err := r.pool.Query(ctx, `SELECT to_char(date_trunc('day', created_at AT TIME ZONE 'UTC'),'YYYY-MM-DD'), COUNT(*)
FROM admin_audit_log WHERE organization_id=$1 AND installation_id=$2 AND created_at >= $3 GROUP BY 1 ORDER BY 1`, organizationID, installationID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaffDay{}
	for rows.Next() {
		var d StaffDay
		if err := rows.Scan(&d.Day, &d.Actions); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StaffActivityEntry is one action, without its before/after snapshots.
type StaffActivityEntry struct {
	ID             int64     `json:"id"`
	ActorDiscordID string    `json:"actorDiscordId"`
	ActorName      string    `json:"actorName"`
	Action         string    `json:"action"`
	Category       string    `json:"category"`
	Target         string    `json:"target,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Result         string    `json:"result"`
	At             time.Time `json:"at"`
}

// StaffActivityFilter narrows Entries. Include/Exclude are SQL LIKE patterns on the action.
type StaffActivityFilter struct {
	Since            time.Time
	ActorDiscordID   string
	Include, Exclude []string
	BeforeID         int64
	Limit            int
}

func (r *StaffActivityRepository) Entries(ctx context.Context, organizationID, installationID int64, f StaffActivityFilter) ([]StaffActivityEntry, error) {
	args := []any{organizationID, installationID, f.Since}
	q := `SELECT l.id, COALESCE(l.actor_discord_id,''), COALESCE(COALESCE(u.discord_global_name,u.discord_username),''), l.action,
       COALESCE(l.target,''), COALESCE(l.reason,''), l.result, l.created_at
FROM admin_audit_log l LEFT JOIN app_users u ON u.discord_user_id=l.actor_discord_id
WHERE l.organization_id=$1 AND l.installation_id=$2 AND l.created_at >= $3`
	add := func(cond string, v any) {
		args = append(args, v)
		q += " AND " + cond + "$" + strconv.Itoa(len(args))
	}
	if f.ActorDiscordID != "" {
		add("l.actor_discord_id=", f.ActorDiscordID)
	}
	if len(f.Include) > 0 {
		args = append(args, f.Include)
		q += " AND l.action LIKE ANY($" + strconv.Itoa(len(args)) + ")"
	}
	if len(f.Exclude) > 0 {
		args = append(args, f.Exclude)
		q += " AND NOT (l.action LIKE ANY($" + strconv.Itoa(len(args)) + "))"
	}
	if f.BeforeID > 0 {
		add("l.id<", f.BeforeID)
	}
	args = append(args, clampLimit(f.Limit, 50, 200))
	q += " ORDER BY l.id DESC LIMIT $" + strconv.Itoa(len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StaffActivityEntry{}
	for rows.Next() {
		var e StaffActivityEntry
		if err := rows.Scan(&e.ID, &e.ActorDiscordID, &e.ActorName, &e.Action, &e.Target, &e.Reason, &e.Result, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
