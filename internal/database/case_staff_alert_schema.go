package database

// CASEStaffAlertSQL stores the owner's "send C.A.S.E. alerts to staff" switch
// (off by default) and a delivery queue for staff alerts. A row is only
// queued for a finding from a released detector (caseintel.ModuleReleased);
// none is released, so nothing is queued until a detector passes review.
const CASEStaffAlertSQL = `
CREATE TABLE IF NOT EXISTS case_alert_settings (
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (installation_id,server_id),
 CONSTRAINT fk_case_alert_settings_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_case_alert_settings_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS case_alert_deliveries (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 detector_id TEXT NOT NULL CHECK (char_length(detector_id) BETWEEN 1 AND 80),
 incident_key CHAR(64) NOT NULL CHECK (incident_key ~ '^[0-9a-f]{64}$'),
 player_id BIGINT NOT NULL,
 finding JSONB NOT NULL,
 status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','LEASED','RETRY_WAIT','SENT','DEAD')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 lease_until TIMESTAMPTZ,
 last_error_code TEXT NOT NULL DEFAULT '' CHECK (char_length(last_error_code) <= 60),
 channel_id TEXT NOT NULL DEFAULT '',
 message_id TEXT NOT NULL DEFAULT '',
 sent_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT uq_case_alert_incident UNIQUE (installation_id,incident_key),
 CONSTRAINT case_alert_sent CHECK (status <> 'SENT' OR sent_at IS NOT NULL),
 CONSTRAINT case_alert_lease CHECK ((status = 'LEASED') = (lease_until IS NOT NULL)),
 CONSTRAINT fk_case_alert_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_case_alert_guild_server FOREIGN KEY (guild_id,server_id)
  REFERENCES game_servers(guild_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_case_alert_due
 ON case_alert_deliveries(status,next_attempt_at,id) WHERE status IN ('PENDING','RETRY_WAIT','LEASED');
CREATE INDEX IF NOT EXISTS idx_case_alert_recent
 ON case_alert_deliveries(installation_id,server_id,created_at DESC);
`
