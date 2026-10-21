package database

// PerimeterWatchSQL adds Perimeter Watch: a base owner gets a DM when someone
// who isn't them, their faction or on the base's friend list is seen inside
// the area around their registered base. Off until the server owner turns it
// on. The Security Marketplace can sell it like the Base Raid Alarm.
const PerimeterWatchSQL = `
CREATE TABLE IF NOT EXISTS perimeter_watch_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 margin_meters INTEGER NOT NULL DEFAULT 100 CHECK (margin_meters BETWEEN 25 AND 300),
 cooldown_seconds INTEGER NOT NULL DEFAULT 1800 CHECK (cooldown_seconds BETWEEN 300 AND 7200),
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CONSTRAINT fk_perimeter_settings_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_perimeter_settings_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS perimeter_watch_alerts (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 visitor_player_id BIGINT NOT NULL,
 visitor_name TEXT NOT NULL CHECK (char_length(visitor_name) BETWEEN 1 AND 128),
 distance_meters INTEGER NOT NULL CHECK (distance_meters >= 0),
 delivery TEXT NOT NULL DEFAULT 'PENDING' CHECK (delivery IN ('PENDING','SENT','OWNER_NOT_LINKED','FAILED')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT fk_perimeter_alert_base FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_perimeter_alerts_base
 ON perimeter_watch_alerts(installation_id,server_id,base_id,created_at DESC);

ALTER TABLE security_service_offers DROP CONSTRAINT IF EXISTS security_service_offers_service_id_check;
ALTER TABLE security_service_offers ADD CONSTRAINT security_service_offers_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING'));
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_service_id_check;
ALTER TABLE security_service_purchases ADD CONSTRAINT security_service_purchases_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING'));
`
