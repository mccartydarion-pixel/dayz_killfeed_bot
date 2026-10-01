package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PerimeterWatchRepository backs Perimeter Watch: the owner's switch, matching
// a player position to the area around registered bases, and the alert log.
type PerimeterWatchRepository struct{ pool *pgxpool.Pool }

func NewPerimeterWatchRepository(pool *pgxpool.Pool) *PerimeterWatchRepository {
	return &PerimeterWatchRepository{pool: pool}
}

const (
	PerimeterDefaultMargin   = 100
	PerimeterMinMargin       = 25
	PerimeterMaxMargin       = 300
	PerimeterDefaultCooldown = 1800
	PerimeterMinCooldown     = 300
	PerimeterMaxCooldown     = 7200
	// One alert per base at most every two minutes, whoever walks in.
	perimeterBaseCooldownSeconds = 120
)

type PerimeterWatchSettings struct {
	Enabled         bool       `json:"enabled"`
	MarginMeters    int        `json:"marginMeters"`
	CooldownSeconds int        `json:"cooldownSeconds"`
	UpdatedAt       *time.Time `json:"updatedAt,omitempty"`
}

type PerimeterMatch struct {
	InstallationID     int64
	GuildID            int64
	ServerID           int64
	BaseID             int64
	BaseName           string
	OwnerPlayerID      int64
	OwnerDiscordUserID string
	DistanceMeters     int
	CooldownSeconds    int
}

