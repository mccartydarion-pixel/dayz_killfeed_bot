package canary

// ProposedAttemptMigrationName / ProposedAttemptMigrationSQL are the durable attempt ledger PROPOSAL
// (Gate C). They are deliberately NOT registered in internal/database/migrations.go: registering and
// deploying them is its own approval, separate from any file write. The integration test
// (ledger_integration_test.go) applies the SQL inside a transaction that is always rolled back.
//
// What the database itself guarantees once deployed:
//   - Tenant isolation: an attempt references its delivery by (id, organization_id, installation_id),
//     so it can never be attached to another tenant's delivery; transitions are scoped by the same keys.
//   - One open attempt per delivery (partial unique index) and no new attempt after a fulfilled or
//     uncertain one (insert guard, serialized by a lock on the delivery row).
//   - A real identity: attempt_id is exactly champion:d<delivery_id>:a<attempt>, so the preview id
//     champion:d0:a1 can never be stored (no delivery 0 exists and the format is checked).
//   - Compare-and-set transitions along the state machine only (guard trigger), each one written to an
//     append-only event log with a mandatory actor (SET LOCAL champion.actor).
//   - Refund / fulfil restrictions: from FILE_PREPARED (an upload may be in flight) until the entry is
//     verified removed, the delivery can be neither cancelled (refund) nor fulfilled by hand; a new
//     attempt, and PLAN_CREATED -> FILE_PREPARED, need an open delivery and a PENDING_FULFILLMENT purchase.
//   - FULFILLED only with the unstage verified and a named verifier, and only together with the
//     delivery's own FULFILLED (deferred constraint trigger, checked at commit).
//   - Crash recovery: every field Reconcile/DecideRestart needs (staged/unstaged digests, staged boot,
//     restart boot, unstage time) is durable; an attempt is never deleted by the application.
const ProposedAttemptMigrationName = "0054_shop_delivery_attempts"

