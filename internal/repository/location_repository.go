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
	const colsPerRow = 9
	query := `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,y,event_type,observed_at) VALUES `
	args := make([]any, 0, len(events)*colsPerRow)
	for i, e := range events {
		if i > 0 {
			query += ","
		}
		base := i * colsPerRow
		query += "(" + placeholder(base+1) + "," + placeholder(base+2) + "," + placeholder(base+3) + "," + placeholder(base+4) + "," +
			placeholder(base+5) + "," + placeholder(base+6) + "," + placeholder(base+7) + "," + placeholder(base+8) + "," + placeholder(base+9) + ")"
		args = append(args, e.GuildID, e.ServerID, e.PlayerID, e.Gamertag, e.X, e.Z, e.Y, e.EventType, e.ObservedAt)
	}
	query += ` ON CONFLICT (player_id, server_id, event_type, observed_at) DO NOTHING RETURNING id`
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
}

const locationEventCols = "id,player_id,x,z,y,event_type,observed_at"

func scanLocationEvent(row pgx.Row) (LocationEvent, error) {
	var e LocationEvent
	err := row.Scan(&e.ID, &e.PlayerID, &e.X, &e.Z, &e.Y, &e.EventType, &e.ObservedAt)
	return e, err
}

// LatestLocation returns a player's single most recent location event, or nil if none exists.
// This is the ONLY "current location" read path (task section 6: "Do not store a second
// conflicting truth unless necessary") - always derived from player_location_events at query
// time, never a separately-maintained column.
func (r *LocationRepository) LatestLocation(ctx context.Context, guildID, serverID, playerID int64) (*LocationEvent, error) {
	e, err := scanLocationEvent(r.pool.QueryRow(ctx, `
SELECT `+locationEventCols+` FROM player_location_events
WHERE guild_id=$1 AND server_id=$2 AND player_id=$3
ORDER BY observed_at DESC, id DESC LIMIT 1`, guildID, serverID, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
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
	query := `SELECT ` + locationEventCols + ` FROM player_location_events WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`
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
	CurrentLocation         *LocationEvent
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
func (r *LocationRepository) ListPlayerDirectory(ctx context.Context, guildID, serverID int64, f PlayerDirectoryFilter) ([]PlayerDirectoryEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `
SELECT p.id, p.display_name,
       pl.discord_user_id, u.discord_username,
       COALESCE(a.currently_connected,false), a.last_seen_at, a.first_seen_at, a.current_session_started_at,
       COALESCE(k.n,0), COALESCE(d.n,0),
       fm.faction_id, f.name,
       COALESCE(w.n,0)
FROM players p
LEFT JOIN player_links pl ON pl.guild_id=p.guild_id AND pl.player_id=p.id AND pl.status='VERIFIED'
LEFT JOIN app_users u ON u.discord_user_id=pl.discord_user_id
LEFT JOIN player_server_activity a ON a.guild_id=p.guild_id AND a.server_id=$2 AND a.player_id=p.id
LEFT JOIN (SELECT killer_player_id, COUNT(*) n FROM kills WHERE guild_id=$1 AND server_id=$2 GROUP BY killer_player_id) k ON k.killer_player_id=p.id
LEFT JOIN (SELECT player_id, COUNT(*) n FROM deaths WHERE guild_id=$1 AND server_id=$2 GROUP BY player_id) d ON d.player_id=p.id
LEFT JOIN faction_members fm ON fm.guild_id=p.guild_id AND fm.player_id=p.id AND fm.active
LEFT JOIN factions f ON f.id=fm.faction_id
LEFT JOIN (SELECT player_id, COUNT(*) n FROM player_warnings WHERE guild_id=$1 AND cleared=false GROUP BY player_id) w ON w.player_id=p.id
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
	for i := range out {
		loc, err := r.LatestLocation(ctx, guildID, serverID, out[i].PlayerID)
		if err == nil {
			out[i].CurrentLocation = loc
		}
	}
	return out, nil
}

// OnlinePlayers returns every currently-connected player on a server plus their latest known
// location (task section 8). Never marks a disconnected player online - sourced directly from
// player_server_activity.currently_connected, the same column the existing lastOnline endpoint
// (Phase 1) already treats as authoritative.
func (r *LocationRepository) OnlinePlayers(ctx context.Context, guildID, serverID int64) ([]PlayerDirectoryEntry, error) {
	online := true
	return r.ListPlayerDirectory(ctx, guildID, serverID, PlayerDirectoryFilter{Online: &online, Limit: 200})
}
