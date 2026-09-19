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
