package database

// BaseRentSQL adds base rent: the server owner sets one price and period for
// bases registered from players' requests; players pay ahead from the
// Security Store (a BASE_RENT debit on the existing ledger, never automatic).
// When rent runs out, a 3-day grace period follows; after that the base is
// paused (its base services stop) until rent is paid. The base is kept.
const BaseRentSQL = `
CREATE TABLE IF NOT EXISTS base_rent_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 price_points BIGINT NOT NULL CHECK (price_points BETWEEN 1 AND 1000000000),
 period_days INTEGER NOT NULL CHECK (period_days BETWEEN 1 AND 30),
 enabled_since TIMESTAMPTZ,
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CHECK (NOT enabled OR enabled_since IS NOT NULL),
 CONSTRAINT fk_base_rent_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_base_rent_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS base_rent_payments (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 player_id BIGINT NOT NULL,
 price_points BIGINT NOT NULL CHECK (price_points > 0),
 period_days INTEGER NOT NULL CHECK (period_days > 0),
 starts_at TIMESTAMPTZ NOT NULL,
 ends_at TIMESTAMPTZ NOT NULL,
 ledger_entry_id BIGINT NOT NULL,
 request_key TEXT NOT NULL CHECK (char_length(request_key) BETWEEN 8 AND 80),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CHECK (ends_at > starts_at),
 CONSTRAINT uq_base_rent_request UNIQUE (installation_id,player_id,request_key),
 CONSTRAINT fk_base_rent_base FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE,
 CONSTRAINT fk_base_rent_player FOREIGN KEY (guild_id,player_id)
  REFERENCES players(guild_id,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_base_rent_payments_base ON base_rent_payments(base_id,ends_at DESC);

-- One reminder of each kind per due date, so nobody is messaged twice.
CREATE TABLE IF NOT EXISTS base_rent_notices (
 base_id BIGINT NOT NULL,
 kind TEXT NOT NULL CHECK (kind IN ('DUE_SOON','PAUSED')),
 due_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (base_id,kind,due_at)
);

-- base_rent_due_at is when a rented base's rent is (or was) due: the end of
-- its latest payment, or when rent first applied to it. NULL when the base
-- pays no rent (rent off, or not a player-requested base).
CREATE OR REPLACE FUNCTION base_rent_due_at(p_base BIGINT) RETURNS TIMESTAMPTZ
LANGUAGE sql STABLE AS $$
 SELECT COALESCE((SELECT MAX(rp.ends_at) FROM base_rent_payments rp WHERE rp.base_id=b.id),
                 GREATEST(b.created_at, rs.enabled_since))
 FROM case_registered_bases b
 JOIN base_rent_settings rs ON rs.installation_id=b.installation_id AND rs.server_id=b.server_id AND rs.enabled
 WHERE b.id=p_base AND b.state<>'REVOKED'
  AND EXISTS (SELECT 1 FROM case_base_requests rq WHERE rq.base_id=b.id AND rq.status='APPROVED')
$$;

-- base_rent_paused: rent is owed and the 3-day grace period has passed.
CREATE OR REPLACE FUNCTION base_rent_paused(p_base BIGINT) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
 SELECT COALESCE(base_rent_due_at(p_base) + INTERVAL '3 days' < NOW(), FALSE)
$$;
`
