package database

// ProgressionSQL holds daily and weekly challenges, the battle pass and territory control
// (docs/PROGRESSION.md). Every feature is off until an owner switches it on. Additive.
const ProgressionSQL = `
-- Challenges: one row of settings per server; each day's and week's challenges are drawn once and
-- kept, so a later change to the catalog never changes a period already running.
CREATE TABLE IF NOT EXISTS challenge_settings (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    daily_count INTEGER NOT NULL DEFAULT 3 CHECK (daily_count BETWEEN 1 AND 5),
    weekly_count INTEGER NOT NULL DEFAULT 2 CHECK (weekly_count BETWEEN 0 AND 4),
    daily_points BIGINT NOT NULL DEFAULT 50 CHECK (daily_points BETWEEN 0 AND 1000000),
    weekly_points BIGINT NOT NULL DEFAULT 250 CHECK (weekly_points BETWEEN 0 AND 1000000),
    announce BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS challenge_sets (
    id BIGSERIAL PRIMARY KEY,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    period TEXT NOT NULL CHECK (period IN ('DAY','WEEK')),
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    challenges JSONB NOT NULL,
    announced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (server_id, period, starts_at)
);

-- One row per player and challenge done. The points are paid after the row is written; paid_at
-- stays NULL until the ledger credit went through, so a failed payment is retried.
CREATE TABLE IF NOT EXISTS challenge_completions (
    set_id BIGINT NOT NULL REFERENCES challenge_sets(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    completed_at TIMESTAMPTZ NOT NULL,
    points BIGINT NOT NULL DEFAULT 0,
    paid_at TIMESTAMPTZ,
    PRIMARY KEY (set_id, idx, player_id)
);
CREATE INDEX IF NOT EXISTS idx_challenge_completions_player ON challenge_completions(player_id, completed_at DESC);
CREATE INDEX IF NOT EXISTS idx_challenge_completions_unpaid ON challenge_completions(set_id) WHERE paid_at IS NULL;

-- Battle pass: one open season per server. The reward track is the season's own JSON list
-- (progression.Reward), so editing it never touches what was already granted.
CREATE TABLE IF NOT EXISTS battle_pass_seasons (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL,
    name TEXT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    levels INTEGER NOT NULL CHECK (levels BETWEEN 5 AND 100),
    xp_per_level BIGINT NOT NULL CHECK (xp_per_level BETWEEN 100 AND 100000),
    premium_price BIGINT NOT NULL DEFAULT 0 CHECK (premium_price BETWEEN 0 AND 10000000),
    xp_kill INTEGER NOT NULL DEFAULT 100 CHECK (xp_kill BETWEEN 0 AND 100000),
    xp_kill_daily_cap INTEGER NOT NULL DEFAULT 20 CHECK (xp_kill_daily_cap BETWEEN 0 AND 200),
    xp_hour INTEGER NOT NULL DEFAULT 150 CHECK (xp_hour BETWEEN 0 AND 100000),
    xp_hours_daily_cap INTEGER NOT NULL DEFAULT 6 CHECK (xp_hours_daily_cap BETWEEN 0 AND 24),
    xp_daily_challenge INTEGER NOT NULL DEFAULT 300 CHECK (xp_daily_challenge BETWEEN 0 AND 100000),
    xp_weekly_challenge INTEGER NOT NULL DEFAULT 1000 CHECK (xp_weekly_challenge BETWEEN 0 AND 100000),
    rewards JSONB NOT NULL DEFAULT '[]',
    announced_at TIMESTAMPTZ,
    end_announced_at TIMESTAMPTZ,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at > starts_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_battle_pass_open_season ON battle_pass_seasons(server_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_battle_pass_seasons_server ON battle_pass_seasons(server_id, starts_at DESC);

-- Every piece of XP is a row with a reference that makes it count once (a kill id, a day and an
-- hour played, a challenge completion).
CREATE TABLE IF NOT EXISTS battle_pass_xp (
    season_id BIGINT NOT NULL REFERENCES battle_pass_seasons(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    source TEXT NOT NULL CHECK (source IN ('KILL','PLAYTIME','DAILY','WEEKLY')),
    ref TEXT NOT NULL,
    xp INTEGER NOT NULL CHECK (xp >= 0),
    earned_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (season_id, player_id, source, ref)
);
CREATE INDEX IF NOT EXISTS idx_battle_pass_xp_player ON battle_pass_xp(season_id, player_id);

CREATE TABLE IF NOT EXISTS battle_pass_premium (
    season_id BIGINT NOT NULL REFERENCES battle_pass_seasons(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    price BIGINT NOT NULL,
    owner_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    bought_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (season_id, player_id)
);

CREATE TABLE IF NOT EXISTS battle_pass_grants (
    season_id BIGINT NOT NULL REFERENCES battle_pass_seasons(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    level INTEGER NOT NULL,
    track TEXT NOT NULL CHECK (track IN ('FREE','PREMIUM')),
    kind TEXT NOT NULL CHECK (kind IN ('POINTS','TITLE','BADGE')),
    amount BIGINT NOT NULL DEFAULT 0,
    text TEXT NOT NULL DEFAULT '',
    label TEXT NOT NULL DEFAULT '',
    granted_at TIMESTAMPTZ NOT NULL,
    paid_at TIMESTAMPTZ,
    PRIMARY KEY (season_id, player_id, level, track)
);
CREATE INDEX IF NOT EXISTS idx_battle_pass_grants_unpaid ON battle_pass_grants(season_id) WHERE paid_at IS NULL;

-- Titles and badges a player owns on a server, and the ones they show.
CREATE TABLE IF NOT EXISTS player_cosmetics (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('TITLE','BADGE')),
    value TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (server_id, player_id, kind, value)
);

CREATE TABLE IF NOT EXISTS player_cosmetic_choice (
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT '',
    badge TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (server_id, player_id)
);

-- Territory control: hub factions hold named zones by out-killing each other inside them.
CREATE TABLE IF NOT EXISTS territory_settings (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    window_days INTEGER NOT NULL DEFAULT 7 CHECK (window_days BETWEEN 1 AND 14),
    min_kills INTEGER NOT NULL DEFAULT 3 CHECK (min_kills BETWEEN 1 AND 50),
    income_points BIGINT NOT NULL DEFAULT 100 CHECK (income_points BETWEEN 0 AND 1000000),
    announce BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT NOT NULL DEFAULT ''
);

-- Each PvP kill inside a zone, with the killer's and victim's hub factions when it happened. A
-- faction scores a killer-victim pair at most once an hour, so friends cannot farm a zone.
CREATE TABLE IF NOT EXISTS territory_kills (
    kill_id BIGINT PRIMARY KEY REFERENCES kills(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    zone_key TEXT NOT NULL,
    killer_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    victim_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL,
    victim_faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL,
    at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_territory_kills_server ON territory_kills(server_id, at DESC);

CREATE TABLE IF NOT EXISTS territory_holds (
    id BIGSERIAL PRIMARY KEY,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    zone_key TEXT NOT NULL,
    faction_id BIGINT NOT NULL REFERENCES hub_factions(id) ON DELETE CASCADE,
    previous_faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL,
    since TIMESTAMPTZ NOT NULL,
    until TIMESTAMPTZ,
    announced_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_territory_holds_open ON territory_holds(server_id, zone_key) WHERE until IS NULL;

-- Each held zone pays its holder once a UTC day, at the first pass it is held that day.
CREATE TABLE IF NOT EXISTS territory_payouts (
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    day DATE NOT NULL,
    zone_key TEXT NOT NULL,
    faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL,
    players INTEGER NOT NULL DEFAULT 0,
    points BIGINT NOT NULL DEFAULT 0,
    paid_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (server_id, day, zone_key)
);
`
