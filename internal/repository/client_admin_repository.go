package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInstallationScopeNotFound is returned when an (organizationID, installationID) pair does not
// resolve to a real installation - either it doesn't exist, or it belongs to another organization.
var ErrInstallationScopeNotFound = errors.New("installation not found")

// AdminScope is an installation resolved to the identifiers every client-admin capability needs:
// the internal guild id (for guild-scoped repository queries), the installation's Discord guild
// snowflake (for a live Discord role/permission lookup), and the optional game server id.
type AdminScope struct {
	OrganizationID int64
	InstallationID int64
	GuildID        int64
	DiscordGuildID string
	ServerID       *int64
}

type ClientAdminRepository struct{ pool *pgxpool.Pool }

func NewClientAdminRepository(pool *pgxpool.Pool) *ClientAdminRepository {
	return &ClientAdminRepository{pool: pool}
}

// Scope resolves organizationID+installationID to an AdminScope, matching the join shape already
// used by EconomyRepository.InstallationScope/HubStatsRepository.LeaderboardScope, plus the
// Discord guild snowflake those two don't need but every capability here does (to look up an
// actor's live Discord roles).
func (r *ClientAdminRepository) Scope(ctx context.Context, organizationID, installationID int64) (AdminScope, error) {
	s := AdminScope{OrganizationID: organizationID, InstallationID: installationID}
	var server *int64
	err := r.pool.QueryRow(ctx, `
SELECT g.id, g.discord_guild_id, i.game_server_id
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
WHERE i.id = $1 AND i.organization_id = $2`, installationID, organizationID).Scan(&s.GuildID, &s.DiscordGuildID, &server)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminScope{}, ErrInstallationScopeNotFound
	}
	if err != nil {
		return AdminScope{}, err
	}
	s.ServerID = server
	return s, nil
}

// --- warnings (task "DISCORD MODERATION" / "warnings") ---------------------------------------

type PlayerWarning struct {
	ID              int64
	GuildID         int64
	PlayerID        int64
	Reason          string
	IssuedByUserID  *int64
	IssuedAt        time.Time
	Cleared         bool
	ClearedByUserID *int64
	ClearedAt       *time.Time
}

const warningCols = "id,guild_id,player_id,reason,issued_by_user_id,issued_at,cleared,cleared_by_user_id,cleared_at"

func scanWarning(row pgx.Row) (PlayerWarning, error) {
	var w PlayerWarning
	err := row.Scan(&w.ID, &w.GuildID, &w.PlayerID, &w.Reason, &w.IssuedByUserID, &w.IssuedAt, &w.Cleared, &w.ClearedByUserID, &w.ClearedAt)
	return w, err
}

// IssueWarning records a new warning. Warnings are never deleted (only cleared - see
// ClearWarning), so this is a pure insert.
func (r *ClientAdminRepository) IssueWarning(ctx context.Context, guildID, playerID int64, reason string, issuedByUserID int64) (PlayerWarning, error) {
	return scanWarning(r.pool.QueryRow(ctx, `
INSERT INTO player_warnings(guild_id,player_id,reason,issued_by_user_id) VALUES($1,$2,$3,$4)
RETURNING `+warningCols, guildID, playerID, reason, issuedByUserID))
}

