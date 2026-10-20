package database

// FactionSecuritySQL adds Faction Security: Base Raid Alarm and Perimeter Watch
// messages also go to the base owner's faction (everyone, or only its leaders,
// as the base owner chooses). Off until the server owner turns it on; the
// Security Marketplace can sell it like the other base services.
const FactionSecuritySQL = `
CREATE TABLE IF NOT EXISTS faction_security_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CONSTRAINT fk_faction_security_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_faction_security_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

-- The base owner's choice of who in their faction gets the messages.
CREATE TABLE IF NOT EXISTS faction_security_preferences (
 installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
 player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
 recipients TEXT NOT NULL DEFAULT 'ALL' CHECK (recipients IN ('ALL','LEADERS')),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,player_id)
);

-- One row per shared alert: how many faction members were messaged.
CREATE TABLE IF NOT EXISTS faction_security_shares (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 source TEXT NOT NULL CHECK (source IN ('RAID_ALARM','PERIMETER_WATCH')),
 sent INTEGER NOT NULL DEFAULT 0 CHECK (sent >= 0),
 failed INTEGER NOT NULL DEFAULT 0 CHECK (failed >= 0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT fk_faction_share_base FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_faction_security_shares_server
 ON faction_security_shares(installation_id,server_id,created_at DESC);

ALTER TABLE security_service_offers DROP CONSTRAINT IF EXISTS security_service_offers_service_id_check;
ALTER TABLE security_service_offers ADD CONSTRAINT security_service_offers_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING','BASE_BLACK_BOX','FACTION_SECURITY'));
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_service_id_check;
ALTER TABLE security_service_purchases ADD CONSTRAINT security_service_purchases_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING','BASE_BLACK_BOX','FACTION_SECURITY'));
`
