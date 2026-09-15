package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDuplicate is returned when a unique constraint rejects an insert (durable dedupe).
var ErrDuplicate = errors.New("duplicate record")

// GuildRecord is the persisted guild configuration, decoupled from the discord
// package to avoid an import cycle. Callers map to/from their own setup type.
type GuildRecord struct {
	DiscordGuildID         string
	WelcomeEnabled         bool
	SelectedPublicServerID int64

	CategoryID               string
	WelcomeChannelID         string
	ServerStatusChannelID    string
	KillfeedChannelID        string
	OnlinePlayersChannelID   string
	LeaderboardsChannelID    string
	PlayerStatsChannelID     string
	ADMMonitorChannelID      string
	LinkPanelChannelID       string
	ServerStatusMessageID    string
	OnlinePlayersMessageID   string
	LeaderboardMessageID     string
	PlayerStatsInfoMessageID string
	LinkPanelMessageID       string
	NitradoServiceID         string
	SetupComplete            bool
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// GuildRepository persists guild configuration keyed by Discord guild ID.
type GuildRepository struct {
	pool *pgxpool.Pool
}

// NewGuildRepository creates a guild repository.
func NewGuildRepository(pool *pgxpool.Pool) *GuildRepository {
	return &GuildRepository{pool: pool}
}

// UpsertGuild inserts or updates a guild's Champion setup, returning the row ID.
// It updates only the provided non-empty values, preserving existing IDs otherwise.
func (r *GuildRepository) UpsertGuild(ctx context.Context, s GuildRecord) (int64, error) {
	const q = `
INSERT INTO guilds (
	discord_guild_id, welcome_enabled, category_id, welcome_channel_id, server_status_channel_id, killfeed_channel_id,
    online_players_channel_id, leaderboards_channel_id, player_stats_channel_id, adm_monitor_channel_id, link_panel_channel_id,
	server_status_message_id, online_players_message_id, leaderboard_message_id, player_stats_info_message_id, link_panel_message_id, nitrado_service_id, selected_public_server_id, setup_complete
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
ON CONFLICT (discord_guild_id) DO UPDATE SET
	category_id = COALESCE(EXCLUDED.category_id, guilds.category_id),
	welcome_enabled = EXCLUDED.welcome_enabled,
	welcome_channel_id = COALESCE(EXCLUDED.welcome_channel_id, guilds.welcome_channel_id),
    server_status_channel_id = COALESCE(EXCLUDED.server_status_channel_id, guilds.server_status_channel_id),
    killfeed_channel_id = COALESCE(EXCLUDED.killfeed_channel_id, guilds.killfeed_channel_id),
    online_players_channel_id = COALESCE(EXCLUDED.online_players_channel_id, guilds.online_players_channel_id),
    leaderboards_channel_id = COALESCE(EXCLUDED.leaderboards_channel_id, guilds.leaderboards_channel_id),
    player_stats_channel_id = COALESCE(EXCLUDED.player_stats_channel_id, guilds.player_stats_channel_id),
    adm_monitor_channel_id = COALESCE(EXCLUDED.adm_monitor_channel_id, guilds.adm_monitor_channel_id),
    link_panel_channel_id = COALESCE(EXCLUDED.link_panel_channel_id, guilds.link_panel_channel_id),
    server_status_message_id = COALESCE(EXCLUDED.server_status_message_id, guilds.server_status_message_id),
    online_players_message_id = COALESCE(EXCLUDED.online_players_message_id, guilds.online_players_message_id),
	leaderboard_message_id = COALESCE(EXCLUDED.leaderboard_message_id, guilds.leaderboard_message_id),
	player_stats_info_message_id = COALESCE(EXCLUDED.player_stats_info_message_id, guilds.player_stats_info_message_id),
	link_panel_message_id = COALESCE(EXCLUDED.link_panel_message_id, guilds.link_panel_message_id),
    nitrado_service_id = COALESCE(EXCLUDED.nitrado_service_id, guilds.nitrado_service_id),
	selected_public_server_id = CASE WHEN EXCLUDED.selected_public_server_id > 0 THEN EXCLUDED.selected_public_server_id ELSE guilds.selected_public_server_id END,
    setup_complete = EXCLUDED.setup_complete,
    updated_at = NOW()
RETURNING id`

	var id int64
	err := r.pool.QueryRow(ctx, q,
		s.DiscordGuildID, s.WelcomeEnabled, emptyToNil(s.CategoryID), emptyToNil(s.WelcomeChannelID), emptyToNil(s.ServerStatusChannelID), emptyToNil(s.KillfeedChannelID),
		emptyToNil(s.OnlinePlayersChannelID), emptyToNil(s.LeaderboardsChannelID), emptyToNil(s.PlayerStatsChannelID), emptyToNil(s.ADMMonitorChannelID), emptyToNil(s.LinkPanelChannelID),
		emptyToNil(s.ServerStatusMessageID), emptyToNil(s.OnlinePlayersMessageID), emptyToNil(s.LeaderboardMessageID), emptyToNil(s.PlayerStatsInfoMessageID), emptyToNil(s.LinkPanelMessageID), emptyToNil(s.NitradoServiceID), nullableInt64(s.SelectedPublicServerID),
		isComplete(s),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert guild: %w", err)
	}
	return id, nil
}

func isComplete(s GuildRecord) bool {
	return s.CategoryID != "" && s.WelcomeChannelID != "" && s.KillfeedChannelID != "" && s.OnlinePlayersChannelID != ""
}

func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

// GetGuild returns the stored setup for a Discord guild, or nil if none exists.
func (r *GuildRepository) GetGuild(ctx context.Context, discordGuildID string) (*GuildRecord, int64, error) {
	const q = `
SELECT id, welcome_enabled, COALESCE(category_id,''), COALESCE(welcome_channel_id,''), COALESCE(server_status_channel_id,''),
       COALESCE(killfeed_channel_id,''), COALESCE(online_players_channel_id,''),
       COALESCE(leaderboards_channel_id,''), COALESCE(player_stats_channel_id,''), COALESCE(adm_monitor_channel_id,''), COALESCE(link_panel_channel_id,''),
	COALESCE(server_status_message_id,''), COALESCE(online_players_message_id,''), COALESCE(leaderboard_message_id,''), COALESCE(player_stats_info_message_id,''), COALESCE(link_panel_message_id,''),
	COALESCE(nitrado_service_id,''), COALESCE(selected_public_server_id,0), setup_complete
FROM guilds WHERE discord_guild_id=$1`

	var rowID int64
	var s GuildRecord
	err := r.pool.QueryRow(ctx, q, discordGuildID).Scan(
		&rowID, &s.WelcomeEnabled, &s.CategoryID, &s.WelcomeChannelID, &s.ServerStatusChannelID, &s.KillfeedChannelID,
		&s.OnlinePlayersChannelID, &s.LeaderboardsChannelID, &s.PlayerStatsChannelID, &s.ADMMonitorChannelID, &s.LinkPanelChannelID,
		&s.ServerStatusMessageID, &s.OnlinePlayersMessageID, &s.LeaderboardMessageID, &s.PlayerStatsInfoMessageID, &s.LinkPanelMessageID,
		&s.NitradoServiceID, &s.SelectedPublicServerID, &s.SetupComplete,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get guild: %w", err)
	}
	s.DiscordGuildID = discordGuildID
	return &s, rowID, nil
}

func (r *GuildRepository) SetSelectedPublicServer(ctx context.Context, discordGuildID string, serverID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE guilds SET selected_public_server_id=$2, updated_at=NOW() WHERE discord_guild_id=$1`, discordGuildID, serverID)
	if err != nil {
		return fmt.Errorf("set selected public server: %w", err)
	}
	return nil
}
