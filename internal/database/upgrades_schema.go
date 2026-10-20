package database

// FeatureUpgradesSQL stores the automation switches (docs/FEATURE_UPGRADES.md) and the state each
// automation keeps. Additive.
const FeatureUpgradesSQL = `
CREATE TABLE IF NOT EXISTS upgrade_settings (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 1. Names Champion put on a Nitrado priority list itself (only these are ever taken off).
CREATE TABLE IF NOT EXISTS priority_auto_grants (
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    name_key TEXT NOT NULL,
    name TEXT NOT NULL,
    reason TEXT NOT NULL,
    player_id BIGINT NOT NULL DEFAULT 0,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (server_id, name_key)
);

-- Direct messages and posts the automations already sent, so each goes out once. kind names the
-- automation; ref says what it was about (a day, a season, an order step...).
CREATE TABLE IF NOT EXISTS upgrade_notices (
    kind TEXT NOT NULL,
    server_id BIGINT NOT NULL,
    player_id BIGINT NOT NULL DEFAULT 0,
    ref TEXT NOT NULL DEFAULT '',
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (kind, server_id, player_id, ref)
);
CREATE INDEX IF NOT EXISTS idx_upgrade_notices_sent ON upgrade_notices(kind, sent_at);

-- 13. Each leaderboard's positions on a day, so the board can show who moved since the day before.
CREATE TABLE IF NOT EXISTS leaderboard_positions (
    guild_id BIGINT NOT NULL,
    board TEXT NOT NULL,
    day DATE NOT NULL,
    positions JSONB NOT NULL,
    PRIMARY KEY (guild_id, board, day)
);

-- 24. The note a player wrote with a gifted perk.
CREATE TABLE IF NOT EXISTS perk_gift_messages (
    purchase_id BIGINT PRIMARY KEY,
    message TEXT NOT NULL CHECK (char_length(message) BETWEEN 1 AND 200),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 26. Appeals players send staff from the Player Hub (a ban, a case, anything else).
CREATE TABLE IF NOT EXISTS player_appeals (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    topic TEXT NOT NULL CHECK (topic IN ('BAN','CASE','OTHER')),
    message TEXT NOT NULL CHECK (char_length(message) BETWEEN 10 AND 1000),
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','ACCEPTED','REJECTED')),
    staff_note TEXT NOT NULL DEFAULT '',
    decided_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_player_appeals_server ON player_appeals(server_id, status, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_player_appeals_open ON player_appeals(server_id, player_id) WHERE status='OPEN';

-- 27. The player a zone's intrusion alerts also go to by direct message (e.g. the base owner).
CREATE TABLE IF NOT EXISTS zone_alert_players (
    zone_id BIGINT PRIMARY KEY,
    installation_id BIGINT NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    set_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 28. Server of the week on the public network: one pick per week (Monday 00:00 UTC).
CREATE TABLE IF NOT EXISTS network_spotlight (
    week_start DATE PRIMARY KEY,
    installation_id BIGINT NOT NULL,
    server_id BIGINT NOT NULL,
    guild_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    platform TEXT NOT NULL,
    active_players INT NOT NULL,
    kills INT NOT NULL,
    picked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
