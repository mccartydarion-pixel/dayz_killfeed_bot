package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlatformOpsRepository stores what the Owner Hub operations features write (docs/OWNER_OPS.md):
// the owner's automation switches, fleet incidents, customer broadcasts and the daily briefing
// guard. Cross-tenant by design; reached only through /api/admin and the owner-ops worker.
type PlatformOpsRepository struct{ pool *pgxpool.Pool }

func NewPlatformOpsRepository(pool *pgxpool.Pool) *PlatformOpsRepository {
	return &PlatformOpsRepository{pool: pool}
}

// --- settings ---------------------------------------------------------------------------------------

// OwnerOpsSettings are the owner's automation switches. Everything that acts on a customer's
// installation or sends a message is off until the owner turns it on.
type OwnerOpsSettings struct {
	// AlertsEnabled: DM the platform admins when an incident opens or resolves.
	AlertsEnabled bool `json:"alertsEnabled"`
	// SelfHealEnabled: let the monitor restart stalled or missing workers by itself.
	SelfHealEnabled bool `json:"selfHealEnabled"`
	// CustomerNoticesEnabled: DM an organization's owner when their setup needs them
	// (Nitrado or Discord access lost) and when it is fixed.
	CustomerNoticesEnabled bool `json:"customerNoticesEnabled"`
	// BriefingEnabled: DM the platform admins the daily briefing at BriefingHourUTC.
	BriefingEnabled bool `json:"briefingEnabled"`
	BriefingHourUTC int  `json:"briefingHourUtc"`
}

// DefaultOwnerOpsSettings is what a deployment that never saved any has: everything off.
func DefaultOwnerOpsSettings() OwnerOpsSettings { return OwnerOpsSettings{BriefingHourUTC: 13} }

// Validate bounds the one non-boolean setting.
func (s OwnerOpsSettings) Validate() error {
	if s.BriefingHourUTC < 0 || s.BriefingHourUTC > 23 {
		return errors.New("briefingHourUtc must be between 0 and 23")
	}
	return nil
}

const ownerOpsSettingsKey = "owner_ops"

// Settings returns the stored switches, or the defaults when none were saved.
func (r *PlatformOpsRepository) Settings(ctx context.Context) (OwnerOpsSettings, *time.Time, error) {
	s := DefaultOwnerOpsSettings()
	var raw []byte
	var at time.Time
	err := r.pool.QueryRow(ctx, `SELECT value, updated_at FROM platform_settings WHERE key=$1`, ownerOpsSettingsKey).Scan(&raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil, nil
	}
	if err != nil {
		return s, nil, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		// A row this code cannot read must never switch automation on.
		return DefaultOwnerOpsSettings(), &at, nil
	}
	if s.Validate() != nil {
		s.BriefingHourUTC = DefaultOwnerOpsSettings().BriefingHourUTC
	}
	return s, &at, nil
}

