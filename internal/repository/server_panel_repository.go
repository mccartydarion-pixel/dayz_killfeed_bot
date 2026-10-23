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
	// Maintenance is the server's maintenance switch (server_configs.maintenance_mode).
	Maintenance bool
}

// Servers lists the installations of the Discord server discordGuildID (its snowflake) that have a
// Nitrado game server, in installation order.
func (r *ServerPanelRepository) Servers(ctx context.Context, discordGuildID string) ([]PanelServer, error) {
	if r == nil || r.pool == nil || discordGuildID == "" {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT i.id, i.organization_id, g.id, gs.id, gs.provider_service_id,
 COALESCE(NULLIF(gs.display_name,''), 'Server '||gs.id::text), COALESCE(sd.map_key,''), COALESCE(u.discord_user_id,''),
 COALESCE(sc.maintenance_mode, FALSE)
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
JOIN game_servers gs ON gs.id = i.game_server_id AND UPPER(gs.provider) = 'NITRADO'
JOIN organizations o ON o.id = i.organization_id
LEFT JOIN app_users u ON u.id = o.owner_user_id
LEFT JOIN shop_delivery_settings sd ON sd.installation_id = i.id
LEFT JOIN server_configs sc ON sc.server_id = gs.id
WHERE g.discord_guild_id = $1
ORDER BY i.id`, discordGuildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PanelServer
	for rows.Next() {
		var s PanelServer
		if err := rows.Scan(&s.InstallationID, &s.OrganizationID, &s.GuildID, &s.GameServerID, &s.ServiceID, &s.Name, &s.MapKey, &s.OwnerDiscordID, &s.Maintenance); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PanelPost is a server panel posted in a channel.
type PanelPost struct {
	MessageID, ChannelID, DiscordGuildID string
	InstallationID                       int64
}

// RecordPost records a panel posted in channelID for installationID, replacing the one posted
// there before; replaced is that earlier panel's message id, "" when there was none.
func (r *ServerPanelRepository) RecordPost(ctx context.Context, installationID int64, channelID, messageID, postedBy string) (replaced string, err error) {
	var old *string
	err = r.pool.QueryRow(ctx, `WITH old AS (SELECT message_id FROM server_panel_posts WHERE installation_id=$1 AND channel_id=$2)
INSERT INTO server_panel_posts(message_id, installation_id, channel_id, posted_by_discord_id) VALUES($3,$1,$2,$4)
ON CONFLICT (installation_id, channel_id) DO UPDATE SET message_id=EXCLUDED.message_id, posted_by_discord_id=EXCLUDED.posted_by_discord_id, posted_at=NOW()
RETURNING (SELECT message_id FROM old)`, installationID, channelID, messageID, postedBy).Scan(&old)
	if err != nil || old == nil || *old == messageID {
		return "", err
	}
	return *old, nil
}

// Posts lists every posted panel with its Discord server.
func (r *ServerPanelRepository) Posts(ctx context.Context) ([]PanelPost, error) {
	rows, err := r.pool.Query(ctx, `SELECT p.message_id, p.channel_id, g.discord_guild_id, p.installation_id
FROM server_panel_posts p
JOIN installations i ON i.id = p.installation_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
ORDER BY p.posted_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PanelPost
	for rows.Next() {
		var p PanelPost
		if err := rows.Scan(&p.MessageID, &p.ChannelID, &p.DiscordGuildID, &p.InstallationID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePost forgets a posted panel (its message or channel is gone).
func (r *ServerPanelRepository) DeletePost(ctx context.Context, messageID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM server_panel_posts WHERE message_id=$1`, messageID)
	return err
}

// Maintenance reports whether game server serverID's maintenance switch is on.
func (r *ServerPanelRepository) Maintenance(ctx context.Context, serverID int64) (bool, error) {
	var on bool
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT maintenance_mode FROM server_configs WHERE server_id=$1), FALSE)`, serverID).Scan(&on)
	return on, err
}
