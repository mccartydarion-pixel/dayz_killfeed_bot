package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Migration is one ordered, named schema change.
type Migration struct {
	Name string
	SQL  string
}

// migrations is the ordered list of schema changes. New migrations append at the
// end; never edit an applied migration. Each runs once, transactionally.
var migrations = []Migration{
	{
		Name: "0001_init",
		SQL: `
CREATE TABLE IF NOT EXISTS schema_migrations (
    name TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS guilds (
    id BIGSERIAL PRIMARY KEY,
    discord_guild_id TEXT UNIQUE NOT NULL,
    category_id TEXT,
    server_status_channel_id TEXT,
    killfeed_channel_id TEXT,
    online_players_channel_id TEXT,
    leaderboards_channel_id TEXT,
    player_stats_channel_id TEXT,
    server_status_message_id TEXT,
    online_players_message_id TEXT,
    nitrado_service_id TEXT,
    setup_complete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS players (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    dayz_player_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id, dayz_player_id)
);
CREATE INDEX IF NOT EXISTS idx_players_guild_name ON players(guild_id, display_name);

CREATE TABLE IF NOT EXISTS kills (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    event_fingerprint TEXT NOT NULL,
    killer_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    victim_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    weapon_raw TEXT,
    weapon_display TEXT,
    distance DOUBLE PRECISION,
    headshot BOOLEAN NOT NULL DEFAULT FALSE,
    kill_style TEXT,
    event_time TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id, event_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_kills_killer ON kills(guild_id, killer_player_id);
CREATE INDEX IF NOT EXISTS idx_kills_victim ON kills(guild_id, victim_player_id);
CREATE INDEX IF NOT EXISTS idx_kills_distance ON kills(guild_id, distance DESC);
CREATE INDEX IF NOT EXISTS idx_kills_created ON kills(guild_id, created_at DESC);

CREATE TABLE IF NOT EXISTS deaths (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    event_fingerprint TEXT NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    death_type TEXT NOT NULL,
    event_time TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id, event_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_deaths_player ON deaths(guild_id, player_id);

CREATE TABLE IF NOT EXISTS adm_sessions (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    adm_filename TEXT NOT NULL,
    started_at TIMESTAMPTZ,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at TIMESTAMPTZ,
    UNIQUE(guild_id, session_id)
);
CREATE INDEX IF NOT EXISTS idx_adm_sessions_detected ON adm_sessions(guild_id, detected_at DESC);

CREATE TABLE IF NOT EXISTS adm_checkpoints (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    adm_filename TEXT NOT NULL,
    byte_offset BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id, session_id)
);
`,
	},
	{
		Name: "0002_welcome_and_links",
		SQL: `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS welcome_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS welcome_channel_id TEXT;

CREATE TABLE IF NOT EXISTS player_links (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    status TEXT NOT NULL,
    requested_username TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    verified_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id, discord_user_id),
    UNIQUE(guild_id, player_id)
);
CREATE INDEX IF NOT EXISTS idx_player_links_discord ON player_links(guild_id, discord_user_id);

CREATE TABLE IF NOT EXISTS link_verifications (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    challenge_hash TEXT NOT NULL,
    status TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    verified_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_link_verifications_expiry ON link_verifications(guild_id, expires_at);
`,
	},
}

// Migrate applies all pending migrations in order, each transactionally. A
// failure stops startup (returns an error) rather than running a partial schema.
func (d *DB) Migrate(ctx context.Context) error {
	if d == nil || d.Pool == nil {
		return fmt.Errorf("database not connected")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Ensure the migrations bookkeeping table exists first.
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		applied, err := d.isApplied(ctx, m.Name)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.Name, err)
		}
		if applied {
			continue
		}

		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, m.Name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
		slog.Info("component=database", "msg", "migration applied", "name", m.Name)
	}
	return nil
}

func (d *DB) isApplied(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}
