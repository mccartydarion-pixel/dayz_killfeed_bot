package database

// CASEDetectorSettingsSQL stores inert owner preferences for the approved Core
// Eight. No runtime detector or Discord sender reads this table.
const CASEDetectorSettingsSQL = `
CREATE TABLE IF NOT EXISTS case_detector_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 module_id TEXT NOT NULL CHECK (module_id IN (
  'CASE-BASE-001','CASE-SKYWALK-001','CASE-DUPE-001','CASE-PC-XBOX-001',
  'CASE-NOCLIP-001','CASE-UNDERMAP-001','CASE-LOGIN-001','CASE-TELEPORT-001'
 )),
 sensitivity TEXT NOT NULL CHECK (sensitivity IN ('RELAXED','BALANCED','STRICT')),
 revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,guild_id,server_id,module_id),
 CONSTRAINT fk_case_settings_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_case_settings_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);
`
