package database

// CASEReviewSkeletonSQL is an additive, inert schema only. No production code
// reads or writes these tables. Review and explicit approval are required
// before any migration-containing branch is merged or deployed.
const CASEReviewSkeletonSQL = `
-- Enforce both the Discord installation's guild and its selected game server.
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_server_scope ON game_servers(guild_id,id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_connection_scope ON discord_guild_connections(id,guild_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_installation_scope
 ON installations(id,game_server_id,discord_guild_connection_id);

CREATE TABLE IF NOT EXISTS case_review_cases (
 id BIGSERIAL PRIMARY KEY,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 installation_id BIGINT NOT NULL,
 discord_guild_connection_id BIGINT NOT NULL,
 detector_id TEXT NOT NULL CHECK (char_length(detector_id) BETWEEN 1 AND 80),
 detector_version TEXT NOT NULL CHECK (char_length(detector_version) BETWEEN 1 AND 40),
 evidence_fingerprint CHAR(64) NOT NULL CHECK (evidence_fingerprint ~ '^[0-9a-f]{64}$'),
 source_quality_ref CHAR(64) NOT NULL CHECK (source_quality_ref ~ '^[0-9a-f]{64}$'),
 status TEXT NOT NULL CHECK (status IN ('PENDING_REVIEW','REVIEWED','DISMISSED','RESOLVED')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT uq_case_review_scope_id UNIQUE(guild_id,server_id,installation_id,id),
 CONSTRAINT uq_case_review_fingerprint UNIQUE(installation_id,detector_id,detector_version,evidence_fingerprint),
 FOREIGN KEY(guild_id,server_id) REFERENCES game_servers(guild_id,id) ON DELETE CASCADE,
 FOREIGN KEY(discord_guild_connection_id,guild_id)
  REFERENCES discord_guild_connections(id,guild_id) ON DELETE CASCADE,
 FOREIGN KEY(installation_id,server_id,discord_guild_connection_id)
  REFERENCES installations(id,game_server_id,discord_guild_connection_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_case_review_queue
 ON case_review_cases(installation_id,status,id DESC);

-- Case provenance is immutable after admission. Only neutral review status and
-- updated_at may change, via a future independently authorized transaction.
CREATE OR REPLACE FUNCTION case_review_identity_immutable() RETURNS TRIGGER AS $
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'C.A.S.E. review cases cannot be deleted by the runtime';
 END IF;
 IF ROW(NEW.guild_id,NEW.server_id,NEW.installation_id,
        NEW.discord_guild_connection_id,NEW.detector_id,NEW.detector_version,
        NEW.evidence_fingerprint,NEW.source_quality_ref,NEW.created_at)
    IS DISTINCT FROM
    ROW(OLD.guild_id,OLD.server_id,OLD.installation_id,
        OLD.discord_guild_connection_id,OLD.detector_id,OLD.detector_version,
        OLD.evidence_fingerprint,OLD.source_quality_ref,OLD.created_at) THEN
  RAISE EXCEPTION 'C.A.S.E. review case identity is immutable';
 END IF;
 RETURN NEW;
END;
$ LANGUAGE plpgsql;
CREATE TRIGGER trg_case_review_identity_immutable
 BEFORE UPDATE OR DELETE ON case_review_cases
 FOR EACH ROW EXECUTE FUNCTION case_review_identity_immutable();


CREATE TABLE IF NOT EXISTS case_review_evidence (
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 installation_id BIGINT NOT NULL,
 case_id BIGINT NOT NULL,
 evidence_id BIGINT NOT NULL,
 PRIMARY KEY(case_id,evidence_id),
 FOREIGN KEY(guild_id,server_id,installation_id,case_id)
  REFERENCES case_review_cases(guild_id,server_id,installation_id,id) ON DELETE RESTRICT,
 FOREIGN KEY(guild_id,server_id,evidence_id)
  REFERENCES case_evidence_events(guild_id,server_id,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_case_review_evidence_case ON case_review_evidence(guild_id,server_id,installation_id,case_id);

CREATE OR REPLACE FUNCTION case_review_evidence_immutable() RETURNS TRIGGER AS $
BEGIN
 RAISE EXCEPTION 'C.A.S.E. case evidence links are immutable';
END;
$ LANGUAGE plpgsql;
CREATE TRIGGER trg_case_review_evidence_immutable
 BEFORE UPDATE OR DELETE ON case_review_evidence
 FOR EACH ROW EXECUTE FUNCTION case_review_evidence_immutable();


CREATE TABLE IF NOT EXISTS case_review_audit (
 id BIGSERIAL PRIMARY KEY,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 installation_id BIGINT NOT NULL,
 case_id BIGINT NOT NULL,
 action_key CHAR(64) NOT NULL CHECK (action_key ~ '^[0-9a-f]{64}$'),
 actor_user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE RESTRICT,
 from_status TEXT NOT NULL CHECK (from_status IN ('PENDING_REVIEW','REVIEWED','DISMISSED','RESOLVED')),
 to_status TEXT NOT NULL CHECK (to_status IN ('PENDING_REVIEW','REVIEWED','DISMISSED','RESOLVED')),
 reason_code TEXT NOT NULL CHECK (char_length(reason_code) BETWEEN 1 AND 60),
 note TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT uq_case_review_action UNIQUE(case_id,action_key),
 FOREIGN KEY(guild_id,server_id,installation_id,case_id)
  REFERENCES case_review_cases(guild_id,server_id,installation_id,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_case_review_audit_history ON case_review_audit(case_id,id);

-- Application-level audit history must not be edited or deleted. Coupled
-- with RESTRICT on case references, a direct case DELETE cannot erase it.
-- Privileged schema owners still control migrations; this does not replace
-- role separation and audited administrative maintenance.
CREATE OR REPLACE FUNCTION case_review_audit_immutable() RETURNS TRIGGER AS $
BEGIN
 RAISE EXCEPTION 'C.A.S.E. review audit is immutable';
END;
$ LANGUAGE plpgsql;
CREATE TRIGGER trg_case_review_audit_immutable
 BEFORE UPDATE OR DELETE ON case_review_audit
 FOR EACH ROW EXECUTE FUNCTION case_review_audit_immutable();


-- No destination ID or arbitrary payload: a future publisher must resolve
-- CASE_ALERTS privately and revalidate every permission before delivery.
CREATE TABLE IF NOT EXISTS case_staff_outbox (
 id BIGSERIAL PRIMARY KEY,
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 installation_id BIGINT NOT NULL,
 case_id BIGINT NOT NULL,
 event_version BIGINT NOT NULL CHECK (event_version > 0),
 delivery_key CHAR(64) NOT NULL CHECK (delivery_key ~ '^[0-9a-f]{64}$'),
 status TEXT NOT NULL CHECK (status IN ('PENDING','LEASED','RETRY_WAIT','SENT','DEAD','SUPPRESSED')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 lease_token CHAR(64) CHECK (lease_token IS NULL OR lease_token ~ '^[0-9a-f]{64}$'),
 lease_until TIMESTAMPTZ,
 sent_at TIMESTAMPTZ,
 last_error_code TEXT NOT NULL DEFAULT '' CHECK (
  last_error_code IN ('','RATE_LIMIT','TEMPORARY_TRANSPORT','DISCORD_UNAVAILABLE',
                      'DESTINATION_UNAVAILABLE','PERMISSION_REVOKED','STAFF_DISMISSED',
                      'OWNER_DISABLED','EVIDENCE_INVALIDATED')
 ),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT uq_case_staff_outbox_event UNIQUE(installation_id,case_id,event_version),
 CONSTRAINT uq_case_staff_outbox_key UNIQUE(installation_id,delivery_key),
 CONSTRAINT case_staff_outbox_sent CHECK (status <> 'SENT' OR sent_at IS NOT NULL),
 CONSTRAINT case_staff_outbox_lease CHECK (
  (status = 'LEASED' AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
  OR (status <> 'LEASED' AND lease_token IS NULL AND lease_until IS NULL)
 ),
 FOREIGN KEY(guild_id,server_id,installation_id,case_id)
  REFERENCES case_review_cases(guild_id,server_id,installation_id,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_case_staff_outbox_due
 ON case_staff_outbox(status,next_attempt_at,id) WHERE status IN ('PENDING','RETRY_WAIT');
`
