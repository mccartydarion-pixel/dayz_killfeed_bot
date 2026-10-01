package database

// BaseRaidAlarmSQL stores the owner's Base Raid Alarm switch and a log of the
// alarms sent. The alarm is off until the server owner turns it on. It reads
// the owner-registered C.A.S.E. bases and the ADM "dismantled" build lines; it
// never touches the economy, the shop or any detector.
const BaseRaidAlarmSQL = `
CREATE TABLE IF NOT EXISTS base_raid_alarm_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 cooldown_seconds INTEGER NOT NULL DEFAULT 600 CHECK (cooldown_seconds BETWEEN 60 AND 3600),
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CONSTRAINT fk_base_raid_settings_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_base_raid_settings_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS base_raid_alerts (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 base_id BIGINT NOT NULL,
 raider_player_id BIGINT, -- informational; no FK so player cleanup never blocks
 raider_name TEXT NOT NULL CHECK (char_length(raider_name) BETWEEN 1 AND 128),
 part TEXT NOT NULL CHECK (char_length(part) BETWEEN 1 AND 64),
 target TEXT NOT NULL DEFAULT '' CHECK (char_length(target) <= 64),
 tool TEXT NOT NULL DEFAULT '' CHECK (char_length(tool) <= 64),
 time_of_day TEXT NOT NULL DEFAULT '' CHECK (char_length(time_of_day) <= 16),
 delivery TEXT NOT NULL DEFAULT 'PENDING' CHECK (delivery IN ('PENDING','SENT','OWNER_NOT_LINKED','FAILED')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT fk_base_raid_alert_base FOREIGN KEY (installation_id,guild_id,server_id,base_id)
  REFERENCES case_registered_bases(installation_id,guild_id,server_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_base_raid_alerts_base
 ON base_raid_alerts(installation_id,server_id,base_id,created_at DESC);
`
