package database

// CASEShadowVerdictSQL stores staff verdicts on test-run (shadow) findings:
// was the flag a false alarm with a normal reason, or did it look suspicious.
// The review score helps decide when a detector is ready; it never releases a
// detector, sends an alert or acts on a player by itself.
const CASEShadowVerdictSQL = `
CREATE TABLE IF NOT EXISTS case_shadow_verdicts (
 id BIGSERIAL PRIMARY KEY,
 installation_id BIGINT NOT NULL,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 detector_id TEXT NOT NULL CHECK (detector_id IN ('CASE-LOGIN-001','CASE-BASE-001')),
 incident_key CHAR(64) NOT NULL CHECK (incident_key ~ '^[0-9a-f]{64}$'),
 player_id BIGINT NOT NULL,
 verdict TEXT NOT NULL CHECK (verdict IN ('FALSE_ALARM','SUSPICIOUS')),
 note TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 300),
 reviewer_user_id BIGINT,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT uq_case_shadow_verdict UNIQUE (installation_id,detector_id,incident_key),
 CONSTRAINT fk_case_verdict_installation_server FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE,
 CONSTRAINT fk_case_verdict_player FOREIGN KEY (guild_id,player_id)
  REFERENCES players(guild_id,id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_case_shadow_verdicts_scope
 ON case_shadow_verdicts(installation_id,server_id,detector_id,updated_at DESC);
`
