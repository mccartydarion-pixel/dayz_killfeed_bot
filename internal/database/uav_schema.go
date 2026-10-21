package database

// LiveMapUAVSQL holds the bought UAV of the live map (docs/LIVE_MAP.md "UAV"): the owner's settings
// per server and every pass a player bought. Two UAV tiers: BASIC shows a dot per player, PRECISION
// adds who they are and what they are doing. GHOST is the counter: its buyer is left out of every
// other player's UAV. Additive.
const LiveMapUAVSQL = `
CREATE TABLE IF NOT EXISTS uav_settings (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    block_minutes INTEGER NOT NULL DEFAULT 15 CHECK (block_minutes BETWEEN 5 AND 120),
    basic_price BIGINT NOT NULL DEFAULT 250 CHECK (basic_price BETWEEN 1 AND 10000000),
    precision_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    precision_price BIGINT NOT NULL DEFAULT 600 CHECK (precision_price BETWEEN 1 AND 10000000),
    ghost_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ghost_price BIGINT NOT NULL DEFAULT 400 CHECK (ghost_price BETWEEN 1 AND 10000000),
    max_blocks INTEGER NOT NULL DEFAULT 8 CHECK (max_blocks BETWEEN 1 AND 48),
    delay_seconds INTEGER NOT NULL DEFAULT 0 CHECK (delay_seconds BETWEEN 0 AND 600),
    share_faction BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT NOT NULL DEFAULT ''
);

-- One row per purchase. A purchase while a pass of the same tier is running starts when that pass
-- ends, so buying more extends the UAV. request_key makes a purchase idempotent.
CREATE TABLE IF NOT EXISTS uav_passes (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    faction_id BIGINT REFERENCES hub_factions(id) ON DELETE SET NULL,
    tier TEXT NOT NULL CHECK (tier IN ('BASIC','PRECISION','GHOST')),
    blocks INTEGER NOT NULL,
    minutes INTEGER NOT NULL,
    price BIGINT NOT NULL,
    owner_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    request_key TEXT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (server_id, player_id, request_key)
);
CREATE INDEX IF NOT EXISTS idx_uav_passes_server_end ON uav_passes(server_id, ends_at DESC);
`
