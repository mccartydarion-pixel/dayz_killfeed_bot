package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/caseintel"
)

// CaseStaffAlertRepository backs real C.A.S.E. staff alerts: the owner's
// switch, the queue of findings to post to the private CASE_ALERTS channel,
// and delivery bookkeeping. Rows are queued only for released detectors.
type CaseStaffAlertRepository struct{ pool *pgxpool.Pool }

func NewCaseStaffAlertRepository(pool *pgxpool.Pool) *CaseStaffAlertRepository {
	return &CaseStaffAlertRepository{pool: pool}
}

const (
	CaseAlertMaxAttempts = 5

	CaseAlertStatusPending = "PENDING"
	CaseAlertStatusSent    = "SENT"
	CaseAlertStatusRetry   = "RETRY_WAIT"
	CaseAlertStatusDead    = "DEAD"
)

var caseIncidentKeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

type CaseAlertSettings struct {
	Enabled   bool       `json:"enabled"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// CaseAlertServer is one server whose owner turned staff alerts on.
type CaseAlertServer struct {
	InstallationID int64
	GuildID        int64
	ServerID       int64
}

// CaseAlertDelivery is one queued alert, with what the sender needs to route it.
type CaseAlertDelivery struct {
	ID             int64
	InstallationID int64
	GuildID        int64
	ServerID       int64
	OrganizationID int64
	DiscordGuildID string
	DetectorID     string
	Attempts       int
	Finding        caseintel.Finding
}

// CaseAlertRecent is a dashboard row.
type CaseAlertRecent struct {
	ID         int64      `json:"id"`
	DetectorID string     `json:"detectorId"`
	PlayerID   int64      `json:"playerId"`
	PlayerName string     `json:"playerName,omitempty"`
	Status     string     `json:"status"`
	LastError  string     `json:"lastError,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	SentAt     *time.Time `json:"sentAt,omitempty"`
}

func validCaseAlertScope(installationID, guildID, serverID int64) bool {
	return installationID > 0 && guildID > 0 && serverID > 0
}

func (r *CaseStaffAlertRepository) ready() bool { return r != nil && r.pool != nil }

