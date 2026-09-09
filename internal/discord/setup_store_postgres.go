package discord

import (
	"context"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// GuildStore is the repository contract the Postgres setup store depends on.
// Implemented by repository.GuildRepository in production; faked in tests.
// It uses repository.GuildRecord to avoid a discord <-> repository import cycle.
type GuildStore interface {
	UpsertGuild(ctx context.Context, s repository.GuildRecord) (int64, error)
	GetGuild(ctx context.Context, discordGuildID string) (*repository.GuildRecord, int64, error)
}

// PostgresSetupStore is the durable SetupStore backed by PostgreSQL. It replaces
// the in-memory store so /setup survives Railway restarts. The SetupStore
// interface (Get/Save/Delete) is unchanged, so callers don't change.
type PostgresSetupStore struct {
	guilds  GuildStore
	timeout time.Duration
}

// NewPostgresSetupStore creates the durable store.
func NewPostgresSetupStore(guilds GuildStore) *PostgresSetupStore {
	return &PostgresSetupStore{guilds: guilds, timeout: 8 * time.Second}
}

// Get loads the stored setup for a guild, or nil if none exists.
func (s *PostgresSetupStore) Get(guildID string) (*GuildSetup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	rec, _, err := s.guilds.GetGuild(ctx, guildID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	return recordToSetup(rec), nil
}

// Save upserts the setup, preserving existing IDs where fields are empty.
func (s *PostgresSetupStore) Save(setup GuildSetup) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	_, err := s.guilds.UpsertGuild(ctx, setupToRecord(setup))
	return err
}

// Delete removes a guild's configuration by clearing stored IDs.
func (s *PostgresSetupStore) Delete(guildID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	_, err := s.guilds.UpsertGuild(ctx, repository.GuildRecord{DiscordGuildID: guildID, SetupComplete: false})
	return err
}

// setupToRecord maps the Discord-facing setup to the persistence record.
func setupToRecord(s GuildSetup) repository.GuildRecord {
	return repository.GuildRecord{
		DiscordGuildID:           s.GuildID,
		WelcomeEnabled:           s.WelcomeEnabled,
		CategoryID:               s.CategoryID,
		WelcomeChannelID:         s.WelcomeChannelID,
		ServerStatusChannelID:    s.ServerStatusChannelID,
		KillfeedChannelID:        s.KillfeedChannelID,
		OnlinePlayersChannelID:   s.OnlinePlayersChannelID,
		LeaderboardsChannelID:    s.LeaderboardsChannelID,
		PlayerStatsChannelID:     s.PlayerStatsChannelID,
		ServerStatusMessageID:    s.ServerStatusMessageID,
		OnlinePlayersMessageID:   s.OnlinePlayersMessageID,
		LeaderboardMessageID:     s.LeaderboardMessageID,
		PlayerStatsInfoMessageID: s.PlayerStatsInfoMessageID,
		SetupComplete:            s.CategoryID != "" && s.WelcomeChannelID != "" && s.KillfeedChannelID != "" && s.OnlinePlayersChannelID != "",
	}
}

// recordToSetup maps the persistence record back to the Discord-facing setup.
func recordToSetup(r *repository.GuildRecord) *GuildSetup {
	return &GuildSetup{
		GuildID:                  r.DiscordGuildID,
		WelcomeEnabled:           r.WelcomeEnabled,
		CategoryID:               r.CategoryID,
		WelcomeChannelID:         r.WelcomeChannelID,
		ServerStatusChannelID:    r.ServerStatusChannelID,
		KillfeedChannelID:        r.KillfeedChannelID,
		OnlinePlayersChannelID:   r.OnlinePlayersChannelID,
		LeaderboardsChannelID:    r.LeaderboardsChannelID,
		PlayerStatsChannelID:     r.PlayerStatsChannelID,
		ServerStatusMessageID:    r.ServerStatusMessageID,
		OnlinePlayersMessageID:   r.OnlinePlayersMessageID,
		LeaderboardMessageID:     r.LeaderboardMessageID,
		PlayerStatsInfoMessageID: r.PlayerStatsInfoMessageID,
	}
}
