package database

// CaseBaseRequestSQL adds base registration requests: a verified player asks
// for their base to be registered at the position the server log last reported
// for them; the server owner approves (creating the registered base) or
// declines. Nothing is registered until the owner approves.
const CaseBaseRequestSQL = `
CREATE TABLE IF NOT EXISTS case_base_requests (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 player_id BIGINT NOT NULL,
 name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
 note TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 300),
 center_x DOUBLE PRECISION NOT NULL CHECK (center_x BETWEEN -100000 AND 100000),
 center_z DOUBLE PRECISION NOT NULL CHECK (center_z BETWEEN -100000 AND 100000),
 radius DOUBLE PRECISION NOT NULL CHECK (radius BETWEEN 10 AND 150),
 position_seen_at TIMESTAMPTZ NOT NULL,
 status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','APPROVED','DECLINED','CANCELLED')),
 base_id BIGINT,
 decline_reason TEXT NOT NULL DEFAULT '' CHECK (char_length(decline_reason) <= 300),
 decided_by_user_id BIGINT,
 decided_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CHECK ((status='APPROVED') = (base_id IS NOT NULL)),
 CHECK ((status='PENDING') = (decided_at IS NULL)),
 CONSTRAINT fk_base_request_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_base_request_player FOREIGN KEY (guild_id,player_id)
  REFERENCES players(guild_id,id) ON DELETE CASCADE
);
-- One open request per player per server.
CREATE UNIQUE INDEX IF NOT EXISTS uq_base_request_pending
 ON case_base_requests(installation_id,server_id,player_id) WHERE status='PENDING';
CREATE INDEX IF NOT EXISTS idx_base_requests_scope
 ON case_base_requests(installation_id,server_id,status,created_at DESC);
`
