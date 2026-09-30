package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BaseRaidAlarmRepository backs the Base Raid Alarm: the owner's on/off switch,
// matching a dismantle line to an owner-registered base, and the alarm log.
type BaseRaidAlarmRepository struct{ pool *pgxpool.Pool }

func NewBaseRaidAlarmRepository(pool *pgxpool.Pool) *BaseRaidAlarmRepository {
	return &BaseRaidAlarmRepository{pool: pool}
}

const (
	BaseRaidDefaultCooldownSeconds = 600
	BaseRaidMinCooldownSeconds     = 60
	BaseRaidMaxCooldownSeconds     = 3600

	BaseRaidDeliverySent           = "SENT"
	BaseRaidDeliveryOwnerNotLinked = "OWNER_NOT_LINKED"
	BaseRaidDeliveryFailed         = "FAILED"
)

type BaseRaidAlarmSettings struct {
	Enabled         bool       `json:"enabled"`
	CooldownSeconds int        `json:"cooldownSeconds"`
	UpdatedAt       *time.Time `json:"updatedAt,omitempty"`
}

type BaseRaidAlert struct {
	ID         int64     `json:"id"`
	BaseID     int64     `json:"baseId"`
	BaseName   string    `json:"baseName"`
	RaiderName string    `json:"raiderName"`
	Part       string    `json:"part"`
	TimeOfDay  string    `json:"timeOfDay,omitempty"`
	Delivery   string    `json:"delivery"`
	CreatedAt  time.Time `json:"createdAt"`
}

// BaseRaidMatch is one registered base a dismantle happened inside, where the
// player was neither the owner, the owner's faction, nor on the friend list.
type BaseRaidMatch struct {
	InstallationID     int64
	GuildID            int64
	ServerID           int64
	BaseID             int64
	BaseName           string
	OwnerPlayerID      int64
	OwnerDiscordUserID string // verified link only; "" when the owner is not linked
	RaiderPlayerID     *int64
	CooldownSeconds    int
}

// BaseRaidEvent is what the dismantle line said.
type BaseRaidEvent struct {
	RaiderName string
	Part       string
	Target     string
	Tool       string
	TimeOfDay  string
}

func validBaseRaidScope(installationID, guildID, serverID int64) bool {
	return installationID > 0 && guildID > 0 && serverID > 0
}

