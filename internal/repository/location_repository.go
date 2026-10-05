package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LocationRepository backs Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): the authoritative
// player directory, location-event persistence, and location-history/online/retention queries.
type LocationRepository struct{ pool *pgxpool.Pool }

func NewLocationRepository(pool *pgxpool.Pool) *LocationRepository {
	return &LocationRepository{pool: pool}
}

// UpsertPlayer matches killfeed.LocationStore's method exactly (same idempotent
// ON CONFLICT(guild_id, dayz_player_id) upsert PlayerRepository already uses) so a location
// event's player_id is resolved through the identical, already-proven path - never a second
// player-identity implementation.
func (r *LocationRepository) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
INSERT INTO players(guild_id, dayz_player_id, display_name, last_seen_at)
VALUES($1,$2,$3,$4)
ON CONFLICT(guild_id, dayz_player_id) DO UPDATE SET display_name=$3, last_seen_at=$4, updated_at=NOW()
RETURNING id`, guildID, dayzID, displayName, seenAt).Scan(&id)
	return id, err
}

// InsertLocationEvents durably inserts a batch in the given order, using a single multi-row
// INSERT so batching is one round trip regardless of size. ON CONFLICT DO NOTHING against the
// migration's UNIQUE(player_id, server_id, event_type, observed_at) is the durable dedupe
// backstop against a duplicate ADM replay (task section 14). inserted counts only rows actually
// written (RETURNING id), excluding conflicts.
func (r *LocationRepository) InsertLocationEvents(ctx context.Context, events []LocationEventInput) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	const colsPerRow = 13
	query := `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,y,event_type,observed_at,source_file,source_offset,source_local_time,snapshot_ref) VALUES `
	args := make([]any, 0, len(events)*colsPerRow)
	for i, e := range events {
		if i > 0 {
			query += ","
		}
		base := i * colsPerRow
		query += "("
		for c := 1; c <= colsPerRow; c++ {
			if c > 1 {
				query += ","
			}
			query += placeholder(base + c)
		}
		query += ")"
		var file, snap any
		var offset any
		if e.SourceFile != "" {
			file, offset = e.SourceFile, e.SourceOffset
		}
		if e.SnapshotRef != "" {
			snap = e.SnapshotRef
		}
		args = append(args, e.GuildID, e.ServerID, e.PlayerID, e.Gamertag, e.X, e.Z, e.Y, e.EventType, e.ObservedAt, file, offset, e.SourceLocalTime, snap)
	}
	// Two partial unique indexes (migration 0050) deduplicate: a sourced row by its physical source
	// (server, file, offset, player, type) - an exact replay guard - and a legacy unsourced row by
	// the original (player, server, type, observed_at) key.
	query += ` ON CONFLICT DO NOTHING RETURNING id`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	inserted := 0
	for rows.Next() {
		inserted++
	}
	return inserted, rows.Err()
}

func placeholder(n int) string { return "$" + strconv.Itoa(n) }

// LocationEventInput is one location event to persist, referenced directly by
// killfeed.LocationStore's InsertLocationEvents signature - matching this codebase's existing
// layering exactly (internal/killfeed already depends on internal/repository for
// repository.KillRecord/DeathRecord in PersistenceStore; the dependency never runs the other way).
type LocationEventInput struct {
	GuildID    int64
	ServerID   int64
	PlayerID   int64
	Gamertag   string
	X, Z       float64
	Y          *float64
	EventType  string
	ObservedAt time.Time
	Source     string
	// Physical source (docs/CHAMPION_LIVE_SYNC.md): canonical ADM file name, byte offset at the end
	// of the line, the line's server-local time (zone-less) and the player-list snapshot identity.
	// SourceFile "" = unknown source (legacy rows; deduplicated by the old key).
	SourceFile      string
	SourceOffset    int64
	SourceLocalTime *time.Time
	SnapshotRef     string
}

// --- location freshness (task section 5) ---------------------------------------------------------

const (
	LocationFreshnessLiveRecent = "LIVE_RECENT"
	LocationFreshnessRecent     = "RECENT"
	LocationFreshnessStale      = "STALE"
)

// liveRecentMaxAge/recentMaxAge are the freshness thresholds (task: "Do not use the word 'live'
// unless age threshold qualifies"). ADM is polled roughly every 10s (docs/PERFORMANCE.md), so
// 60s tolerates a couple of missed/slow polls before downgrading from LIVE_RECENT; 5 minutes is
// the outer bound before a location is STALE rather than merely RECENT.
const (
	liveRecentMaxAge = 60 * time.Second
	recentMaxAge     = 5 * time.Minute
)

// ClassifyFreshness returns the freshness label for a location observed `age` ago.
func ClassifyFreshness(age time.Duration) string {
	switch {
	case age <= liveRecentMaxAge:
		return LocationFreshnessLiveRecent
	case age <= recentMaxAge:
		return LocationFreshnessRecent
	default:
		return LocationFreshnessStale
	}
}

// --- location history / latest (query-time derivation, no second truth) -------------------------

// LocationEvent is one read row of player_location_events.
type LocationEvent struct {
	ID         int64
	PlayerID   int64
	X, Z       float64
	Y          *float64
	EventType  string
	ObservedAt time.Time
	// SourceFile/SourceLocalTime are empty for rows written before Live Sync phase 1.
	SourceFile      string
	SourceLocalTime *time.Time
	// SourceUTC is SourceLocalTime converted with the server's UTC offset - set only when that
	// offset was learned from a restart.log line that states it (live_sync_server_clock). It is when
	// DayZ says the observation happened; ObservedAt is when Champion ingested it.
	SourceUTC *time.Time
	// CurrentSession is set by CurrentLocation: this row is from the server's current ADM session
	// and the player's current connection.
	CurrentSession bool
}

const locationEventCols = "e.id,e.player_id,e.x,e.z,e.y,e.event_type,e.observed_at,COALESCE(e.source_file,''),e.source_local_time," +
	"(e.source_local_time - make_interval(mins => (SELECT c.utc_offset_minutes FROM live_sync_server_clock c WHERE c.server_id = e.server_id))) AT TIME ZONE 'UTC'"

func scanLocationEvent(row pgx.Row) (LocationEvent, error) {
	var e LocationEvent
	err := row.Scan(&e.ID, &e.PlayerID, &e.X, &e.Z, &e.Y, &e.EventType, &e.ObservedAt, &e.SourceFile, &e.SourceLocalTime, &e.SourceUTC)
	return e, err
}

// SetCurrentADMSession records the ADM file the ingestion engine currently reads as the server's
// current boot session (killfeed.ADMSessionStore). The file identity is the server-session epoch.
//
// Live Sync phase 2: the session only moves forward. A file whose boot stamp is older than the
// recorded one (a lagging mount alias re-selected after a restart) never replaces it, and
// re-recording the same file keeps an end the live sync watcher already proved (ended_at), so a
// finished boot can never become CURRENT again. A genuinely new file starts a fresh, open session.
func (r *LocationRepository) SetCurrentADMSession(ctx context.Context, guildID, serverID int64, admFile string, localStart *time.Time) error {
	_, err := r.RecordADMSession(ctx, guildID, serverID, admFile, localStart)
	return err
}

// RecordADMSession is SetCurrentADMSession reporting whether the row was written: false means the
// database kept a newer recorded boot (killfeed.ADMSessionStore).
func (r *LocationRepository) RecordADMSession(ctx context.Context, guildID, serverID int64, admFile string, localStart *time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
INSERT INTO server_adm_sessions(server_id, guild_id, adm_file, session_local_start, selected_at) VALUES($1,$2,$3,$4::timestamp,NOW())
ON CONFLICT (server_id) DO UPDATE SET guild_id=EXCLUDED.guild_id, adm_file=EXCLUDED.adm_file, session_local_start=EXCLUDED.session_local_start, selected_at=NOW(),
    ended_at       = CASE WHEN server_adm_sessions.adm_file = EXCLUDED.adm_file THEN server_adm_sessions.ended_at END,
    ended_reason   = CASE WHEN server_adm_sessions.adm_file = EXCLUDED.adm_file THEN server_adm_sessions.ended_reason END,
    ended_evidence = CASE WHEN server_adm_sessions.adm_file = EXCLUDED.adm_file THEN server_adm_sessions.ended_evidence END
WHERE server_adm_sessions.adm_file = EXCLUDED.adm_file
   OR server_adm_sessions.session_local_start IS NULL OR EXCLUDED.session_local_start IS NULL
   OR EXCLUDED.session_local_start >= server_adm_sessions.session_local_start`,
		serverID, guildID, admFile, localStart)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// EndADMSessionBefore ends the server's current boot session when DayZ-written evidence proves a
