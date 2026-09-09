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
	DiscordGuildID string

	CategoryID             string
	WelcomeChannelID       string
	ServerStatusChannelID  string
	KillfeedChannelID      string
	OnlinePlayersChannelID string
	LeaderboardsChannelID  string
	PlayerStatsChannelID   string
	ServerStatusMessageID  string
	OnlinePlayersMessageID string
	NitradoServiceID       string
	SetupComplete          bool
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
	discord_guild_id, category_id, welcome_channel_id, server_status_channel_id, killfeed_channel_id,
    online_players_channel_id, leaderboards_channel_id, player_stats_channel_id,
    server_status_message_id, online_players_message_id, nitrado_service_id, setup_complete
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (discord_guild_id) DO UPDATE SET
	category_id = COALESCE(EXCLUDED.category_id, guilds.category_id),
	welcome_channel_id = COALESCE(EXCLUDED.welcome_channel_id, guilds.welcome_channel_id),
    server_status_channel_id = COALESCE(EXCLUDED.server_status_channel_id, guilds.server_status_channel_id),
    killfeed_channel_id = COALESCE(EXCLUDED.killfeed_channel_id, guilds.killfeed_channel_id),
    online_players_channel_id = COALESCE(EXCLUDED.online_players_channel_id, guilds.online_players_channel_id),
    leaderboards_channel_id = COALESCE(EXCLUDED.leaderboards_channel_id, guilds.leaderboards_channel_id),
    player_stats_channel_id = COALESCE(EXCLUDED.player_stats_channel_id, guilds.player_stats_channel_id),
    server_status_message_id = COALESCE(EXCLUDED.server_status_message_id, guilds.server_status_message_id),
    online_players_message_id = COALESCE(EXCLUDED.online_players_message_id, guilds.online_players_message_id),
    nitrado_service_id = COALESCE(EXCLUDED.nitrado_service_id, guilds.nitrado_service_id),
    setup_complete = EXCLUDED.setup_complete,
    updated_at = NOW()
RETURNING id`

	var id int64
	err := r.pool.QueryRow(ctx, q,
		s.DiscordGuildID, emptyToNil(s.CategoryID), emptyToNil(s.WelcomeChannelID), emptyToNil(s.ServerStatusChannelID), emptyToNil(s.KillfeedChannelID),
		emptyToNil(s.OnlinePlayersChannelID), emptyToNil(s.LeaderboardsChannelID), emptyToNil(s.PlayerStatsChannelID),
		emptyToNil(s.ServerStatusMessageID), emptyToNil(s.OnlinePlayersMessageID), emptyToNil(s.NitradoServiceID),
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

// GetGuild returns the stored setup for a Discord guild, or nil if none exists.
func (r *GuildRepository) GetGuild(ctx context.Context, discordGuildID string) (*GuildRecord, int64, error) {
	const q = `
SELECT id, COALESCE(category_id,''), COALESCE(welcome_channel_id,''), COALESCE(server_status_channel_id,''),
       COALESCE(killfeed_channel_id,''), COALESCE(online_players_channel_id,''),
       COALESCE(leaderboards_channel_id,''), COALESCE(player_stats_channel_id,''),
       COALESCE(server_status_message_id,''), COALESCE(online_players_message_id,''),
       COALESCE(nitrado_service_id,''), setup_complete
FROM guilds WHERE discord_guild_id=$1`

	var rowID int64
	var s GuildRecord
	err := r.pool.QueryRow(ctx, q, discordGuildID).Scan(
		&rowID, &s.CategoryID, &s.WelcomeChannelID, &s.ServerStatusChannelID, &s.KillfeedChannelID,
		&s.OnlinePlayersChannelID, &s.LeaderboardsChannelID, &s.PlayerStatsChannelID,
		&s.ServerStatusMessageID, &s.OnlinePlayersMessageID, &s.NitradoServiceID, &s.SetupComplete,
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
