//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hubSeedMember is one member seedHubFaction adds: a DayZ player with a hub role (LEADER, OFFICER
// or MEMBER). Unlinked members join through a website account with no verified gamertag link.
type hubSeedMember struct {
	Player   int64
	Role     string
	Unlinked bool
}

// seedHubFaction founds a Faction Hub faction on guildRowID's installation and adds members the way
// the website does: each through an app_users account, with a VERIFIED player_links row for a
// linked member (an existing verified link is reused). It returns the hub faction id.
func seedHubFaction(t *testing.T, pool *pgxpool.Pool, guildRowID int64, tag string, members ...hubSeedMember) int64 {
	t.Helper()
	ctx := context.Background()
	var inst, org int64
	var server *int64
	if err := pool.QueryRow(ctx, `SELECT i.id, i.organization_id, i.game_server_id FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id WHERE c.guild_id=$1 ORDER BY i.id LIMIT 1`, guildRowID).Scan(&inst, &org, &server); err != nil {
		t.Fatalf("seedHubFaction: installation of guild %d: %v", guildRowID, err)
	}
	if server == nil {
		var id int64
		if err := pool.QueryRow(ctx, `SELECT id FROM game_servers WHERE guild_id=$1 ORDER BY id LIMIT 1`, guildRowID).Scan(&id); err != nil {
			t.Fatalf("seedHubFaction: server of guild %d: %v", guildRowID, err)
		}
		server = &id
	}
	suffix := time.Now().UnixNano()
	users := make([]int64, len(members))
	for i, m := range members {
		var discord string
		err := pool.QueryRow(ctx, `SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$2 AND status='VERIFIED'`, guildRowID, m.Player).Scan(&discord)
		if err != nil {
			discord = fmt.Sprintf("hubseed-%d-%d", m.Player, suffix)
			if !m.Unlinked {
				if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, guildRowID, m.Player, discord); err != nil {
					t.Fatalf("seedHubFaction: link player %d: %v", m.Player, err)
				}
			}
		}
		if err := pool.QueryRow(ctx, `INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,$1)
ON CONFLICT(discord_user_id) DO UPDATE SET discord_username=EXCLUDED.discord_username RETURNING id`, discord).Scan(&users[i]); err != nil {
			t.Fatalf("seedHubFaction: account of player %d: %v", m.Player, err)
		}
	}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO hub_factions(organization_id,installation_id,game_server_id,name,tag,slug,created_by_user_id)
VALUES($1,$2,$3,$4,$4,$5,$6) RETURNING id`, org, inst, *server, tag, fmt.Sprintf("%s-%d", tag, suffix), users[0]).Scan(&faction); err != nil {
		t.Fatalf("seedHubFaction: faction: %v", err)
	}
	for i, m := range members {
		role := m.Role
		if role == "" {
			role = "MEMBER"
		}
		var player any = m.Player
		if m.Unlinked {
			player = nil
		}
		if _, err := pool.Exec(ctx, `INSERT INTO hub_faction_members(faction_id,installation_id,user_id,player_id,role_key) VALUES($1,$2,$3,$4,$5)`,
			faction, inst, users[i], player, role); err != nil {
			t.Fatalf("seedHubFaction: member %d: %v", m.Player, err)
		}
	}
	return faction
}

// leaveHubFaction removes player's hub membership (a member who left).
func leaveHubFaction(t *testing.T, pool *pgxpool.Pool, guildRowID, player int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM hub_faction_members m USING app_users u, player_links l
WHERE u.id=m.user_id AND l.discord_user_id=u.discord_user_id AND l.guild_id=$1 AND l.player_id=$2`, guildRowID, player); err != nil {
		t.Fatal(err)
	}
}
