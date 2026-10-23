package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ServerPanelRepository answers the Discord server control panel (/server panel): which game
// servers a Discord server can control and who owns them.
type ServerPanelRepository struct{ pool *pgxpool.Pool }

func NewServerPanelRepository(pool *pgxpool.Pool) *ServerPanelRepository {
	return &ServerPanelRepository{pool: pool}
}

// PanelServer is one installation of a Discord server that has a game server.
type PanelServer struct {
	InstallationID, OrganizationID, GuildID, GameServerID int64
	// ServiceID is the Nitrado service id.
	ServiceID string
	Name      string
	// MapKey is the map the server runs (as followed from Nitrado, or set by staff); "" when unknown.
	MapKey string
	// OwnerDiscordID is the Discord account of the organization's owner.
	OwnerDiscordID string
}

// Servers lists the installations of the Discord server discordGuildID (its snowflake) that have a
// Nitrado game server, in installation order.
func (r *ServerPanelRepository) Servers(ctx context.Context, discordGuildID string) ([]PanelServer, error) {
	if r == nil || r.pool == nil || discordGuildID == "" {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT i.id, i.organization_id, g.id, gs.id, gs.provider_service_id,
 COALESCE(NULLIF(gs.display_name,''), 'Server '||gs.id::text), COALESCE(sd.map_key,''), COALESCE(u.discord_user_id,'')
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
JOIN game_servers gs ON gs.id = i.game_server_id AND gs.provider = 'nitrado'
JOIN organizations o ON o.id = i.organization_id
LEFT JOIN app_users u ON u.id = o.owner_user_id
LEFT JOIN shop_delivery_settings sd ON sd.installation_id = i.id
WHERE g.discord_guild_id = $1
ORDER BY i.id`, discordGuildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PanelServer
	for rows.Next() {
		var s PanelServer
		if err := rows.Scan(&s.InstallationID, &s.OrganizationID, &s.GuildID, &s.GameServerID, &s.ServiceID, &s.Name, &s.MapKey, &s.OwnerDiscordID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
