package database

// SecurityServiceSQL lets a server owner sell a Security Marketplace service
// for Champion Points. Only the Base Raid Alarm can be offered. An offer is
// off until the owner turns it on and sets a price. A purchase is paid by one
// SECURITY_PURCHASE debit on the existing point ledger (no new currency) and
// never renews by itself.
const SecurityServiceSQL = `
CREATE TABLE IF NOT EXISTS security_service_offers (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 service_id TEXT NOT NULL CHECK (service_id IN ('BASE_RAID_ALARM')),
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 price_points BIGINT NOT NULL CHECK (price_points BETWEEN 1 AND 1000000000),
 duration_days INTEGER NOT NULL CHECK (duration_days BETWEEN 1 AND 90),
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id,service_id),
 CONSTRAINT fk_security_offer_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_security_offer_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS security_service_purchases (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 service_id TEXT NOT NULL CHECK (service_id IN ('BASE_RAID_ALARM')),
 player_id BIGINT NOT NULL,
 price_points BIGINT NOT NULL CHECK (price_points > 0),
 duration_days INTEGER NOT NULL CHECK (duration_days > 0),
 starts_at TIMESTAMPTZ NOT NULL,
 ends_at TIMESTAMPTZ NOT NULL,
 ledger_entry_id BIGINT NOT NULL,
 request_key TEXT NOT NULL CHECK (char_length(request_key) BETWEEN 8 AND 80),
 expiry_notified_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CHECK (ends_at > starts_at),
 CONSTRAINT uq_security_purchase_request UNIQUE (installation_id,player_id,request_key),
 CONSTRAINT fk_security_purchase_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_security_purchase_player FOREIGN KEY (guild_id,player_id)
  REFERENCES players(guild_id,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_security_purchase_active
 ON security_service_purchases(installation_id,service_id,player_id,ends_at DESC);
CREATE INDEX IF NOT EXISTS idx_security_purchase_expiry
 ON security_service_purchases(ends_at) WHERE expiry_notified_at IS NULL;
`
