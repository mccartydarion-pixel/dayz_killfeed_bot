package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FactionHubMembershipRepository answers which Faction Hub faction players belong to on a game
// server, through their verified gamertag link (live_faction_members).
type FactionHubMembershipRepository struct{ pool *pgxpool.Pool }

func NewFactionHubMembershipRepository(pool *pgxpool.Pool) *FactionHubMembershipRepository {
	return &FactionHubMembershipRepository{pool: pool}
}

// FactionsOnServer maps each of playerIDs that is in a Faction Hub faction on the installation
// of serverID to that faction's id, in one query. Players in no faction are absent.
func (r *FactionHubMembershipRepository) FactionsOnServer(ctx context.Context, guildID, serverID int64, playerIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || len(playerIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT DISTINCT ON (lfm.player_id) lfm.player_id, lfm.faction_id
FROM live_faction_members lfm
JOIN installations i ON i.id = lfm.installation_id AND i.game_server_id = $2
WHERE lfm.guild_id = $1 AND lfm.player_id = ANY($3)
ORDER BY lfm.player_id, lfm.faction_id`, guildID, serverID, playerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var player, faction int64
		if err := rows.Scan(&player, &faction); err != nil {
			return nil, err
		}
		out[player] = faction
	}
	return out, rows.Err()
}

// HubFactionRef names a Faction Hub faction and the installation it belongs to.
type HubFactionRef struct {
	ID, OrganizationID, InstallationID int64
	Tag, Name                          string
	// Role is the player's role in the faction (MemberFactions only).
	Role string
}

// Label is the faction's "[TAG] Name".
func (f HubFactionRef) Label() string { return "[" + f.Tag + "] " + f.Name }

const hubFactionRefSelect = `SELECT f.id,f.organization_id,f.installation_id,f.tag,f.name`

func scanHubFactionRefs(rows pgx.Rows, withRole bool) ([]HubFactionRef, error) {
	defer rows.Close()
	var out []HubFactionRef
	for rows.Next() {
		var f HubFactionRef
		dest := []any{&f.ID, &f.OrganizationID, &f.InstallationID, &f.Tag, &f.Name}
		if withRole {
			dest = append(dest, &f.Role)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FactionsByTag returns the Faction Hub factions tagged tag (case-insensitively) on the
// installations of the Discord server guildID (one per installation at most).
func (r *FactionHubMembershipRepository) FactionsByTag(ctx context.Context, guildID int64, tag string) ([]HubFactionRef, error) {
	tag = strings.TrimSpace(strings.Trim(strings.TrimSpace(tag), "[]"))
	if r == nil || r.pool == nil || guildID <= 0 || tag == "" {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, hubFactionRefSelect+`
FROM hub_factions f
JOIN installations i ON i.id=f.installation_id
JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
WHERE c.guild_id=$1 AND LOWER(f.tag)=LOWER($2)
ORDER BY f.installation_id`, guildID, tag)
	if err != nil {
		return nil, err
	}
	return scanHubFactionRefs(rows, false)
}

// MemberFactions returns the Faction Hub factions playerID belongs to on the Discord server
// guildID (one per installation at most), with the player's role in each.
func (r *FactionHubMembershipRepository) MemberFactions(ctx context.Context, guildID, playerID int64) ([]HubFactionRef, error) {
	if r == nil || r.pool == nil || guildID <= 0 || playerID <= 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, hubFactionRefSelect+`,lfm.role
FROM live_faction_members lfm
JOIN hub_factions f ON f.id=lfm.faction_id
WHERE lfm.guild_id=$1 AND lfm.player_id=$2
ORDER BY f.installation_id,f.id`, guildID, playerID)
	if err != nil {
		return nil, err
	}
	return scanHubFactionRefs(rows, true)
}

// HubRivalry is the kill exchange between two Faction Hub factions, from the factions recorded on
// each kill.
type HubRivalry struct {
	AKills, BKills, Wars int64
	// LongestPlayerID / TopPlayerID are 0 when there is no such kill.
	LongestPlayerID, TopPlayerID int64
	Longest                      float64
	TopKills                     int64
}

// Rivalry counts the kills between factions a and b on the Discord server guildID, their ended
// wars, the longest kill between them and the player with the most of those kills.
func (r *FactionHubMembershipRepository) Rivalry(ctx context.Context, guildID, a, b int64) (HubRivalry, error) {
	var out HubRivalry
	if r == nil || r.pool == nil || guildID <= 0 || a <= 0 || b <= 0 || a == b {
		return out, errors.New("invalid rivalry")
	}
	const between = `guild_id=$1 AND ((killer_hub_faction_id=$2 AND victim_hub_faction_id=$3) OR (killer_hub_faction_id=$3 AND victim_hub_faction_id=$2))`
	if err := r.pool.QueryRow(ctx, `SELECT
 COUNT(*) FILTER (WHERE killer_hub_faction_id=$2),COUNT(*) FILTER (WHERE killer_hub_faction_id=$3),
 (SELECT COUNT(*) FROM faction_wars WHERE guild_id=$1 AND status='ENDED'
   AND ((hub_faction_a_id=$2 AND hub_faction_b_id=$3) OR (hub_faction_a_id=$3 AND hub_faction_b_id=$2)))
FROM kills WHERE `+between, guildID, a, b).Scan(&out.AKills, &out.BKills, &out.Wars); err != nil {
		return out, err
	}
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,distance FROM kills WHERE `+between+`
 AND killer_player_id IS NOT NULL AND distance IS NOT NULL ORDER BY distance DESC,id LIMIT 1`, guildID, a, b).Scan(&out.LongestPlayerID, &out.Longest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	err = r.pool.QueryRow(ctx, `SELECT killer_player_id,COUNT(*) FROM kills WHERE `+between+`
 AND killer_player_id IS NOT NULL GROUP BY killer_player_id ORDER BY COUNT(*) DESC,killer_player_id LIMIT 1`, guildID, a, b).Scan(&out.TopPlayerID, &out.TopKills)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	return out, nil
}

// HubInstallationRef is an installation of a Discord server, with its game server's name.
type HubInstallationRef struct {
	ID, OrganizationID int64
	ServerName         string
}

// GuildInstallations lists the installations of the Discord server guildID that have a game
// server, in id order.
func (r *FactionHubMembershipRepository) GuildInstallations(ctx context.Context, guildID int64) ([]HubInstallationRef, error) {
	if r == nil || r.pool == nil || guildID <= 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT i.id,i.organization_id,COALESCE(NULLIF(gs.display_name,''),'Server '||gs.id::text)
FROM installations i
JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
JOIN game_servers gs ON gs.id=i.game_server_id
WHERE c.guild_id=$1
ORDER BY i.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HubInstallationRef
	for rows.Next() {
		var in HubInstallationRef
		if err := rows.Scan(&in.ID, &in.OrganizationID, &in.ServerName); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
