package database

// RankedBonusesSQL stores the optional ranked bonuses (docs/RANKED_BONUSES.md): each server's
// settings, the bonus RP and kinds on every award, and what the scheduler has already announced
// (wanted players, rank-ups, weekly recaps). Additive: existing awards read as having no bonus.
const RankedBonusesSQL = `
CREATE TABLE IF NOT EXISTS ranked_bonus_settings (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    bounty_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    bounty_streak INT NOT NULL DEFAULT 5 CHECK (bounty_streak BETWEEN 3 AND 20),
    bounty_percent INT NOT NULL DEFAULT 100 CHECK (bounty_percent BETWEEN 10 AND 500),
    underdog_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    underdog_percent INT NOT NULL DEFAULT 50 CHECK (underdog_percent BETWEEN 10 AND 500),
    revenge_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    revenge_percent INT NOT NULL DEFAULT 25 CHECK (revenge_percent BETWEEN 10 AND 500),
    revenge_minutes INT NOT NULL DEFAULT 30 CHECK (revenge_minutes BETWEEN 5 AND 120),
    daily_first_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    daily_first_percent INT NOT NULL DEFAULT 50 CHECK (daily_first_percent BETWEEN 10 AND 500),
    rankup_cards BOOLEAN NOT NULL DEFAULT FALSE,
    rankup_dms BOOLEAN NOT NULL DEFAULT FALSE,
    weekly_recap BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE ranked_awards ADD COLUMN IF NOT EXISTS bonus_rp BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ranked_awards ADD COLUMN IF NOT EXISTS bonuses JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE ranked_awards ADD COLUMN IF NOT EXISTS bounty_announced_at TIMESTAMPTZ;
-- Streaks and revenge look up a player's deaths in the season.
CREATE INDEX IF NOT EXISTS idx_ranked_awards_victim ON ranked_awards(season_id,victim_key,event_time DESC);
CREATE INDEX IF NOT EXISTS idx_ranked_awards_bounty_due ON ranked_awards(created_at)
    WHERE bounty_announced_at IS NULL AND bonus_rp > 0;

-- A player who is wanted on a server, from when the card went out until the bounty ended.
CREATE TABLE IF NOT EXISTS ranked_wanted (
    id BIGSERIAL PRIMARY KEY,
    season_id BIGINT NOT NULL REFERENCES ranked_seasons(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('TOP','STREAK')),
    since TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cleared_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ranked_wanted_open ON ranked_wanted(season_id,player_id,reason) WHERE cleared_at IS NULL;

-- The last tier each player was told about, so a rank-up is announced once.
CREATE TABLE IF NOT EXISTS ranked_player_tiers (
    season_id BIGINT NOT NULL REFERENCES ranked_seasons(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL,
    tier TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (season_id, player_id)
);
-- A season's tiers were first recorded silently (so turning rank-ups on does not announce a backlog).
CREATE TABLE IF NOT EXISTS ranked_tier_watch (
    season_id BIGINT PRIMARY KEY REFERENCES ranked_seasons(id) ON DELETE CASCADE,
    seeded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ranked_weekly_recaps (
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    week_start DATE NOT NULL,
    posted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (server_id, week_start)
);
`