// GetSettings returns the switch for one server. No row means off.
func (r *BaseRaidAlarmRepository) GetSettings(ctx context.Context, installationID, guildID, serverID int64) (BaseRaidAlarmSettings, error) {
	out := BaseRaidAlarmSettings{CooldownSeconds: BaseRaidDefaultCooldownSeconds}
	if r == nil || r.pool == nil || !validBaseRaidScope(installationID, guildID, serverID) {
		return out, errors.New("invalid base raid alarm scope")
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,cooldown_seconds,updated_at FROM base_raid_alarm_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, installationID, guildID, serverID).
		Scan(&out.Enabled, &out.CooldownSeconds, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// SetSettings stores the switch. The installation/server pairing is enforced
// by the table's foreign keys, so a mismatched scope fails.
func (r *BaseRaidAlarmRepository) SetSettings(ctx context.Context, installationID, guildID, serverID int64, enabled bool, cooldownSeconds int, actorUserID *int64) (BaseRaidAlarmSettings, error) {
	if r == nil || r.pool == nil || !validBaseRaidScope(installationID, guildID, serverID) {
		return BaseRaidAlarmSettings{}, errors.New("invalid base raid alarm scope")
	}
	if cooldownSeconds < BaseRaidMinCooldownSeconds || cooldownSeconds > BaseRaidMaxCooldownSeconds {
		return BaseRaidAlarmSettings{}, errors.New("cooldown out of range")
	}
	out := BaseRaidAlarmSettings{}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO base_raid_alarm_settings
 (installation_id,guild_id,server_id,enabled,cooldown_seconds,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET
  enabled=EXCLUDED.enabled,cooldown_seconds=EXCLUDED.cooldown_seconds,
  updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 WHERE base_raid_alarm_settings.guild_id=EXCLUDED.guild_id
 RETURNING enabled,cooldown_seconds,updated_at`,
		installationID, guildID, serverID, enabled, cooldownSeconds, actorUserID).
		Scan(&out.Enabled, &out.CooldownSeconds, &updated)
	if err != nil {
		return BaseRaidAlarmSettings{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// MatchRaid finds the registered, non-withdrawn bases on this server whose
// circle contains (mapX, mapZ), when the alarm is on for the server and the
// player is not the owner, not in the owner's faction, and not covered by an
// active friend-list grant (player or faction).
func (r *BaseRaidAlarmRepository) MatchRaid(ctx context.Context, guildID, serverID int64, raiderAdmID string, mapX, mapZ float64) ([]BaseRaidMatch, error) {
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || raiderAdmID == "" {
		return nil, errors.New("invalid base raid match")
	}
	rows, err := r.pool.Query(ctx, `
WITH raider AS (
 SELECT p.id FROM players p WHERE p.guild_id=$1 AND p.dayz_player_id=$3
), raider_faction AS (
 SELECT fm.faction_id FROM faction_members fm, raider
 WHERE fm.guild_id=$1 AND fm.active AND fm.player_id=raider.id
)
SELECT b.installation_id,b.guild_id,b.server_id,b.id,b.name,b.owner_player_id,
 COALESCE(pl.discord_user_id,''),(SELECT id FROM raider),s.cooldown_seconds
FROM case_registered_bases b
JOIN base_raid_alarm_settings s
 ON s.installation_id=b.installation_id AND s.guild_id=b.guild_id AND s.server_id=b.server_id AND s.enabled
JOIN players owner ON owner.guild_id=b.guild_id AND owner.id=b.owner_player_id
LEFT JOIN player_links pl
 ON pl.guild_id=b.guild_id AND pl.player_id=b.owner_player_id AND pl.status='VERIFIED'
WHERE b.guild_id=$1 AND b.server_id=$2 AND b.state<>'REVOKED'
 AND (b.center_x-$4)*(b.center_x-$4)+(b.center_z-$5)*(b.center_z-$5) <= b.radius*b.radius
 AND owner.dayz_player_id<>$3
 AND NOT EXISTS (
  SELECT 1 FROM faction_members ofm
  WHERE ofm.guild_id=b.guild_id AND ofm.active AND ofm.player_id=b.owner_player_id
   AND ofm.faction_id IN (SELECT faction_id FROM raider_faction))
 AND NOT EXISTS (
  SELECT 1 FROM case_base_authorizations a
  WHERE a.installation_id=b.installation_id AND a.guild_id=b.guild_id
   AND a.server_id=b.server_id AND a.base_id=b.id
   AND a.valid_from<=NOW() AND (a.valid_until IS NULL OR a.valid_until>NOW())
   AND (a.player_id IN (SELECT id FROM raider)
     OR a.faction_id IN (SELECT faction_id FROM raider_faction)))
ORDER BY b.id`, guildID, serverID, raiderAdmID, mapX, mapZ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BaseRaidMatch
	for rows.Next() {
		var m BaseRaidMatch
		if err := rows.Scan(&m.InstallationID, &m.GuildID, &m.ServerID, &m.BaseID, &m.BaseName, &m.OwnerPlayerID,
			&m.OwnerDiscordUserID, &m.RaiderPlayerID, &m.CooldownSeconds); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordAlert logs one alarm for a base unless one was already logged within
// the cooldown. It returns the new alert id, or 0 when still cooling down.
func (r *BaseRaidAlarmRepository) RecordAlert(ctx context.Context, m BaseRaidMatch, ev BaseRaidEvent) (int64, error) {
	if r == nil || r.pool == nil || !validBaseRaidScope(m.InstallationID, m.GuildID, m.ServerID) || m.BaseID <= 0 {
		return 0, errors.New("invalid base raid alert")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serialize alerts per base so two dismantles in the same instant
	// cannot both pass the cooldown check.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('base_raid_alert:'||$1::BIGINT::TEXT,0))`, m.BaseID); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO base_raid_alerts
 (installation_id,guild_id,server_id,base_id,raider_player_id,raider_name,part,target,tool,time_of_day)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10
 WHERE NOT EXISTS (
  SELECT 1 FROM base_raid_alerts
  WHERE installation_id=$1 AND server_id=$3 AND base_id=$4
   AND created_at > NOW() - make_interval(secs => $11))
 RETURNING id`,
		m.InstallationID, m.GuildID, m.ServerID, m.BaseID, m.RaiderPlayerID,
		baseRaidClip(ev.RaiderName, 128), baseRaidClip(ev.Part, 64), baseRaidClip(ev.Target, 64), baseRaidClip(ev.Tool, 64), baseRaidClip(ev.TimeOfDay, 16),
		float64(m.CooldownSeconds)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// MarkDelivery records how an alert was delivered.
func (r *BaseRaidAlarmRepository) MarkDelivery(ctx context.Context, alertID int64, delivery string) error {
	if r == nil || r.pool == nil || alertID <= 0 {
		return errors.New("invalid base raid alert")
	}
	_, err := r.pool.Exec(ctx, `UPDATE base_raid_alerts SET delivery=$2 WHERE id=$1`, alertID, delivery)
	return err
}

// RecentAlerts lists the newest alarms for one server, for the dashboard.
func (r *BaseRaidAlarmRepository) RecentAlerts(ctx context.Context, installationID, guildID, serverID int64, limit int) ([]BaseRaidAlert, error) {
	if r == nil || r.pool == nil || !validBaseRaidScope(installationID, guildID, serverID) {
		return nil, errors.New("invalid base raid alarm scope")
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `SELECT a.id,a.base_id,b.name,a.raider_name,a.part,a.time_of_day,a.delivery,a.created_at
 FROM base_raid_alerts a
 JOIN case_registered_bases b
  ON b.installation_id=a.installation_id AND b.guild_id=a.guild_id AND b.server_id=a.server_id AND b.id=a.base_id
 WHERE a.installation_id=$1 AND a.guild_id=$2 AND a.server_id=$3
 ORDER BY a.created_at DESC, a.id DESC LIMIT $4`, installationID, guildID, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BaseRaidAlert, 0)
	for rows.Next() {
		var a BaseRaidAlert
		if err := rows.Scan(&a.ID, &a.BaseID, &a.BaseName, &a.RaiderName, &a.Part, &a.TimeOfDay, &a.Delivery, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func baseRaidClip(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