type PerimeterAlert struct {
	ID          int64     `json:"id"`
	BaseID      int64     `json:"baseId"`
	BaseName    string    `json:"baseName"`
	VisitorName string    `json:"visitorName"`
	Distance    int       `json:"distanceMeters"`
	Delivery    string    `json:"delivery"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (r *PerimeterWatchRepository) ready() bool { return r != nil && r.pool != nil }

// GetSettings returns the switch for one server. No row means off.
func (r *PerimeterWatchRepository) GetSettings(ctx context.Context, installationID, guildID, serverID int64) (PerimeterWatchSettings, error) {
	out := PerimeterWatchSettings{MarginMeters: PerimeterDefaultMargin, CooldownSeconds: PerimeterDefaultCooldown}
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return out, errors.New("invalid perimeter watch scope")
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,margin_meters,cooldown_seconds,updated_at FROM perimeter_watch_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, installationID, guildID, serverID).
		Scan(&out.Enabled, &out.MarginMeters, &out.CooldownSeconds, &updated)
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
func (r *PerimeterWatchRepository) SetSettings(ctx context.Context, installationID, guildID, serverID int64, enabled bool, margin, cooldown int, actorUserID *int64) (PerimeterWatchSettings, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 ||
		margin < PerimeterMinMargin || margin > PerimeterMaxMargin || cooldown < PerimeterMinCooldown || cooldown > PerimeterMaxCooldown {
		return PerimeterWatchSettings{}, errors.New("invalid perimeter watch settings")
	}
	var out PerimeterWatchSettings
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO perimeter_watch_settings
 (installation_id,guild_id,server_id,enabled,margin_meters,cooldown_seconds,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET enabled=EXCLUDED.enabled,margin_meters=EXCLUDED.margin_meters,
  cooldown_seconds=EXCLUDED.cooldown_seconds,updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 WHERE perimeter_watch_settings.guild_id=EXCLUDED.guild_id
 RETURNING enabled,margin_meters,cooldown_seconds,updated_at`,
		installationID, guildID, serverID, enabled, margin, cooldown, actorUserID).
		Scan(&out.Enabled, &out.MarginMeters, &out.CooldownSeconds, &updated)
	if err != nil {
		return PerimeterWatchSettings{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// MatchPerimeter finds registered, non-withdrawn bases on this server whose
// circle plus the owner's margin contains (mapX, mapZ), when Perimeter Watch is
// on and the visitor is not the owner, in the owner's faction or covered by an
// active friend-list grant. While the owner sells Perimeter Watch, only bases
// whose owner has paid time match.
func (r *PerimeterWatchRepository) MatchPerimeter(ctx context.Context, guildID, serverID, visitorPlayerID int64, mapX, mapZ float64) ([]PerimeterMatch, error) {
	if !r.ready() || guildID <= 0 || serverID <= 0 || visitorPlayerID <= 0 {
		return nil, errors.New("invalid perimeter match")
	}
	rows, err := r.pool.Query(ctx, `
WITH visitor_faction AS (
 SELECT faction_id FROM faction_members WHERE guild_id=$1 AND player_id=$3 AND active
)
SELECT b.installation_id,b.guild_id,b.server_id,b.id,b.name,b.owner_player_id,COALESCE(pl.discord_user_id,''),
 ROUND(SQRT((b.center_x-$4)*(b.center_x-$4)+(b.center_z-$5)*(b.center_z-$5)))::INT,s.cooldown_seconds
FROM case_registered_bases b
JOIN perimeter_watch_settings s
 ON s.installation_id=b.installation_id AND s.guild_id=b.guild_id AND s.server_id=b.server_id AND s.enabled
LEFT JOIN player_links pl ON pl.guild_id=b.guild_id AND pl.player_id=b.owner_player_id AND pl.status='VERIFIED'
WHERE b.guild_id=$1 AND b.server_id=$2 AND b.state<>'REVOKED' AND b.owner_player_id<>$3 AND NOT base_rent_paused(b.id)
 AND (b.center_x-$4)*(b.center_x-$4)+(b.center_z-$5)*(b.center_z-$5) <= (b.radius+s.margin_meters)*(b.radius+s.margin_meters)
 AND NOT EXISTS (
  SELECT 1 FROM faction_members ofm
  WHERE ofm.guild_id=b.guild_id AND ofm.active AND ofm.player_id=b.owner_player_id
   AND ofm.faction_id IN (SELECT faction_id FROM visitor_faction))
 AND NOT EXISTS (
  SELECT 1 FROM case_base_authorizations a
  WHERE a.installation_id=b.installation_id AND a.guild_id=b.guild_id AND a.server_id=b.server_id AND a.base_id=b.id
   AND a.valid_from<=NOW() AND (a.valid_until IS NULL OR a.valid_until>NOW())
   AND (a.player_id=$3 OR a.faction_id IN (SELECT faction_id FROM visitor_faction)))
 AND (NOT EXISTS (
  SELECT 1 FROM security_service_offers o
  WHERE o.installation_id=b.installation_id AND o.server_id=b.server_id
   AND o.service_id IN ('PERIMETER_MONITORING','SENTINEL_PRO') AND o.enabled)
  OR EXISTS (
  SELECT 1 FROM security_service_purchases sp
  WHERE sp.installation_id=b.installation_id AND sp.player_id=b.owner_player_id
   AND sp.service_id IN ('PERIMETER_MONITORING','SENTINEL_PRO') AND sp.starts_at<=NOW() AND sp.ends_at>NOW()))
ORDER BY b.id`, guildID, serverID, visitorPlayerID, mapX, mapZ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PerimeterMatch
	for rows.Next() {
		var m PerimeterMatch
		if err := rows.Scan(&m.InstallationID, &m.GuildID, &m.ServerID, &m.BaseID, &m.BaseName, &m.OwnerPlayerID, &m.OwnerDiscordUserID,
			&m.DistanceMeters, &m.CooldownSeconds); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordAlert logs one alert unless this visitor already triggered one for the
// base within the cooldown, or the base alerted in the last two minutes. It
// returns the new alert id, or 0 when cooling down.
func (r *PerimeterWatchRepository) RecordAlert(ctx context.Context, m PerimeterMatch, visitorPlayerID int64, visitorName string) (int64, error) {
	if !r.ready() || m.BaseID <= 0 || visitorPlayerID <= 0 {
		return 0, errors.New("invalid perimeter alert")
	}
	if visitorName == "" {
		visitorName = "Unknown player"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('perimeter_alert:'||$1::BIGINT::TEXT,0))`, m.BaseID); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO perimeter_watch_alerts
 (installation_id,guild_id,server_id,base_id,visitor_player_id,visitor_name,distance_meters)
 SELECT $1,$2,$3,$4,$5,$6,$7
 WHERE NOT EXISTS (SELECT 1 FROM perimeter_watch_alerts
   WHERE installation_id=$1 AND server_id=$3 AND base_id=$4 AND visitor_player_id=$5
    AND created_at > NOW() - make_interval(secs => $8))
  AND NOT EXISTS (SELECT 1 FROM perimeter_watch_alerts
   WHERE installation_id=$1 AND server_id=$3 AND base_id=$4 AND created_at > NOW() - make_interval(secs => $9))
 RETURNING id`, m.InstallationID, m.GuildID, m.ServerID, m.BaseID, visitorPlayerID, baseRaidClip(visitorName, 128),
		m.DistanceMeters, float64(m.CooldownSeconds), float64(perimeterBaseCooldownSeconds)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// MarkDelivery records how an alert was delivered.
func (r *PerimeterWatchRepository) MarkDelivery(ctx context.Context, alertID int64, delivery string) error {
	if !r.ready() || alertID <= 0 {
		return errors.New("invalid perimeter alert")
	}
	_, err := r.pool.Exec(ctx, `UPDATE perimeter_watch_alerts SET delivery=$2 WHERE id=$1`, alertID, delivery)
	return err
}

// RecentAlerts lists the newest alerts for one server, for the dashboard.
func (r *PerimeterWatchRepository) RecentAlerts(ctx context.Context, installationID, guildID, serverID int64, limit int) ([]PerimeterAlert, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return nil, errors.New("invalid perimeter watch scope")
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `SELECT a.id,a.base_id,b.name,a.visitor_name,a.distance_meters,a.delivery,a.created_at
 FROM perimeter_watch_alerts a
 JOIN case_registered_bases b ON b.installation_id=a.installation_id AND b.guild_id=a.guild_id AND b.server_id=a.server_id AND b.id=a.base_id
 WHERE a.installation_id=$1 AND a.guild_id=$2 AND a.server_id=$3
 ORDER BY a.created_at DESC,a.id DESC LIMIT $4`, installationID, guildID, serverID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PerimeterAlert, 0)
	for rows.Next() {
		var a PerimeterAlert
		if err := rows.Scan(&a.ID, &a.BaseID, &a.BaseName, &a.VisitorName, &a.Distance, &a.Delivery, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
