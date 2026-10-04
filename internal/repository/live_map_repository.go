package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LiveMapRepository reads what the live map shows (docs/LIVE_MAP.md): the installation's public
// identity and settings, the server's clock and occupancy, the pressure points, members' latest
// positions, their bases and raid alerts, and the staff history. Nothing is written. Every
// position query is one bulk statement - the map polls, so it must never cost one query per
// player.
type LiveMapRepository struct{ pool *pgxpool.Pool }

func NewLiveMapRepository(pool *pgxpool.Pool) *LiveMapRepository {
	return &LiveMapRepository{pool: pool}
}

// LiveMapInstallation is one installation with a game server, as the public map needs it.
type LiveMapInstallation struct {
	InstallationID, OrganizationID, GuildID, ServerID int64
	Name, Platform                                    string
	ProviderServiceID                                 string
	MapKey                                            string // shop_delivery_settings.map_key, "" when unset
	Settings                                          LiveMapSettings
}

// Installation resolves installationID, or (nil, nil) when it does not exist or has no game server.
func (r *LiveMapRepository) Installation(ctx context.Context, installationID int64) (*LiveMapInstallation, error) {
	var in LiveMapInstallation
	err := r.pool.QueryRow(ctx, `
SELECT i.id, i.organization_id, c.guild_id, gs.id, COALESCE(NULLIF(gs.display_name, ''), 'DayZ Server'), COALESCE(gs.platform, ''), COALESCE(gs.provider_service_id, ''),
       COALESCE(sd.map_key, ''), COALESCE(s.live_map_public, TRUE), COALESCE(s.live_map_delay_seconds, 0), COALESCE(s.live_map_faction_layer, TRUE)
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
LEFT JOIN installation_feature_settings s ON s.installation_id = i.id
LEFT JOIN shop_delivery_settings sd ON sd.installation_id = i.id
WHERE i.id = $1`, installationID).Scan(&in.InstallationID, &in.OrganizationID, &in.GuildID, &in.ServerID, &in.Name, &in.Platform, &in.ProviderServiceID,
		&in.MapKey, &in.Settings.Public, &in.Settings.DelaySeconds, &in.Settings.FactionLayer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// LiveMapClock is the latest ADM wall-clock reading of the current boot: the server-local
// (zone-less) time stamped on the newest kill or location line, and when Champion ingested it.
type LiveMapClock struct {
	LocalTime  time.Time
	ObservedAt time.Time
}

// LiveMapStatus is the server's occupancy and clock facts.
type LiveMapStatus struct {
	PlayersOnline    int
	LastActivityAt   *time.Time
	UTCOffsetMinutes *int        // live_sync_server_clock, nil until learned
	Session          *ADMSession // the current boot, nil when none was recorded or it has ended
	Clock            *LiveMapClock
}

// Status reads the occupancy and clock facts in two statements. The clock reading comes from the
// current ADM session's own lines (source_file = the session's file); without an open session it
// falls back to the newest stamped line of the last day, so a server whose boot was never
// recorded still shows a clock.
func (r *LiveMapRepository) Status(ctx context.Context, guildID, serverID int64, now time.Time) (LiveMapStatus, error) {
	var st LiveMapStatus
	var session ADMSession
	var admFile *string
	err := r.pool.QueryRow(ctx, `
SELECT (SELECT COUNT(*) FROM player_server_activity a WHERE a.guild_id=$1 AND a.server_id=$2 AND a.currently_connected)::int,
       (SELECT MAX(a.last_seen_at) FROM player_server_activity a WHERE a.guild_id=$1 AND a.server_id=$2),
       (SELECT c.utc_offset_minutes FROM live_sync_server_clock c WHERE c.server_id=$2),
       s.adm_file, s.session_local_start, s.selected_at
FROM (SELECT 1) one
LEFT JOIN server_adm_sessions s ON s.server_id=$2 AND s.guild_id=$1 AND s.ended_at IS NULL`, guildID, serverID).
		Scan(&st.PlayersOnline, &st.LastActivityAt, &st.UTCOffsetMinutes, &admFile, &session.LocalStart, &session.SelectedAt)
	if err != nil {
		return st, err
	}
	if admFile != nil {
		session.ADMFile = *admFile
		st.Session = &session
	}
	file := ""
	if st.Session != nil {
		file = st.Session.ADMFile
	}
	since := now.Add(-24 * time.Hour)
	var clock LiveMapClock
	err = r.pool.QueryRow(ctx, `
SELECT local_time, observed_at FROM (
  SELECT e.source_local_time AS local_time, e.observed_at FROM player_location_events e
  WHERE e.server_id=$1 AND e.guild_id=$2 AND e.observed_at >= $3 AND e.source_local_time IS NOT NULL AND ($4 = '' OR e.source_file = $4)
  UNION ALL
  SELECT k.source_local_time, k.created_at FROM kills k
  WHERE k.server_id=$1 AND k.guild_id=$2 AND k.created_at >= $3 AND k.source_local_time IS NOT NULL AND ($4 = '' OR k.source_file = $4)
) t ORDER BY local_time DESC, observed_at DESC LIMIT 1`, serverID, guildID, since, file).Scan(&clock.LocalTime, &clock.ObservedAt)
	if err == nil {
		st.Clock = &clock
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return st, err
	}
	return st, nil
}

// LiveMapPoint is one position with its time.
type LiveMapPoint struct {
	X, Z float64
	At   time.Time
}

// PressurePoints returns the KILL and HIT location rows of [from, to): every participant's
// position on every kill and hit line, the raw material of the pressure grid. At most limit rows,
// newest first, so a saturated window keeps the freshest fights.
func (r *LiveMapRepository) PressurePoints(ctx context.Context, guildID, serverID int64, from, to time.Time, limit int) ([]LiveMapPoint, error) {
	rows, err := r.pool.Query(ctx, `
SELECT x, z, observed_at FROM player_location_events
WHERE server_id=$1 AND guild_id=$2 AND observed_at >= $3 AND observed_at < $4 AND event_type IN ('KILL','HIT')
ORDER BY observed_at DESC LIMIT $5`, serverID, guildID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveMapPoint{}
	for rows.Next() {
		var p LiveMapPoint
		if err := rows.Scan(&p.X, &p.Z, &p.At); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LiveMapPosition is one player's persisted position sample.
type LiveMapPosition struct {
	PlayerID   int64
	X, Z       float64
	ObservedAt time.Time
	EventType  string
}

// sessionFilter restricts location rows to the current boot: rows from the session's own ADM
// file, plus rows without a recorded file (written before Live Sync) observed since the session
// was selected. With no file every row since `since` qualifies.
const sessionFilter = ` AND e.observed_at >= $3 AND ($4 = '' OR e.source_file = $4 OR e.source_file IS NULL)`

// LatestPositions returns each player's newest location row (any event type, PLAYER_LIST
// included) observed since `since` and, when admFile is set, within that boot - one statement
// for every player, keyed by player id. Players without a row are absent.
func (r *LiveMapRepository) LatestPositions(ctx context.Context, serverID int64, playerIDs []int64, since time.Time, admFile string) (map[int64]LiveMapPosition, error) {
	out := map[int64]LiveMapPosition{}
	if len(playerIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT DISTINCT ON (e.player_id) e.player_id, e.x, e.z, e.observed_at, e.event_type
FROM player_location_events e
WHERE e.server_id=$1 AND e.player_id = ANY($2)`+sessionFilter+`
ORDER BY e.player_id, e.observed_at DESC, e.id DESC`, serverID, playerIDs, since, admFile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p LiveMapPosition
		if err := rows.Scan(&p.PlayerID, &p.X, &p.Z, &p.ObservedAt, &p.EventType); err != nil {
			return nil, err
		}
		out[p.PlayerID] = p
	}
	return out, rows.Err()
}

// Trails returns, per player, up to perPlayer positions before the latest one (the latest
// itself excluded), oldest first, observed since `since` under the same boot rule as
// LatestPositions. One statement for every player.
func (r *LiveMapRepository) Trails(ctx context.Context, serverID int64, playerIDs []int64, since time.Time, admFile string, perPlayer int) (map[int64][]LiveMapPosition, error) {
	out := map[int64][]LiveMapPosition{}
	if len(playerIDs) == 0 || perPlayer <= 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT player_id, x, z, observed_at, event_type FROM (
  SELECT e.player_id, e.x, e.z, e.observed_at, e.event_type,
         ROW_NUMBER() OVER (PARTITION BY e.player_id ORDER BY e.observed_at DESC, e.id DESC) AS rn
  FROM player_location_events e
  WHERE e.server_id=$1 AND e.player_id = ANY($2)`+sessionFilter+`
) t WHERE rn BETWEEN 2 AND $5 + 1
ORDER BY player_id, observed_at, rn DESC`, serverID, playerIDs, since, admFile, perPlayer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p LiveMapPosition
		if err := rows.Scan(&p.PlayerID, &p.X, &p.Z, &p.ObservedAt, &p.EventType); err != nil {
			return nil, err
		}
		out[p.PlayerID] = append(out[p.PlayerID], p)
	}
	return out, rows.Err()
}

// LiveMapFaction is the acting player's hub faction identity.
type LiveMapFaction struct {
	ID             int64
	Name, Tag      string
	PrimaryColor   *string
	SecondaryColor *string
}

// CurrentFaction returns the Discord user's hub faction on the installation (the same membership
// PlayerServerRepository.CurrentFaction reads, with the identity the map draws), or (nil, nil)
// when they are in none.
func (r *LiveMapRepository) CurrentFaction(ctx context.Context, installationID int64, discordUserID string) (*LiveMapFaction, error) {
	var f LiveMapFaction
	err := r.pool.QueryRow(ctx, `
SELECT f.id, f.name, f.tag, f.primary_color, f.secondary_color
FROM hub_faction_members m
JOIN hub_factions f ON f.id = m.faction_id
JOIN app_users u ON u.id = m.user_id
WHERE m.installation_id=$1 AND u.discord_user_id=$2`, installationID, discordUserID).Scan(&f.ID, &f.Name, &f.Tag, &f.PrimaryColor, &f.SecondaryColor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// LiveMapPlayer is one player on the map: identity, online truth and hub faction tag.
type LiveMapPlayer struct {
	PlayerID   int64
	Gamertag   string
	Online     bool
	FactionTag *string
}

// FactionMembers lists the hub faction's members that have a DayZ identity, with their online
// state on serverID, by gamertag.
func (r *LiveMapRepository) FactionMembers(ctx context.Context, installationID, factionID, guildID, serverID int64) ([]LiveMapPlayer, error) {
	rows, err := r.pool.Query(ctx, `
SELECT p.id, p.display_name, COALESCE(a.currently_connected, FALSE), f.tag
FROM hub_faction_members m
JOIN hub_factions f ON f.id = m.faction_id
JOIN players p ON p.id = m.player_id
LEFT JOIN player_server_activity a ON a.guild_id=$3 AND a.server_id=$4 AND a.player_id=p.id
WHERE m.installation_id=$1 AND m.faction_id=$2 AND m.player_id IS NOT NULL
ORDER BY LOWER(p.display_name), p.id`, installationID, factionID, guildID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLiveMapPlayers(rows)
}

// Player returns one player's identity and online state (the acting player when they have no
// faction), or (nil, nil) when the player does not exist.
func (r *LiveMapRepository) Player(ctx context.Context, installationID, guildID, serverID, playerID int64) (*LiveMapPlayer, error) {
	var p LiveMapPlayer
	err := r.pool.QueryRow(ctx, `
SELECT p.id, p.display_name, COALESCE(a.currently_connected, FALSE), f.tag
FROM players p
LEFT JOIN player_server_activity a ON a.guild_id=$2 AND a.server_id=$3 AND a.player_id=p.id
LEFT JOIN hub_faction_members m ON m.installation_id=$1 AND m.player_id=p.id
LEFT JOIN hub_factions f ON f.id = m.faction_id
WHERE p.id=$4 AND p.guild_id=$2`, installationID, guildID, serverID, playerID).Scan(&p.PlayerID, &p.Gamertag, &p.Online, &p.FactionTag)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// OnlinePlayers lists every currently connected player of the server with their hub faction tag
// on the installation, by gamertag. At most 500.
func (r *LiveMapRepository) OnlinePlayers(ctx context.Context, installationID, guildID, serverID int64) ([]LiveMapPlayer, error) {
	rows, err := r.pool.Query(ctx, `
SELECT p.id, p.display_name, TRUE, f.tag
FROM player_server_activity a
JOIN players p ON p.id = a.player_id
LEFT JOIN hub_faction_members m ON m.installation_id=$1 AND m.player_id=p.id
LEFT JOIN hub_factions f ON f.id = m.faction_id
WHERE a.guild_id=$2 AND a.server_id=$3 AND a.currently_connected
ORDER BY LOWER(p.display_name), p.id LIMIT 500`, installationID, guildID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLiveMapPlayers(rows)
}

func scanLiveMapPlayers(rows pgx.Rows) ([]LiveMapPlayer, error) {
	out := []LiveMapPlayer{}
	for rows.Next() {
		var p LiveMapPlayer
		if err := rows.Scan(&p.PlayerID, &p.Gamertag, &p.Online, &p.FactionTag); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LiveMapBase is one registered base the faction layer draws.
type LiveMapBase struct {
	ID               int64
	Name             string
	CenterX, CenterZ float64
	Radius           float64
	OwnerPlayerID    int64
}

// MemberBases lists the live (not revoked) registered bases of the installation's server that a
// member owns, or that a member - or a classic faction a member belongs to - holds a current
// authorization for. One statement.
func (r *LiveMapRepository) MemberBases(ctx context.Context, installationID, guildID, serverID int64, playerIDs []int64, now time.Time) ([]LiveMapBase, error) {
	out := []LiveMapBase{}
	if len(playerIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT b.id, b.name, b.center_x, b.center_z, b.radius, b.owner_player_id
FROM case_registered_bases b
WHERE b.installation_id=$1 AND b.guild_id=$2 AND b.server_id=$3 AND b.state <> 'REVOKED'
  AND (b.owner_player_id = ANY($4)
    OR EXISTS (SELECT 1 FROM case_base_authorizations a
               WHERE a.base_id=b.id AND a.installation_id=b.installation_id AND a.valid_from <= $5 AND (a.valid_until IS NULL OR a.valid_until > $5)
                 AND (a.player_id = ANY($4)
                   OR a.faction_id IN (SELECT fm.faction_id FROM faction_members fm WHERE fm.guild_id=$2 AND fm.player_id = ANY($4) AND fm.active))))
ORDER BY b.id`, installationID, guildID, serverID, playerIDs, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b LiveMapBase
		if err := rows.Scan(&b.ID, &b.Name, &b.CenterX, &b.CenterZ, &b.Radius, &b.OwnerPlayerID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LiveMapAlert is one base raid alert.
type LiveMapAlert struct {
	BaseID             int64
	BaseName           string
	RaiderName         string
	Part, Target, Tool string
	At                 time.Time
}

// RaidAlerts lists the raid alerts of baseIDs since `since`, newest first, at most limit.
func (r *LiveMapRepository) RaidAlerts(ctx context.Context, installationID int64, baseIDs []int64, since time.Time, limit int) ([]LiveMapAlert, error) {
	out := []LiveMapAlert{}
	if len(baseIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT al.base_id, b.name, al.raider_name, al.part, al.target, al.tool, al.created_at
FROM base_raid_alerts al JOIN case_registered_bases b ON b.id = al.base_id
WHERE al.installation_id=$1 AND al.base_id = ANY($2) AND al.created_at >= $3
ORDER BY al.created_at DESC, al.id DESC LIMIT $4`, installationID, baseIDs, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a LiveMapAlert
		if err := rows.Scan(&a.BaseID, &a.BaseName, &a.RaiderName, &a.Part, &a.Target, &a.Tool, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LiveMapTrackPoint is one position sample of the staff history, with the player's name.
type LiveMapTrackPoint struct {
	PlayerID  int64
	Gamertag  string
	At        time.Time
	X, Z      float64
	EventType string
}

// Tracks returns every player's location rows in [from, to] (all event types, PLAYER_LIST
// included), newest first, at most limit rows - the caller asks for one more than it will keep
// to learn whether the window was truncated.
func (r *LiveMapRepository) Tracks(ctx context.Context, guildID, serverID int64, from, to time.Time, limit int) ([]LiveMapTrackPoint, error) {
	rows, err := r.pool.Query(ctx, `
SELECT e.player_id, COALESCE(NULLIF(p.display_name, ''), e.gamertag), e.observed_at, e.x, e.z, e.event_type
FROM player_location_events e LEFT JOIN players p ON p.id = e.player_id
WHERE e.server_id=$1 AND e.guild_id=$2 AND e.observed_at >= $3 AND e.observed_at <= $4
ORDER BY e.observed_at DESC, e.id DESC LIMIT $5`, serverID, guildID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveMapTrackPoint{}
	for rows.Next() {
		var p LiveMapTrackPoint
		if err := rows.Scan(&p.PlayerID, &p.Gamertag, &p.At, &p.X, &p.Z, &p.EventType); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
