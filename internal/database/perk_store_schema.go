package database

// PerkStoreSQL is the storage behind the perk store (docs/PERK_STORE.md): what an owner sells for
// Champion Points, what players bought, and every charge taken for it. Additive only.
const PerkStoreSQL = `
CREATE TABLE IF NOT EXISTS perk_store_settings (
    guild_id BIGINT PRIMARY KEY REFERENCES guilds(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    shoutouts BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS perk_offers (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    price_points BIGINT NOT NULL CHECK (price_points > 0),
    billing TEXT NOT NULL CHECK (billing IN ('ONE_TIME','MONTHLY')),
    duration_days INT NOT NULL DEFAULT 0 CHECK (duration_days >= 0),
    vip_tier_id BIGINT REFERENCES vip_tiers(id) ON DELETE SET NULL,
    priority_queue BOOLEAN NOT NULL DEFAULT FALSE,
    custom_perk TEXT NOT NULL DEFAULT '',
    giftable BOOLEAN NOT NULL DEFAULT TRUE,
    available_from TIMESTAMPTZ,
    available_until TIMESTAMPTZ,
    stock_limit INT CHECK (stock_limit IS NULL OR stock_limit > 0),
    sold_count INT NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order INT NOT NULL DEFAULT 0,
    archived_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_perk_offers_guild ON perk_offers(guild_id, sort_order, id) WHERE archived_at IS NULL;

-- One row per purchase. The perks are a snapshot of the offer when it was bought, so editing or
-- archiving an offer never changes what a player already holds.
CREATE TABLE IF NOT EXISTS perk_purchases (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL,
    server_id BIGINT NOT NULL DEFAULT 0,
    offer_id BIGINT NOT NULL REFERENCES perk_offers(id) ON DELETE CASCADE,
    offer_name TEXT NOT NULL,
    buyer_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    recipient_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    price_points BIGINT NOT NULL,
    billing TEXT NOT NULL,
    duration_days INT NOT NULL,
    vip_tier_id BIGINT,
    priority_queue BOOLEAN NOT NULL DEFAULT FALSE,
    custom_perk TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','ENDED','REFUNDED')),
    end_reason TEXT NOT NULL DEFAULT '',
    auto_renew BOOLEAN NOT NULL DEFAULT FALSE,
    renewals INT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    -- Set when this purchase created the supporter membership (so ending it early ends that too).
    vip_member_id BIGINT,
    -- The name this purchase put on the Nitrado priority list; '' when it added nothing.
    priority_name TEXT NOT NULL DEFAULT '',
    perks_applied_at TIMESTAMPTZ,
    perks_removed_at TIMESTAMPTZ,
    perks_error TEXT NOT NULL DEFAULT '',
    perk_attempts INT NOT NULL DEFAULT 0,
    custom_done_at TIMESTAMPTZ,
    custom_done_by TEXT NOT NULL DEFAULT '',
    request_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (guild_id, buyer_player_id, request_key)
);
-- A player holds an offer at most once at a time.
CREATE UNIQUE INDEX IF NOT EXISTS uq_perk_purchases_active ON perk_purchases(guild_id, recipient_player_id, offer_id) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_perk_purchases_due ON perk_purchases(guild_id, expires_at) WHERE status = 'ACTIVE' AND expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_perk_purchases_guild ON perk_purchases(guild_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_perk_purchases_buyer ON perk_purchases(guild_id, buyer_player_id);

-- Every charge: the purchase itself and each monthly renewal. owner_player_id is who received the
-- points (NULL when the owner had no linked character), which is who a refund takes them back from.
CREATE TABLE IF NOT EXISTS perk_charges (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    purchase_id BIGINT NOT NULL REFERENCES perk_purchases(id) ON DELETE CASCADE,
    buyer_player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    owner_player_id BIGINT,
    amount BIGINT NOT NULL CHECK (amount > 0),
    kind TEXT NOT NULL CHECK (kind IN ('PURCHASE','RENEWAL')),
    refunded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_perk_charges_board ON perk_charges(guild_id, created_at) WHERE refunded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_perk_charges_purchase ON perk_charges(purchase_id, id DESC);
`
