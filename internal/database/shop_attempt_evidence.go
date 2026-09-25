package database

// ShopAttemptEvidenceSQL is migration 0055_shop_delivery_attempt_evidence (docs/SHOP_DELIVERY_PHASE2C4.md):
// the structured, append-only evidence an operator records for a canary delivery attempt. It builds on
// 0054 (the attempt ledger) and changes nothing in it. Each record names its kind, its source and the
// recording actor.
//
//   - The attempt's write-once columns (staged hash, boots, unstage, physical evidence) are filled from
//     these records by the operator service, so every ledger fact traces back to a recorded observation.
//   - Physical kinds (item observed, pickup, no additional spawn) accept ONLY an in-game observation:
//     an RPT log line (SPAWNER_LOG) is informational and can never stand in for physical proof.
//   - A kind is accepted only in the attempt states where it can be true (a new boot only while
//     awaiting the restart, a second boot only after the verified unstage, ...), serialized against
//     concurrent transitions by a share lock on the attempt row.
//
// Custom SQLSTATE: SA423 evidence not acceptable for the attempt's current state.
const ShopAttemptEvidenceSQL = `
CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_delivery_attempts_id_tenant ON shop_delivery_attempts(id, organization_id, installation_id);

CREATE TABLE IF NOT EXISTS shop_delivery_attempt_evidence (
    id BIGSERIAL PRIMARY KEY,
    attempt_row_id BIGINT NOT NULL,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('STAGED_FILE_HASH','STAGING_BOOT','NEW_BOOT','SPAWNER_LOG','ITEM_OBSERVED','PICKUP_CONFIRMED',
        'UNSTAGED_FILE_HASH','SECOND_BOOT','NO_ADDITIONAL_SPAWN','REVIEW_UNCERTAIN')),
    source TEXT NOT NULL CHECK (source IN ('NITRADO_READBACK','BOOT_AUTHORITY','IN_GAME_OBSERVATION','RPT_LOG','OPERATOR_ASSESSMENT')),
    recorded_by TEXT NOT NULL CHECK (recorded_by <> '' AND length(recorded_by) <= 100),
    sha256 TEXT CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
    previous_sha256 TEXT CHECK (previous_sha256 IS NULL OR previous_sha256 ~ '^[0-9a-f]{64}$'),
    boot_file TEXT CHECK (boot_file IS NULL OR (boot_file LIKE '%.ADM' AND length(boot_file) <= 300)),
    boot_started_at TIMESTAMPTZ,
    observed_by TEXT CHECK (observed_by IS NULL OR (observed_by <> '' AND length(observed_by) <= 100)),
    observed_at TIMESTAMPTZ NOT NULL,
    detail TEXT NOT NULL DEFAULT '' CHECK (length(detail) <= 500),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (attempt_row_id, organization_id, installation_id)
        REFERENCES shop_delivery_attempts(id, organization_id, installation_id) ON DELETE CASCADE,
    -- Which source may prove which kind. Physical kinds: in-game observation only.
    CONSTRAINT shop_attempt_evidence_source CHECK (
        (kind IN ('STAGED_FILE_HASH','UNSTAGED_FILE_HASH') AND source = 'NITRADO_READBACK')
        OR (kind IN ('STAGING_BOOT','NEW_BOOT','SECOND_BOOT') AND source = 'BOOT_AUTHORITY')
        OR (kind IN ('ITEM_OBSERVED','PICKUP_CONFIRMED','NO_ADDITIONAL_SPAWN') AND source = 'IN_GAME_OBSERVATION')
        OR (kind = 'SPAWNER_LOG' AND source = 'RPT_LOG')
        OR (kind = 'REVIEW_UNCERTAIN' AND source = 'OPERATOR_ASSESSMENT')),
    -- What each kind must carry.
    CONSTRAINT shop_attempt_evidence_fields CHECK (
        (kind = 'STAGED_FILE_HASH' AND sha256 IS NOT NULL AND previous_sha256 IS NOT NULL AND sha256 <> previous_sha256)
        OR (kind = 'UNSTAGED_FILE_HASH' AND sha256 IS NOT NULL)
        OR (kind = 'STAGING_BOOT' AND boot_file IS NOT NULL)
        OR (kind IN ('NEW_BOOT','SECOND_BOOT') AND boot_file IS NOT NULL AND boot_started_at IS NOT NULL AND boot_started_at <= observed_at)
        OR (kind IN ('ITEM_OBSERVED','PICKUP_CONFIRMED','NO_ADDITIONAL_SPAWN') AND observed_by IS NOT NULL)
        OR (kind IN ('SPAWNER_LOG','REVIEW_UNCERTAIN') AND detail <> ''))
);
-- One record per proof kind (write-once); log notes and uncertain assessments may repeat.
CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_attempt_evidence_kind ON shop_delivery_attempt_evidence(attempt_row_id, kind)
    WHERE kind NOT IN ('SPAWNER_LOG','REVIEW_UNCERTAIN');
CREATE INDEX IF NOT EXISTS idx_shop_attempt_evidence_attempt ON shop_delivery_attempt_evidence(attempt_row_id, id);

CREATE OR REPLACE FUNCTION shop_attempt_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
    st TEXT; resolved TEXT;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'shop attempt evidence is append-only' USING ERRCODE = 'SA422';
    END IF;
    SELECT a.state, a.review_resolution INTO st, resolved FROM shop_delivery_attempts a
     WHERE a.id = NEW.attempt_row_id AND a.organization_id = NEW.organization_id AND a.installation_id = NEW.installation_id
     FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'shop attempt not found for this tenant' USING ERRCODE = 'SA423';
    END IF;
    IF NOT (
        (NEW.kind IN ('STAGED_FILE_HASH','STAGING_BOOT') AND st = 'FILE_PREPARED')
        OR (NEW.kind = 'NEW_BOOT' AND st = 'AWAITING_RESTART')
        OR (NEW.kind IN ('SPAWNER_LOG','ITEM_OBSERVED','PICKUP_CONFIRMED') AND st IN ('AWAITING_RESTART','RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED'))
        OR (NEW.kind = 'UNSTAGED_FILE_HASH' AND st IN ('FILE_STAGED','AWAITING_RESTART','UNSTAGE_REQUIRED'))
        OR (NEW.kind IN ('SECOND_BOOT','NO_ADDITIONAL_SPAWN') AND st = 'VERIFICATION_REQUIRED')
        OR (NEW.kind = 'REVIEW_UNCERTAIN' AND st = 'FAILED_REVIEW' AND resolved IS NULL)) THEN
        RAISE EXCEPTION 'evidence % is not acceptable while the attempt is %', NEW.kind, st USING ERRCODE = 'SA423';
    END IF;
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_attempt_evidence_guard ON shop_delivery_attempt_evidence;
CREATE TRIGGER trg_shop_attempt_evidence_guard BEFORE INSERT OR UPDATE ON shop_delivery_attempt_evidence
    FOR EACH ROW EXECUTE FUNCTION shop_attempt_evidence_guard();
`
