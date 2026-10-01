package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BaseBlackBoxRepository backs the Base Black Box: the owner's switch, matching
// a position to registered bases, the per-base history and its clean-up.
type BaseBlackBoxRepository struct{ pool *pgxpool.Pool }

func NewBaseBlackBoxRepository(pool *pgxpool.Pool) *BaseBlackBoxRepository {
	return &BaseBlackBoxRepository{pool: pool}
}

const (
	ServiceBaseBlackBox = "BASE_BLACK_BOX"

	BlackBoxDefaultRetention = 14
	BlackBoxMinRetention     = 3
	BlackBoxMaxRetention     = 30
	// The recorded area reaches this far past the base circle.
	BlackBoxMarginMeters = 100
	// Sightings of the same player at the same base this close together are
	// one visit (or one dismantling spree).
	blackBoxMergeMinutes = 10

	BlackBoxVisit     = "VISIT"
	BlackBoxDismantle = "DISMANTLE"
)

var ErrInvalidBlackBox = errors.New("invalid base black box request")

type BaseBlackBoxSettings struct {
	Enabled       bool       `json:"enabled"`
	RetentionDays int        `json:"retentionDays"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
}

// BlackBoxMatch is one base a position falls near.
type BlackBoxMatch struct {
	InstallationID int64
	GuildID        int64
	ServerID       int64
	BaseID         int64
	PlayerID       int64
	DistanceMeters int
}

type BlackBoxEvent struct {
	ID            int64     `json:"id"`
	BaseID        int64     `json:"baseId"`
	BaseName      string    `json:"baseName"`
	Kind          string    `json:"kind"`
	PlayerName    string    `json:"playerName"`
	Detail        string    `json:"detail,omitempty"`
	ClosestMeters int       `json:"closestMeters"`
	Sightings     int       `json:"sightings"`
	FirstSeenAt   time.Time `json:"firstSeenAt"`
	LastSeenAt    time.Time `json:"lastSeenAt"`
}

type BlackBoxBase struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (r *BaseBlackBoxRepository) ready() bool { return r != nil && r.pool != nil }

// GetSettings returns the switch for one server. No row means off.
func (r *BaseBlackBoxRepository) GetSettings(ctx context.Context, installationID, guildID, serverID int64) (BaseBlackBoxSettings, error) {
	out := BaseBlackBoxSettings{RetentionDays: BlackBoxDefaultRetention}
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return out, ErrInvalidBlackBox
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,retention_days,updated_at FROM base_black_box_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, installationID, guildID, serverID).
		Scan(&out.Enabled, &out.RetentionDays, &updated)
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
func (r *BaseBlackBoxRepository) SetSettings(ctx context.Context, installationID, guildID, serverID int64, enabled bool, retentionDays int, actorUserID *int64) (BaseBlackBoxSettings, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 ||
		retentionDays < BlackBoxMinRetention || retentionDays > BlackBoxMaxRetention {
		return BaseBlackBoxSettings{}, ErrInvalidBlackBox
	}
	var out BaseBlackBoxSettings
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO base_black_box_settings
 (installation_id,guild_id,server_id,enabled,retention_days,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET enabled=EXCLUDED.enabled,retention_days=EXCLUDED.retention_days,
  updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 WHERE base_black_box_settings.guild_id=EXCLUDED.guild_id
 RETURNING enabled,retention_days,updated_at`,
		installationID, guildID, serverID, enabled, retentionDays, actorUserID).
		Scan(&out.Enabled, &out.RetentionDays, &updated)
	if err != nil {
		return BaseBlackBoxSettings{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// matchSQL finds registered, non-withdrawn bases on this server whose circle
// plus the fixed margin contains the position, when the Black Box is on and the
// player is not the owner, in the owner's faction or on the friend list. While
// the owner sells the Black Box, only bases whose owner has paid time match.
// $3 is the player's id; $6 is the extra reach past the circle.
const blackBoxMatchSQL = `
WITH who AS (
 SELECT faction_id FROM faction_members WHERE guild_id=$1 AND player_id=$3 AND active
)
SELECT b.installation_id,b.guild_id,b.server_id,b.id,
 ROUND(SQRT((b.center_x-$4)*(b.center_x-$4)+(b.center_z-$5)*(b.center_z-$5)))::INT
FROM case_registered_bases b
JOIN base_black_box_settings s
 ON s.installation_id=b.installation_id AND s.guild_id=b.guild_id AND s.server_id=b.server_id AND s.enabled
WHERE b.guild_id=$1 AND b.server_id=$2 AND b.state<>'REVOKED' AND b.owner_player_id<>$3
 AND (b.center_x-$4)*(b.center_x-$4)+(b.center_z-$5)*(b.center_z-$5) <= (b.radius+$6)*(b.radius+$6)
 AND NOT EXISTS (
  SELECT 1 FROM faction_members ofm
  WHERE ofm.guild_id=b.guild_id AND ofm.active AND ofm.player_id=b.owner_player_id
   AND ofm.faction_id IN (SELECT faction_id FROM who))
 AND NOT EXISTS (
  SELECT 1 FROM case_base_authorizations a
  WHERE a.installation_id=b.installation_id AND a.guild_id=b.guild_id AND a.server_id=b.server_id AND a.base_id=b.id
   AND a.valid_from<=NOW() AND (a.valid_until IS NULL OR a.valid_until>NOW())
   AND (a.player_id=$3 OR a.faction_id IN (SELECT faction_id FROM who)))
 AND (NOT EXISTS (
  SELECT 1 FROM security_service_offers o
  WHERE o.installation_id=b.installation_id AND o.server_id=b.server_id
   AND o.service_id IN ('BASE_BLACK_BOX','SENTINEL_PRO') AND o.enabled)
  OR EXISTS (
  SELECT 1 FROM security_service_purchases sp
  WHERE sp.installation_id=b.installation_id AND sp.player_id=b.owner_player_id
   AND sp.service_id IN ('BASE_BLACK_BOX','SENTINEL_PRO') AND sp.starts_at<=NOW() AND sp.ends_at>NOW()))
ORDER BY b.id`

func (r *BaseBlackBoxRepository) match(ctx context.Context, guildID, serverID, playerID int64, x, z float64, reach int) ([]BlackBoxMatch, error) {
	rows, err := r.pool.Query(ctx, blackBoxMatchSQL, guildID, serverID, playerID, x, z, reach)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlackBoxMatch
	for rows.Next() {
		m := BlackBoxMatch{PlayerID: playerID}
		if err := rows.Scan(&m.InstallationID, &m.GuildID, &m.ServerID, &m.BaseID, &m.DistanceMeters); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MatchVisit finds the bases a player position is near.
func (r *BaseBlackBoxRepository) MatchVisit(ctx context.Context, guildID, serverID, playerID int64, x, z float64) ([]BlackBoxMatch, error) {
	if !r.ready() || guildID <= 0 || serverID <= 0 || playerID <= 0 {
		return nil, ErrInvalidBlackBox
	}
	return r.match(ctx, guildID, serverID, playerID, x, z, BlackBoxMarginMeters)
}

// MatchDismantle finds the bases a dismantle line (by the player's in-game id)
// falls inside. Only the base circle counts, as for the Base Raid Alarm.
func (r *BaseBlackBoxRepository) MatchDismantle(ctx context.Context, guildID, serverID int64, admPlayerID string, x, z float64) ([]BlackBoxMatch, error) {
	if !r.ready() || guildID <= 0 || serverID <= 0 || admPlayerID == "" {
		return nil, ErrInvalidBlackBox
	}
	var playerID int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM players WHERE guild_id=$1 AND dayz_player_id=$2`, guildID, admPlayerID).Scan(&playerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r.match(ctx, guildID, serverID, playerID, x, z, 0)
}

// Record adds a sighting to the base's history. Sightings of the same player
// and kind within ten minutes extend the latest entry instead of adding one.
func (r *BaseBlackBoxRepository) Record(ctx context.Context, m BlackBoxMatch, kind, playerName, detail string) error {
	if !r.ready() || m.BaseID <= 0 || m.PlayerID <= 0 || (kind != BlackBoxVisit && kind != BlackBoxDismantle) {
		return ErrInvalidBlackBox
	}
	if playerName == "" {
		playerName = "Unknown player"
	}
	playerName, detail = baseRaidClip(playerName, 128), baseRaidClip(detail, 128)
	_, err := r.pool.Exec(ctx, `
WITH latest AS (
 SELECT id FROM base_black_box_events
 WHERE installation_id=$1 AND server_id=$3 AND base_id=$4 AND kind=$5 AND player_id=$6
  AND last_seen_at > NOW() - make_interval(mins => $10)
 ORDER BY last_seen_at DESC LIMIT 1
), upd AS (
 UPDATE base_black_box_events e SET last_seen_at=NOW(),sightings=e.sightings+1,
  closest_meters=LEAST(e.closest_meters,$9),player_name=$7,detail=CASE WHEN $8<>'' THEN $8 ELSE e.detail END
 FROM latest WHERE e.id=latest.id RETURNING e.id
)
INSERT INTO base_black_box_events
 (installation_id,guild_id,server_id,base_id,kind,player_id,player_name,detail,closest_meters)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE NOT EXISTS (SELECT 1 FROM upd)`,
		m.InstallationID, m.GuildID, m.ServerID, m.BaseID, kind, m.PlayerID, playerName, detail, m.DistanceMeters, blackBoxMergeMinutes)
	return err
}

// Prune deletes history older than each server's retention (the default when
// the owner never saved one). It returns how many entries were removed.
func (r *BaseBlackBoxRepository) Prune(ctx context.Context) (int64, error) {
	if !r.ready() {
		return 0, ErrInvalidBlackBox
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM base_black_box_events e
 WHERE e.last_seen_at < NOW() - make_interval(days => COALESCE(
  (SELECT s.retention_days FROM base_black_box_settings s WHERE s.installation_id=e.installation_id AND s.server_id=e.server_id), $1))`,
		BlackBoxDefaultRetention)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Recent lists the newest history entries for one server, for the server owner.
func (r *BaseBlackBoxRepository) Recent(ctx context.Context, installationID, guildID, serverID int64, limit int) ([]BlackBoxEvent, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return nil, ErrInvalidBlackBox
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	return r.events(ctx, `WHERE e.installation_id=$1 AND e.guild_id=$2 AND e.server_id=$3`, limit, installationID, guildID, serverID)
}

// OwnerHistory lists the history of the bases one player owns on this server.
func (r *BaseBlackBoxRepository) OwnerHistory(ctx context.Context, installationID, guildID, serverID, ownerPlayerID int64, limit int) ([]BlackBoxBase, []BlackBoxEvent, error) {
	if !r.ready() || installationID <= 0 || guildID <= 0 || serverID <= 0 || ownerPlayerID <= 0 {
		return nil, nil, ErrInvalidBlackBox
	}
	if limit < 1 || limit > 500 {
		limit = 200
	}
	rows, err := r.pool.Query(ctx, `SELECT id,name FROM case_registered_bases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND owner_player_id=$4 AND state<>'REVOKED' ORDER BY id`,
		installationID, guildID, serverID, ownerPlayerID)
	if err != nil {
		return nil, nil, err
	}
	bases := make([]BlackBoxBase, 0)
	for rows.Next() {
		var b BlackBoxBase
		if err := rows.Scan(&b.ID, &b.Name); err != nil {
			rows.Close()
			return nil, nil, err
		}
		bases = append(bases, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	events, err := r.events(ctx, `WHERE e.installation_id=$1 AND e.guild_id=$2 AND e.server_id=$3 AND b.owner_player_id=$4 AND b.state<>'REVOKED'`,
		limit, installationID, guildID, serverID, ownerPlayerID)
	return bases, events, err
}

func (r *BaseBlackBoxRepository) events(ctx context.Context, where string, limit int, args ...any) ([]BlackBoxEvent, error) {
	args = append(args, limit)
	rows, err := r.pool.Query(ctx, `SELECT e.id,e.base_id,b.name,e.kind,e.player_name,e.detail,e.closest_meters,e.sightings,e.first_seen_at,e.last_seen_at
 FROM base_black_box_events e
 JOIN case_registered_bases b ON b.installation_id=e.installation_id AND b.guild_id=e.guild_id AND b.server_id=e.server_id AND b.id=e.base_id
 `+where+`
 ORDER BY e.last_seen_at DESC,e.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BlackBoxEvent, 0)
	for rows.Next() {
		var e BlackBoxEvent
		if err := rows.Scan(&e.ID, &e.BaseID, &e.BaseName, &e.Kind, &e.PlayerName, &e.Detail, &e.ClosestMeters, &e.Sightings,
			&e.FirstSeenAt, &e.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
