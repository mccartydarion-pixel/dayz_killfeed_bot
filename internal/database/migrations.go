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

// EconomyBackfillSQL is migration 0030's one-time data backfill: every historical
// point award was a credit (there were never debits), so the spendable balance
// starts equal to the lifetime total, and each ledger row gets its running
// balance. It is a constant so a test can run it against legacy-shaped rows.
const EconomyBackfillSQL = `
UPDATE player_points SET balance = lifetime_points WHERE balance = 0 AND lifetime_points > 0;
UPDATE point_transactions t SET balance_after = r.running
FROM (SELECT id, SUM(amount) OVER (PARTITION BY guild_id, player_id ORDER BY id) AS running FROM point_transactions) r
WHERE t.id = r.id AND t.balance_after IS NULL;
`

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
	{
		Name: "0010_phase44_45_completion_rewards",
		SQL: `
ALTER TABLE season_results ADD COLUMN IF NOT EXISTS completion_announced_at TIMESTAMPTZ;
ALTER TABLE faction_wars ADD COLUMN IF NOT EXISTS completion_announced_at TIMESTAMPTZ;
ALTER TABLE event_results ADD COLUMN IF NOT EXISTS completion_announced_at TIMESTAMPTZ;
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS winner_points INTEGER NOT NULL DEFAULT 1000;
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS second_place_points INTEGER NOT NULL DEFAULT 500;
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS third_place_points INTEGER NOT NULL DEFAULT 250;
`,
	},
	{
		Name: "0011_phase45_announcement_claims",
		SQL: `
CREATE TABLE IF NOT EXISTS completion_announcements (
    kind TEXT NOT NULL,
    object_id BIGINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'PENDING',
    claimed_at TIMESTAMPTZ,
    announced_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(kind,object_id),
    CHECK(status IN ('PENDING','CLAIMED','ANNOUNCED'))
);
CREATE INDEX IF NOT EXISTS idx_completion_announcements_retry ON completion_announcements(status,claimed_at);
`,
	},
	{
		Name: "0012_phase48_multitenant_servers_credentials",
		SQL: `
CREATE TABLE IF NOT EXISTS game_servers (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_service_id TEXT NOT NULL,
    game TEXT NOT NULL,
    platform TEXT NOT NULL,
    display_name TEXT,
    status TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id,provider,provider_service_id)
);
CREATE INDEX IF NOT EXISTS idx_game_servers_guild_active ON game_servers(guild_id,active);

CREATE TABLE IF NOT EXISTS nitrado_connections (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    credential_ciphertext BYTEA NOT NULL,
    credential_nonce BYTEA NOT NULL,
    credential_key_version INTEGER NOT NULL,
    status TEXT NOT NULL,
    last_validated_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_failure_at TIMESTAMPTZ,
    last_error_class TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(guild_id)
);

CREATE TABLE IF NOT EXISTS server_configs (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    adm_poll_interval_ms INTEGER NOT NULL DEFAULT 2000 CHECK(adm_poll_interval_ms >= 1000),
    directory_rescan_interval_seconds INTEGER NOT NULL DEFAULT 45,
    startup_mode TEXT NOT NULL DEFAULT 'tail',
    publish_pvp_kills BOOLEAN NOT NULL DEFAULT TRUE,
    publish_suicides BOOLEAN NOT NULL DEFAULT FALSE,
    publish_unknown_deaths BOOLEAN NOT NULL DEFAULT FALSE,
    online_counter_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    killfeed_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    analytics_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
	},
	{
		Name: "0013_phase48_welcome_configuration",
		SQL: `
CREATE TABLE IF NOT EXISTS guild_welcome_configs (
    guild_id BIGINT PRIMARY KEY REFERENCES guilds(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    channel_id TEXT,
    message_text TEXT,
    title_text TEXT,
    footer_text TEXT,
    image_url TEXT,
    thumbnail_url TEXT,
    color INTEGER,
    mention_user BOOLEAN NOT NULL DEFAULT TRUE,
    welcome_bots BOOLEAN NOT NULL DEFAULT FALSE,
    show_member_count BOOLEAN NOT NULL DEFAULT TRUE,
    show_server_name BOOLEAN NOT NULL DEFAULT TRUE,
    show_link_instructions BOOLEAN NOT NULL DEFAULT TRUE,
    last_welcome_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO guild_welcome_configs(guild_id,channel_id)
SELECT id,welcome_channel_id FROM guilds WHERE welcome_channel_id IS NOT NULL
ON CONFLICT(guild_id) DO NOTHING;
`,
	},
	{
		Name: "0014_link_observed_server_playtime",
		SQL: `
CREATE TABLE IF NOT EXISTS player_server_activity (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    total_observed_seconds BIGINT NOT NULL DEFAULT 0,
    current_session_started_at TIMESTAMPTZ,
    currently_connected BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(guild_id,server_id,player_id)
);
CREATE INDEX IF NOT EXISTS idx_player_server_activity_lookup ON player_server_activity(guild_id,server_id,last_seen_at);
`,
	},
	{
		Name: "0015_link_activity_observation_checkpoints",
		SQL: `
ALTER TABLE player_server_activity ADD COLUMN IF NOT EXISTS last_observed_at TIMESTAMPTZ;
UPDATE player_server_activity SET last_observed_at=COALESCE(last_observed_at,last_seen_at) WHERE currently_connected;
`,
	},
	{
		Name: "0016_server_context_backfill",
		SQL: `
INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,display_name,status,active)
SELECT id,'NITRADO',nitrado_service_id,'DAYZ','PLAYSTATION','Legacy DayZ Server','CONNECTED',TRUE
FROM guilds WHERE COALESCE(nitrado_service_id,'')<>''
ON CONFLICT(guild_id,provider,provider_service_id) DO NOTHING;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL;
UPDATE kills k SET server_id=s.id FROM game_servers s WHERE k.server_id IS NULL AND s.guild_id=k.guild_id AND s.provider_service_id=(SELECT g.nitrado_service_id FROM guilds g WHERE g.id=k.guild_id) AND s.active;
UPDATE deaths d SET server_id=s.id FROM game_servers s WHERE d.server_id IS NULL AND s.guild_id=d.guild_id AND s.provider_service_id=(SELECT g.nitrado_service_id FROM guilds g WHERE g.id=d.guild_id) AND s.active;
CREATE INDEX IF NOT EXISTS idx_kills_server ON kills(guild_id,server_id,created_at DESC);
CREATE INDEX IF NOT EXISTS idx_deaths_server ON deaths(guild_id,server_id,event_time DESC);
`,
	},
	{
		Name: "0017_adm_monitor",
		SQL: `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS adm_monitor_channel_id TEXT;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS adm_monitor_message_id TEXT;
`,
	},
	{
		Name: "0018_public_link_panel",
		SQL: `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS link_panel_channel_id TEXT;
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS link_panel_message_id TEXT;
`,
	},
	{
		Name: "0019_adm_incremental_checkpoints",
		SQL: `
ALTER TABLE adm_checkpoints ADD COLUMN IF NOT EXISTS server_id BIGINT;
ALTER TABLE adm_checkpoints ADD COLUMN IF NOT EXISTS remote_modified_at TIMESTAMPTZ;
ALTER TABLE adm_checkpoints ADD COLUMN IF NOT EXISTS remote_size BIGINT NOT NULL DEFAULT 0;
ALTER TABLE adm_checkpoints ADD COLUMN IF NOT EXISTS pending_partial_line TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_adm_checkpoints_server ON adm_checkpoints(guild_id, server_id);
`,
	},
	{
		Name: "0020_selected_public_server",
		SQL: `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS selected_public_server_id BIGINT;
CREATE INDEX IF NOT EXISTS idx_guilds_selected_public_server ON guilds(selected_public_server_id);
`,
	},
	{
		Name: "0021_death_channel_and_link_challenge",
		SQL: `
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS death_channel_id TEXT;
ALTER TABLE guilds ADD COLUMN IF NOT EXISTS verified_role_id TEXT;
ALTER TABLE link_verifications ADD COLUMN IF NOT EXISTS challenge_disconnect_at TIMESTAMPTZ;
`,
	},
	{
		// Backfill runs in the same transaction as the column add, so it applies
		// exactly once, atomically with the schema change (see Migrate below).
		Name: "0022_kills_longshot",
		SQL: `
ALTER TABLE kills ADD COLUMN IF NOT EXISTS longshot BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE kills SET longshot = TRUE WHERE distance IS NOT NULL AND distance >= 100.0 AND longshot = FALSE;
`,
	},
	{
		// No backfill: unlike longshot (derivable from the still-stored distance),
		// KILLING_SPREE/STREAK_ENDED depend on the killer/victim streak count at
		// the moment of that specific historical kill. player_combat_stats only
		// tracks the CURRENT streak, which has long since moved on for old rows,
		// and no per-kill streak snapshot exists for rows inserted before this
		// migration. Guessing from current state would misreport history, so
		// every pre-existing kill is left as killing_spree=false/streak_ended=false
		// with NULL counts - only kills persisted after this deploy get real values.
		Name: "0023_kills_streak_events",
		SQL: `
ALTER TABLE kills ADD COLUMN IF NOT EXISTS killing_spree BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS killer_streak_after INTEGER;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS streak_ended BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS ended_streak_count INTEGER;
`,
	},
	{
		// SaaS Phase 1 / Stage B2: the Go backend is the authoritative owner of
		// the shared Champion production schema (see the ownership rule
		// documented in docs/SAAS_SCHEMA.md) - the website reads/writes these
		// tables through server-side services but never runs its own
		// migrations against them. Purely additive: no existing table is
		// renamed, no existing column is dropped or retyped, and every new
		// foreign key back into gameplay tables (game_servers, guilds) uses
		// SET NULL/CASCADE only where the cascaded row is SaaS metadata, never
		// kill/death/player history (section 19/20 of the task).
		//
		// organization_id is added directly to the two existing tables that
		// already represent "DayZ server connection" (game_servers) and
		// "encrypted Nitrado credential" (nitrado_connections) rather than
		// creating parallel dayz_server_connections/nitrado_credentials
		// tables - see docs/SAAS_SCHEMA.md for the full reuse mapping.
		Name: "0024_saas_foundation",
		SQL: `
CREATE TABLE IF NOT EXISTS app_users (
    id BIGSERIAL PRIMARY KEY,
    discord_user_id TEXT UNIQUE NOT NULL,
    discord_username TEXT NOT NULL,
    discord_global_name TEXT,
    avatar TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS organizations (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    slug TEXT UNIQUE NOT NULL,
    owner_user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_organizations_owner ON organizations(owner_user_id);

CREATE TABLE IF NOT EXISTS organization_members (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_organization_members_user ON organization_members(user_id);

CREATE TABLE IF NOT EXISTS discord_guild_connections (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL UNIQUE REFERENCES guilds(id) ON DELETE CASCADE,
    guild_name TEXT,
    guild_icon TEXT,
    bot_installed BOOLEAN NOT NULL DEFAULT TRUE,
    permissions_verified BOOLEAN NOT NULL DEFAULT FALSE,
    connected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_discord_guild_connections_org ON discord_guild_connections(organization_id);

-- game_servers already represents "a customer's DayZ/Nitrado server
-- connection" (see 0012_phase48_multitenant_servers_credentials) - extended
-- rather than duplicated under a dayz_server_connections table.
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS organization_id BIGINT REFERENCES organizations(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_game_servers_org ON game_servers(organization_id);

-- nitrado_connections already represents the encrypted Nitrado credential
-- envelope (ciphertext/nonce/key_version) - extended rather than duplicated
-- under a nitrado_credentials table.
ALTER TABLE nitrado_connections ADD COLUMN IF NOT EXISTS organization_id BIGINT REFERENCES organizations(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_nitrado_connections_org ON nitrado_connections(organization_id);

CREATE TABLE IF NOT EXISTS installations (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    discord_guild_connection_id BIGINT NOT NULL REFERENCES discord_guild_connections(id) ON DELETE CASCADE,
    game_server_id BIGINT REFERENCES game_servers(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'NOT_STARTED',
    plan TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    setup_completed_at TIMESTAMPTZ,
    last_health_check_at TIMESTAMPTZ,
    UNIQUE(discord_guild_connection_id, game_server_id)
);
CREATE INDEX IF NOT EXISTS idx_installations_org ON installations(organization_id);
CREATE INDEX IF NOT EXISTS idx_installations_status ON installations(status);
CREATE INDEX IF NOT EXISTS idx_installations_guild_connection ON installations(discord_guild_connection_id);
CREATE INDEX IF NOT EXISTS idx_installations_server ON installations(game_server_id);

CREATE TABLE IF NOT EXISTS installation_setup_progress (
    installation_id BIGINT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    current_step TEXT NOT NULL DEFAULT 'DISCORD',
    discord_completed BOOLEAN NOT NULL DEFAULT FALSE,
    nitrado_completed BOOLEAN NOT NULL DEFAULT FALSE,
    server_selected BOOLEAN NOT NULL DEFAULT FALSE,
    channels_completed BOOLEAN NOT NULL DEFAULT FALSE,
    validation_completed BOOLEAN NOT NULL DEFAULT FALSE,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS installation_settings (
    installation_id BIGINT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    killfeed_channel_id TEXT,
    leaderboard_channel_id TEXT,
    player_status_channel_id TEXT,
    admin_log_channel_id TEXT,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    distance_unit TEXT NOT NULL DEFAULT 'METERS',
    online_display_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    leaderboard_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS subscriptions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    provider TEXT,
    provider_customer_id TEXT,
    provider_subscription_id TEXT,
    plan TEXT NOT NULL DEFAULT 'TRIAL',
    status TEXT NOT NULL DEFAULT 'TRIAL',
    trial_ends_at TIMESTAMPTZ,
    current_period_end TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
	},
	{
		// SaaS Phase 1 / console DayZ backend: the "connect Nitrado" flow is
		// organization-scoped (one Nitrado account credential per
		// organization, POST /api/saas/organizations/{id}/nitrado/connect -
		// no guild/installation in that path), unlike the original
		// guild-scoped bot /setup flow that created nitrado_connections.
		// guild_id NOT NULL. Relaxing it to nullable (rather than adding a
		// parallel nitrado_credentials table - reuse, not duplicate, per
		// this task) lets a SaaS-created row exist with organization_id set
		// and guild_id NULL; existing guild-scoped rows are untouched.
		// UNIQUE(organization_id) is safe alongside the existing
		// UNIQUE(guild_id): Postgres never treats NULLs as conflicting, so
		// legacy guild_id-only rows (organization_id NULL) never collide
		// with each other or with SaaS rows.
		Name: "0025_saas_nitrado_console",
		SQL: `
ALTER TABLE nitrado_connections ALTER COLUMN guild_id DROP NOT NULL;
ALTER TABLE nitrado_connections ADD CONSTRAINT nitrado_connections_organization_id_key UNIQUE(organization_id);
`,
	},
	{
		// SaaS Step 5 one-click channel auto-setup: channel_setup_source
		// distinguishes an explicit customer PUT (.../channels, "MANUAL")
		// from the one-click auto-setup endpoint ("AUTO") so a repeat
		// auto-setup call can safely reuse its own prior work while never
		// silently overwriting a customer's manual customization -
		// installation_settings' existing killfeed/leaderboard/player-status/
		// admin-log channel ID columns remain the single source of truth for
		// which channels are actually configured (section 4/13 - "once
		// created, those IDs become authoritative"). champion_category_id
		// remembers the Champion-managed category so it can be looked up by
		// ID (authoritative) instead of by name on every repeat run - name
		// matching is only a recovery fallback (section 4).
		Name: "0026_saas_channel_auto_setup",
		SQL: `
ALTER TABLE installation_settings ADD COLUMN IF NOT EXISTS channel_setup_source TEXT NOT NULL DEFAULT '';
ALTER TABLE installation_settings ADD COLUMN IF NOT EXISTS champion_category_id TEXT;
`,
	},
	{
		// SaaS Step 5 full channel routing: replaces the four-field
		// installation_settings model with a scalable feature -> Discord
		// channel table, one row per (installation, route_key). Additive
		// only - installation_settings' four legacy columns are neither
		// dropped nor stop being read (section 20/5): GET/PUT
		// .../installations/{id}/channels keeps working exactly as before
		// for any consumer that hasn't migrated to
		// GET/PUT .../channel-routes yet.
		//
		// The one-time backfill below seeds the new table from whatever
		// legacy columns are already set, so an existing installation's
		// configuration is visible under the new system immediately -
		// route_key choices per section 5's explicit mapping:
		//   killfeed_channel_id      -> KILLFEED
		//   leaderboard_channel_id   -> STATS_LEADERBOARDS (explicit instruction)
		//   player_status_channel_id -> STATS_LEADERBOARDS, but only as a
		//     fallback when leaderboard_channel_id is unset - audited via
		//     docs/SAAS_SCHEMA.md's own doc comment, which decouples
		//     player_status_channel_id from guilds.player_stats_channel_id
		//     (internal/discord/setup_store.go's PlayerStatsChannelID: an
		//     on-demand "my stats / search player" panel - the same
		//     "manually viewed... statistics" purpose STATS_LEADERBOARDS
		//     describes, not a connections/join-leave log). CONNECTIONS has
		//     no legacy source: its closest runtime analog,
		//     OnlinePlayersChannelID, drives a voice-channel-name counter,
		//     not a text channel, so backfilling it here would be wrong.
		//   admin_log_channel_id     -> ADMIN_LOGS (explicit instruction)
		// managed_by_champion is backfilled from channel_setup_source
		// ('AUTO' -> true), which already distinguishes exactly this.
		Name: "0027_saas_channel_routes",
		SQL: `
CREATE TABLE IF NOT EXISTS installation_channel_routes (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    managed_by_champion BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, route_key)
);
CREATE INDEX IF NOT EXISTS idx_installation_channel_routes_installation ON installation_channel_routes(installation_id);

INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT installation_id, 'KILLFEED', killfeed_channel_id, (channel_setup_source = 'AUTO')
FROM installation_settings WHERE killfeed_channel_id IS NOT NULL
ON CONFLICT (installation_id, route_key) DO NOTHING;

INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT installation_id, 'STATS_LEADERBOARDS', COALESCE(leaderboard_channel_id, player_status_channel_id), (channel_setup_source = 'AUTO')
FROM installation_settings WHERE COALESCE(leaderboard_channel_id, player_status_channel_id) IS NOT NULL
ON CONFLICT (installation_id, route_key) DO NOTHING;

INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT installation_id, 'ADMIN_LOGS', admin_log_channel_id, (channel_setup_source = 'AUTO')
FROM installation_settings WHERE admin_log_channel_id IS NOT NULL
ON CONFLICT (installation_id, route_key) DO NOTHING;
`,
	},
	{
		Name: "0028_guild_route_panels",
		SQL: `
-- Which persistent panel message lives in which routed channel, per guild and
-- route key (LINK_GAMERTAG, STATS_LEADERBOARDS, AUTO_LEADERBOARD). Keyed by
-- channel so several servers routing to the same channel share one message and
-- a route change can never leave two live panels behind.
CREATE TABLE IF NOT EXISTS guild_route_panels (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (guild_id, route_key, channel_id)
);
`,
	},
	{
		Name: "0029_bounties_server_scope",
		SQL: `
-- Server-scoped bounties (Phase 1). server_id NULL keeps every pre-existing row
-- guild-wide (claimable on any server of the guild, exactly as before); a set
-- server_id makes the bounty claimable only by a kill on that server.
ALTER TABLE bounties ADD COLUMN IF NOT EXISTS server_id BIGINT REFERENCES game_servers(id) ON DELETE CASCADE;

-- Several manual bounties may now be active on one target (stacking). The
-- streak-driven AUTOMATIC bounty keeps its "at most one active per target"
-- rule, so the old blanket index is replaced by one scoped to AUTOMATIC.
DROP INDEX IF EXISTS uq_active_bounty_target;
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_automatic_bounty ON bounties(guild_id,target_player_id) WHERE status='ACTIVE' AND created_by_type='AUTOMATIC';

CREATE INDEX IF NOT EXISTS idx_bounties_server_status ON bounties(guild_id,server_id,status);
CREATE INDEX IF NOT EXISTS idx_bounties_target_player ON bounties(target_player_id);
CREATE INDEX IF NOT EXISTS idx_bounties_created_at ON bounties(created_at);
CREATE INDEX IF NOT EXISTS idx_bounties_claimed_at ON bounties(claimed_at) WHERE claimed_at IS NOT NULL;
`,
	},
	{
		Name: "0030_economy_ledger",
		SQL: `
-- Economy Phase 1: the existing Champion Points tables become the economy's
-- ledger and balance store (no second currency). Everything stays guild-wide.

-- The ledger amount is now signed (debits are negative) and 64-bit: economy
-- totals can exceed what a 32-bit column holds. (Rewrites the table once.)
ALTER TABLE point_transactions ALTER COLUMN amount TYPE BIGINT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS balance_after BIGINT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS created_by TEXT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL;

-- The spendable balance. lifetime_points/season_points stay earn-only leaderboard
-- scores (season_points resets each season; balance never does).
ALTER TABLE player_points ADD COLUMN IF NOT EXISTS balance BIGINT NOT NULL DEFAULT 0;
` + EconomyBackfillSQL + `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'player_points_balance_nonneg') THEN
        ALTER TABLE player_points ADD CONSTRAINT player_points_balance_nonneg CHECK (balance >= 0);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_point_transactions_history ON point_transactions(guild_id, player_id, id DESC);

-- Append-only: a ledger row's facts can never be rewritten. (balance_after may be
-- filled in once for legacy rows, and the ON DELETE SET NULL foreign keys may
-- null season_id/server_id; row deletion by cascade is unaffected.)
CREATE OR REPLACE FUNCTION point_transactions_append_only() RETURNS trigger AS $fn$
BEGIN
    IF NEW.guild_id IS DISTINCT FROM OLD.guild_id
       OR NEW.player_id IS DISTINCT FROM OLD.player_id
       OR NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.reason_type IS DISTINCT FROM OLD.reason_type
       OR NEW.source_key IS DISTINCT FROM OLD.source_key
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (OLD.balance_after IS NOT NULL AND NEW.balance_after IS DISTINCT FROM OLD.balance_after) THEN
        RAISE EXCEPTION 'point_transactions is append-only';
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_point_transactions_append_only ON point_transactions;
CREATE TRIGGER trg_point_transactions_append_only BEFORE UPDATE ON point_transactions FOR EACH ROW EXECUTE FUNCTION point_transactions_append_only();
`,
	},
	{
		// Embed Designer Phase 2: durable custom embed templates. One row per
		// (installation, route). Storage only - no Discord publisher reads this yet.
		// route_key is TEXT validated in Go against the fixed route set (this schema's
		// convention: no CHECK for enumerated values); config_json is the typed,
		// server-validated template (never a raw client blob). Deleting an installation
		// deletes its templates (same as its other child rows).
		Name: "0031_installation_embed_templates",
		SQL: `
CREATE TABLE IF NOT EXISTS installation_embed_templates (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    config_json JSONB NOT NULL CHECK (jsonb_typeof(config_json) = 'object' AND octet_length(config_json::text) <= 65536),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, route_key)
);
`,
	},
	{
		// Faction Hub Phase 1: the web-first, installation-scoped faction directory,
		// membership and recruitment model. Deliberately PARALLEL to the Discord-side
		// factions/faction_members tables (migration 0005): those are keyed by guild and
		// DayZ player_id and are referenced by kills, wars, events and seasons, while the
		// Hub is keyed by organization + installation (+ DayZ server) and by website
		// user (app_users). Hub tables carry the hub_ prefix; legacy_faction_id is a
		// nullable, unused bridge column for a later phase. Nothing here alters the
		// existing faction tables.
		//
		// Tenant integrity is enforced by the schema, not only by handlers:
		//   * (installation_id, organization_id) is a composite FK to installations, so a
		//     faction can never claim an organization that does not own its installation;
		//   * children carry (faction_id, installation_id) as a composite FK to the
		//     faction, so a member/application can never sit in a different installation
		//     than its faction;
		//   * uq_hub_members_installation_user makes "one active faction per user per
		//     installation" a database guarantee (the service also checks it, for a
		//     friendly error and inside the accept-application transaction).
		// Unlike most status text in this schema, the few columns whose corruption
		// would break the membership rules also carry a CHECK.
		Name: "0032_faction_hub",
		SQL: `
CREATE UNIQUE INDEX IF NOT EXISTS uq_installations_id_org ON installations(id, organization_id);

CREATE TABLE IF NOT EXISTS hub_factions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    tag TEXT NOT NULL,
    slug TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    recruitment_status TEXT NOT NULL DEFAULT 'CLOSED' CHECK (recruitment_status IN ('OPEN','INVITE_ONLY','CLOSED')),
    -- Visual keys: placeholders for the approved catalogs (never URLs; not writable yet).
    logo_key TEXT,
    flag_key TEXT,
    armband_key TEXT,
    primary_color TEXT CHECK (primary_color IS NULL OR primary_color ~ '^#[0-9A-Fa-f]{6}$'),
    secondary_color TEXT CHECK (secondary_color IS NULL OR secondary_color ~ '^#[0-9A-Fa-f]{6}$'),
    -- The founder. RESTRICT: a faction is never left without its founding account.
    created_by_user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE RESTRICT,
    -- Unused bridge to the Discord-side faction (a later phase).
    legacy_faction_id BIGINT REFERENCES factions(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    UNIQUE (id, installation_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_slug ON hub_factions(installation_id, slug);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_name ON hub_factions(installation_id, LOWER(name));
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_tag ON hub_factions(installation_id, LOWER(tag));
CREATE INDEX IF NOT EXISTS idx_hub_factions_installation_recruiting ON hub_factions(installation_id, recruitment_status);
CREATE INDEX IF NOT EXISTS idx_hub_factions_org ON hub_factions(organization_id);

CREATE TABLE IF NOT EXISTS hub_faction_settings (
    faction_id BIGINT PRIMARY KEY REFERENCES hub_factions(id) ON DELETE CASCADE,
    minimum_hours INTEGER CHECK (minimum_hours IS NULL OR minimum_hours >= 0),
    minimum_age INTEGER CHECK (minimum_age IS NULL OR minimum_age BETWEEN 0 AND 120),
    pvp_required BOOLEAN NOT NULL DEFAULT FALSE,
    builder_needed BOOLEAN NOT NULL DEFAULT FALSE,
    mic_required BOOLEAN NOT NULL DEFAULT FALSE,
    custom_requirements TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Role catalog. Phase 1 has only the three built-in (system) roles, seeded below with
-- faction_id NULL; a later phase can add per-faction custom roles as rows with a
-- faction_id and grant them through hub_faction_role_memberships. Neither is exposed
-- through the API yet.
CREATE TABLE IF NOT EXISTS hub_faction_roles (
    id BIGSERIAL PRIMARY KEY,
    faction_id BIGINT REFERENCES hub_factions(id) ON DELETE CASCADE,
    role_key TEXT NOT NULL,
    display_name TEXT NOT NULL,
    rank INTEGER NOT NULL,
    is_system BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_faction_roles_system_key ON hub_faction_roles(role_key) WHERE faction_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_faction_roles_faction_key ON hub_faction_roles(faction_id, role_key) WHERE faction_id IS NOT NULL;
INSERT INTO hub_faction_roles(faction_id, role_key, display_name, rank, is_system) VALUES
    (NULL, 'LEADER', 'Leader', 0, TRUE),
    (NULL, 'OFFICER', 'Officer', 1, TRUE),
    (NULL, 'MEMBER', 'Member', 2, TRUE)
ON CONFLICT (role_key) WHERE faction_id IS NULL DO NOTHING;

CREATE TABLE IF NOT EXISTS hub_faction_members (
    id BIGSERIAL PRIMARY KEY,
    faction_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    -- The DayZ identity, when the user has a verified gamertag link on the guild.
    player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    -- The primary (built-in) role: LEADER, OFFICER or MEMBER.
    role_key TEXT NOT NULL CHECK (role_key IN ('LEADER','OFFICER','MEMBER')),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    CONSTRAINT uq_hub_members_faction_user UNIQUE (faction_id, user_id),
    CONSTRAINT uq_hub_members_installation_user UNIQUE (installation_id, user_id)
);
-- At most one LEADER per faction (the creation transaction supplies the first one).
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_members_one_leader ON hub_faction_members(faction_id) WHERE role_key = 'LEADER';
CREATE INDEX IF NOT EXISTS idx_hub_members_faction ON hub_faction_members(faction_id, role_key);
CREATE INDEX IF NOT EXISTS idx_hub_members_user ON hub_faction_members(user_id);

CREATE TABLE IF NOT EXISTS hub_faction_role_memberships (
    id BIGSERIAL PRIMARY KEY,
    member_id BIGINT NOT NULL REFERENCES hub_faction_members(id) ON DELETE CASCADE,
    role_id BIGINT NOT NULL REFERENCES hub_faction_roles(id) ON DELETE CASCADE,
    granted_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (member_id, role_id)
);

-- Application history is never deleted: every outcome is a status.
CREATE TABLE IF NOT EXISTS hub_faction_applications (
    id BIGSERIAL PRIMARY KEY,
    faction_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','ACCEPTED','DENIED','WITHDRAWN','CANCELLED')),
    message TEXT NOT NULL DEFAULT '',
    -- Reserved for leader-defined questions; the API keeps it disabled in Phase 1.
    answers_json JSONB CHECK (answers_json IS NULL OR jsonb_typeof(answers_json) = 'object'),
    reviewed_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_applications_pending ON hub_faction_applications(faction_id, user_id) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS idx_hub_applications_faction_status ON hub_faction_applications(faction_id, status, id DESC);
CREATE INDEX IF NOT EXISTS idx_hub_applications_user ON hub_faction_applications(installation_id, user_id, status);
`,
	},
	{
		// Faction Hub Phase 4: faction logo storage (docs/FACTIONS.md). Metadata lives in
		// hub_faction_assets; the bytes live behind the assetstore.Store abstraction (in
		// production the durable PostgreSQL-backed store below - Champion has no object
		// storage, and the Railway service filesystem is not durable). A faction never holds
		// image bytes itself: hub_factions.logo_asset_id only points at a metadata row.
		//
		// Tenant integrity is in the schema: an asset carries (faction_id, installation_id)
		// as a composite FK to its faction, and the faction's logo pointer is a composite FK
		// (logo_asset_id, id) -> (asset id, faction_id), so a faction can only ever point at
		// one of its OWN assets. The pointer clears (SET NULL on the one column) if the asset
		// row goes; the asset rows cascade with the faction. storage_key is generated by the
		// server (never derived from an uploaded name); public_id is the unguessable id used
		// in the public URL. Additive only: no existing column or row changes.
		Name: "0033_faction_logo_assets",
		SQL: `
CREATE TABLE IF NOT EXISTS hub_faction_assets (
    id BIGSERIAL PRIMARY KEY,
    public_id UUID NOT NULL UNIQUE,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    asset_type TEXT NOT NULL CHECK (asset_type IN ('LOGO')),
    storage_key TEXT NOT NULL UNIQUE CHECK (length(storage_key) <= 200 AND storage_key !~ '\.\.' AND storage_key ~ '^[A-Za-z0-9][A-Za-z0-9/._-]*$'),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png','image/jpeg','image/webp')),
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 5242880),
    width INTEGER NOT NULL CHECK (width BETWEEN 1 AND 8192),
    height INTEGER NOT NULL CHECK (height BETWEEN 1 AND 8192),
    -- Sanitized display/debug name only; never used as a path or executable input.
    original_filename TEXT NOT NULL DEFAULT '' CHECK (length(original_filename) <= 120),
    created_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    UNIQUE (id, faction_id)
);
CREATE INDEX IF NOT EXISTS idx_hub_faction_assets_faction ON hub_faction_assets(faction_id, asset_type);

ALTER TABLE hub_factions ADD COLUMN IF NOT EXISTS logo_asset_id BIGINT;
ALTER TABLE hub_factions ADD CONSTRAINT fk_hub_factions_logo_asset
    FOREIGN KEY (logo_asset_id, id) REFERENCES hub_faction_assets(id, faction_id) ON DELETE SET NULL (logo_asset_id);

-- Bytes for the PostgreSQL-backed assetstore.Store. One row per stored object; the metadata
-- above says what it is. Kept in its own table so listing/serving faction rows never reads
-- image data, and a different store (an S3-compatible bucket) can replace it without touching
-- the metadata model.
CREATE TABLE IF NOT EXISTS hub_asset_blobs (
    storage_key TEXT PRIMARY KEY CHECK (length(storage_key) <= 200),
    content_type TEXT NOT NULL,
    data BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_hub_asset_blobs_created ON hub_asset_blobs(created_at);
`,
	},
	{
		// Faction Hub Phase 5: competitive stats, achievements and activity (docs/FACTION_STATS.md).
		// Nothing here duplicates killfeed data: kills, deaths and bounties stay authoritative in
		// their existing tables and the Hub only DERIVES faction figures from them. What the Hub
		// must add is TIME: a kill counts for a faction only while its player was a member, so
		// membership needs history (hub_faction_members holds only the current state and its rows
		// are deleted on leave/removal).
		//
		//   * hub_faction_membership_history: one row per membership period [joined_at, left_at);
		//     open while left_at IS NULL (at most one open period per user per installation, the
		//     same rule as the live membership). player_identity_id is an informational snapshot
		//     of the verified DayZ identity at join time - attribution always uses the live
		//     verified link (player_links), never this column.
		//   * hub_faction_activity: public-safe, non-combat events the Hub itself produces
		//     (joins, leaves, role changes, leadership transfers, branding). Combat events are
		//     derived from kills/bounties at read time, achievements from the unlock table; this is
		//     NOT the internal audit log (slog) and holds no free text.
		//   * hub_faction_achievement_unlocks: one row per (faction, achievement) - the unique key
		//     is what makes concurrent evaluation unlock exactly once.
		//
		// Current members are backfilled into history with their real joined_at, so their kills
		// since joining are attributed correctly. Members who left BEFORE this migration have no
		// recorded period and cannot be reconstructed: their earlier kills are not credited.
		Name: "0034_faction_stats_history",
		SQL: `
CREATE TABLE IF NOT EXISTS hub_faction_membership_history (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    player_identity_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    joined_at TIMESTAMPTZ NOT NULL,
    left_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (left_at IS NULL OR left_at >= joined_at),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_history_open ON hub_faction_membership_history(installation_id, user_id) WHERE left_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_hub_history_faction ON hub_faction_membership_history(faction_id, joined_at);
CREATE INDEX IF NOT EXISTS idx_hub_history_user ON hub_faction_membership_history(user_id, installation_id);

INSERT INTO hub_faction_membership_history(organization_id, installation_id, faction_id, user_id, player_identity_id, joined_at)
SELECT f.organization_id, m.installation_id, m.faction_id, m.user_id, m.player_id, m.joined_at
FROM hub_faction_members m
JOIN hub_factions f ON f.id = m.faction_id
WHERE NOT EXISTS (SELECT 1 FROM hub_faction_membership_history h WHERE h.faction_id = m.faction_id AND h.user_id = m.user_id AND h.left_at IS NULL);

CREATE TABLE IF NOT EXISTS hub_faction_activity (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    event_type TEXT NOT NULL CHECK (event_type IN ('FACTION_CREATED','MEMBER_JOINED','MEMBER_LEFT','MEMBER_PROMOTED','MEMBER_DEMOTED','LEADERSHIP_TRANSFERRED','FACTION_UPDATED','FACTION_LOGO_CHANGED')),
    -- The member the event is about, and who caused it (leadership transfer: new / previous leader).
    subject_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    actor_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    -- A short fixed value (a role key), never free text.
    detail TEXT CHECK (detail IS NULL OR detail IN ('LEADER','OFFICER','MEMBER')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_hub_activity_faction ON hub_faction_activity(faction_id, occurred_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS hub_faction_achievement_unlocks (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    achievement_key TEXT NOT NULL,
    unlocked_at TIMESTAMPTZ NOT NULL,
    metadata_json JSONB CHECK (metadata_json IS NULL OR jsonb_typeof(metadata_json) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT uq_hub_achievement_unlock UNIQUE (faction_id, achievement_key)
);
CREATE INDEX IF NOT EXISTS idx_hub_unlocks_faction ON hub_faction_achievement_unlocks(faction_id, unlocked_at DESC, id DESC);
`,
	},
	{
		// Economy web foundation: the ledger can no longer be edited (0030) OR deleted directly.
		// A correction is a compensating transaction, never a removal. Deletion caused by a
		// foreign-key cascade (a guild or player being deleted) still works: cascade actions run
		// inside an internal trigger, so pg_trigger_depth() is > 1 there, while a direct
		// DELETE statement reaches this trigger at depth 1. No table or column changes.
		Name: "0035_economy_ledger_no_delete",
		SQL: `
CREATE OR REPLACE FUNCTION point_transactions_no_delete() RETURNS trigger AS $fn$
BEGIN
    IF pg_trigger_depth() <= 1 THEN
        RAISE EXCEPTION 'point_transactions is append-only: correct a mistake with a compensating transaction';
    END IF;
    RETURN OLD;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_point_transactions_no_delete ON point_transactions;
CREATE TRIGGER trg_point_transactions_no_delete BEFORE DELETE ON point_transactions FOR EACH ROW EXECUTE FUNCTION point_transactions_no_delete();
`,
	},
	{
		// Champion Shop Phase 1 (docs/SHOP.md): an installation-scoped product catalog and purchase
		// records paid with Champion Points. The Points themselves are NOT stored here: a purchase's
		// debit is a row of the existing ledger (point_transactions, type SHOP_PURCHASE, source_key
		// 'purchase:<id>'), written in the same transaction as the purchase, and a refund is a
		// compensating SHOP_REFUND credit. Everything is scoped by organization + installation
		// (composite FKs), so a product or purchase can never belong to another tenant.
		//
		// Products are never hard-deleted (is_active=false disables them); purchase items snapshot the
		// product name and price, so editing or disabling a product never rewrites history.
		Name: "0036_shop_foundation",
		SQL: `
CREATE TABLE IF NOT EXISTS shop_categories (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    slug TEXT NOT NULL CHECK (char_length(slug) BETWEEN 1 AND 60),
    description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_categories_installation_slug UNIQUE (installation_id, slug),
    CONSTRAINT uq_shop_categories_id_installation UNIQUE (id, installation_id)
);

CREATE TABLE IF NOT EXISTS shop_products (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    category_id BIGINT,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    slug TEXT NOT NULL CHECK (char_length(slug) BETWEEN 1 AND 60),
    description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 1000),
    price_points BIGINT NOT NULL CHECK (price_points > 0 AND price_points <= 1000000000),
    product_type TEXT NOT NULL CHECK (product_type IN ('ITEM','LOADOUT','VEHICLE','SERVICE','CUSTOM')),
    delivery_type TEXT NOT NULL DEFAULT 'MANUAL' CHECK (delivery_type IN ('MANUAL','DISCORD_ROLE','IN_GAME_FUTURE')),
    image_key TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    is_featured BOOLEAN NOT NULL DEFAULT FALSE,
    stock_mode TEXT NOT NULL DEFAULT 'UNLIMITED' CHECK (stock_mode IN ('UNLIMITED','FINITE')),
    stock_quantity BIGINT,
    purchase_limit INTEGER CHECK (purchase_limit IS NULL OR purchase_limit BETWEEN 1 AND 1000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    FOREIGN KEY (category_id, installation_id) REFERENCES shop_categories(id, installation_id),
    CONSTRAINT uq_shop_products_installation_slug UNIQUE (installation_id, slug),
    CONSTRAINT uq_shop_products_id_installation UNIQUE (id, installation_id),
    CONSTRAINT shop_products_stock_consistent CHECK (
        (stock_mode = 'UNLIMITED' AND stock_quantity IS NULL)
        OR (stock_mode = 'FINITE' AND stock_quantity IS NOT NULL AND stock_quantity >= 0 AND stock_quantity <= 1000000000))
);
CREATE INDEX IF NOT EXISTS idx_shop_products_catalog ON shop_products(installation_id, is_active, is_featured DESC, sort_order, id);
CREATE INDEX IF NOT EXISTS idx_shop_products_category ON shop_products(installation_id, category_id);

CREATE TABLE IF NOT EXISTS shop_purchases (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL,
    user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('PENDING','PAID','PENDING_FULFILLMENT','FULFILLED','CANCELLED','REFUNDED','FAILED')),
    total_points BIGINT NOT NULL CHECK (total_points > 0),
    delivery_type TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 64),
    paid_at TIMESTAMPTZ,
    fulfilled_at TIMESTAMPTZ,
    fulfilled_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    cancelled_at TIMESTAMPTZ,
    refunded_at TIMESTAMPTZ,
    refunded_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    refund_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_purchases_id_installation UNIQUE (id, installation_id),
    CONSTRAINT uq_shop_purchases_idempotency UNIQUE (installation_id, user_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_shop_purchases_installation ON shop_purchases(installation_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_purchases_player ON shop_purchases(installation_id, player_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_purchases_status ON shop_purchases(installation_id, status, id DESC);

CREATE TABLE IF NOT EXISTS shop_purchase_items (
    id BIGSERIAL PRIMARY KEY,
    purchase_id BIGINT NOT NULL REFERENCES shop_purchases(id) ON DELETE CASCADE,
    product_id BIGINT REFERENCES shop_products(id) ON DELETE SET NULL,
    product_name TEXT NOT NULL,
    unit_price_points BIGINT NOT NULL CHECK (unit_price_points > 0),
    quantity INTEGER NOT NULL CHECK (quantity > 0 AND quantity <= 1000),
    line_total_points BIGINT NOT NULL CHECK (line_total_points = unit_price_points * quantity)
);
CREATE INDEX IF NOT EXISTS idx_shop_purchase_items_purchase ON shop_purchase_items(purchase_id);
CREATE INDEX IF NOT EXISTS idx_shop_purchase_items_product ON shop_purchase_items(product_id);
`,
	},
	{
		// Champion Billing Phase 1 (docs/BILLING.md): Stripe fields on the existing subscriptions row
		// (still one row per organization - no parallel customer/subscription table) plus a webhook
		// dedupe log. provider/provider_customer_id/provider_subscription_id already existed
		// (0024_saas_foundation) and stay NULL until an organization actually checks out.
		Name: "0037_billing_stripe",
		SQL: `
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS provider_price_id TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS billing_interval TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS current_period_start TIMESTAMPTZ;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS canceled_at TIMESTAMPTZ;
-- trial_consumed prevents an organization from getting a fresh Stripe trial on every checkout
-- (cancel, wait, check out again): once a Stripe subscription has ever been recorded for the
-- organization, no later checkout is given subscription_data.trial_period_days.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS trial_consumed BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS idx_subscriptions_provider_customer ON subscriptions(provider_customer_id) WHERE provider_customer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_provider_subscription ON subscriptions(provider_subscription_id) WHERE provider_subscription_id IS NOT NULL;

-- One row per processed Stripe event id: webhook handling starts with an INSERT ... ON CONFLICT DO
-- NOTHING against this table, so a duplicated delivery (Stripe retries on anything but a 2xx) is
-- detected before any subscription row is touched, race-safe under the UNIQUE constraint.
CREATE TABLE IF NOT EXISTS billing_webhook_events (
    id BIGSERIAL PRIMARY KEY,
    provider TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    organization_id BIGINT REFERENCES organizations(id) ON DELETE SET NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_webhook_events UNIQUE (provider, event_id)
);
CREATE INDEX IF NOT EXISTS idx_billing_webhook_events_org ON billing_webhook_events(organization_id, received_at DESC);
`,
	},
	{
		Name: "0038_billing_transactions",
		SQL: `
-- Normalized payment/invoice history (Champion Access Model Phase 2, Part D). One row per
-- processed invoice.paid/invoice.payment_failed webhook event - never fabricated, never backfilled
-- from anything but the webhook itself. stripe_event_id ties a row 1:1 to the already-deduped
-- billing_webhook_events row (UNIQUE(provider, event_id) there), so redelivery of the same Stripe
-- event can never create a second transaction row even without re-checking that table.
CREATE TABLE IF NOT EXISTS billing_transactions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_invoice_id TEXT NOT NULL,
    provider_payment_intent_id TEXT,
    provider_subscription_id TEXT,
    -- Only PAID and FAILED are ever written: Champion only subscribes to invoice.paid and
    -- invoice.payment_failed (docs/BILLING.md "Webhook events"). OPEN/VOID/REFUNDED are not
    -- fabricated states - they would need Champion to also handle invoice.voided/charge.refunded,
    -- which it does not yet.
    status TEXT NOT NULL CHECK (status IN ('PAID','FAILED')),
    amount_cents BIGINT NOT NULL,
    currency TEXT NOT NULL,
    period_start TIMESTAMPTZ,
    period_end TIMESTAMPTZ,
    paid_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    stripe_event_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_transactions_event UNIQUE (provider, stripe_event_id)
);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_org ON billing_transactions(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_status ON billing_transactions(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_invoice ON billing_transactions(provider, provider_invoice_id);
`,
	},
	{
		Name: "0039_player_api_lookup_index",
		SQL: `
-- Champion Access Model Phase 2 Part A/B (docs/PLAYER_API.md "Performance"): the player-facing
-- API resolves FROM a Discord user id TO their player_links row before it knows a guild id, so
-- neither existing UNIQUE constraint (guild_id, discord_user_id) or (guild_id, player_id) - both
-- guild_id-first - can be used as an index for that direction. EXPLAIN ANALYZE against this exact
-- query (SELECT ... FROM player_links WHERE discord_user_id=$1 AND status='VERIFIED') showed a
-- sequential scan without this index, and an index scan with it.
CREATE INDEX IF NOT EXISTS idx_player_links_discord_user ON player_links(discord_user_id, status);
`,
	},
	{
		Name: "0040_client_admin_control_plane",
		SQL: `
-- Champion Client Admin Control Plane Phase 1 (docs/CLIENT_ADMIN.md): Discord-role -> Champion
-- tenant-permission-level mapping. This is deliberately separate from organization_members' flat
-- OWNER/ADMIN/MEMBER role and from Champion's own platform-admin allowlist (admin_api.go) - see
-- docs/CLIENT_ADMIN.md "Permission model" for how the three relate. permission_level is enforced
-- at the app layer (internal/permissions) against a fixed, known set; the CHECK constraint is a
-- second, redundant guard against a bad direct write.
CREATE TABLE IF NOT EXISTS installation_role_permissions (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    discord_role_id TEXT NOT NULL,
    permission_level TEXT NOT NULL CHECK (permission_level IN ('OWNER','ADMINISTRATOR','MODERATOR','GATEKEEPER')),
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, discord_role_id)
);
CREATE INDEX IF NOT EXISTS idx_installation_role_permissions_installation ON installation_role_permissions(installation_id);

-- Tenant-level admin audit log (task's "ADMIN AUDIT LOG" section). No persisted audit table
-- existed anywhere before this migration (confirmed by a full-repo audit ahead of Access Model
-- Phase 2, PR #60, and reconfirmed here) - every website admin action from this phase on must
-- write one row here. before_state/after_state are small sanitized JSON snapshots, never secrets.
CREATE TABLE IF NOT EXISTS admin_audit_log (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    installation_id BIGINT REFERENCES installations(id) ON DELETE SET NULL,
    actor_user_id BIGINT REFERENCES app_users(id),
    actor_discord_id TEXT,
    action TEXT NOT NULL,
    target TEXT,
    reason TEXT,
    before_state JSONB,
    after_state JSONB,
    result TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_org_time ON admin_audit_log(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_installation_time ON admin_audit_log(installation_id, created_at DESC);

-- Player warnings (task's "DISCORD MODERATION" / warnings section). A clear preserves the row
-- (audit trail) rather than deleting it - only cleared/clearedBy/clearedAt are set.
CREATE TABLE IF NOT EXISTS player_warnings (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    issued_by_user_id BIGINT REFERENCES app_users(id),
    issued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cleared BOOLEAN NOT NULL DEFAULT FALSE,
    cleared_by_user_id BIGINT REFERENCES app_users(id),
    cleared_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_player_warnings_player ON player_warnings(guild_id, player_id, cleared);

-- Per-route location-field visibility (task's "location" capability) and maintenance mode /
-- autostart monitor state (task's "SERVER OPERATIONS" / "SERVER MAINTENANCE MODE" sections).
ALTER TABLE installation_channel_routes ADD COLUMN IF NOT EXISTS show_location BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS maintenance_mode BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_offline_minutes INTEGER NOT NULL DEFAULT 10 CHECK (autostart_offline_minutes > 0);
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_cooldown_minutes INTEGER NOT NULL DEFAULT 30 CHECK (autostart_cooldown_minutes > 0);
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_last_attempt_at TIMESTAMPTZ;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_attempt_count INTEGER NOT NULL DEFAULT 0;

-- Champion-side whitelist/ban-list records (task's "ACCESS CONTROL"). Nitrado's own whitelist/
-- banlist API (services/{id}/gameservers/games/{whitelist|banlist}, add/remove by identifier only)
-- carries no reason/notes/expiry metadata, so this table is the authoritative source for those
-- fields; the Nitrado call is the enforcement side effect on add/remove. A row is soft-removed
-- (removed_at set) rather than deleted, preserving history; the partial unique index allows an
-- identifier to be re-added after a prior removal without colliding with its own history.
CREATE TABLE IF NOT EXISTS installation_access_entries (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    list_type TEXT NOT NULL CHECK (list_type IN ('WHITELIST','BANLIST')),
    identifier TEXT NOT NULL,
    reason TEXT,
    notes TEXT,
    expires_at TIMESTAMPTZ,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at TIMESTAMPTZ,
    removed_by_user_id BIGINT REFERENCES app_users(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_installation_access_entries_active ON installation_access_entries(installation_id, list_type, identifier) WHERE removed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_installation_access_entries_lookup ON installation_access_entries(installation_id, list_type, removed_at);
`,
	},
	{
		Name: "0041_player_location_events",
		SQL: `
-- Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): persisted ADM position history. ADM parsing
-- already extracts Position{X,Y,Z} on any event whose metadata block includes "pos=<...>"
-- (internal/killfeed/parser.go's posRe) but it was previously discarded immediately after use
-- (only ever read transiently for kill-distance/embed rendering) - this table is the first
-- durable store for it. guild_id/server_id (not organization_id/installation_id) match every
-- other bot-native, guild-scoped table in this schema (kills, deaths, player_warnings,
-- player_server_activity); the SaaS API resolves organization/installation -> guild/server the
-- same way every other Client Admin route already does (ClientAdminRepository.Scope), so no
-- second identity needs to be stored per row.
--
-- "Current location" is deliberately NOT a second table: it is derived at query time (SELECT ...
-- ORDER BY observed_at DESC LIMIT 1, served by this table's own leading index), so there is only
-- ever one truth for a player's location - never a second, potentially-stale copy to reconcile.
--
-- UNIQUE(player_id, server_id, event_type, observed_at) is the durable backstop against a
-- duplicate ADM replay creating a duplicate location row (task section 14's "no duplicate
-- location events after ADM replay") - the same pattern kills/deaths already use
-- (UNIQUE(guild_id, event_fingerprint)), at ADM's own timestamp resolution (whole seconds); the
-- writer inserts with ON CONFLICT DO NOTHING.
CREATE TABLE IF NOT EXISTS player_location_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    gamertag TEXT NOT NULL,
    x DOUBLE PRECISION NOT NULL,
    z DOUBLE PRECISION NOT NULL,
    y DOUBLE PRECISION,
    event_type TEXT NOT NULL CHECK (event_type IN ('CONNECT','DISCONNECT','HIT','KILL','DEATH','RESPAWN','UNCONSCIOUS','OTHER_ADM')),
    observed_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL DEFAULT 'ADM',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_player_location_events_dedupe UNIQUE (player_id, server_id, event_type, observed_at)
);
CREATE INDEX IF NOT EXISTS idx_player_location_events_player_time ON player_location_events(player_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_player_location_events_server_time ON player_location_events(server_id, observed_at DESC);
-- Backs the retention cleanup job's DELETE ... WHERE created_at < cutoff.
CREATE INDEX IF NOT EXISTS idx_player_location_events_retention ON player_location_events(created_at);
`,
	},
	{
		Name: "0042_zones_uav_base_radar",
		SQL: `
-- Champion Phase 4 (docs/ZONES_UAV_RADAR.md): installation-scoped geographic zones plus a stateful
-- intrusion engine consuming Phase 3's player_location_events. installation_zones carries both
-- installation_id (tenant-CRUD identity, matching every other Client Admin table) AND guild_id/
-- server_id, resolved once at creation time from ClientAdminRepository.Scope and denormalized here
-- so the intrusion engine - which lives in internal/killfeed and only ever knows guild_id/server_id,
-- never organization_id/installation_id - can list a server's active zones without joining through
-- installations on every location event (the per-server zone cache still avoids even this lookup
-- on the hot path; see internal/killfeed/zone_cache.go).
--
-- zone_type's UAV/BASE_RADAR values share this exact same table and the exact same intrusion
-- engine as every other zone type (task: "do NOT duplicate intrusion logic") - they only change
-- alert presentation and (internal/permissions) which capability is required to manage them.
CREATE TABLE IF NOT EXISTS installation_zones (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    zone_type TEXT NOT NULL CHECK (zone_type IN ('SAFEZONE','PVP','RESTRICTED','EVENT','UAV','BASE_RADAR','CUSTOM')),
    center_x DOUBLE PRECISION NOT NULL,
    center_z DOUBLE PRECISION NOT NULL,
    radius DOUBLE PRECISION NOT NULL CHECK (radius > 0),
    -- alert_channel_id is the ONLY Discord destination the intrusion engine ever sends to (task
    -- section 17: "never invent a fallback channel"). NULL means alerting is silently disabled for
    -- this zone - intrusions/presence/audit are still tracked, nothing is ever posted to Discord.
    alert_channel_id TEXT,
    cooldown_seconds INTEGER NOT NULL DEFAULT 300 CHECK (cooldown_seconds >= 0),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_installation_zones_installation ON installation_zones(installation_id);
-- Backs the zone cache's refresh query (the hot lookup: "every enabled zone on this server").
CREATE INDEX IF NOT EXISTS idx_installation_zones_server_enabled ON installation_zones(server_id, enabled);

-- Entities fully excluded from intrusion detection for a zone (no presence tracking, no intrusion,
-- no alert - e.g. server admins patrolling a restricted zone). Applies to every zone type.
CREATE TABLE IF NOT EXISTS zone_ignore_entries (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    entry_type TEXT NOT NULL CHECK (entry_type IN ('PLAYER','FACTION','DISCORD_ROLE')),
    entry_value TEXT NOT NULL,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(zone_id, entry_type, entry_value)
);
CREATE INDEX IF NOT EXISTS idx_zone_ignore_entries_zone ON zone_ignore_entries(zone_id);

-- Entities authorized to be inside a zone without triggering an ALERT (presence/intrusion history
-- is still recorded - only Discord alerting is suppressed). Only meaningful for UAV/BASE_RADAR
-- zones (task: "authorized entities never trigger intrusion alerts for UAV/Base Radar") - the
-- intrusion engine ignores this list entirely for every other zone type.
CREATE TABLE IF NOT EXISTS zone_authorized_entries (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    entry_type TEXT NOT NULL CHECK (entry_type IN ('PLAYER','FACTION')),
    entry_value TEXT NOT NULL,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(zone_id, entry_type, entry_value)
);
CREATE INDEX IF NOT EXISTS idx_zone_authorized_entries_zone ON zone_authorized_entries(zone_id);

-- Zone bans are explicitly NOT server bans (task: "never auto-adds to the DayZ banlist") - a
-- banned player entering a zone is tracked and flagged (zone_intrusions.banned, a ZONE_BAN_VIOLATION
-- operational event) exactly like any other intrusion, purely for admin awareness/history.
CREATE TABLE IF NOT EXISTS zone_bans (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    reason TEXT,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lifted_at TIMESTAMPTZ,
    lifted_by_user_id BIGINT REFERENCES app_users(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_zone_bans_active ON zone_bans(zone_id, player_id) WHERE active;
CREATE INDEX IF NOT EXISTS idx_zone_bans_zone ON zone_bans(zone_id);
CREATE INDEX IF NOT EXISTS idx_zone_bans_player ON zone_bans(player_id);

-- zone_presence is a deliberate exception to the "no second truth" principle Phase 3 established
-- for location data: this is not duplicated location data, it is the intrusion engine's own
-- persisted STATE MACHINE result (task section 19/24: "persist current presence state...use this
-- for efficient transition detection" and "restore presence from persisted state on restart, never
-- generate fake entry alerts for players already inside"). last_alert_at is the cooldown anchor -
-- it survives an exit/re-entry cycle for the SAME zone+player pair so a boundary-jitter flap can
-- never bypass the cooldown by technically closing and reopening an intrusion.
CREATE TABLE IF NOT EXISTS zone_presence (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('INSIDE','OUTSIDE')),
    entered_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ NOT NULL,
    last_location_event_id BIGINT,
    last_alert_at TIMESTAMPTZ,
    UNIQUE(zone_id, player_id)
);
CREATE INDEX IF NOT EXISTS idx_zone_presence_zone_status ON zone_presence(zone_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_presence_player ON zone_presence(player_id);

-- zone_intrusions is the durable, append-mostly history: a row is created on every OUTSIDE->INSIDE
-- transition and NEVER deleted on exit (task section 8), only updated (exited_at/status). status is
-- a 3-stage lifecycle: ACTIVE (ongoing, unacknowledged) -> ACKNOWLEDGED (ongoing, an admin has seen
-- it - task section 16) -> EXITED (set whenever the player leaves, from either ACTIVE or
-- ACKNOWLEDGED; acknowledged_at/acknowledged_by are preserved as history either way). The partial
-- unique index enforces "at most one OPEN (non-EXITED) intrusion per zone+player", which is also
-- the intrusion engine's own hot lookup for "is this player already tracked as inside this zone".
CREATE TABLE IF NOT EXISTS zone_intrusions (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    gamertag TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE','EXITED','ACKNOWLEDGED')),
    banned BOOLEAN NOT NULL DEFAULT FALSE,
    entered_at TIMESTAMPTZ NOT NULL,
    exited_at TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by_user_id BIGINT REFERENCES app_users(id),
    last_alert_at TIMESTAMPTZ,
    alert_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_installation_status ON zone_intrusions(installation_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_zone_status ON zone_intrusions(zone_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_player ON zone_intrusions(player_id);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_entered ON zone_intrusions(entered_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_zone_intrusions_open ON zone_intrusions(zone_id, player_id) WHERE status <> 'EXITED';
`,
	},
	{
		Name: "0043_heatmap_indexes",
		SQL: `
-- Champion Phase 5 (docs/HEATMAPS.md): heatmap aggregation queries never scan kills/deaths/
-- player_location_events/zone_intrusions without a covering index - audited against the existing
-- index set first (task section 17), adding only what's actually missing:
--   - kills already has idx_kills_server(guild_id,server_id,created_at DESC), but heatmap queries
--     filter by event_time (the column that matches player_location_events.observed_at exactly -
--     both are set from the same ev.Timestamp during the same processLine call), not created_at.
--   - deaths already has idx_deaths_server(guild_id,server_id,event_time DESC) - covers heatmap
--     death queries as-is, nothing to add.
--   - player_location_events has (player_id,observed_at) and (server_id,observed_at), but every
--     heatmap join additionally filters by event_type ('KILL'/'DEATH') or needs it implicitly - a
--     composite (server_id,event_type,observed_at) index serves both the kill/death coordinate
--     joins and the player-activity aggregation's own time-range scan.
--   - zone_intrusions has (installation_id,status) and (entered_at DESC) separately, but heatmap
--     intrusion queries filter by (installation_id, entered_at range) together.
CREATE INDEX IF NOT EXISTS idx_kills_server_event_time ON kills(guild_id, server_id, event_time DESC);
CREATE INDEX IF NOT EXISTS idx_player_location_events_server_type_time ON player_location_events(server_id, event_type, observed_at);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_installation_entered ON zone_intrusions(installation_id, entered_at DESC);
`,
	},
	{
		Name: "0044_remove_casino_route",
		SQL: `
-- Champion Channel System V2: CASINO no longer exists as a feature or route key. Stored
-- routes and embed templates for it are removed; the Discord channels themselves are never
-- touched (auto-setup reports them as retirable instead).
DELETE FROM installation_channel_routes WHERE route_key = 'CASINO';
DELETE FROM installation_embed_templates WHERE route_key = 'CASINO';
`,
	},
	{
		Name: "0045_player_location_events_adm_axis_fix",
		SQL:  admLocationAxisFixSQL,
	},
	{
		Name: "0046_installation_retired_channels",
		SQL: `
-- Champion Channel System V2: Champion-managed Discord channels/categories that setup or repair
-- stopped routing to. Recorded so a later, explicit, customer-confirmed cleanup can prove a
-- channel is Champion-owned without ever deciding by name. source is ROUTE (a managed route used
-- to point at it) or LEGACY_SETUP (created by the legacy /setup command).
CREATE TABLE IF NOT EXISTS installation_retired_channels (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('CHANNEL', 'CATEGORY')),
    source TEXT NOT NULL CHECK (source IN ('ROUTE', 'LEGACY_SETUP')),
    former_routes TEXT[] NOT NULL DEFAULT '{}',
    legacy_field TEXT NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, channel_id)
);
`,
	},
	{
		Name: "0047_no_card_trial_grants",
		SQL: `
-- Champion Customer Onboarding V2: one no-card 14-day trial per Discord account.
-- trial_grants records the one trial a user has started (user_id is the key, so a second
-- organization never grants a second trial) and the organization it went to (UNIQUE, so one
-- organization is never trialed twice). intended_plan is the plan the customer picked during the
-- trial - it is never used as the paid plan; Stripe webhooks alone set subscriptions.plan.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS intended_plan TEXT;
CREATE TABLE IF NOT EXISTS trial_grants (
    user_id BIGINT PRIMARY KEY REFERENCES app_users(id) ON DELETE CASCADE,
    organization_id BIGINT NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
` + TrialGrantBackfillSQL,
	},
	{
		Name: "0048_case_evidence_observation",
		SQL: `
-- C.A.S.E. Phase 2B: immutable, source-addressed ADM evidence.
-- No client input, no scoring, no bans, no speculative event timestamp.
CREATE TABLE IF NOT EXISTS case_evidence_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    source_end_offset BIGINT NOT NULL CHECK (source_end_offset >= 0),
    line_sha256 CHAR(64) NOT NULL,
    event_type TEXT NOT NULL,
    adm_clock TEXT NOT NULL DEFAULT '',
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    subject_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    actor_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    target_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    subject_name TEXT NOT NULL DEFAULT '',
    actor_name TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',
    actor_x DOUBLE PRECISION,
    actor_z DOUBLE PRECISION,
    actor_altitude DOUBLE PRECISION,
    target_x DOUBLE PRECISION,
    target_z DOUBLE PRECISION,
    target_altitude DOUBLE PRECISION,
    subject_x DOUBLE PRECISION,
    subject_z DOUBLE PRECISION,
    subject_altitude DOUBLE PRECISION,
    weapon TEXT NOT NULL DEFAULT '',
    ammo TEXT NOT NULL DEFAULT '',
    hit_zone TEXT NOT NULL DEFAULT '',
    hit_zone_id TEXT NOT NULL DEFAULT '',
    damage DOUBLE PRECISION,
    hp DOUBLE PRECISION,
    distance_meters DOUBLE PRECISION,
    boundary_kind TEXT NOT NULL DEFAULT '' CHECK (
        boundary_kind IN ('','CONNECT','DISCONNECT','RESPAWN','DEATH','SUICIDE')
    ),
    CONSTRAINT uq_case_evidence_source UNIQUE (guild_id, server_id, source_id, source_end_offset)
);
CREATE INDEX IF NOT EXISTS idx_case_evidence_server_id ON case_evidence_events(guild_id,server_id,id DESC);
CREATE INDEX IF NOT EXISTS idx_case_evidence_actor_id ON case_evidence_events(guild_id,server_id,actor_player_id,id DESC) WHERE actor_player_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_evidence_target_id ON case_evidence_events(guild_id,server_id,target_player_id,id DESC) WHERE target_player_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_evidence_subject_id ON case_evidence_events(guild_id,server_id,subject_player_id,id DESC) WHERE subject_player_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_evidence_hit_time ON case_evidence_events(guild_id,server_id,ingested_at DESC) WHERE event_type='PLAYER_HIT';
`,
	},
	{
		Name: "0049_shop_delivery_engine",
		SQL: `
-- Champion Shop Delivery Engine 2.0 (docs/SHOP_DELIVERY.md). Additive: a delivery policy on
-- products, a per-installation delivery map setting, and one shop_deliveries row per purchase.
-- Every delivery is still fulfilled by staff (delivery_type MANUAL); nothing here spawns items,
-- writes server files or restarts servers. No Nitrado credential is ever stored here.
ALTER TABLE shop_products ADD COLUMN IF NOT EXISTS delivery_policy TEXT NOT NULL DEFAULT 'MANUAL_PICKUP';
DO $$ BEGIN
    ALTER TABLE shop_products ADD CONSTRAINT shop_products_delivery_policy_check CHECK (delivery_policy IN ('MANUAL_PICKUP','MANUAL_COORDINATE'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS shop_delivery_settings (
    installation_id BIGINT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    map_key TEXT CHECK (map_key IS NULL OR char_length(map_key) BETWEEN 1 AND 40),
    updated_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS shop_deliveries (
    id BIGSERIAL PRIMARY KEY,
    purchase_id BIGINT NOT NULL,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    delivery_type TEXT NOT NULL DEFAULT 'MANUAL' CHECK (delivery_type = 'MANUAL'),
    delivery_policy TEXT NOT NULL CHECK (delivery_policy IN ('MANUAL_PICKUP','MANUAL_COORDINATE')),
    map_key TEXT,
    coord_x DOUBLE PRECISION,
    coord_z DOUBLE PRECISION,
    -- Only the Phase 2A states are allowed; the reserved automatic-delivery states need a later migration.
    status TEXT NOT NULL CHECK (status IN ('MANUAL_READY','FULFILLED','CANCELLED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    fulfilled_at TIMESTAMPTZ,
    fulfilled_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    cancelled_at TIMESTAMPTZ,
    cancelled_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    cancel_reason TEXT CHECK (cancel_reason IS NULL OR cancel_reason IN ('REFUNDED','PURCHASE_CANCELLED','PURCHASE_FAILED')),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    FOREIGN KEY (purchase_id, installation_id) REFERENCES shop_purchases(id, installation_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_deliveries_purchase UNIQUE (purchase_id),
    -- A coordinate delivery carries a map and a finite ground position (NaN and +-Infinity fail the
    -- range comparison); a pickup carries none. There is no y/altitude column on purpose.
    CONSTRAINT shop_deliveries_coordinates CHECK (
        (delivery_policy = 'MANUAL_PICKUP' AND map_key IS NULL AND coord_x IS NULL AND coord_z IS NULL)
        OR (delivery_policy = 'MANUAL_COORDINATE' AND map_key IS NOT NULL
            AND coord_x >= 0 AND coord_x <= 100000 AND coord_z >= 0 AND coord_z <= 100000)),
    CONSTRAINT shop_deliveries_fulfilled CHECK (status <> 'FULFILLED' OR fulfilled_at IS NOT NULL),
    CONSTRAINT shop_deliveries_cancelled CHECK (status <> 'CANCELLED' OR (cancelled_at IS NOT NULL AND cancel_reason IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_shop_deliveries_queue ON shop_deliveries(installation_id, status, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_deliveries_installation ON shop_deliveries(installation_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_deliveries_player ON shop_deliveries(installation_id, player_id, id DESC);
` + ShopDeliveryBackfillSQL,
	},
	{
		Name: "0050_live_sync_player_list_and_sessions",
		SQL: `
-- Champion Live Sync phase 1 (docs/CHAMPION_LIVE_SYNC.md). Additive.
-- 1. Every ADM location observation carries its physical source: the canonical ADM file, the byte
--    offset at the end of the line, its server-local time (DayZ writes zone-less local time, so it is
--    stored without a zone) and, for player lists, the snapshot identity.
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS source_file TEXT;
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS source_offset BIGINT;
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS source_local_time TIMESTAMP;
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS snapshot_ref TEXT;
-- 2. PLAYER_LIST: the routine five-minute ADM player-list observation.
DO $$
DECLARE c TEXT;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'player_location_events'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%event_type%'
    LOOP
        EXECUTE format('ALTER TABLE player_location_events DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
ALTER TABLE player_location_events ADD CONSTRAINT player_location_events_event_type_check
    CHECK (event_type IN ('CONNECT','DISCONNECT','HIT','KILL','DEATH','RESPAWN','UNCONSCIOUS','OTHER_ADM','PLAYER_LIST'));
ALTER TABLE player_location_events ADD CONSTRAINT player_location_events_source_complete
    CHECK ((source_file IS NULL AND source_offset IS NULL) OR (source_file IS NOT NULL AND source_offset IS NOT NULL AND source_offset >= 0));
-- 3. Dedupe: a sourced row is unique by its physical source (an exact replay guard that also keeps
--    every five-minute observation of a stationary player); legacy unsourced rows keep the old key,
--    which previously deduplicated on Champion's ingestion time.
ALTER TABLE player_location_events DROP CONSTRAINT IF EXISTS uq_player_location_events_dedupe;
CREATE UNIQUE INDEX IF NOT EXISTS uq_player_location_events_legacy
    ON player_location_events(player_id, server_id, event_type, observed_at) WHERE source_file IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_player_location_events_source
    ON player_location_events(server_id, source_file, source_offset, player_id, event_type) WHERE source_file IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_player_location_events_session
    ON player_location_events(server_id, player_id, source_file, source_offset DESC) WHERE source_file IS NOT NULL;
-- 4. The server's current boot session = the ADM file the ingestion engine currently reads.
CREATE TABLE IF NOT EXISTS server_adm_sessions (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    adm_file TEXT NOT NULL,
    session_local_start TIMESTAMP,
    selected_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
	},
	{
		Name: "0051_live_sync_sources_and_records",
		SQL: `
-- Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md). Additive.
-- 1. One durable checkpoint per watched non-ADM source file (RPT, script, crash, restart.log). The
--    checkpoint is the end of the last complete line whose records are committed; it advances in the
--    same transaction as those records.
CREATE TABLE IF NOT EXISTS live_sync_sources (
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    family TEXT NOT NULL,
    source_file TEXT NOT NULL,
    remote_path TEXT NOT NULL,
    file_local_start TIMESTAMP,
    checkpoint_offset BIGINT NOT NULL DEFAULT 0 CHECK (checkpoint_offset >= 0),
    -- bytes that existed when the source was first attached; records at or before it are BACKFILL
    backfill_until BIGINT NOT NULL DEFAULT 0,
    read_size BIGINT NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    attached_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_read_at TIMESTAMPTZ,
    last_growth_at TIMESTAMPTZ,
    records BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (server_id, family, source_file)
);
-- 2. Every complete record read from those sources, recognized or UNKNOWN (sanitized), with its
--    physical identity. event_id is deterministic (scope + source + offset + category), so a replay
--    is a no-op. delivery separates bytes observed live from history read late.
CREATE TABLE IF NOT EXISTS live_sync_records (
    id BIGSERIAL PRIMARY KEY,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    event_id TEXT NOT NULL,
    family TEXT NOT NULL,
    source_file TEXT NOT NULL,
    source_offset BIGINT NOT NULL CHECK (source_offset > 0),
    category TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PARSED','PARTIAL','UNKNOWN')),
    delivery TEXT NOT NULL CHECK (delivery IN ('LIVE','BACKFILL')),
    boot_id TEXT NOT NULL DEFAULT '',
    source_local_time TIMESTAMP,
    source_utc TIMESTAMPTZ,
    visible_after TIMESTAMPTZ,
    detected_at TIMESTAMPTZ NOT NULL,
    persisted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    evidence TEXT NOT NULL DEFAULT '' CHECK (length(evidence) <= 1024),
    parser TEXT NOT NULL,
    UNIQUE (server_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_live_sync_records_recent ON live_sync_records(server_id, family, id DESC);
CREATE INDEX IF NOT EXISTS idx_live_sync_records_category ON live_sync_records(server_id, category, id DESC);
-- 3. A boot session ends on DayZ-written evidence (RPT shutdown completed, a restart.log restart or
--    pre-start line, a newer boot's file). An ended session is never CURRENT.
ALTER TABLE server_adm_sessions ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ;
ALTER TABLE server_adm_sessions ADD COLUMN IF NOT EXISTS ended_reason TEXT;
ALTER TABLE server_adm_sessions ADD COLUMN IF NOT EXISTS ended_evidence TEXT;
-- 4. The server's UTC offset, learned only from a restart.log line that states it.
CREATE TABLE IF NOT EXISTS live_sync_server_clock (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    utc_offset_minutes INT NOT NULL CHECK (utc_offset_minutes BETWEEN -840 AND 840),
    learned_from TEXT NOT NULL,
    learned_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- 5. Kills and deaths carry the ADM line's physical source, so heatmaps join them to the location
--    row written from the same line (the old join on event_time never matched: event_time is NULL).
ALTER TABLE kills ADD COLUMN IF NOT EXISTS source_file TEXT;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS source_offset BIGINT;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS source_local_time TIMESTAMP;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS source_file TEXT;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS source_offset BIGINT;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS source_local_time TIMESTAMP;
CREATE INDEX IF NOT EXISTS idx_kills_source ON kills(server_id, source_file, source_offset) WHERE source_file IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_deaths_source ON deaths(server_id, source_file, source_offset) WHERE source_file IS NOT NULL;
`,
	},
	{
		Name: "0052_live_sync_rpt_command_line_cleanup",
		SQL:  LiveSyncCommandLineCleanupSQL,
	},
	{
		Name: "0053_installation_embed_activation",
		SQL: `
-- Embed Designer runtime activation (docs/EMBED_RUNTIME.md "Activation"). Additive.
-- One row per installation and route: which presentation the installation SELECTED for that route.
-- DEFAULT (or no row) = the Champion default card; CUSTOM = the installation's saved template. A
-- custom template renders only when (1) the operator rollout switch CHAMPION_CUSTOM_EMBEDS_ENABLED
-- is on, (2) the route supports runtime rendering, (3) this row says CUSTOM and (4) the saved
-- template is valid and enabled. Scoped by installation: one installation never affects another.
CREATE TABLE IF NOT EXISTS installation_embed_activation (
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('DEFAULT', 'CUSTOM')),
    updated_by_user_id BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (installation_id, route_key)
);
-- Before this migration a saved, enabled template was meant to render whenever the rollout switch
-- was on. That intent is carried over as an explicit CUSTOM selection, for the runtime routes only;
-- every other route starts at DEFAULT. Templates themselves are untouched.
INSERT INTO installation_embed_activation (installation_id, route_key, mode)
SELECT installation_id, route_key, 'CUSTOM' FROM installation_embed_templates
WHERE route_key IN ('KILLFEED','HITFEED','PVE_FEED','BOUNTY_TRACKING','CONNECTIONS','ECONOMY')
  AND COALESCE((config_json->>'enabled')::boolean, false)
ON CONFLICT (installation_id, route_key) DO NOTHING;
`,
	},
	{
		Name: "0054_case_addon_subscriptions",
		SQL: `
-- Phase 6.1: C.A.S.E. is an ADDITIVE per-server purchase, never a new base plan.
-- An installation may be repointed to a different game server; the purchased
-- game_server_id remains bound, and runtime access checks require equality.
-- RESTRICT deletion of bound installation/server: paid Stripe subscriptions
-- must be cancelled/reconciled before removing their local binding.
CREATE TABLE IF NOT EXISTS case_addon_subscriptions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    tier TEXT NOT NULL CHECK (tier IN ('CASE_WATCH','CASE_PRO','CASE_COMMAND')),
    status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','TRIAL','ACTIVE','PAST_DUE','CANCELED','SUSPENDED')),
    provider TEXT CHECK (provider IS NULL OR provider = 'stripe'),
    provider_customer_id TEXT,
    provider_subscription_id TEXT,
    provider_price_id TEXT,
    current_period_start TIMESTAMPTZ,
    current_period_end TIMESTAMPTZ,
    trial_started_at TIMESTAMPTZ,
    trial_ends_at TIMESTAMPTZ,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT case_addon_installation_scope
        FOREIGN KEY (installation_id, organization_id)
        REFERENCES installations(id, organization_id) ON DELETE RESTRICT,
    CONSTRAINT uq_case_addon_org_installation UNIQUE (organization_id, installation_id),
    CONSTRAINT uq_case_addon_org_server UNIQUE (organization_id, game_server_id),
    CONSTRAINT case_addon_period_order CHECK (
        current_period_start IS NULL OR current_period_end IS NULL OR
        current_period_start < current_period_end
    ),
    CONSTRAINT case_addon_trial_order CHECK (
        trial_started_at IS NULL OR trial_ends_at IS NULL OR
        trial_started_at < trial_ends_at
    ),
    CONSTRAINT case_addon_subscription_id_nonempty CHECK (
        provider_subscription_id IS NULL OR LENGTH(BTRIM(provider_subscription_id)) > 0
    ),
    CONSTRAINT case_addon_active_provider CHECK (
        status NOT IN ('ACTIVE','TRIAL') OR (
            COALESCE(provider,'') = 'stripe' AND
            NULLIF(BTRIM(COALESCE(provider_subscription_id,'')),'') IS NOT NULL AND
            NULLIF(BTRIM(COALESCE(provider_price_id,'')),'') IS NOT NULL AND
            current_period_end IS NOT NULL
        )
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_addon_provider_subscription
    ON case_addon_subscriptions(provider, provider_subscription_id)
    WHERE provider_subscription_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_addon_org_status
    ON case_addon_subscriptions(organization_id, status, installation_id);
-- No backfill and no write/API route in this milestone. All packages start
-- with zero rows; only the later verified add-on webhook may grant access.
`,
	},

	{
		Name: "0055_case_checkout_reconciliation",
		SQL: `
-- Phase 6.2: retain a single per-server pending checkout and its Stripe
-- idempotency identity; do not create a new subscription when a retry races.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_session_id TEXT;
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_url TEXT;
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_reserved_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_checkout_session
    ON case_addon_subscriptions(checkout_session_id)
    WHERE checkout_session_id IS NOT NULL;
-- Checkout and subscription webhooks are recorded only in the same
-- transaction that successfully applies the event. An error rolls both back,
-- so Stripe retries can never be silently ignored.
CREATE TABLE IF NOT EXISTS case_addon_webhook_events (
    provider TEXT NOT NULL CHECK (provider = 'stripe'),
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    addon_id BIGINT NOT NULL REFERENCES case_addon_subscriptions(id) ON DELETE RESTRICT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider,event_id)
);
CREATE INDEX IF NOT EXISTS idx_case_addon_webhook_addon
    ON case_addon_webhook_events(addon_id, received_at DESC);
`,
	},

	{
		Name: "0056_case_payment_confirmation",
		SQL: `
-- A Stripe subscription can appear ACTIVE before asynchronous payment
-- succeeds. Preserve an independent invoice-paid proof for paid access.
-- C.A.S.E. ACTIVE access is withheld until a signed invoice.paid webhook.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS paid_through TIMESTAMPTZ;
`,
	},
	{
		Name: "0057_case_founder_trial_ledger",
		SQL: `
-- Additive, immutable one-time founder trial identity. No grants/backfill.
-- Future code must write a grant only after verifying an eligible existing
-- base customer and a real Stripe Pro trial on this exact bound game server.
CREATE TABLE IF NOT EXISTS case_addon_trial_grants (
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    installation_id BIGINT NOT NULL,
    addon_id BIGINT NOT NULL REFERENCES case_addon_subscriptions(id) ON DELETE RESTRICT,
    provider_subscription_id TEXT NOT NULL CHECK (LENGTH(BTRIM(provider_subscription_id)) > 0),
    tier TEXT NOT NULL DEFAULT 'CASE_PRO' CHECK (tier = 'CASE_PRO'),
    trial_started_at TIMESTAMPTZ NOT NULL,
    trial_ends_at TIMESTAMPTZ NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT case_founder_trial_scope FOREIGN KEY (installation_id, organization_id)
        REFERENCES installations(id, organization_id) ON DELETE RESTRICT,
    CONSTRAINT case_founder_trial_duration CHECK (
        trial_ends_at > trial_started_at AND
        trial_ends_at <= trial_started_at + INTERVAL '7 days'
    ),
    PRIMARY KEY (organization_id, game_server_id),
    CONSTRAINT uq_case_founder_trial_subscription UNIQUE (provider_subscription_id),
    CONSTRAINT uq_case_founder_trial_addon UNIQUE (addon_id)
);
-- The unique (organization_id, game_server_id) key survives a change of
-- installation or cancellation. A new trial for the same server is impossible.
`,
	},

	{
		Name: "0058_case_checkout_attempt",
		SQL: `
-- Recovery after a Stripe-confirmed expired Checkout Session must never
-- reuse the old Stripe idempotency identity.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_attempt BIGINT NOT NULL DEFAULT 1
        CHECK (checkout_attempt > 0);
`,
	},
	{
		Name: "0059_case_watch_digest_outbox",
		SQL: `
-- Durable per-server paid staff digest. A pre-send claim can expire and be
-- retried safely, but a SENDING row must NEVER be automatically resent:
-- a crash or network error may occur after Discord accepted a message.
CREATE TABLE IF NOT EXISTS case_watch_digest_outbox (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    installation_id BIGINT NOT NULL,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE RESTRICT,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    source_lines BIGINT NOT NULL CHECK (source_lines >= 0),
    hit_lines BIGINT NOT NULL CHECK (hit_lines >= 0),
    kill_lines BIGINT NOT NULL CHECK (kill_lines >= 0),
    collector_enabled BOOLEAN NOT NULL,
    status TEXT NOT NULL DEFAULT 'READY'
        CHECK (status IN ('READY','CLAIMED','SENDING','SENT','UNKNOWN','BLOCKED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 3),
    claim_version INTEGER NOT NULL DEFAULT 0 CHECK (claim_version >= 0),
    claim_expires_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    discord_channel_id TEXT,
    discord_message_id TEXT,
    reason_code TEXT,
    sent_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT case_digest_installation_scope FOREIGN KEY (installation_id,organization_id)
        REFERENCES installations(id,organization_id) ON DELETE RESTRICT,
    CONSTRAINT case_digest_window CHECK (window_start < window_end),
    CONSTRAINT case_digest_sent_receipt CHECK (
        status <> 'SENT' OR
        (NULLIF(BTRIM(COALESCE(discord_message_id,'')),'') IS NOT NULL
         AND NULLIF(BTRIM(COALESCE(discord_channel_id,'')),'') IS NOT NULL
         AND sent_at IS NOT NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_case_digest_claim
    ON case_watch_digest_outbox(status,next_attempt_at,id)
    WHERE status IN ('READY','CLAIMED');
CREATE INDEX IF NOT EXISTS idx_case_digest_scope
    ON case_watch_digest_outbox(organization_id,installation_id,game_server_id,requested_at DESC);
`,
	},
	{
		Name: "0060_case_watch_requester",
		SQL: `
-- Old rows from pre-release 0059 have NULL and are blocked by the worker.
-- New paid messages must retain the authenticated requester, so a role
-- revocation before delivery can be checked against fresh Discord roles.
ALTER TABLE case_watch_digest_outbox
    ADD COLUMN IF NOT EXISTS requested_by_user_id BIGINT REFERENCES app_users(id) ON DELETE RESTRICT;
`,
	},
}

// LiveSyncCommandLineCleanupSQL (migration 0052, Champion Live Sync phase 2.1, docs/
// CHAMPION_LIVE_SYNC.md section 7.8): parser cls-1.1 stored each RPT's command-line header with its IP
// and service name redacted but its game port and config file name kept - one record per RPT. Parser
// cls-1.2 stores no evidence for that line. This removes exactly those records (RPT, LOG_HEADER,
// parser cls-1.1, an "==" line carrying "-port=") and nothing else.
const LiveSyncCommandLineCleanupSQL = `
DELETE FROM live_sync_records
WHERE family = 'RPT' AND category = 'LOG_HEADER' AND parser = 'cls-1.1'
  AND evidence LIKE '==%' AND evidence LIKE '%-port=%';
`

// ShopDeliveryBackfillSQL gives every purchase that has no delivery record a MANUAL_PICKUP one,
// derived from - never changing - the purchase's own fulfillment/refund state: an open purchase is
// MANUAL_READY; a fulfilled purchase (including one refunded after it was handed over) keeps its
// historical fulfillment; a purchase refunded, cancelled or failed before fulfillment is
// CANCELLED. Idempotent (ON CONFLICT on the one-delivery-per-purchase key).
const ShopDeliveryBackfillSQL = shopDeliveryBackfillInsert + ` ON CONFLICT (purchase_id) DO NOTHING;
`

// ShopDeliveryHealSQL is the same derivation for one purchase ($1): the repository runs it inside
// fulfill/refund/read paths so a purchase written by an older instance during a rolling deploy
// (after this migration ran) still gets its delivery record. A no-op when the record exists.
const ShopDeliveryHealSQL = shopDeliveryBackfillInsert + ` AND sp.id = $1 ON CONFLICT (purchase_id) DO NOTHING`

const shopDeliveryBackfillInsert = `
INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, status,
    created_at, updated_at, fulfilled_at, fulfilled_by_user_id, cancelled_at, cancelled_by_user_id, cancel_reason)
SELECT sp.id, sp.organization_id, sp.installation_id, sp.game_server_id, sp.player_id, 'MANUAL_PICKUP',
    CASE WHEN sp.fulfilled_at IS NOT NULL THEN 'FULFILLED'
         WHEN sp.status IN ('REFUNDED','CANCELLED','FAILED') THEN 'CANCELLED'
         ELSE 'MANUAL_READY' END,
    sp.created_at, NOW(), sp.fulfilled_at, sp.fulfilled_by_user_id,
    CASE WHEN sp.fulfilled_at IS NULL AND sp.status IN ('REFUNDED','CANCELLED','FAILED') THEN COALESCE(sp.refunded_at, sp.cancelled_at, sp.updated_at) END,
    CASE WHEN sp.fulfilled_at IS NULL AND sp.status = 'REFUNDED' THEN sp.refunded_by_user_id END,
    CASE WHEN sp.fulfilled_at IS NOT NULL THEN NULL
         WHEN sp.status = 'REFUNDED' THEN 'REFUNDED'
         WHEN sp.status = 'CANCELLED' THEN 'PURCHASE_CANCELLED'
         WHEN sp.status = 'FAILED' THEN 'PURCHASE_FAILED' END
FROM shop_purchases sp
WHERE NOT EXISTS (SELECT 1 FROM shop_deliveries d WHERE d.purchase_id = sp.id)`

// TrialGrantBackfillSQL records, for every owner whose organization already had a trial before
// 0047 (every organization received one at creation), that their one trial is used - so existing
// trials are preserved as they are and those owners' next organizations get none. Idempotent.
const TrialGrantBackfillSQL = `
INSERT INTO trial_grants(user_id, organization_id, granted_at)
SELECT DISTINCT ON (m.user_id) m.user_id, s.organization_id, s.created_at
FROM subscriptions s
JOIN organization_members m ON m.organization_id = s.organization_id AND m.role = 'OWNER'
WHERE s.trial_ends_at IS NOT NULL OR s.trial_consumed
ORDER BY m.user_id, s.created_at, s.organization_id
ON CONFLICT DO NOTHING;
`

// admLocationAxisFixSQL repairs player_location_events rows written before the ADM axis fix.
// DayZ's ADM prints "pos=<x, z, y>" (PluginAdminLog.GetPlayerPrefix builds it from engine
// [0],[2],[1]: east, north, altitude), but the Phase 3 writer stored the second value as y and
// the third as z - so z held altitude and y held the real north coordinate. Heatmaps, zone
// distance and location history all key on x/z, so every pre-fix ADM row is swapped back.
// Postgres evaluates every SET expression against the row's old values, so "z = y, y = z" is a
// true swap. Rows with a NULL y cannot be repaired (the north coordinate was never stored) and
// the writer never produced any, so they are left alone rather than guessed at. The
// (player_id, server_id, event_type, observed_at) dedupe key does not include coordinates, so
// the swap can never collide. Runs exactly once via schema_migrations; rows written afterwards
// already use the corrected mapping (killfeed.Position.MapX/MapZ/Altitude).
const admLocationAxisFixSQL = `
UPDATE player_location_events
   SET z = y, y = z
 WHERE source = 'ADM' AND y IS NOT NULL;
`

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
