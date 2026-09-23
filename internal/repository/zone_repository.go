package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ZoneRepository backs Champion Phase 4 (docs/ZONES_UAV_RADAR.md): installation-scoped geographic
// zones, their ignore/authorization/ban lists, and the stateful intrusion engine's persisted
// presence/history tables. Zone CRUD is tenant-scoped (organizationID+installationID, matching
// every other Client Admin table); the intrusion-engine-facing methods (ActiveZonesForServer and
// everything below "intrusion engine support") are guild_id/server_id-scoped instead, since
// internal/killfeed - where the engine runs - only ever knows those, never organization/
// installation ids (see installation_zones' own denormalized guild_id/server_id columns,
// migration 0042).
type ZoneRepository struct{ pool *pgxpool.Pool }

func NewZoneRepository(pool *pgxpool.Pool) *ZoneRepository { return &ZoneRepository{pool: pool} }

// ErrZoneNotFound is returned when a zone does not exist or belongs to another installation.
var ErrZoneNotFound = errors.New("zone not found")

const (
	ZoneTypeSafezone   = "SAFEZONE"
	ZoneTypePVP        = "PVP"
	ZoneTypeRestricted = "RESTRICTED"
	ZoneTypeEvent      = "EVENT"
	ZoneTypeUAV        = "UAV"
	ZoneTypeBaseRadar  = "BASE_RADAR"
	ZoneTypeCustom     = "CUSTOM"
)

// IsUAVOrRadar reports whether zoneType requires permissions.CapUAVManage in addition to
// CapZoneManage (internal/app/saas_api_zones.go's create/update handlers).
func IsUAVOrRadar(zoneType string) bool {
	return zoneType == ZoneTypeUAV || zoneType == ZoneTypeBaseRadar
}

var validZoneTypes = map[string]bool{
	ZoneTypeSafezone: true, ZoneTypePVP: true, ZoneTypeRestricted: true, ZoneTypeEvent: true,
	ZoneTypeUAV: true, ZoneTypeBaseRadar: true, ZoneTypeCustom: true,
}

// ValidZoneType reports whether zoneType is one of the fixed, known zone types.
func ValidZoneType(zoneType string) bool { return validZoneTypes[zoneType] }

// Zone is one installation_zones row.
type Zone struct {
	ID                   int64
	InstallationID       int64
	GuildID              int64
	ServerID             int64
	Name                 string
	ZoneType             string
	CenterX, CenterZ     float64
	Radius               float64
	AlertChannelID       *string
	CooldownSeconds      int
	Enabled              bool
	CreatedByUserID      *int64
	CreatedAt, UpdatedAt time.Time
}

const zoneCols = "id,installation_id,guild_id,server_id,name,zone_type,center_x,center_z,radius,alert_channel_id,cooldown_seconds,enabled,created_by_user_id,created_at,updated_at"

func scanZone(row pgx.Row) (*Zone, error) {
	var z Zone
	if err := row.Scan(&z.ID, &z.InstallationID, &z.GuildID, &z.ServerID, &z.Name, &z.ZoneType, &z.CenterX, &z.CenterZ, &z.Radius,
		&z.AlertChannelID, &z.CooldownSeconds, &z.Enabled, &z.CreatedByUserID, &z.CreatedAt, &z.UpdatedAt); err != nil {
		return nil, err
	}
	return &z, nil
}

// --- zone CRUD (tenant-scoped) -------------------------------------------------------------------