// ListWarnings returns a player's warnings newest-first, including cleared ones (the caller's DTO
// layer decides what to show by default - the audit trail itself is never filtered out here).
func (r *ClientAdminRepository) ListWarnings(ctx context.Context, guildID, playerID int64, limit int) ([]PlayerWarning, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `SELECT `+warningCols+` FROM player_warnings WHERE guild_id=$1 AND player_id=$2 ORDER BY issued_at DESC LIMIT $3`, guildID, playerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerWarning
	for rows.Next() {
		w, err := scanWarning(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ErrWarningNotFound is returned by ClearWarning when the warning doesn't exist, belongs to
// another guild, or was already cleared.
var ErrWarningNotFound = errors.New("warning not found or already cleared")

// ClearWarning marks one warning CLEARED (task: "Do not erase audit trail. Mark warnings:
// CLEARED with: clearedBy, clearedAt") - the row and its original reason/issuer are preserved.
func (r *ClientAdminRepository) ClearWarning(ctx context.Context, guildID, warningID, clearedByUserID int64) (PlayerWarning, error) {
	w, err := scanWarning(r.pool.QueryRow(ctx, `
UPDATE player_warnings SET cleared=true, cleared_by_user_id=$3, cleared_at=NOW()
WHERE guild_id=$1 AND id=$2 AND cleared=false
RETURNING `+warningCols, guildID, warningID, clearedByUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PlayerWarning{}, ErrWarningNotFound
	}
	return w, err
}

// --- server display name / feed location / maintenance mode ----------------------------------

// SetServerDisplayName updates a game server's Champion-facing display name (task's
// "setServerName" - explicitly NOT a Nitrado server rename, just the name Champion shows in its
// own feeds/panels).
func (r *ClientAdminRepository) SetServerDisplayName(ctx context.Context, guildID, serverID int64, name string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE game_servers SET display_name=$1,updated_at=NOW() WHERE id=$2 AND guild_id=$3`, name, serverID, guildID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrInstallationScopeNotFound
	}
	return nil
}

// SetFeedShowLocation toggles whether a channel route's embeds include location fields (task's
// "location" capability).
func (r *ClientAdminRepository) SetFeedShowLocation(ctx context.Context, installationID int64, routeKey string, show bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE installation_channel_routes SET show_location=$1,updated_at=NOW() WHERE installation_id=$2 AND route_key=$3`, show, installationID, routeKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrInstallationScopeNotFound
	}
	return nil
}

// SetMaintenanceMode toggles a server's maintenance flag (task's "SERVER MAINTENANCE MODE" -
// Champion-side state only; this deliberately never touches the Nitrado server itself).
func (r *ClientAdminRepository) SetMaintenanceMode(ctx context.Context, serverID int64, enabled bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE server_configs SET maintenance_mode=$1,updated_at=NOW() WHERE server_id=$2`, enabled, serverID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrInstallationScopeNotFound
	}
	return nil
}

// --- last online (task's "lastOnline") ----------------------------------------------------------

// LastOnline is one player's most recently observed connect/disconnect state on a server, backed
// entirely by the existing player_server_activity tracking (internal/killfeed's presence
// pipeline) - no new tracking is introduced here.
type LastOnline struct {
	PlayerID             int64
	ServerID             int64
	FirstSeenAt          time.Time
	LastSeenAt           time.Time
	CurrentlyConnected   bool
	CurrentSessionStart  *time.Time
	TotalObservedSeconds int64
}

func (r *ClientAdminRepository) LastOnline(ctx context.Context, guildID, serverID, playerID int64) (*LastOnline, error) {
	var lo LastOnline
	lo.PlayerID, lo.ServerID = playerID, serverID
	err := r.pool.QueryRow(ctx, `
SELECT first_seen_at,last_seen_at,currently_connected,current_session_started_at,total_observed_seconds
FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`, guildID, serverID, playerID).
		Scan(&lo.FirstSeenAt, &lo.LastSeenAt, &lo.CurrentlyConnected, &lo.CurrentSessionStart, &lo.TotalObservedSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lo, nil
}

// --- whitelist / ban list metadata (task's "ACCESS CONTROL") ------------------------------------

// AccessEntry is one whitelist or ban-list record. Nitrado's own list has no notion of reason,
// notes or expiry (its API takes only a player identifier), so this table is authoritative for
// that metadata; Nitrado is only ever the enforcement side effect (see
// internal/nitrado.WhitelistAdd/BanlistAdd).
type AccessEntry struct {
	ID              int64
	InstallationID  int64
	ListType        string // "WHITELIST" or "BANLIST"
	Identifier      string
	Reason          string
	Notes           string
	ExpiresAt       *time.Time
	CreatedByUserID *int64
	CreatedAt       time.Time
	RemovedAt       *time.Time
	RemovedByUserID *int64
}

const accessEntryCols = "id,installation_id,list_type,identifier,COALESCE(reason,''),COALESCE(notes,''),expires_at,created_by_user_id,created_at,removed_at,removed_by_user_id"

func scanAccessEntry(row pgx.Row) (AccessEntry, error) {
	var e AccessEntry
	err := row.Scan(&e.ID, &e.InstallationID, &e.ListType, &e.Identifier, &e.Reason, &e.Notes, &e.ExpiresAt, &e.CreatedByUserID, &e.CreatedAt, &e.RemovedAt, &e.RemovedByUserID)
	return e, err
}

// AddAccessEntry records a new active whitelist/banlist entry. The caller must call the matching
// internal/nitrado enforcement method (WhitelistAdd/BanlistAdd) and only persist this record once
// that call succeeds (task: "Audit enforcement mechanism first" - Champion's record must never
// claim an entry is enforced when Nitrado rejected it).
func (r *ClientAdminRepository) AddAccessEntry(ctx context.Context, installationID int64, listType, identifier, reason, notes string, expiresAt *time.Time, createdByUserID int64) (AccessEntry, error) {
	return scanAccessEntry(r.pool.QueryRow(ctx, `
INSERT INTO installation_access_entries(installation_id,list_type,identifier,reason,notes,expires_at,created_by_user_id)
VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7)
RETURNING `+accessEntryCols, installationID, listType, identifier, reason, notes, expiresAt, createdByUserID))
}

// ErrAccessEntryNotFound is returned when an active entry for (installationID, listType,
// identifier) does not exist.
var ErrAccessEntryNotFound = errors.New("access entry not found")

// RemoveAccessEntry soft-removes the currently-active entry for identifier (preserving history).
func (r *ClientAdminRepository) RemoveAccessEntry(ctx context.Context, installationID int64, listType, identifier string, removedByUserID int64) (AccessEntry, error) {
	e, err := scanAccessEntry(r.pool.QueryRow(ctx, `
UPDATE installation_access_entries SET removed_at=NOW(), removed_by_user_id=$4
WHERE installation_id=$1 AND list_type=$2 AND identifier=$3 AND removed_at IS NULL
RETURNING `+accessEntryCols, installationID, listType, identifier, removedByUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessEntry{}, ErrAccessEntryNotFound
	}
	return e, err
}

// ListAccessEntries returns currently-active entries for a list type, newest first.
func (r *ClientAdminRepository) ListAccessEntries(ctx context.Context, installationID int64, listType string, limit int) ([]AccessEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.pool.Query(ctx, `SELECT `+accessEntryCols+` FROM installation_access_entries WHERE installation_id=$1 AND list_type=$2 AND removed_at IS NULL ORDER BY created_at DESC LIMIT $3`, installationID, listType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessEntry
	for rows.Next() {
		e, err := scanAccessEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- faction admin (task's "FACTION ADMINISTRATION") --------------------------------------------

// FactionSummary is the admin-console view of one faction - enough to list, judge activity, and
// decide whether to dissolve, without pulling in the full Faction Hub stats aggregation.
type FactionSummary struct {
	ID            int64
	Name, Tag     string
	OwnerPlayerID int64
	Active        bool
	MemberCount   int
	CreatedAt     time.Time
}

// ListFactionsForAdmin lists every faction in a guild (active and dissolved), newest first, with a
// live member count - the admin console's own list, separate from any player-facing faction
// browse endpoint.
func (r *ClientAdminRepository) ListFactionsForAdmin(ctx context.Context, guildID int64, limit int) ([]FactionSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
SELECT f.id,f.name,f.tag,f.owner_player_id,f.active,f.created_at,
       (SELECT COUNT(*) FROM faction_members m WHERE m.guild_id=f.guild_id AND m.faction_id=f.id AND m.active)
FROM factions f WHERE f.guild_id=$1 ORDER BY f.created_at DESC LIMIT $2`, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactionSummary
	for rows.Next() {
		var s FactionSummary
		if err := rows.Scan(&s.ID, &s.Name, &s.Tag, &s.OwnerPlayerID, &s.Active, &s.CreatedAt, &s.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