const ProposedAttemptMigrationSQL = `
-- Tenant-bound identity of a delivery, for the attempt foreign key.
CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_deliveries_id_tenant ON shop_deliveries(id, organization_id, installation_id);

CREATE TABLE IF NOT EXISTS shop_delivery_attempts (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    delivery_id BIGINT NOT NULL,
    attempt INT NOT NULL CHECK (attempt BETWEEN 1 AND 100),
    attempt_id TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL DEFAULT 'PLAN_CREATED' CHECK (state IN ('PLAN_CREATED','FILE_PREPARED','FILE_STAGED','AWAITING_RESTART',
        'RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED','FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW')),
    fingerprint TEXT NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    artifact_path TEXT NOT NULL CHECK (artifact_path = 'champion/champion_shop_delivery.json'),
    class_name TEXT NOT NULL CHECK (class_name ~ '^[A-Za-z][A-Za-z0-9_]{1,63}$'),
    quantity INT NOT NULL CHECK (quantity BETWEEN 1 AND 10),
    pos_x DOUBLE PRECISION NOT NULL,
    pos_y DOUBLE PRECISION NOT NULL CHECK (pos_y BETWEEN -50 AND 1500),
    pos_z DOUBLE PRECISION NOT NULL,
    drop_source_file TEXT NOT NULL CHECK (drop_source_file LIKE '%.ADM'),
    drop_source_offset BIGINT NOT NULL CHECK (drop_source_offset > 0),
    before_sha256 TEXT,
    staged_sha256 TEXT,
    unstaged_sha256 TEXT,
    staged_at TIMESTAMPTZ,
    staged_boot_file TEXT,
    restart_boot_file TEXT,
    restart_observed_at TIMESTAMPTZ,
    unstage_verified_at TIMESTAMPTZ,
    verified_by TEXT,
    fulfilled_at TIMESTAMPTZ,
    failure_reason TEXT CHECK (failure_reason IS NULL OR length(failure_reason) <= 500),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (delivery_id, organization_id, installation_id)
        REFERENCES shop_deliveries(id, organization_id, installation_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_delivery_attempts_number UNIQUE (delivery_id, attempt),
    CONSTRAINT shop_delivery_attempts_id_format CHECK (attempt_id = 'champion:d' || delivery_id::text || ':a' || attempt::text),
    CONSTRAINT shop_delivery_attempts_staged CHECK (state NOT IN ('FILE_STAGED','AWAITING_RESTART','RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED','FULFILLED')
        OR (staged_sha256 IS NOT NULL AND staged_at IS NOT NULL AND staged_boot_file IS NOT NULL)),
    CONSTRAINT shop_delivery_attempts_restarted CHECK (state NOT IN ('RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED','FULFILLED')
        OR (restart_boot_file IS NOT NULL AND restart_observed_at IS NOT NULL)),
    CONSTRAINT shop_delivery_attempts_unstaged CHECK (state NOT IN ('VERIFICATION_REQUIRED','FULFILLED','UNSTAGED')
        OR (unstage_verified_at IS NOT NULL AND unstaged_sha256 IS NOT NULL)),
    CONSTRAINT shop_delivery_attempts_fulfilled CHECK (state <> 'FULFILLED' OR (verified_by IS NOT NULL AND fulfilled_at IS NOT NULL)),
    CONSTRAINT shop_delivery_attempts_review CHECK (state <> 'FAILED_REVIEW' OR failure_reason IS NOT NULL)
);
-- At most one open attempt per delivery.
CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_delivery_attempts_open ON shop_delivery_attempts(delivery_id)
    WHERE state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW');
CREATE INDEX IF NOT EXISTS idx_shop_delivery_attempts_installation ON shop_delivery_attempts(installation_id, state, id DESC);

-- Append-only transition history.
CREATE TABLE IF NOT EXISTS shop_delivery_attempt_events (
    id BIGSERIAL PRIMARY KEY,
    attempt_row_id BIGINT NOT NULL REFERENCES shop_delivery_attempts(id) ON DELETE CASCADE,
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    actor TEXT NOT NULL CHECK (actor <> ''),
    evidence TEXT NOT NULL DEFAULT '' CHECK (length(evidence) <= 1024),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_shop_delivery_attempt_events_attempt ON shop_delivery_attempt_events(attempt_row_id, id);

-- Attempt guard: creation rules, immutable identity, the state machine and the event log.
CREATE OR REPLACE FUNCTION shop_delivery_attempt_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
    d_status TEXT; d_policy TEXT; p_status TEXT; actor TEXT;
BEGIN
    actor := COALESCE(current_setting('champion.actor', true), '');
    IF actor = '' THEN
        RAISE EXCEPTION 'shop attempt change without champion.actor' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'PLAN_CREATED' THEN
            RAISE EXCEPTION 'a shop attempt starts at PLAN_CREATED' USING ERRCODE = 'check_violation';
        END IF;
        SELECT sd.status, sd.delivery_policy, sp.status INTO d_status, d_policy, p_status
          FROM shop_deliveries sd JOIN shop_purchases sp ON sp.id = sd.purchase_id AND sp.installation_id = sd.installation_id
         WHERE sd.id = NEW.delivery_id AND sd.organization_id = NEW.organization_id AND sd.installation_id = NEW.installation_id
         FOR UPDATE OF sd;
        IF NOT FOUND OR d_status <> 'MANUAL_READY' OR d_policy <> 'MANUAL_COORDINATE' OR p_status <> 'PENDING_FULFILLMENT' THEN
            RAISE EXCEPTION 'the delivery is not open for an automatic attempt' USING ERRCODE = 'check_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM shop_delivery_attempts a WHERE a.delivery_id = NEW.delivery_id AND a.state NOT IN ('ABANDONED','UNSTAGED')) THEN
            RAISE EXCEPTION 'an earlier attempt is open, fulfilled or under review' USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.attempt <> (SELECT COUNT(*) + 1 FROM shop_delivery_attempts a WHERE a.delivery_id = NEW.delivery_id) THEN
            RAISE EXCEPTION 'the attempt number does not follow the delivery history' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.organization_id <> OLD.organization_id OR NEW.installation_id <> OLD.installation_id OR NEW.delivery_id <> OLD.delivery_id
       OR NEW.attempt <> OLD.attempt OR NEW.attempt_id <> OLD.attempt_id OR NEW.fingerprint <> OLD.fingerprint
       OR NEW.artifact_path <> OLD.artifact_path OR NEW.class_name <> OLD.class_name OR NEW.quantity <> OLD.quantity
       OR NEW.pos_x <> OLD.pos_x OR NEW.pos_y <> OLD.pos_y OR NEW.pos_z <> OLD.pos_z
       OR NEW.drop_source_file <> OLD.drop_source_file OR NEW.drop_source_offset <> OLD.drop_source_offset THEN
        RAISE EXCEPTION 'a shop attempt identity is immutable' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state = OLD.state THEN
        IF OLD.state IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW') THEN
            RAISE EXCEPTION 'a terminal shop attempt is immutable' USING ERRCODE = 'check_violation';
        END IF;
        NEW.updated_at := NOW();
        RETURN NEW;
    END IF;
    IF (OLD.state, NEW.state) NOT IN (
        ('PLAN_CREATED','FILE_PREPARED'), ('PLAN_CREATED','ABANDONED'),
        ('FILE_PREPARED','FILE_STAGED'), ('FILE_PREPARED','ABANDONED'),
        ('FILE_STAGED','AWAITING_RESTART'), ('FILE_STAGED','UNSTAGED'), ('FILE_STAGED','FAILED_REVIEW'),
        ('AWAITING_RESTART','RESTART_OBSERVED'), ('AWAITING_RESTART','UNSTAGED'), ('AWAITING_RESTART','FAILED_REVIEW'),
        ('RESTART_OBSERVED','UNSTAGE_REQUIRED'), ('RESTART_OBSERVED','FAILED_REVIEW'),
        ('UNSTAGE_REQUIRED','VERIFICATION_REQUIRED'), ('UNSTAGE_REQUIRED','FAILED_REVIEW'),
        ('VERIFICATION_REQUIRED','FULFILLED'), ('VERIFICATION_REQUIRED','FAILED_REVIEW')) THEN
        RAISE EXCEPTION 'invalid shop attempt transition % -> %', OLD.state, NEW.state USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.state = 'FILE_PREPARED' THEN
        SELECT sd.status, sp.status INTO d_status, p_status
          FROM shop_deliveries sd JOIN shop_purchases sp ON sp.id = sd.purchase_id AND sp.installation_id = sd.installation_id
         WHERE sd.id = NEW.delivery_id FOR UPDATE OF sd;
        IF d_status <> 'MANUAL_READY' OR p_status <> 'PENDING_FULFILLMENT' THEN
            RAISE EXCEPTION 'the delivery was refunded or closed: abandon this attempt' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    INSERT INTO shop_delivery_attempt_events(attempt_row_id, from_state, to_state, actor, evidence)
    VALUES (OLD.id, OLD.state, NEW.state, actor, left(COALESCE(current_setting('champion.evidence', true), ''), 1024));
    NEW.updated_at := NOW();
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_guard ON shop_delivery_attempts;
CREATE TRIGGER trg_shop_delivery_attempt_guard BEFORE INSERT OR UPDATE ON shop_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_guard();

-- Refund / manual-fulfilment guard: while an attempt may be on the server, the delivery cannot close.
CREATE OR REPLACE FUNCTION shop_delivery_exposure_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status AND NEW.status IN ('FULFILLED','CANCELLED') AND EXISTS (
        SELECT 1 FROM shop_delivery_attempts a WHERE a.delivery_id = OLD.id
           AND a.state IN ('FILE_PREPARED','FILE_STAGED','AWAITING_RESTART','RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED')) THEN
        RAISE EXCEPTION 'an automatic delivery attempt may be on the server: resolve it before refunding or fulfilling' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_exposure_guard ON shop_deliveries;
CREATE TRIGGER trg_shop_delivery_exposure_guard BEFORE UPDATE OF status ON shop_deliveries
    FOR EACH ROW EXECUTE FUNCTION shop_delivery_exposure_guard();

-- A FULFILLED attempt commits only together with its delivery's FULFILLED.
CREATE OR REPLACE FUNCTION shop_delivery_attempt_fulfilled_check() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.state = 'FULFILLED' AND NOT EXISTS (SELECT 1 FROM shop_deliveries sd WHERE sd.id = NEW.delivery_id AND sd.status = 'FULFILLED') THEN
        RAISE EXCEPTION 'a FULFILLED attempt requires its delivery to be FULFILLED in the same transaction' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_fulfilled ON shop_delivery_attempts;
CREATE CONSTRAINT TRIGGER trg_shop_delivery_attempt_fulfilled AFTER UPDATE ON shop_delivery_attempts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_fulfilled_check();
`

// ProposedTransitionSQL is the compare-and-set a durable ledger uses: it moves an attempt only from the
// expected state and only inside the acting tenant, so two workers (or two tenants) can never both
// advance it. Run it after SET LOCAL champion.actor (and optionally champion.evidence).
const ProposedTransitionSQL = `UPDATE shop_delivery_attempts SET state=$3
 WHERE attempt_id=$1 AND state=$2 AND organization_id=$4 AND installation_id=$5 RETURNING id`

// ProposedOpenAttemptsSQL is what a restarted worker reads to reconcile (Reconcile / DecideRestart).
const ProposedOpenAttemptsSQL = `SELECT attempt_id, delivery_id, state, staged_sha256, unstaged_sha256, staged_at, staged_boot_file,
 restart_boot_file, unstage_verified_at FROM shop_delivery_attempts
 WHERE organization_id=$1 AND installation_id=$2 AND state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW') ORDER BY id`
