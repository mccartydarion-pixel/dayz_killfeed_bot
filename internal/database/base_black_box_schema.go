package database

// BaseBlackBoxSQL adds the Base Black Box: a history, per registered base, of
// players who came near it or took parts off it, for the base owner to look
// back on. Off until the server owner turns it on; the Security Marketplace
// can sell it like the Base Raid Alarm and Perimeter Watch. It only records
// what the server log already reports and never acts on anyone.
const BaseBlackBoxSQL = `
CREATE TABLE IF NOT EXISTS base_black_box_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 retention_days INTEGER NOT NULL DEFAULT 14 CHECK (retention_days BETWEEN 3 AND 30),
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CONSTRAINT fk_black_box_settings_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_black_box_settings_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS base_black_box_events (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 kind TEXT NOT NULL CHECK (kind IN ('VISIT','DISMANTLE')),
 player_id BIGINT NOT NULL,
 player_name TEXT NOT NULL CHECK (char_length(player_name) BETWEEN 1 AND 128),
 detail TEXT NOT NULL DEFAULT '' CHECK (char_length(detail) <= 128),
 closest_meters INTEGER NOT NULL CHECK (closest_meters >= 0),
 sightings INTEGER NOT NULL DEFAULT 1 CHECK (sightings >= 1),
 first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT fk_black_box_event_base FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_black_box_events_base
 ON base_black_box_events(installation_id,server_id,base_id,last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_black_box_events_age ON base_black_box_events(last_seen_at);

ALTER TABLE security_service_offers DROP CONSTRAINT IF EXISTS security_service_offers_service_id_check;
ALTER TABLE security_service_offers ADD CONSTRAINT security_service_offers_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING','BASE_BLACK_BOX'));
ALTER TABLE security_service_purchases DROP CONSTRAINT IF EXISTS security_service_purchases_service_id_check;
ALTER TABLE security_service_purchases ADD CONSTRAINT security_service_purchases_service_id_check
 CHECK (service_id IN ('BASE_RAID_ALARM','PERIMETER_MONITORING','BASE_BLACK_BOX'));
`
