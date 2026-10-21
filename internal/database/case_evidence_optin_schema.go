package database

// CaseEvidenceOptinSQL stores a server owner's own choice to collect C.A.S.E.
// evidence for their server. It only counts when the platform setting
// CASE_EVIDENCE_SELF_SERVE=true (default off) and CASE_EVIDENCE_ENABLED=true,
// and the Owner Hub flag still overrides it. Additive (one table).
const CaseEvidenceOptinSQL = `
CREATE TABLE IF NOT EXISTS case_evidence_optins (
 installation_id BIGINT PRIMARY KEY,
 server_id BIGINT NOT NULL,
 enabled BOOLEAN NOT NULL,
 updated_by_user_id BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT fk_case_evidence_optin_installation FOREIGN KEY (installation_id,server_id)
  REFERENCES installations(id,game_server_id) ON DELETE CASCADE
);
`
