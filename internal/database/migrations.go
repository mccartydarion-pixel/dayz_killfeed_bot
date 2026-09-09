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
	{
		Name: "0003_phase41_panels_records",
		SQL: `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS leaderboard_message_id TEXT;
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS player_stats_info_message_id TEXT;

CREATE TABLE IF NOT EXISTS server_records (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    record_type TEXT NOT NULL,
    player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    kill_id BIGINT REFERENCES kills(id) ON DELETE SET NULL,
    numeric_value DOUBLE PRECISION,
    text_value TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id, record_type)
);
`,
	},
	{
		Name: "0004_phase42_streaks_achievements",
		SQL: `
CREATE TABLE IF NOT EXISTS player_combat_stats (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    current_streak INTEGER NOT NULL DEFAULT 0,
    best_streak INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(guild_id, player_id)
);

CREATE TABLE IF NOT EXISTS player_session_stats (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    kills BIGINT NOT NULL DEFAULT 0,
    deaths BIGINT NOT NULL DEFAULT 0,
    best_streak INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(guild_id, session_id, player_id)
);

CREATE TABLE IF NOT EXISTS achievements (
    key TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    emoji TEXT NOT NULL,
    hidden BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS player_achievements (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    achievement_key TEXT NOT NULL REFERENCES achievements(key),
    unlocked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    source_kill_id BIGINT REFERENCES kills(id) ON DELETE SET NULL,
    UNIQUE(guild_id, player_id, achievement_key)
);
CREATE INDEX IF NOT EXISTS idx_player_achievements_player ON player_achievements(guild_id, player_id);

CREATE TABLE IF NOT EXISTS record_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    record_type TEXT NOT NULL,
    player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    kill_id BIGINT REFERENCES kills(id) ON DELETE SET NULL,
    old_value DOUBLE PRECISION,
    new_value DOUBLE PRECISION NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
	},
	{
		Name: "0005_phase43_factions",
		SQL: `
CREATE TABLE IF NOT EXISTS factions (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    tag TEXT NOT NULL,
    owner_player_id BIGINT NOT NULL REFERENCES players(id),
    discord_role_id TEXT,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_factions_guild_name ON factions(guild_id, LOWER(name));
CREATE UNIQUE INDEX IF NOT EXISTS uq_factions_guild_tag ON factions(guild_id, LOWER(tag));
CREATE INDEX IF NOT EXISTS idx_factions_active ON factions(guild_id, active);

CREATE TABLE IF NOT EXISTS faction_members (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    faction_id BIGINT NOT NULL REFERENCES factions(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    active BOOLEAN NOT NULL DEFAULT TRUE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_faction_player ON faction_members(guild_id, player_id) WHERE active;
CREATE INDEX IF NOT EXISTS idx_faction_members_faction ON faction_members(faction_id, active);

CREATE TABLE IF NOT EXISTS faction_invites (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    faction_id BIGINT NOT NULL REFERENCES factions(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    invited_by_player_id BIGINT NOT NULL REFERENCES players(id),
    status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_pending_faction_invite ON faction_invites(guild_id, faction_id, player_id) WHERE status='PENDING';
CREATE INDEX IF NOT EXISTS idx_faction_invites_player ON faction_invites(guild_id, player_id, status);

CREATE TABLE IF NOT EXISTS faction_membership_history (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    faction_id BIGINT NOT NULL REFERENCES factions(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    left_at TIMESTAMPTZ
);

ALTER TABLE kills ADD COLUMN IF NOT EXISTS killer_faction_id BIGINT REFERENCES factions(id) ON DELETE SET NULL;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS victim_faction_id BIGINT REFERENCES factions(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_kills_killer_faction ON kills(guild_id, killer_faction_id);
CREATE INDEX IF NOT EXISTS idx_kills_victim_faction ON kills(guild_id, victim_faction_id);
CREATE INDEX IF NOT EXISTS idx_kills_faction_pair ON kills(guild_id, killer_faction_id, victim_faction_id);
`,
	},
	{
		Name: "0006_phase44_seasons_wars",
		SQL: `
CREATE TABLE IF NOT EXISTS seasons (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ,
    status TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_season_per_guild ON seasons(guild_id) WHERE status='ACTIVE';
CREATE INDEX IF NOT EXISTS idx_seasons_guild_status ON seasons(guild_id,status);

CREATE TABLE IF NOT EXISTS faction_wars (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL,
    faction_a_id BIGINT NOT NULL REFERENCES factions(id),
    faction_b_id BIGINT NOT NULL REFERENCES factions(id),
    status TEXT NOT NULL,
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    created_by_player_id BIGINT REFERENCES players(id),
    faction_a_score BIGINT NOT NULL DEFAULT 0,
    faction_b_score BIGINT NOT NULL DEFAULT 0,
    winner_faction_id BIGINT REFERENCES factions(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK(faction_a_id < faction_b_id),
    CHECK(faction_a_id <> faction_b_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_war_pair ON faction_wars(guild_id,faction_a_id,faction_b_id) WHERE status='ACTIVE';
CREATE INDEX IF NOT EXISTS idx_faction_wars_status ON faction_wars(guild_id,status);

CREATE TABLE IF NOT EXISTS faction_rivalries (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    faction_a_id BIGINT NOT NULL REFERENCES factions(id),
    faction_b_id BIGINT NOT NULL REFERENCES factions(id),
    total_a_kills BIGINT NOT NULL DEFAULT 0,
    total_b_kills BIGINT NOT NULL DEFAULT 0,
    war_count BIGINT NOT NULL DEFAULT 0,
    last_fought_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(guild_id,faction_a_id,faction_b_id),
    CHECK(faction_a_id < faction_b_id)
);

CREATE TABLE IF NOT EXISTS season_results (
    season_id BIGINT PRIMARY KEY REFERENCES seasons(id) ON DELETE CASCADE,
    top_player_id BIGINT REFERENCES players(id),
    top_faction_id BIGINT REFERENCES factions(id),
    top_player_kills BIGINT,
    top_faction_kills BIGINT,
    longest_kill_player_id BIGINT REFERENCES players(id),
    longest_kill_value DOUBLE PRECISION,
    best_streak_player_id BIGINT REFERENCES players(id),
    best_streak_value INTEGER,
    finalized_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE kills ADD COLUMN IF NOT EXISTS season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS war_id BIGINT REFERENCES faction_wars(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_kills_guild_season_killer ON kills(guild_id,season_id,killer_player_id);
CREATE INDEX IF NOT EXISTS idx_kills_guild_season_faction ON kills(guild_id,season_id,killer_faction_id);
CREATE INDEX IF NOT EXISTS idx_kills_guild_war ON kills(guild_id,war_id);
`,
	},
	{
		Name: "0007_phase45_events_bounties_points",
		SQL: `
CREATE TABLE IF NOT EXISTS competitive_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL,
    event_type TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    status TEXT NOT NULL,
    starts_at TIMESTAMPTZ,
    ends_at TIMESTAMPTZ,
    created_by_discord_user_id TEXT,
    config JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_competitive_events_guild_status ON competitive_events(guild_id,status);
CREATE INDEX IF NOT EXISTS idx_competitive_events_starts ON competitive_events(guild_id,starts_at);
CREATE INDEX IF NOT EXISTS idx_competitive_events_ends ON competitive_events(guild_id,ends_at);

CREATE TABLE IF NOT EXISTS event_scores (
    id BIGSERIAL PRIMARY KEY,
    event_id BIGINT NOT NULL REFERENCES competitive_events(id) ON DELETE CASCADE,
    player_id BIGINT REFERENCES players(id) ON DELETE CASCADE,
    faction_id BIGINT REFERENCES factions(id) ON DELETE CASCADE,
    score DOUBLE PRECISION NOT NULL DEFAULT 0,
    kills BIGINT NOT NULL DEFAULT 0,
    best_distance DOUBLE PRECISION,
    best_streak INTEGER,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((player_id IS NOT NULL) <> (faction_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_player_score ON event_scores(event_id,player_id) WHERE player_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_faction_score ON event_scores(event_id,faction_id) WHERE faction_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_event_scores_rank ON event_scores(event_id,score DESC);

CREATE TABLE IF NOT EXISTS event_kills (
    event_id BIGINT NOT NULL REFERENCES competitive_events(id) ON DELETE CASCADE,
    kill_id BIGINT NOT NULL REFERENCES kills(id) ON DELETE CASCADE,
    points DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(event_id,kill_id)
);
CREATE INDEX IF NOT EXISTS idx_event_kills_event ON event_kills(event_id,kill_id);

CREATE TABLE IF NOT EXISTS event_results (
    event_id BIGINT PRIMARY KEY REFERENCES competitive_events(id) ON DELETE CASCADE,
    winner_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    winner_faction_id BIGINT REFERENCES factions(id) ON DELETE SET NULL,
    winning_score DOUBLE PRECISION NOT NULL,
    runner_up_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    runner_up_faction_id BIGINT REFERENCES factions(id) ON DELETE SET NULL,
    finalized_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata JSONB NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS player_event_results (
    event_id BIGINT NOT NULL REFERENCES competitive_events(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    placement INTEGER NOT NULL,
    score DOUBLE PRECISION NOT NULL,
    reward_points INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(event_id,player_id)
);
CREATE TABLE IF NOT EXISTS faction_event_results (
    event_id BIGINT NOT NULL REFERENCES competitive_events(id) ON DELETE CASCADE,
    faction_id BIGINT NOT NULL REFERENCES factions(id) ON DELETE CASCADE,
    placement INTEGER NOT NULL,
    score DOUBLE PRECISION NOT NULL,
    reward_points INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(event_id,faction_id)
);

CREATE TABLE IF NOT EXISTS bounties (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL,
    target_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    created_by_type TEXT NOT NULL,
    created_by_discord_user_id TEXT,
    status TEXT NOT NULL,
    reward_points INTEGER NOT NULL CHECK (reward_points > 0),
    reason TEXT,
    starts_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    claimed_by_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    claimed_kill_id BIGINT REFERENCES kills(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_bounty_target ON bounties(guild_id,target_player_id) WHERE status='ACTIVE';
CREATE INDEX IF NOT EXISTS idx_bounties_status ON bounties(guild_id,status);
CREATE INDEX IF NOT EXISTS idx_bounties_target_status ON bounties(guild_id,target_player_id,status);
CREATE INDEX IF NOT EXISTS idx_bounties_expiry ON bounties(expires_at);

CREATE TABLE IF NOT EXISTS player_points (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    lifetime_points BIGINT NOT NULL DEFAULT 0,
    season_points BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(guild_id,player_id)
);
CREATE TABLE IF NOT EXISTS point_transactions (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    amount INTEGER NOT NULL,
    reason_type TEXT NOT NULL,
    source_id BIGINT,
    source_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id,player_id,reason_type,source_key)
);
CREATE INDEX IF NOT EXISTS idx_point_transactions_player ON point_transactions(guild_id,player_id);
CREATE INDEX IF NOT EXISTS idx_point_transactions_season ON point_transactions(guild_id,season_id);

CREATE TABLE IF NOT EXISTS combat_pair_activity (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    killer_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    victim_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    kill_count BIGINT NOT NULL DEFAULT 0,
    first_kill_at TIMESTAMPTZ,
    last_kill_at TIMESTAMPTZ,
    suspicious BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY(guild_id,killer_player_id,victim_player_id)
);
`,
	},
	{
		Name: "0008_phase44_death_season_snapshots",
		SQL: `
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_deaths_guild_season_player ON deaths(guild_id,season_id,player_id);
`,
	},
	{
		Name: "0009_phase45_combat_anomaly_flags",
		SQL: `
CREATE TABLE IF NOT EXISTS combat_anomaly_flags (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL,
    killer_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    victim_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    flag_type TEXT NOT NULL,
    occurrences BIGINT NOT NULL DEFAULT 1,
    window_started_at TIMESTAMPTZ NOT NULL,
    window_ended_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_combat_anomaly_guild_created ON combat_anomaly_flags(guild_id,created_at DESC);
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
