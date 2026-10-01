package database

// BaseTransferSQL adds base transfers: a base owner asks to hand one of their
// registered bases to an active member of their faction, and the server
// owner approves or declines. One waiting request per base. Additive.
const BaseTransferSQL = `
CREATE TABLE IF NOT EXISTS base_transfer_requests (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 from_player_id BIGINT NOT NULL,
 to_player_id BIGINT NOT NULL,
 status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','APPROVED','DECLINED','CANCELLED')),
 decline_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(decline_reason) <= 300),
 decided_by_user_id BIGINT,
 decided_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CHECK (from_player_id <> to_player_id),
 CHECK ((status='PENDING') = (decided_at IS NULL)),
 CONSTRAINT fk_base_transfer_base FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE,
 CONSTRAINT fk_base_transfer_from FOREIGN KEY (guild_id,from_player_id) REFERENCES players(guild_id,id) ON DELETE CASCADE,
 CONSTRAINT fk_base_transfer_to FOREIGN KEY (guild_id,to_player_id) REFERENCES players(guild_id,id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_base_transfer_pending ON base_transfer_requests(base_id) WHERE status='PENDING';
CREATE INDEX IF NOT EXISTS idx_base_transfer_scope ON base_transfer_requests(installation_id,server_id,created_at DESC);
`
