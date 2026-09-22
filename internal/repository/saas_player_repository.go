package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlayerServerRepository backs the player-facing SaaS API (docs/PLAYER_API.md): given a Discord-
// authenticated website user, which Champion installations they have a legitimate, PROVABLE
// association with as a tracked DayZ player (never as an organization member - players are not
// organization members, docs/SAAS_API.md), and their stats on one of those installations.
//
// "Legitimate association" always means BOTH of:
//  1. a VERIFIED player_links row for the installation's guild (identity - never a name match,
//     never an installation id the browser merely supplied);
//  2. observed activity (a player_server_activity row, or at least one kill/death) on that
//     installation's OWN game server - a verified link proves identity for the whole guild, but one
//     guild can back several installations/servers (installations.discord_guild_connection_id +
//     game_server_id), so identity alone is not proof for any one specific server.
//
// Neither guild membership nor faction membership alone is ever used as proof (both are checked
// separately from - and are weaker than - the verified link + observed activity used here).
type PlayerServerRepository struct{ pool *pgxpool.Pool }

func NewPlayerServerRepository(pool *pgxpool.Pool) *PlayerServerRepository {
	return &PlayerServerRepository{pool: pool}
}

// PlayerInstallation is one installation a verified player has observed activity on - the exact
// row shape PlayerServerSummary (docs/PLAYER_API.md) is built from.
type PlayerInstallation struct {
	InstallationID    int64
	OrganizationID    int64
	GuildID, ServerID int64
	PlayerID          int64
	ServerName        string
	Platform          string
	ServerStatus      string // installations.status
	DiscordGuildName  string
	LastSeenAt        *time.Time // player_server_activity.last_seen_at; nil = observed only via a kill/death row, no tracked session yet
}