// later boot or a completed shutdown at server-local time bootLocal: the session ends only if it
// started before bootLocal and is still open. ended=false means nothing changed (already ended,
// no session, or the session is the later boot itself). reason/evidence are short, sanitized
// labels (never a raw log line).
func (r *LocationRepository) EndADMSessionBefore(ctx context.Context, guildID, serverID int64, bootLocal time.Time, reason, evidence string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
UPDATE server_adm_sessions SET ended_at=NOW(), ended_reason=$4, ended_evidence=$5
WHERE server_id=$2 AND guild_id=$1 AND ended_at IS NULL
  AND session_local_start IS NOT NULL AND session_local_start < $3::timestamp`,
		guildID, serverID, bootLocal, reason, evidence)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ADMSession is the server's recorded boot session.
type ADMSession struct {
	ADMFile       string
	LocalStart    *time.Time
	SelectedAt    time.Time
	EndedAt       *time.Time
	EndedReason   string
	EndedEvidence string
}

// CurrentADMSession returns the recorded boot session, or nil when none was recorded.
func (r *LocationRepository) CurrentADMSession(ctx context.Context, guildID, serverID int64) (*ADMSession, error) {
	var s ADMSession
	err := r.pool.QueryRow(ctx, `SELECT adm_file, session_local_start, selected_at, ended_at, COALESCE(ended_reason,''), COALESCE(ended_evidence,'')
FROM server_adm_sessions WHERE guild_id=$1 AND server_id=$2`, guildID, serverID).
		Scan(&s.ADMFile, &s.LocalStart, &s.SelectedAt, &s.EndedAt, &s.EndedReason, &s.EndedEvidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CurrentLocation returns the player's position in the CURRENT session, or nil = UNKNOWN. A row
// qualifies only if (1) the player is currently connected, (2) it comes from the server's current
// ADM file (the boot session) and (3) it is at or after the player's latest connect in that file
// (the player session). Ordering is by byte offset within the one file, so no timezone is needed.
// A previous-session position is never returned as current (docs/CHAMPION_LIVE_SYNC.md), and
// neither is any position from a session DayZ evidence has proven ended (ended_at, phase 2).
func (r *LocationRepository) CurrentLocation(ctx context.Context, guildID, serverID, playerID int64) (*LocationEvent, error) {
	e, err := scanLocationEvent(r.pool.QueryRow(ctx, `
WITH s AS (SELECT adm_file FROM server_adm_sessions WHERE server_id=$2 AND guild_id=$1 AND ended_at IS NULL),
     online AS (SELECT COALESCE(bool_or(currently_connected), false) AS yes FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3),
     lastc AS (SELECT MAX(c.source_offset) AS o FROM player_location_events c JOIN s ON c.source_file = s.adm_file
               WHERE c.server_id=$2 AND c.player_id=$3 AND c.event_type='CONNECT')
SELECT `+locationEventCols+` FROM player_location_events e JOIN s ON e.source_file = s.adm_file CROSS JOIN online
WHERE e.guild_id=$1 AND e.server_id=$2 AND e.player_id=$3 AND online.yes
  AND e.source_offset >= COALESCE((SELECT o FROM lastc), 0)
ORDER BY e.source_offset DESC, e.id DESC LIMIT 1`, guildID, serverID, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.CurrentSession = true
	return &e, nil
}

// LatestLocation returns a player's single most recent location event, or nil if none exists.
// This is the ONLY "current location" read path (task section 6: "Do not store a second
// conflicting truth unless necessary") - always derived from player_location_events at query
// time, never a separately-maintained column.
func (r *LocationRepository) LatestLocation(ctx context.Context, guildID, serverID, playerID int64) (*LocationEvent, error) {
	latest, err := r.LatestLocations(ctx, guildID, serverID, []int64{playerID})
	if err != nil {
		return nil, err
	}
	return latest[playerID], nil
}

// LocationHistoryFilter narrows a location-history query (task section 7).
type LocationHistoryFilter struct {
	From, To  *time.Time
	EventType string // "" = any
	Before    int64  // keyset cursor: only rows with id < Before (0 = no cursor)
	Limit     int
}

// LocationHistory returns a player's location events, newest first, keyset-paginated on id.
func (r *LocationRepository) LocationHistory(ctx context.Context, guildID, serverID, playerID int64, f LocationHistoryFilter) ([]LocationEvent, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + locationEventCols + ` FROM player_location_events e WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`
	args := []any{guildID, serverID, playerID}
	if f.From != nil {
		args = append(args, *f.From)
		query += ` AND observed_at >= $` + strconv.Itoa(len(args))
	}
	if f.To != nil {
		args = append(args, *f.To)
		query += ` AND observed_at <= $` + strconv.Itoa(len(args))
	}
	if f.EventType != "" {
		args = append(args, f.EventType)
		query += ` AND event_type = $` + strconv.Itoa(len(args))
	}
	if f.Before > 0 {
		args = append(args, f.Before)
		query += ` AND id < $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY observed_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocationEvent
	for rows.Next() {
		e, err := scanLocationEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- retention (task section 9) -------------------------------------------------------------------

// DeleteOlderThan removes location events older than cutoff, in bounded batches (batchSize per
// call) so a large backlog never holds one long-running DELETE/lock. Returns the count removed
// by this call; the caller loops until it returns 0. Never touches kills/deaths (task: "Do not
// delete kill/death history" - this table has no FK relationship to either).
func (r *LocationRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 || batchSize > 5000 {
		batchSize = 2000
	}
	tag, err := r.pool.Exec(ctx, `
DELETE FROM player_location_events WHERE id IN (
  SELECT id FROM player_location_events WHERE created_at < $1 ORDER BY created_at LIMIT $2
)`, cutoff, batchSize)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// --- player directory (task sections 1-2) -----------------------------------------------------

// PlayerDirectoryEntry is one row of the authoritative player directory - real, observed data
// only (task: "Do not fabricate unavailable fields"). Nullable fields are pointers.
type PlayerDirectoryEntry struct {
	PlayerID                int64
	Gamertag                string
	DiscordUserID           *string
	DiscordDisplayName      *string
	Linked                  bool
	Online                  bool
	LastSeenAt              time.Time
	LastConnectedAt         *time.Time
	LastDisconnectedAt      *time.Time
	CurrentSessionStartedAt *time.Time
	Kills                   int64
	Deaths                  int64
	FactionID               *int64
	FactionName             *string
	WarningCount            int64
	// CurrentLocation is session-scoped (CurrentLocation); nil = UNKNOWN. LastKnownLocation is the
	// newest observation ever, which may be from an earlier session.
	CurrentLocation   *LocationEvent
	LastKnownLocation *LocationEvent
}

// PlayerDirectoryFilter narrows the directory listing (task section 1).
type PlayerDirectoryFilter struct {
	Query  string // matches display_name (ILIKE); "" = no filter
	Online *bool  // nil = any
	Linked *bool  // nil = any
	Before int64  // keyset cursor: only players.id < Before (0 = no cursor)
	Limit  int
}

// ListPlayerDirectory is the authoritative player directory (task section 1: "Do NOT piggyback
// on Economy account search" - this is a dedicated query against players/player_server_activity/
// kills/deaths/player_links/faction_members/player_warnings, never the economy repository).
// Sorted newest-player-id first (simple, stable keyset cursor - matches this codebase's existing
// admin cursor convention, admin_api.go's encodeAdminCursor/decodeAdminCursor).
//
// The kill, death and warning counts are counted per listed player (correlated subqueries in the
// select list, which Postgres evaluates for the page's rows only) instead of grouping every kill
// and death of the server for each page: the same numbers, without reading the whole server's
// history to show fifty players.
func (r *LocationRepository) ListPlayerDirectory(ctx context.Context, guildID, serverID int64, f PlayerDirectoryFilter) ([]PlayerDirectoryEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `
SELECT p.id, p.display_name,
       pl.discord_user_id, u.discord_username,
       COALESCE(a.currently_connected,false), a.last_seen_at, a.first_seen_at, a.current_session_started_at,
       (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.server_id=$2 AND k.killer_player_id=p.id),
       (SELECT COUNT(*) FROM deaths d WHERE d.guild_id=$1 AND d.server_id=$2 AND d.player_id=p.id),
       fm.faction_id, f.name,
       (SELECT COUNT(*) FROM player_warnings w WHERE w.guild_id=$1 AND w.cleared=false AND w.player_id=p.id)
FROM players p
LEFT JOIN player_links pl ON pl.guild_id=p.guild_id AND pl.player_id=p.id AND pl.status='VERIFIED'
LEFT JOIN app_users u ON u.discord_user_id=pl.discord_user_id
LEFT JOIN player_server_activity a ON a.guild_id=p.guild_id AND a.server_id=$2 AND a.player_id=p.id
LEFT JOIN faction_members fm ON fm.guild_id=p.guild_id AND fm.player_id=p.id AND fm.active
LEFT JOIN factions f ON f.id=fm.faction_id
WHERE p.guild_id=$1`
	args := []any{guildID, serverID}
	if f.Query != "" {
		args = append(args, "%"+escapeLike(f.Query)+"%")
		query += ` AND p.display_name ILIKE $` + strconv.Itoa(len(args))
	}
	if f.Online != nil {
		args = append(args, *f.Online)
		query += ` AND COALESCE(a.currently_connected,false) = $` + strconv.Itoa(len(args))
	}
	if f.Linked != nil {
		if *f.Linked {
			query += ` AND pl.discord_user_id IS NOT NULL`
		} else {
			query += ` AND pl.discord_user_id IS NULL`
		}
	}
	if f.Before > 0 {
		args = append(args, f.Before)
		query += ` AND p.id < $` + strconv.Itoa(len(args))
	}
	args = append(args, limit)
	query += ` ORDER BY p.id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerDirectoryEntry
	for rows.Next() {
		var e PlayerDirectoryEntry
		var lastSeenAt, firstSeenAt *time.Time
		if err := rows.Scan(&e.PlayerID, &e.Gamertag, &e.DiscordUserID, &e.DiscordDisplayName,
			&e.Online, &lastSeenAt, &firstSeenAt, &e.CurrentSessionStartedAt,
			&e.Kills, &e.Deaths, &e.FactionID, &e.FactionName, &e.WarningCount); err != nil {
			return nil, err
		}
		e.Linked = e.DiscordUserID != nil
		if lastSeenAt != nil {
			e.LastSeenAt = *lastSeenAt
			if !e.Online {
				e.LastDisconnectedAt = lastSeenAt
			}
		}
		if e.Online {
			e.LastConnectedAt = e.CurrentSessionStartedAt
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Latest location is attached per row after the main query (task section 1's field list
	// includes it, but it's a per-player derived lookup, not something worth an expensive
	// LATERAL join on every directory page for players a caller may not even scroll to).
	// It is two statements for the whole page, not two per player: a page of 200 online players
	// used to make 400 round trips. A failed lookup leaves the locations unset, as before.
	ids := make([]int64, len(out))
	for i := range out {
		ids[i] = out[i].PlayerID
	}
	if current, err := r.CurrentLocations(ctx, guildID, serverID, ids); err == nil {
		for i := range out {
			out[i].CurrentLocation = current[out[i].PlayerID]
		}
	}
	if latest, err := r.LatestLocations(ctx, guildID, serverID, ids); err == nil {
		for i := range out {
			out[i].LastKnownLocation = latest[out[i].PlayerID]
		}
	}
	return out, nil
}

// CurrentLocations is CurrentLocation for many players in one statement: the same rule, applied
// per player. Players whose current position is unknown are absent from the map.
func (r *LocationRepository) CurrentLocations(ctx context.Context, guildID, serverID int64, playerIDs []int64) (map[int64]*LocationEvent, error) {
	out := map[int64]*LocationEvent{}
	if len(playerIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
WITH s AS (SELECT adm_file FROM server_adm_sessions WHERE server_id=$2 AND guild_id=$1 AND ended_at IS NULL)
SELECT `+locationEventCols+`
FROM unnest($3::bigint[]) AS ids(player_id)
CROSS JOIN LATERAL (
  SELECT ev.* FROM player_location_events ev JOIN s ON ev.source_file = s.adm_file
  WHERE ev.guild_id=$1 AND ev.server_id=$2 AND ev.player_id=ids.player_id
    AND (SELECT COALESCE(bool_or(a.currently_connected), false) FROM player_server_activity a
         WHERE a.guild_id=$1 AND a.server_id=$2 AND a.player_id=ids.player_id)
    AND ev.source_offset >= COALESCE((SELECT MAX(c.source_offset) FROM player_location_events c JOIN s cs ON c.source_file = cs.adm_file
                                      WHERE c.server_id=$2 AND c.player_id=ids.player_id AND c.event_type='CONNECT'), 0)
  ORDER BY ev.source_offset DESC, ev.id DESC LIMIT 1
) e`, guildID, serverID, playerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanLocationEvent(rows)
		if err != nil {
			return nil, err
		}
		e.CurrentSession = true
		out[e.PlayerID] = &e
	}
	return out, rows.Err()
}

// LatestLocations is LatestLocation for many players in one statement. Players without any
// location event on the server are absent from the map.
//
// The newest row is found in two index steps: the newest observed_at for (server, player) read
// off the (player_id, observed_at DESC) index, then the highest id at that instant. It is the row
// "ORDER BY observed_at DESC, id DESC LIMIT 1" returns. Written as that ORDER BY, Postgres read
// and sorted every location row the player has (several milliseconds for a regular player, per
// player). The inner step does not repeat guild_id on purpose: a server belongs to one guild, the
// outer step still checks it, and the extra filter made the planner expect no match and fall
// back to reading everything.
func (r *LocationRepository) LatestLocations(ctx context.Context, guildID, serverID int64, playerIDs []int64) (map[int64]*LocationEvent, error) {
	out := map[int64]*LocationEvent{}
	if len(playerIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT `+locationEventCols+`
FROM unnest($3::bigint[]) AS ids(player_id)
CROSS JOIN LATERAL (
  SELECT ev.* FROM player_location_events ev
  WHERE ev.guild_id=$1 AND ev.server_id=$2 AND ev.player_id=ids.player_id
    AND ev.observed_at = (SELECT m.observed_at FROM player_location_events m
                          WHERE m.server_id=$2 AND m.player_id=ids.player_id
                          ORDER BY m.observed_at DESC LIMIT 1)
  ORDER BY ev.id DESC LIMIT 1
) e`, guildID, serverID, playerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanLocationEvent(rows)
		if err != nil {
			return nil, err
		}
		out[e.PlayerID] = &e
	}
	return out, rows.Err()
}

// OnlinePlayers returns every currently-connected player on a server plus their latest known
// location (task section 8). Never marks a disconnected player online - sourced directly from
// player_server_activity.currently_connected, the same column the existing lastOnline endpoint
// (Phase 1) already treats as authoritative.
func (r *LocationRepository) OnlinePlayers(ctx context.Context, guildID, serverID int64) ([]PlayerDirectoryEntry, error) {
	online := true
	return r.ListPlayerDirectory(ctx, guildID, serverID, PlayerDirectoryFilter{Online: &online, Limit: 200})
}