// CreateZone inserts a new zone. guildID/serverID come from the caller's already-resolved
// AdminScope, never re-derived here, so a zone can never end up scoped to the wrong server.
func (r *ZoneRepository) CreateZone(ctx context.Context, installationID, guildID, serverID int64, name, zoneType string, centerX, centerZ, radius float64, alertChannelID *string, cooldownSeconds int, createdByUserID int64) (*Zone, error) {
	return scanZone(r.pool.QueryRow(ctx, `
INSERT INTO installation_zones(installation_id,guild_id,server_id,name,zone_type,center_x,center_z,radius,alert_channel_id,cooldown_seconds,created_by_user_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+zoneCols,
		installationID, guildID, serverID, name, zoneType, centerX, centerZ, radius, alertChannelID, cooldownSeconds, createdByUserID))
}

// GetZone returns a zone, requiring it belong to installationID (tenant isolation).
func (r *ZoneRepository) GetZone(ctx context.Context, installationID, zoneID int64) (*Zone, error) {
	z, err := scanZone(r.pool.QueryRow(ctx, `SELECT `+zoneCols+` FROM installation_zones WHERE id=$1 AND installation_id=$2`, zoneID, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrZoneNotFound
	}
	return z, err
}

// ListZones returns every zone for installationID, newest first.
func (r *ZoneRepository) ListZones(ctx context.Context, installationID int64) ([]Zone, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+zoneCols+` FROM installation_zones WHERE installation_id=$1 ORDER BY id DESC`, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Zone
	for rows.Next() {
		z, err := scanZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *z)
	}
	return out, rows.Err()
}

// ZoneUpdate carries the mutable fields of UpdateZone; nil means "leave unchanged".
type ZoneUpdate struct {
	Name             *string
	CenterX, CenterZ *float64
	Radius           *float64
	AlertChannelID   **string // set to a non-nil pointer-to-pointer to change (nil inner = clear)
	CooldownSeconds  *int
	Enabled          *bool
}

// UpdateZone applies a partial update, requiring zoneID belong to installationID.
func (r *ZoneRepository) UpdateZone(ctx context.Context, installationID, zoneID int64, u ZoneUpdate) (*Zone, error) {
	existing, err := r.GetZone(ctx, installationID, zoneID)
	if err != nil {
		return nil, err
	}
	if u.Name != nil {
		existing.Name = *u.Name
	}
	if u.CenterX != nil {
		existing.CenterX = *u.CenterX
	}
	if u.CenterZ != nil {
		existing.CenterZ = *u.CenterZ
	}
	if u.Radius != nil {
		existing.Radius = *u.Radius
	}
	if u.AlertChannelID != nil {
		existing.AlertChannelID = *u.AlertChannelID
	}
	if u.CooldownSeconds != nil {
		existing.CooldownSeconds = *u.CooldownSeconds
	}
	if u.Enabled != nil {
		existing.Enabled = *u.Enabled
	}
	return scanZone(r.pool.QueryRow(ctx, `
UPDATE installation_zones SET name=$1,center_x=$2,center_z=$3,radius=$4,alert_channel_id=$5,cooldown_seconds=$6,enabled=$7,updated_at=NOW()
WHERE id=$8 AND installation_id=$9 RETURNING `+zoneCols,
		existing.Name, existing.CenterX, existing.CenterZ, existing.Radius, existing.AlertChannelID, existing.CooldownSeconds, existing.Enabled, zoneID, installationID))
}

// DeleteZone removes a zone (and, via ON DELETE CASCADE, its ignore/authorized/ban/presence/
// intrusion rows), requiring it belong to installationID. Reports whether a row was deleted.
func (r *ZoneRepository) DeleteZone(ctx context.Context, installationID, zoneID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM installation_zones WHERE id=$1 AND installation_id=$2`, zoneID, installationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- ignore / authorized entries -----------------------------------------------------------------

const (
	ZoneEntryPlayer      = "PLAYER"
	ZoneEntryFaction     = "FACTION"
	ZoneEntryDiscordRole = "DISCORD_ROLE"
)

// ZoneIgnoreEntry is one zone_ignore_entries row.
type ZoneIgnoreEntry struct {
	ID              int64
	ZoneID          int64
	EntryType       string
	EntryValue      string
	CreatedByUserID *int64
	CreatedAt       time.Time
}

func (r *ZoneRepository) AddIgnoreEntry(ctx context.Context, zoneID int64, entryType, entryValue string, createdByUserID int64) (*ZoneIgnoreEntry, error) {
	var e ZoneIgnoreEntry
	err := r.pool.QueryRow(ctx, `
INSERT INTO zone_ignore_entries(zone_id,entry_type,entry_value,created_by_user_id) VALUES($1,$2,$3,$4)
ON CONFLICT (zone_id,entry_type,entry_value) DO UPDATE SET entry_type=EXCLUDED.entry_type
RETURNING id,zone_id,entry_type,entry_value,created_by_user_id,created_at`,
		zoneID, entryType, entryValue, createdByUserID).Scan(&e.ID, &e.ZoneID, &e.EntryType, &e.EntryValue, &e.CreatedByUserID, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *ZoneRepository) ListIgnoreEntries(ctx context.Context, zoneID int64) ([]ZoneIgnoreEntry, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,zone_id,entry_type,entry_value,created_by_user_id,created_at FROM zone_ignore_entries WHERE zone_id=$1 ORDER BY id`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneIgnoreEntry
	for rows.Next() {
		var e ZoneIgnoreEntry
		if err := rows.Scan(&e.ID, &e.ZoneID, &e.EntryType, &e.EntryValue, &e.CreatedByUserID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteIgnoreEntry removes an entry, requiring it belong to a zone of installationID.
func (r *ZoneRepository) DeleteIgnoreEntry(ctx context.Context, installationID, entryID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM zone_ignore_entries e USING installation_zones z
WHERE e.id=$1 AND e.zone_id=z.id AND z.installation_id=$2`, entryID, installationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ZoneAuthorizedEntry is one zone_authorized_entries row (PLAYER/FACTION only - never DISCORD_ROLE).
type ZoneAuthorizedEntry struct {
	ID              int64
	ZoneID          int64
	EntryType       string
	EntryValue      string
	CreatedByUserID *int64
	CreatedAt       time.Time
}

func (r *ZoneRepository) AddAuthorizedEntry(ctx context.Context, zoneID int64, entryType, entryValue string, createdByUserID int64) (*ZoneAuthorizedEntry, error) {
	var e ZoneAuthorizedEntry
	err := r.pool.QueryRow(ctx, `
INSERT INTO zone_authorized_entries(zone_id,entry_type,entry_value,created_by_user_id) VALUES($1,$2,$3,$4)
ON CONFLICT (zone_id,entry_type,entry_value) DO UPDATE SET entry_type=EXCLUDED.entry_type
RETURNING id,zone_id,entry_type,entry_value,created_by_user_id,created_at`,
		zoneID, entryType, entryValue, createdByUserID).Scan(&e.ID, &e.ZoneID, &e.EntryType, &e.EntryValue, &e.CreatedByUserID, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *ZoneRepository) ListAuthorizedEntries(ctx context.Context, zoneID int64) ([]ZoneAuthorizedEntry, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,zone_id,entry_type,entry_value,created_by_user_id,created_at FROM zone_authorized_entries WHERE zone_id=$1 ORDER BY id`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneAuthorizedEntry
	for rows.Next() {
		var e ZoneAuthorizedEntry
		if err := rows.Scan(&e.ID, &e.ZoneID, &e.EntryType, &e.EntryValue, &e.CreatedByUserID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *ZoneRepository) DeleteAuthorizedEntry(ctx context.Context, installationID, entryID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM zone_authorized_entries e USING installation_zones z
WHERE e.id=$1 AND e.zone_id=z.id AND z.installation_id=$2`, entryID, installationID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// --- zone bans (explicitly not server bans) --------------------------------------------------

// ZoneBan is one zone_bans row.
type ZoneBan struct {
	ID               int64
	ZoneID, PlayerID int64
	Reason           string
	Active           bool
	CreatedByUserID  *int64
	CreatedAt        time.Time
	LiftedAt         *time.Time
	LiftedByUserID   *int64
}

const zoneBanCols = "id,zone_id,player_id,COALESCE(reason,''),active,created_by_user_id,created_at,lifted_at,lifted_by_user_id"

func scanZoneBan(row pgx.Row) (*ZoneBan, error) {
	var b ZoneBan
	if err := row.Scan(&b.ID, &b.ZoneID, &b.PlayerID, &b.Reason, &b.Active, &b.CreatedByUserID, &b.CreatedAt, &b.LiftedAt, &b.LiftedByUserID); err != nil {
		return nil, err
	}
	return &b, nil
}

// AddZoneBan creates (or reactivates) an active ban for playerID on zoneID.
func (r *ZoneRepository) AddZoneBan(ctx context.Context, zoneID, playerID int64, reason string, createdByUserID int64) (*ZoneBan, error) {
	return scanZoneBan(r.pool.QueryRow(ctx, `
INSERT INTO zone_bans(zone_id,player_id,reason,created_by_user_id) VALUES($1,$2,NULLIF($3,''),$4)
ON CONFLICT (zone_id,player_id) WHERE active DO UPDATE SET reason=NULLIF(EXCLUDED.reason,'')
RETURNING `+zoneBanCols, zoneID, playerID, reason, createdByUserID))
}

func (r *ZoneRepository) ListZoneBans(ctx context.Context, zoneID int64) ([]ZoneBan, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+zoneBanCols+` FROM zone_bans WHERE zone_id=$1 ORDER BY id DESC`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneBan
	for rows.Next() {
		b, err := scanZoneBan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// LiftZoneBan deactivates an active ban, requiring it belong to a zone of installationID.
func (r *ZoneRepository) LiftZoneBan(ctx context.Context, installationID, banID, liftedByUserID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
UPDATE zone_bans b SET active=false, lifted_at=NOW(), lifted_by_user_id=$3
FROM installation_zones z WHERE b.id=$1 AND b.zone_id=z.id AND z.installation_id=$2 AND b.active`,
		banID, installationID, liftedByUserID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ActiveZoneBan returns the active ban for (zoneID, playerID), or nil if none - the intrusion
// engine's per-transition check.
func (r *ZoneRepository) ActiveZoneBan(ctx context.Context, zoneID, playerID int64) (*ZoneBan, error) {
	b, err := scanZoneBan(r.pool.QueryRow(ctx, `SELECT `+zoneBanCols+` FROM zone_bans WHERE zone_id=$1 AND player_id=$2 AND active`, zoneID, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// --- intrusion engine support (guild/server-scoped, called from internal/killfeed) -------------

// ActiveZonesForServer returns every enabled zone on serverID - the zone cache's refresh query
// (internal/killfeed/zone_cache.go). Never called per location event directly.
func (r *ZoneRepository) ActiveZonesForServer(ctx context.Context, serverID int64) ([]Zone, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+zoneCols+` FROM installation_zones WHERE server_id=$1 AND enabled ORDER BY id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Zone
	for rows.Next() {
		z, err := scanZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *z)
	}
	return out, rows.Err()
}

// IsIgnored reports whether playerID (optionally also a factionID) is on zoneID's ignore list via
// a PLAYER or FACTION entry. DISCORD_ROLE entries are handled separately by the caller (a live
// Discord role check, which this DB-only repository cannot perform) - see DiscordRoleIgnoreEntries.
func (r *ZoneRepository) IsIgnored(ctx context.Context, zoneID, playerID int64, factionID *int64) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM zone_ignore_entries
  WHERE zone_id=$1 AND (
    (entry_type='PLAYER' AND entry_value=$2::TEXT) OR
    (entry_type='FACTION' AND $3::BIGINT IS NOT NULL AND entry_value=$3::TEXT)
  )
)`, zoneID, strconv.FormatInt(playerID, 10), factionID).Scan(&ok)
	return ok, err
}

// DiscordRoleIgnoreEntries returns the raw Discord role snowflakes zoneID's ignore list names, for
// the caller to check against an actor's live roles (internal/killfeed.RoleChecker).
func (r *ZoneRepository) DiscordRoleIgnoreEntries(ctx context.Context, zoneID int64) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT entry_value FROM zone_ignore_entries WHERE zone_id=$1 AND entry_type='DISCORD_ROLE'`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// IsAuthorized reports whether playerID/factionID is on zoneID's authorized list (PLAYER/FACTION
// only - meaningful only for UAV/BASE_RADAR zones per task; the caller decides when to check it).
func (r *ZoneRepository) IsAuthorized(ctx context.Context, zoneID, playerID int64, factionID *int64) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM zone_authorized_entries
  WHERE zone_id=$1 AND (
    (entry_type='PLAYER' AND entry_value=$2::TEXT) OR
    (entry_type='FACTION' AND $3::BIGINT IS NOT NULL AND entry_value=$3::TEXT)
  )
)`, zoneID, strconv.FormatInt(playerID, 10), factionID).Scan(&ok)
	return ok, err
}

// PlayerDiscordUserID returns playerID's verified linked Discord user id, or nil if unlinked.
func (r *ZoneRepository) PlayerDiscordUserID(ctx context.Context, guildID, playerID int64) (*string, error) {
	var id *string
	err := r.pool.QueryRow(ctx, `SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$2 AND status='VERIFIED'`, guildID, playerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return id, err
}

// DiscordGuildID returns guildID's Discord guild snowflake, for a live Discord role check
// (internal/killfeed.RoleChecker) - the intrusion engine only ever knows the internal guild id.
func (r *ZoneRepository) DiscordGuildID(ctx context.Context, guildID int64) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `SELECT discord_guild_id FROM guilds WHERE id=$1`, guildID).Scan(&id)
	return id, err
}

// PlayerFactionID returns playerID's current active faction id, or nil if none.
func (r *ZoneRepository) PlayerFactionID(ctx context.Context, guildID, playerID int64) (*int64, error) {
	var id *int64
	err := r.pool.QueryRow(ctx, `SELECT faction_id FROM faction_members WHERE guild_id=$1 AND player_id=$2 AND active`, guildID, playerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return id, err
}

// --- presence (the intrusion engine's persisted transition-detection state) --------------------

const (
	PresenceInside  = "INSIDE"
	PresenceOutside = "OUTSIDE"
)

// ZonePresence is one zone_presence row.
type ZonePresence struct {
	ZoneID, PlayerID    int64
	Status              string
	EnteredAt           *time.Time
	LastSeenAt          time.Time
	LastLocationEventID *int64
	LastAlertAt         *time.Time
}

// GetPresence returns the persisted presence row for (zoneID, playerID), or nil if the pair has
// never been evaluated before (first-ever observation - never a fabricated OUTSIDE default, the
// caller treats "no row" as "unknown, treat this event as the first").
func (r *ZoneRepository) GetPresence(ctx context.Context, zoneID, playerID int64) (*ZonePresence, error) {
	var p ZonePresence
	err := r.pool.QueryRow(ctx, `
SELECT zone_id,player_id,status,entered_at,last_seen_at,last_location_event_id,last_alert_at
FROM zone_presence WHERE zone_id=$1 AND player_id=$2`, zoneID, playerID).
		Scan(&p.ZoneID, &p.PlayerID, &p.Status, &p.EnteredAt, &p.LastSeenAt, &p.LastLocationEventID, &p.LastAlertAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertPresence writes the current presence state. lastAlertAt, when non-nil, updates the
// cooldown anchor; pass nil to leave it unchanged (UpsertPresence never resets an existing anchor
// on its own - only the intrusion engine deciding to actually send an alert should move it).
func (r *ZoneRepository) UpsertPresence(ctx context.Context, zoneID, playerID int64, status string, enteredAt *time.Time, lastSeenAt time.Time, lastLocationEventID int64, lastAlertAt *time.Time) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO zone_presence(zone_id,player_id,status,entered_at,last_seen_at,last_location_event_id,last_alert_at)
VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (zone_id,player_id) DO UPDATE SET
  status=$3, entered_at=$4, last_seen_at=$5, last_location_event_id=$6,
  last_alert_at=COALESCE($7, zone_presence.last_alert_at)`,
		zoneID, playerID, status, enteredAt, lastSeenAt, lastLocationEventID, lastAlertAt)
	return err
}

// ListActivePresenceForZone returns every player currently persisted as INSIDE zoneID (task's
// "active intruders" API) - freshness/uncertainty is computed by the caller at read time from
// LastSeenAt, never stored.
func (r *ZoneRepository) ListActivePresenceForZone(ctx context.Context, zoneID int64) ([]ZonePresence, error) {
	rows, err := r.pool.Query(ctx, `
SELECT zone_id,player_id,status,entered_at,last_seen_at,last_location_event_id,last_alert_at
FROM zone_presence WHERE zone_id=$1 AND status='INSIDE' ORDER BY entered_at`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZonePresence
	for rows.Next() {
		var p ZonePresence
		if err := rows.Scan(&p.ZoneID, &p.PlayerID, &p.Status, &p.EnteredAt, &p.LastSeenAt, &p.LastLocationEventID, &p.LastAlertAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActiveIntruder is one zone's currently-open intrusion joined with its live presence row - the
// exact shape the active-intruders API needs (gamertag + entered_at from the intrusion, last_seen_at
// from presence for freshness/uncertainty classification, computed by the caller, never stored).
type ActiveIntruder struct {
	IntrusionID int64
	PlayerID    int64
	Gamertag    string
	Status      string
	Banned      bool
	EnteredAt   time.Time
	LastSeenAt  time.Time
}

// ListActiveIntrudersForZone returns zoneID's open (ACTIVE or ACKNOWLEDGED) intrusions joined with
// presence for freshness classification.
func (r *ZoneRepository) ListActiveIntrudersForZone(ctx context.Context, zoneID int64) ([]ActiveIntruder, error) {
	rows, err := r.pool.Query(ctx, `
SELECT i.id, i.player_id, i.gamertag, i.status, i.banned, i.entered_at, COALESCE(p.last_seen_at, i.entered_at)
FROM zone_intrusions i
LEFT JOIN zone_presence p ON p.zone_id=i.zone_id AND p.player_id=i.player_id
WHERE i.zone_id=$1 AND i.status<>'EXITED' ORDER BY i.entered_at`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveIntruder
	for rows.Next() {
		var a ActiveIntruder
		if err := rows.Scan(&a.IntrusionID, &a.PlayerID, &a.Gamertag, &a.Status, &a.Banned, &a.EnteredAt, &a.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- intrusion history -----------------------------------------------------------------------

const (
	IntrusionActive       = "ACTIVE"
	IntrusionExited       = "EXITED"
	IntrusionAcknowledged = "ACKNOWLEDGED"
)

// ZoneIntrusion is one zone_intrusions row.
type ZoneIntrusion struct {
	ID                                        int64
	ZoneID, InstallationID, GuildID, ServerID int64
	PlayerID                                  int64
	Gamertag                                  string
	Status                                    string
	Banned                                    bool
	EnteredAt                                 time.Time
	ExitedAt, AcknowledgedAt                  *time.Time
	AcknowledgedByUserID                      *int64
	LastAlertAt                               *time.Time
	AlertCount                                int
	CreatedAt                                 time.Time
	// PresenceLastSeenAt is populated only by ListActiveForInstallation (a LEFT JOIN against
	// zone_presence) - the freshness/uncertainty anchor for that API (task section 19). Zero on
	// every other query that returns a ZoneIntrusion.
	PresenceLastSeenAt time.Time
	// ZoneName/ZoneType are populated only by ListActiveForInstallation (a JOIN against
	// installation_zones) - empty on every other query.
	ZoneName, ZoneType string
}

const zoneIntrusionCols = "id,zone_id,installation_id,guild_id,server_id,player_id,gamertag,status,banned,entered_at,exited_at,acknowledged_at,acknowledged_by_user_id,last_alert_at,alert_count,created_at"

func scanZoneIntrusion(row pgx.Row) (*ZoneIntrusion, error) {
	var it ZoneIntrusion
	if err := row.Scan(&it.ID, &it.ZoneID, &it.InstallationID, &it.GuildID, &it.ServerID, &it.PlayerID, &it.Gamertag, &it.Status, &it.Banned,
		&it.EnteredAt, &it.ExitedAt, &it.AcknowledgedAt, &it.AcknowledgedByUserID, &it.LastAlertAt, &it.AlertCount, &it.CreatedAt); err != nil {
		return nil, err
	}
	return &it, nil
}

// GetOpenIntrusion returns the currently open (non-EXITED) intrusion for (zoneID, playerID), or
// nil - the intrusion engine's hot lookup for "is this player already tracked as inside".
func (r *ZoneRepository) GetOpenIntrusion(ctx context.Context, zoneID, playerID int64) (*ZoneIntrusion, error) {
	it, err := scanZoneIntrusion(r.pool.QueryRow(ctx, `SELECT `+zoneIntrusionCols+` FROM zone_intrusions WHERE zone_id=$1 AND player_id=$2 AND status<>'EXITED'`, zoneID, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// CreateIntrusion inserts a new ACTIVE intrusion row (one per OUTSIDE->INSIDE transition).
func (r *ZoneRepository) CreateIntrusion(ctx context.Context, zoneID, installationID, guildID, serverID, playerID int64, gamertag string, banned bool, enteredAt time.Time, alerted bool) (*ZoneIntrusion, error) {
	var lastAlertAt *time.Time
	alertCount := 0
	if alerted {
		lastAlertAt = &enteredAt
		alertCount = 1
	}
	return scanZoneIntrusion(r.pool.QueryRow(ctx, `
INSERT INTO zone_intrusions(zone_id,installation_id,guild_id,server_id,player_id,gamertag,status,banned,entered_at,last_alert_at,alert_count)
VALUES($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$8,$9,$10) RETURNING `+zoneIntrusionCols,
		zoneID, installationID, guildID, serverID, playerID, gamertag, banned, enteredAt, lastAlertAt, alertCount))
}

// RecordAlert bumps an existing open intrusion's alert bookkeeping (a reminder alert while the
// intrusion remains ACTIVE, once the cooldown has elapsed again).
func (r *ZoneRepository) RecordAlert(ctx context.Context, intrusionID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE zone_intrusions SET last_alert_at=$2, alert_count=alert_count+1 WHERE id=$1`, intrusionID, at)
	return err
}

// MarkExited closes an open intrusion (task: exit never deletes the row, only updates it).
func (r *ZoneRepository) MarkExited(ctx context.Context, intrusionID int64, exitedAt time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE zone_intrusions SET status='EXITED', exited_at=$2 WHERE id=$1 AND status<>'EXITED'`, intrusionID, exitedAt)
	return err
}

// AcknowledgeIntrusion marks an open (ACTIVE) intrusion ACKNOWLEDGED, requiring it belong to
// installationID. Reports whether a row was found and updated (false covers both "not found" and
// "already EXITED/ACKNOWLEDGED").
func (r *ZoneRepository) AcknowledgeIntrusion(ctx context.Context, installationID, intrusionID, ackByUserID int64) (*ZoneIntrusion, error) {
	it, err := scanZoneIntrusion(r.pool.QueryRow(ctx, `
UPDATE zone_intrusions SET status='ACKNOWLEDGED', acknowledged_at=NOW(), acknowledged_by_user_id=$3
WHERE id=$1 AND installation_id=$2 AND status='ACTIVE' RETURNING `+zoneIntrusionCols,
		intrusionID, installationID, ackByUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// ActiveIntrusionFilter narrows the installation-wide active-intrusions query.
type ActiveIntrusionFilter struct {
	ZoneID       int64 // 0 = any
	ZoneType     string
	PlayerID     int64 // 0 = any
	Acknowledged *bool // nil = any (ACTIVE or ACKNOWLEDGED both count as "active" here)
}

// ListActiveForInstallation returns every open (ACTIVE or ACKNOWLEDGED) intrusion for
// installationID, newest first, optionally filtered.
func (r *ZoneRepository) ListActiveForInstallation(ctx context.Context, installationID int64, f ActiveIntrusionFilter) ([]ZoneIntrusion, error) {
	query := `
SELECT i.id,i.zone_id,i.installation_id,i.guild_id,i.server_id,i.player_id,i.gamertag,i.status,i.banned,
       i.entered_at,i.exited_at,i.acknowledged_at,i.acknowledged_by_user_id,i.last_alert_at,i.alert_count,i.created_at,
       COALESCE(p.last_seen_at, i.entered_at), z.name, z.zone_type
FROM zone_intrusions i
JOIN installation_zones z ON z.id=i.zone_id
LEFT JOIN zone_presence p ON p.zone_id=i.zone_id AND p.player_id=i.player_id
WHERE i.installation_id=$1 AND i.status<>'EXITED'`
	args := []any{installationID}
	if f.ZoneID > 0 {
		args = append(args, f.ZoneID)
		query += ` AND i.zone_id=$` + strconv.Itoa(len(args))
	}
	if f.ZoneType != "" {
		args = append(args, f.ZoneType)
		query += ` AND z.zone_type=$` + strconv.Itoa(len(args))
	}
	if f.PlayerID > 0 {
		args = append(args, f.PlayerID)
		query += ` AND i.player_id=$` + strconv.Itoa(len(args))
	}
	if f.Acknowledged != nil {
		if *f.Acknowledged {
			query += ` AND i.status='ACKNOWLEDGED'`
		} else {
			query += ` AND i.status='ACTIVE'`
		}
	}
	query += ` ORDER BY i.entered_at DESC LIMIT 500`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneIntrusion
	for rows.Next() {
		var it ZoneIntrusion
		if err := rows.Scan(&it.ID, &it.ZoneID, &it.InstallationID, &it.GuildID, &it.ServerID, &it.PlayerID, &it.Gamertag, &it.Status, &it.Banned,
			&it.EnteredAt, &it.ExitedAt, &it.AcknowledgedAt, &it.AcknowledgedByUserID, &it.LastAlertAt, &it.AlertCount, &it.CreatedAt,
			&it.PresenceLastSeenAt, &it.ZoneName, &it.ZoneType); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// IntrusionHistoryFilter narrows the intrusion-history query (task section 13).
type IntrusionHistoryFilter struct {
	ZoneID, PlayerID int64 // 0 = any
	From, To         *time.Time
	Status           string // "" = any
	Before           int64  // keyset cursor on id
	Limit            int
}

// ListHistory returns installationID's intrusion history, newest first, keyset-paginated.
func (r *ZoneRepository) ListHistory(ctx context.Context, installationID int64, f IntrusionHistoryFilter) ([]ZoneIntrusion, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + zoneIntrusionCols + ` FROM zone_intrusions WHERE installation_id=$1`
	args := []any{installationID}
	if f.ZoneID > 0 {
		args = append(args, f.ZoneID)
		query += ` AND zone_id=$` + strconv.Itoa(len(args))
	}
	if f.PlayerID > 0 {
		args = append(args, f.PlayerID)
		query += ` AND player_id=$` + strconv.Itoa(len(args))
	}
	if f.From != nil {
		args = append(args, *f.From)
		query += ` AND entered_at >= $` + strconv.Itoa(len(args))
	}
	if f.To != nil {
		args = append(args, *f.To)
		query += ` AND entered_at <= $` + strconv.Itoa(len(args))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		query += ` AND status=$` + strconv.Itoa(len(args))
	}
	if f.Before > 0 {
		args = append(args, f.Before)
		query += ` AND id < $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY id DESC LIMIT $` + strconv.Itoa(len(args))
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneIntrusion
	for rows.Next() {
		it, err := scanZoneIntrusion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

// GetIntrusion returns one intrusion, requiring it belong to installationID.
func (r *ZoneRepository) GetIntrusion(ctx context.Context, installationID, intrusionID int64) (*ZoneIntrusion, error) {
	it, err := scanZoneIntrusion(r.pool.QueryRow(ctx, `SELECT `+zoneIntrusionCols+` FROM zone_intrusions WHERE id=$1 AND installation_id=$2`, intrusionID, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrZoneNotFound
	}
	return it, err
}