// ListForDiscordUser returns every installation discordUserID has a legitimate player association
// with (see the type doc for what "legitimate" requires), most-recently-active first
// (LastSeenAt DESC NULLS LAST), then installation id ascending as a stable tie-break - the
// deterministic default-server rule docs/PLAYER_API.md documents.
func (r *PlayerServerRepository) ListForDiscordUser(ctx context.Context, discordUserID string) ([]PlayerInstallation, error) {
	const q = `
SELECT i.id, i.organization_id, pl.guild_id, i.game_server_id, pl.player_id,
       COALESCE(gs.display_name, ''), COALESCE(gs.platform, ''), i.status,
       COALESCE(c.guild_name, ''), psa.last_seen_at
FROM player_links pl
JOIN discord_guild_connections c ON c.guild_id = pl.guild_id
JOIN installations i ON i.discord_guild_connection_id = c.id AND i.game_server_id IS NOT NULL
JOIN game_servers gs ON gs.id = i.game_server_id
LEFT JOIN player_server_activity psa ON psa.guild_id = pl.guild_id AND psa.server_id = i.game_server_id AND psa.player_id = pl.player_id
WHERE pl.discord_user_id = $1 AND pl.status = 'VERIFIED'
  AND (
    psa.player_id IS NOT NULL
    OR EXISTS (SELECT 1 FROM kills k WHERE k.guild_id = pl.guild_id AND k.server_id = i.game_server_id AND k.killer_player_id = pl.player_id)
    OR EXISTS (SELECT 1 FROM deaths d WHERE d.guild_id = pl.guild_id AND d.server_id = i.game_server_id AND d.player_id = pl.player_id)
  )
ORDER BY psa.last_seen_at DESC NULLS LAST, i.id`
	rows, err := r.pool.Query(ctx, q, discordUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerInstallation
	for rows.Next() {
		var p PlayerInstallation
		if err := rows.Scan(&p.InstallationID, &p.OrganizationID, &p.GuildID, &p.ServerID, &p.PlayerID,
			&p.ServerName, &p.Platform, &p.ServerStatus, &p.DiscordGuildName, &p.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlayerInstallationScope is what installationID resolves to for a stats lookup.
type PlayerInstallationScope struct {
	InstallationID    int64
	OrganizationID    int64
	GuildID, ServerID int64
	ServerName        string
	Platform          string
	ServerStatus      string
	DiscordGuildName  string
	PlayerID          int64 // 0 when linked is false
}

// ResolvePlayerInstallation resolves installationID and, in the same query, checks whether
// discordUserID holds a VERIFIED player_links row for its guild. found=false means installationID
// does not exist (or has no DayZ server selected yet, and therefore can never have player
// activity). linked=false means the installation exists but discordUserID has no VERIFIED link for
// its guild - the caller maps that to PLAYER_IDENTITY_REQUIRED, never NOT_FOUND (section 9's stable
// error is about the CALLER's own identity state, not about installationID's existence, so it is
// safe to be explicit here without leaking anything installation-specific).
func (r *PlayerServerRepository) ResolvePlayerInstallation(ctx context.Context, installationID int64, discordUserID string) (scope PlayerInstallationScope, found, linked bool, err error) {
	const q = `
SELECT i.organization_id, c.guild_id, i.game_server_id, COALESCE(gs.display_name,''), COALESCE(gs.platform,''), i.status, COALESCE(c.guild_name,''),
       pl.player_id
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
LEFT JOIN game_servers gs ON gs.id = i.game_server_id
LEFT JOIN player_links pl ON pl.guild_id = c.guild_id AND pl.discord_user_id = $2 AND pl.status = 'VERIFIED'
WHERE i.id = $1 AND i.game_server_id IS NOT NULL`
	var playerID *int64
	err = r.pool.QueryRow(ctx, q, installationID, discordUserID).Scan(
		&scope.OrganizationID, &scope.GuildID, &scope.ServerID, &scope.ServerName, &scope.Platform, &scope.ServerStatus, &scope.DiscordGuildName, &playerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlayerInstallationScope{}, false, false, nil
	}
	if err != nil {
		return PlayerInstallationScope{}, false, false, err
	}
	scope.InstallationID = installationID
	if playerID == nil {
		return scope, true, false, nil
	}
	scope.PlayerID = *playerID
	return scope, true, true, nil
}

// HasObservedActivity reports whether playerID has any tracked activity (a session, a kill, or a
// death) on (guildID, serverID) - the proof required before that player's stats may be returned for
// that installation (never guild membership, faction membership, or the installation id alone).
func (r *PlayerServerRepository) HasObservedActivity(ctx context.Context, guildID, serverID, playerID int64) (bool, error) {
	const q = `
SELECT EXISTS(SELECT 1 FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3)
    OR EXISTS(SELECT 1 FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3)
    OR EXISTS(SELECT 1 FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=$3)`
	var ok bool
	err := r.pool.QueryRow(ctx, q, guildID, serverID, playerID).Scan(&ok)
	return ok, err
}

// KillStats aggregates kills/deaths/headshots/longshots/longest-kill for one player, scoped to one
// installation's OWN server (never the guild's other servers) - a genuinely new query, since the
// existing Discord-facing GetPlayerProfile is guild-wide only (docs/PLAYER_API.md "Scoping").
func (r *PlayerServerRepository) KillStats(ctx context.Context, guildID, serverID, playerID int64) (kills, deaths, headshots, longshots int, longestMeters float64, err error) {
	const q = `
SELECT
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3),
  (SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=$3),
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3 AND headshot),
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3 AND longshot),
  (SELECT COALESCE(MAX(distance),0) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3)`
	err = r.pool.QueryRow(ctx, q, guildID, serverID, playerID).Scan(&kills, &deaths, &headshots, &longshots, &longestMeters)
	return
}

// BountyStats aggregates claimed bounties for one player, scoped to one installation's server. A
// bounty's own server_id is nullable (NULL = a guild-wide bounty claimable on any server of the
// guild, the same scoping ClaimForKill already enforces at claim time - see bounty_repository.go),
// so a claim counts here when it was either NULL-scoped or scoped to exactly this server.
func (r *PlayerServerRepository) BountyStats(ctx context.Context, guildID, serverID, playerID int64) (claimed int, valuePoints int64, err error) {
	const q = `
SELECT COUNT(*), COALESCE(SUM(reward_points), 0)
FROM bounties
WHERE guild_id=$1 AND status='CLAIMED' AND claimed_by_player_id=$3 AND (server_id IS NULL OR server_id=$2)`
	err = r.pool.QueryRow(ctx, q, guildID, serverID, playerID).Scan(&claimed, &valuePoints)
	return
}

// CurrentFaction returns the acting Champion user's current Faction Hub faction on installationID
// (name, tag), if any - genuinely installation-scoped (hub_faction_members.installation_id), unlike
// the classic faction system's guild-wide FactionRepository.GetActiveFactionForPlayer, which is
// deliberately NOT used here since it would blend in another server's faction on a shared guild.
func (r *PlayerServerRepository) CurrentFaction(ctx context.Context, installationID, appUserID int64) (name, tag string, ok bool, err error) {
	const q = `
SELECT f.name, f.tag
FROM hub_faction_members m JOIN hub_factions f ON f.id = m.faction_id
WHERE m.installation_id=$1 AND m.user_id=$2`
	err = r.pool.QueryRow(ctx, q, installationID, appUserID).Scan(&name, &tag)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return name, tag, err == nil, err
}