// GetSettings returns the switch for one server. No row means off.
func (r *CaseStaffAlertRepository) GetSettings(ctx context.Context, installationID, guildID, serverID int64) (CaseAlertSettings, error) {
	var out CaseAlertSettings
	if !r.ready() || !validCaseAlertScope(installationID, guildID, serverID) {
		return out, errors.New("invalid C.A.S.E. alert scope")
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,updated_at FROM case_alert_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, installationID, guildID, serverID).Scan(&out.Enabled, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// SetSettings stores the switch; the foreign keys reject a mismatched scope.
func (r *CaseStaffAlertRepository) SetSettings(ctx context.Context, installationID, guildID, serverID int64, enabled bool, actorUserID *int64) (CaseAlertSettings, error) {
	var out CaseAlertSettings
	if !r.ready() || !validCaseAlertScope(installationID, guildID, serverID) {
		return out, errors.New("invalid C.A.S.E. alert scope")
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO case_alert_settings
 (installation_id,guild_id,server_id,enabled,updated_by_user_id,updated_at) VALUES ($1,$2,$3,$4,$5,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET
  enabled=EXCLUDED.enabled,updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 WHERE case_alert_settings.guild_id=EXCLUDED.guild_id
 RETURNING enabled,updated_at`, installationID, guildID, serverID, enabled, actorUserID).Scan(&out.Enabled, &updated)
	if err != nil {
		return CaseAlertSettings{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// EnabledServers lists servers with staff alerts on.
func (r *CaseStaffAlertRepository) EnabledServers(ctx context.Context) ([]CaseAlertServer, error) {
	if !r.ready() {
		return nil, errors.New("C.A.S.E. alerts unavailable")
	}
	rows, err := r.pool.Query(ctx, `SELECT installation_id,guild_id,server_id FROM case_alert_settings WHERE enabled ORDER BY installation_id,server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseAlertServer
	for rows.Next() {
		var s CaseAlertServer
		if err := rows.Scan(&s.InstallationID, &s.GuildID, &s.ServerID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecentSessionPlayers lists players with a connect or disconnect recorded
// on this server since the given time, newest first.
func (r *CaseStaffAlertRepository) RecentSessionPlayers(ctx context.Context, guildID, serverID int64, since time.Time, limit int) ([]int64, error) {
	if !r.ready() || guildID <= 0 || serverID <= 0 {
		return nil, errors.New("invalid C.A.S.E. alert scope")
	}
	if limit < 1 || limit > 500 {
		limit = 200
	}
	rows, err := r.pool.Query(ctx, `SELECT subject_player_id FROM case_evidence_events
 WHERE guild_id=$1 AND server_id=$2 AND ingested_at>=$3
  AND event_type IN ('PLAYER_CONNECT','PLAYER_DISCONNECT') AND subject_player_id IS NOT NULL
 GROUP BY subject_player_id ORDER BY MAX(id) DESC LIMIT $4`, guildID, serverID, since, limit)
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

// Enqueue queues one finding for delivery. The same incident is queued at
// most once per installation; it returns false when it was already queued.
func (r *CaseStaffAlertRepository) Enqueue(ctx context.Context, s CaseAlertServer, f caseintel.Finding) (bool, error) {
	if !r.ready() || !validCaseAlertScope(s.InstallationID, s.GuildID, s.ServerID) {
		return false, errors.New("invalid C.A.S.E. alert scope")
	}
	if !caseIncidentKeyRe.MatchString(f.IncidentKey) || f.DetectorID == "" || f.PlayerID <= 0 || len(f.EvidenceIDs) == 0 ||
		f.Scope != (caseintel.Core8Scope{GuildID: s.GuildID, InstallationID: s.InstallationID, ServerID: s.ServerID}) {
		return false, errors.New("finding is not deliverable")
	}
	payload, err := json.Marshal(f)
	if err != nil {
		return false, err
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO case_alert_deliveries
 (installation_id,guild_id,server_id,detector_id,incident_key,player_id,finding)
 VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (installation_id,incident_key) DO NOTHING`,
		s.InstallationID, s.GuildID, s.ServerID, f.DetectorID, f.IncidentKey, f.PlayerID, payload)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimDue leases up to limit due alerts. An expired lease (a crashed send)
// becomes due again; every claim counts as an attempt.
func (r *CaseStaffAlertRepository) ClaimDue(ctx context.Context, limit int, leaseFor time.Duration) ([]CaseAlertDelivery, error) {
	if !r.ready() || limit < 1 || leaseFor <= 0 {
		return nil, errors.New("invalid C.A.S.E. alert claim")
	}
	// A send that crashed on its last attempt is not retried again.
	if _, err := r.pool.Exec(ctx, `UPDATE case_alert_deliveries SET status='DEAD',lease_until=NULL,
 last_error_code='LEASE_EXPIRED',updated_at=NOW() WHERE status='LEASED' AND lease_until<NOW() AND attempts>=$1`,
		CaseAlertMaxAttempts); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `WITH due AS (
  SELECT id FROM case_alert_deliveries
  WHERE attempts<$3 AND ((status IN ('PENDING','RETRY_WAIT') AND next_attempt_at<=NOW())
     OR (status='LEASED' AND lease_until<NOW()))
  ORDER BY next_attempt_at,id LIMIT $1 FOR UPDATE SKIP LOCKED
 )
 UPDATE case_alert_deliveries d SET status='LEASED',lease_until=NOW()+make_interval(secs=>$2),
  attempts=d.attempts+1,updated_at=NOW()
 FROM due, installations i, discord_guild_connections c, guilds g
 WHERE d.id=due.id AND i.id=d.installation_id AND i.game_server_id=d.server_id
  AND c.id=i.discord_guild_connection_id AND c.guild_id=d.guild_id AND g.id=c.guild_id
 RETURNING d.id,d.installation_id,d.guild_id,d.server_id,i.organization_id,g.discord_guild_id,
  d.detector_id,d.attempts,d.finding`, limit, leaseFor.Seconds(), CaseAlertMaxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseAlertDelivery
	for rows.Next() {
		var d CaseAlertDelivery
		var payload []byte
		if err := rows.Scan(&d.ID, &d.InstallationID, &d.GuildID, &d.ServerID, &d.OrganizationID, &d.DiscordGuildID,
			&d.DetectorID, &d.Attempts, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &d.Finding); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkSent records a delivered alert.
func (r *CaseStaffAlertRepository) MarkSent(ctx context.Context, id int64, channelID, messageID string) error {
	return r.finish(ctx, id, `status='SENT',sent_at=NOW(),channel_id=$2,message_id=$3,last_error_code=''`, channelID, messageID)
}

// MarkRetry schedules another attempt, or gives up after the last one.
func (r *CaseStaffAlertRepository) MarkRetry(ctx context.Context, id int64, code string, after time.Duration) error {
	return r.finish(ctx, id, `status=CASE WHEN attempts>=`+strconv.Itoa(CaseAlertMaxAttempts)+` THEN 'DEAD' ELSE 'RETRY_WAIT' END,
  next_attempt_at=NOW()+make_interval(secs=>$3),last_error_code=$2`, baseRaidClip(code, 60), after.Seconds())
}

// MarkDead stops delivery for good (for example, the channel is public).
func (r *CaseStaffAlertRepository) MarkDead(ctx context.Context, id int64, code string) error {
	return r.finish(ctx, id, `status='DEAD',last_error_code=$2`, baseRaidClip(code, 60))
}

func (r *CaseStaffAlertRepository) finish(ctx context.Context, id int64, set string, args ...any) error {
	if !r.ready() || id <= 0 {
		return errors.New("invalid C.A.S.E. alert")
	}
	tag, err := r.pool.Exec(ctx, `UPDATE case_alert_deliveries SET `+set+`,lease_until=NULL,updated_at=NOW()
 WHERE id=$1 AND status='LEASED'`, append([]any{id}, args...)...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("C.A.S.E. alert is not leased")
	}
	return nil
}

// Recent lists the newest alerts for one server, for the dashboard.
func (r *CaseStaffAlertRepository) Recent(ctx context.Context, installationID, guildID, serverID int64, limit int) ([]CaseAlertRecent, error) {
	if !r.ready() || !validCaseAlertScope(installationID, guildID, serverID) {
		return nil, errors.New("invalid C.A.S.E. alert scope")
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `SELECT id,detector_id,player_id,COALESCE(finding->>'playerName',''),status,last_error_code,created_at,sent_at
 FROM case_alert_deliveries WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3
 ORDER BY created_at DESC,id DESC LIMIT $4`, installationID, guildID, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CaseAlertRecent, 0)
	for rows.Next() {
		var a CaseAlertRecent
		if err := rows.Scan(&a.ID, &a.DetectorID, &a.PlayerID, &a.PlayerName, &a.Status, &a.LastError, &a.CreatedAt, &a.SentAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