func (r *PlatformOpsRepository) SaveSettings(ctx context.Context, s OwnerOpsSettings, actor string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
INSERT INTO platform_settings(key, value, updated_by) VALUES($1,$2,$3)
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_by=EXCLUDED.updated_by, updated_at=NOW()`, ownerOpsSettingsKey, raw, actor)
	return err
}

// --- incidents --------------------------------------------------------------------------------------

// Incident kinds the fleet monitor opens.
const (
	IncidentWorkerDown    = "WORKER_DOWN"    // an active installation has no running ADM worker
	IncidentFeedStalled   = "FEED_STALLED"   // the worker runs but completes no poll cycle
	IncidentNitradoAccess = "NITRADO_ACCESS" // Nitrado keeps refusing: the owner must reconnect
	IncidentDiscordAccess = "DISCORD_ACCESS" // the bot was removed or lost its permissions

	IncidentOpen     = "OPEN"
	IncidentResolved = "RESOLVED"
)

type PlatformIncident struct {
	ID                 int64
	InstallationID     int64
	OrganizationID     int64
	OrganizationName   string
	ServerName         string
	Kind               string
	Status             string
	Detail             string
	Attempts           int
	LastAction         string
	LastActionAt       *time.Time
	OwnerNotifiedAt    *time.Time
	CustomerNotifiedAt *time.Time
	DetectedAt         time.Time
	ResolvedAt         *time.Time
	Resolution         string
}

const incidentColumns = `p.id, p.installation_id, p.organization_id, COALESCE(o.name,''), COALESCE(gs.display_name,''), p.kind, p.status, p.detail,
    p.attempts, p.last_action, p.last_action_at, p.owner_notified_at, p.customer_notified_at, p.detected_at, p.resolved_at, p.resolution`

const incidentFrom = ` FROM platform_incidents p
LEFT JOIN organizations o ON o.id = p.organization_id
LEFT JOIN installations i ON i.id = p.installation_id
LEFT JOIN game_servers gs ON gs.id = i.game_server_id`

func scanIncident(row pgx.Row) (PlatformIncident, error) {
	var p PlatformIncident
	err := row.Scan(&p.ID, &p.InstallationID, &p.OrganizationID, &p.OrganizationName, &p.ServerName, &p.Kind, &p.Status, &p.Detail,
		&p.Attempts, &p.LastAction, &p.LastActionAt, &p.OwnerNotifiedAt, &p.CustomerNotifiedAt, &p.DetectedAt, &p.ResolvedAt, &p.Resolution)
	return p, err
}

// OpenIncident records a problem unless the installation already has an open one of that kind.
// opened is true only for the call that created the row, so exactly one caller announces it.
func (r *PlatformOpsRepository) OpenIncident(ctx context.Context, installationID, organizationID int64, kind, detail string) (incident PlatformIncident, opened bool, err error) {
	var id int64
	err = r.pool.QueryRow(ctx, `
INSERT INTO platform_incidents(installation_id, organization_id, kind, detail) VALUES($1,$2,$3,$4)
ON CONFLICT (installation_id, kind) WHERE status = 'OPEN' DO NOTHING RETURNING id`, installationID, organizationID, kind, clip(detail, 300)).Scan(&id)
	switch {
	case err == nil:
		opened = true
	case errors.Is(err, pgx.ErrNoRows):
		if err = r.pool.QueryRow(ctx, `SELECT id FROM platform_incidents WHERE installation_id=$1 AND kind=$2 AND status='OPEN'`, installationID, kind).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Resolved between the two statements: nothing is open, nothing to report.
				return PlatformIncident{}, false, nil
			}
			return PlatformIncident{}, false, err
		}
	default:
		return PlatformIncident{}, false, err
	}
	incident, err = r.GetIncident(ctx, id)
	return incident, opened, err
}

func (r *PlatformOpsRepository) GetIncident(ctx context.Context, id int64) (PlatformIncident, error) {
	return scanIncident(r.pool.QueryRow(ctx, `SELECT `+incidentColumns+incidentFrom+` WHERE p.id=$1`, id))
}

// ClaimIncidentAction reserves the next automatic action on an open incident: it succeeds only
// when fewer than maxAttempts were made and the last one is at least minGap old. The claim is
// one conditional UPDATE, so two instances can never both act.
func (r *PlatformOpsRepository) ClaimIncidentAction(ctx context.Context, id int64, action string, maxAttempts int, minGap time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
UPDATE platform_incidents SET attempts = attempts + 1, last_action = $2, last_action_at = NOW()
WHERE id = $1 AND status = 'OPEN' AND attempts < $3
  AND (last_action_at IS NULL OR last_action_at <= NOW() - make_interval(secs => $4))`, id, action, maxAttempts, minGap.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimIncidentNotice reserves the one owner or customer notice for an open incident.
func (r *PlatformOpsRepository) ClaimIncidentNotice(ctx context.Context, id int64, customer bool) (bool, error) {
	column := "owner_notified_at"
	if customer {
		column = "customer_notified_at"
	}
	tag, err := r.pool.Exec(ctx, `UPDATE platform_incidents SET `+column+` = NOW() WHERE id=$1 AND `+column+` IS NULL`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ResolveIncident closes an open incident. resolved is false when it was already closed.
func (r *PlatformOpsRepository) ResolveIncident(ctx context.Context, id int64, resolution string) (PlatformIncident, bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE platform_incidents SET status='RESOLVED', resolved_at=NOW(), resolution=$2 WHERE id=$1 AND status='OPEN'`, id, clip(resolution, 300))
	if err != nil {
		return PlatformIncident{}, false, err
	}
	incident, err := r.GetIncident(ctx, id)
	return incident, tag.RowsAffected() == 1, err
}

// OpenIncidents returns every open incident, oldest first.
func (r *PlatformOpsRepository) OpenIncidents(ctx context.Context) ([]PlatformIncident, error) {
	return r.listIncidents(ctx, `SELECT `+incidentColumns+incidentFrom+` WHERE p.status='OPEN' ORDER BY p.id LIMIT 1000`)
}

// ListIncidents returns incidents newest first; status "" means any.
func (r *PlatformOpsRepository) ListIncidents(ctx context.Context, status string, limit int) ([]PlatformIncident, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	return r.listIncidents(ctx, `SELECT `+incidentColumns+incidentFrom+` WHERE ($1 = '' OR p.status = $1) ORDER BY p.id DESC LIMIT $2`, status, limit)
}

func (r *PlatformOpsRepository) listIncidents(ctx context.Context, q string, args ...any) ([]PlatformIncident, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlatformIncident{}
	for rows.Next() {
		p, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- broadcasts -------------------------------------------------------------------------------------

const (
	BroadcastInfo        = "INFO"
	BroadcastMaintenance = "MAINTENANCE"
	BroadcastIncident    = "INCIDENT"
)

type PlatformBroadcast struct {
	ID            int64
	Title         string
	Body          string
	Severity      string
	AudiencePlan  string
	PostToDiscord bool
	StartsAt      time.Time
	EndsAt        *time.Time
	CreatedBy     string
	CreatedAt     time.Time
	EndedAt       *time.Time
	EndedBy       *string
	// Discord delivery counts (zero when post_to_discord is false).
	DiscordSent, DiscordFailed, DiscordNoChannel int
}

// ErrInvalidBroadcast wraps a caller-fixable validation message.
var ErrInvalidBroadcast = errors.New("invalid broadcast")

// Normalize trims and Validate bounds a broadcast before it is stored.
func (b PlatformBroadcast) Normalize() PlatformBroadcast {
	b.Title, b.Body = strings.TrimSpace(b.Title), strings.TrimSpace(b.Body)
	b.Severity = strings.ToUpper(strings.TrimSpace(b.Severity))
	if b.Severity == "" {
		b.Severity = BroadcastInfo
	}
	b.AudiencePlan = strings.ToUpper(strings.TrimSpace(b.AudiencePlan))
	return b
}

func (b PlatformBroadcast) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidBroadcast, msg) }
	switch {
	case b.Title == "" || len([]rune(b.Title)) > 120:
		return bad("title is required (max 120 characters)")
	case b.Body == "" || len([]rune(b.Body)) > 1500:
		return bad("body is required (max 1500 characters)")
	case b.Severity != BroadcastInfo && b.Severity != BroadcastMaintenance && b.Severity != BroadcastIncident:
		return bad("severity must be INFO, MAINTENANCE or INCIDENT")
	case len(b.AudiencePlan) > 40:
		return bad("audiencePlan is too long")
	case b.EndsAt != nil && !b.EndsAt.After(b.StartsAt):
		return bad("endsAt must be after startsAt")
	}
	return nil
}

const broadcastColumns = `b.id, b.title, b.body, b.severity, b.audience_plan, b.post_to_discord, b.starts_at, b.ends_at, b.created_by, b.created_at, b.ended_at, b.ended_by,
    (SELECT COUNT(*) FROM platform_broadcast_deliveries d WHERE d.broadcast_id=b.id AND d.status='SENT')::int,
    (SELECT COUNT(*) FROM platform_broadcast_deliveries d WHERE d.broadcast_id=b.id AND d.status='FAILED')::int,
    (SELECT COUNT(*) FROM platform_broadcast_deliveries d WHERE d.broadcast_id=b.id AND d.status='NO_CHANNEL')::int`

func scanBroadcast(row pgx.Row) (PlatformBroadcast, error) {
	var b PlatformBroadcast
	err := row.Scan(&b.ID, &b.Title, &b.Body, &b.Severity, &b.AudiencePlan, &b.PostToDiscord, &b.StartsAt, &b.EndsAt, &b.CreatedBy, &b.CreatedAt, &b.EndedAt, &b.EndedBy,
		&b.DiscordSent, &b.DiscordFailed, &b.DiscordNoChannel)
	return b, err
}

func (r *PlatformOpsRepository) CreateBroadcast(ctx context.Context, b PlatformBroadcast) (PlatformBroadcast, error) {
	b = b.Normalize()
	if b.StartsAt.IsZero() {
		b.StartsAt = time.Now().UTC()
	}
	if err := b.Validate(); err != nil {
		return PlatformBroadcast{}, err
	}
	var id int64
	if err := r.pool.QueryRow(ctx, `
INSERT INTO platform_broadcasts(title, body, severity, audience_plan, post_to_discord, starts_at, ends_at, created_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, b.Title, b.Body, b.Severity, b.AudiencePlan, b.PostToDiscord, b.StartsAt, b.EndsAt, b.CreatedBy).Scan(&id); err != nil {
		return PlatformBroadcast{}, err
	}
	return r.GetBroadcast(ctx, id)
}

func (r *PlatformOpsRepository) GetBroadcast(ctx context.Context, id int64) (PlatformBroadcast, error) {
	return scanBroadcast(r.pool.QueryRow(ctx, `SELECT `+broadcastColumns+` FROM platform_broadcasts b WHERE b.id=$1`, id))
}

// EndBroadcast stops showing a broadcast now. ended is false when it had already ended.
func (r *PlatformOpsRepository) EndBroadcast(ctx context.Context, id int64, actor string) (PlatformBroadcast, bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE platform_broadcasts SET ended_at=NOW(), ended_by=$2 WHERE id=$1 AND ended_at IS NULL`, id, actor)
	if err != nil {
		return PlatformBroadcast{}, false, err
	}
	b, err := r.GetBroadcast(ctx, id)
	return b, tag.RowsAffected() == 1, err
}

func (r *PlatformOpsRepository) ListBroadcasts(ctx context.Context, limit int) ([]PlatformBroadcast, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return r.listBroadcasts(ctx, `SELECT `+broadcastColumns+` FROM platform_broadcasts b ORDER BY b.id DESC LIMIT $1`, limit)
}

// ActiveBroadcasts returns what an organization on plan should see right now: started, not
// ended, and addressed to everyone or to that plan.
func (r *PlatformOpsRepository) ActiveBroadcasts(ctx context.Context, plan string, now time.Time) ([]PlatformBroadcast, error) {
	return r.listBroadcasts(ctx, `SELECT `+broadcastColumns+` FROM platform_broadcasts b
WHERE b.ended_at IS NULL AND b.starts_at <= $2 AND (b.ends_at IS NULL OR b.ends_at > $2)
  AND (b.audience_plan = '' OR b.audience_plan = UPPER($1))
ORDER BY b.id DESC LIMIT 10`, plan, now.UTC())
}

func (r *PlatformOpsRepository) listBroadcasts(ctx context.Context, q string, args ...any) ([]PlatformBroadcast, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlatformBroadcast{}
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BroadcastTarget is one installation a broadcast's Discord copy goes to: its staff alerts
// channel (ADMIN_ALERTS), or "" when the installation routed none.
type BroadcastTarget struct {
	InstallationID int64
	ChannelID      string
}

// BroadcastTargets lists the set-up installations in the broadcast's audience that have not
// been attempted yet. Suspended installations are skipped.
func (r *PlatformOpsRepository) BroadcastTargets(ctx context.Context, broadcastID int64, audiencePlan string) ([]BroadcastTarget, error) {
	rows, err := r.pool.Query(ctx, `
SELECT i.id, COALESCE(cr.channel_id, '')
FROM installations i
LEFT JOIN subscriptions s ON s.organization_id = i.organization_id
LEFT JOIN installation_channel_routes cr ON cr.installation_id = i.id AND cr.route_key = 'ADMIN_ALERTS'
WHERE i.status IN ('READY','DEGRADED') AND i.game_server_id IS NOT NULL
  AND ($2 = '' OR UPPER(COALESCE(s.plan, '')) = $2)
  AND NOT EXISTS (SELECT 1 FROM platform_broadcast_deliveries d WHERE d.broadcast_id = $1 AND d.installation_id = i.id)
ORDER BY i.id LIMIT 2000`, broadcastID, audiencePlan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BroadcastTarget{}
	for rows.Next() {
		var t BroadcastTarget
		if err := rows.Scan(&t.InstallationID, &t.ChannelID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClaimBroadcastDelivery reserves the Discord copy for one installation. Only the caller that
// gets true may post; the row's status is the outcome it reports afterwards.
func (r *PlatformOpsRepository) ClaimBroadcastDelivery(ctx context.Context, broadcastID, installationID int64, status string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO platform_broadcast_deliveries(broadcast_id, installation_id, status) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, broadcastID, installationID, status)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *PlatformOpsRepository) SetBroadcastDelivery(ctx context.Context, broadcastID, installationID int64, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE platform_broadcast_deliveries SET status=$3, attempted_at=NOW() WHERE broadcast_id=$1 AND installation_id=$2`, broadcastID, installationID, status)
	return err
}

// --- briefing ---------------------------------------------------------------------------------------

// ClaimBriefing reserves today's briefing. Only the caller that gets true sends it.
func (r *PlatformOpsRepository) ClaimBriefing(ctx context.Context, day time.Time, body string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO platform_briefings(day, body) VALUES($1::date, $2) ON CONFLICT DO NOTHING`, day.UTC().Format("2006-01-02"), body)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *PlatformOpsRepository) SetBriefingDelivered(ctx context.Context, day time.Time, delivered int) error {
	_, err := r.pool.Exec(ctx, `UPDATE platform_briefings SET delivered=$2 WHERE day=$1::date`, day.UTC().Format("2006-01-02"), delivered)
	return err
}

// LastBriefing returns the most recent briefing that was sent, or nil.
func (r *PlatformOpsRepository) LastBriefing(ctx context.Context) (day *time.Time, delivered int, err error) {
	var d time.Time
	err = r.pool.QueryRow(ctx, `SELECT day, delivered FROM platform_briefings ORDER BY day DESC LIMIT 1`).Scan(&d, &delivered)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return &d, delivered, nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
