package database

// ShopDeliveryAttemptsSQL is migration 0054_shop_delivery_attempts: the durable delivery-attempt ledger
// for automatic console delivery (docs/SHOP_DELIVERY_PHASE2C3.md). It is additive: two new tables, one
// unique index on shop_deliveries (id is already unique, so it cannot fail on existing rows) and
// triggers. It changes no existing row and no existing Shop behaviour while no attempt exists - and
// nothing creates attempts yet (automatic delivery stays disabled).
//
// Custom SQLSTATEs (mapped to typed errors in internal/repository):
//
//	SA409  the delivery may have an item on the server (or an unresolved review): no refund / manual fulfil
//	SA410  a new attempt is refused: an attempt is open, fulfilled or under review
//	SA411  the delivery is not open for an automatic attempt (tenant, policy, status or purchase status)
//	SA412  the attempt number does not follow the delivery's history
//	SA422  invalid transition, immutable identity/evidence, or a change without champion.actor
const ShopDeliveryAttemptsSQL = `
-- Tenant-bound identity of a delivery, referenced by the attempt foreign key.
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
    pos_x DOUBLE PRECISION NOT NULL CHECK (pos_x >= 0 AND pos_x <= 100000),
    pos_y DOUBLE PRECISION NOT NULL CHECK (pos_y BETWEEN -50 AND 1500),
    pos_z DOUBLE PRECISION NOT NULL CHECK (pos_z >= 0 AND pos_z <= 100000),
    drop_source_file TEXT NOT NULL CHECK (drop_source_file LIKE '%.ADM'),
    drop_source_offset BIGINT NOT NULL CHECK (drop_source_offset > 0),
    -- File evidence (SHA-256 of verified read-backs) and boot evidence (boot authority's ADM files).
    before_sha256 TEXT CHECK (before_sha256 IS NULL OR before_sha256 ~ '^[0-9a-f]{64}$'),
    staged_sha256 TEXT CHECK (staged_sha256 IS NULL OR staged_sha256 ~ '^[0-9a-f]{64}$'),
    unstaged_sha256 TEXT CHECK (unstaged_sha256 IS NULL OR unstaged_sha256 ~ '^[0-9a-f]{64}$'),
    staged_at TIMESTAMPTZ,
    staged_boot_file TEXT,
    restart_boot_file TEXT,
    restart_observed_at TIMESTAMPTZ,
    unstage_verified_at TIMESTAMPTZ,
    -- Physical evidence (Gate F/H): a named observer saw the item, it was picked up, and after a second
    -- boot with the entry removed no new item appeared.
    item_observed_by TEXT,
    item_observed_at TIMESTAMPTZ,
    picked_up_by TEXT,
    pickup_observed_at TIMESTAMPTZ,
    second_boot_file TEXT,
    second_boot_started_at TIMESTAMPTZ,
    no_respawn_checked_at TIMESTAMPTZ,
    verified_by TEXT,
    fulfilled_at TIMESTAMPTZ,
    failure_reason TEXT CHECK (failure_reason IS NULL OR length(failure_reason) BETWEEN 1 AND 500),
    -- A FAILED_REVIEW attempt stays uncertain until a human records what happened.
    review_resolution TEXT CHECK (review_resolution IS NULL OR review_resolution IN ('NOT_SPAWNED','SPAWNED')),
    review_resolved_by TEXT,
    review_resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (delivery_id, organization_id, installation_id)
        REFERENCES shop_deliveries(id, organization_id, installation_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_delivery_attempts_number UNIQUE (delivery_id, attempt),
    CONSTRAINT shop_delivery_attempts_id_format CHECK (attempt_id = 'champion:d' || delivery_id::text || ':a' || attempt::text),
    CONSTRAINT shop_delivery_attempts_staged CHECK (state NOT IN ('FILE_STAGED','AWAITING_RESTART','RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED','FULFILLED')
        OR (staged_sha256 IS NOT NULL AND staged_at IS NOT NULL AND staged_boot_file IS NOT NULL)),
    CONSTRAINT shop_delivery_attempts_restarted CHECK (state NOT IN ('RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED','FULFILLED')
        OR (restart_boot_file IS NOT NULL AND restart_observed_at IS NOT NULL AND restart_boot_file <> staged_boot_file)),
    CONSTRAINT shop_delivery_attempts_unstaged CHECK (state NOT IN ('VERIFICATION_REQUIRED','FULFILLED','UNSTAGED')
        OR (unstage_verified_at IS NOT NULL AND unstaged_sha256 IS NOT NULL AND unstaged_sha256 <> staged_sha256 AND unstage_verified_at >= staged_at)),
    CONSTRAINT shop_delivery_attempts_fulfilled CHECK (state <> 'FULFILLED' OR (
        verified_by IS NOT NULL AND fulfilled_at IS NOT NULL
        AND item_observed_by IS NOT NULL AND item_observed_at IS NOT NULL AND item_observed_at >= staged_at
        AND picked_up_by IS NOT NULL AND pickup_observed_at IS NOT NULL AND pickup_observed_at >= item_observed_at
        AND second_boot_file IS NOT NULL AND second_boot_file <> restart_boot_file AND second_boot_file <> staged_boot_file
        AND second_boot_started_at IS NOT NULL AND second_boot_started_at > unstage_verified_at
        AND no_respawn_checked_at IS NOT NULL AND no_respawn_checked_at >= second_boot_started_at)),
    CONSTRAINT shop_delivery_attempts_review CHECK (state <> 'FAILED_REVIEW' OR failure_reason IS NOT NULL),
    CONSTRAINT shop_delivery_attempts_resolution CHECK ((review_resolution IS NULL AND review_resolved_by IS NULL AND review_resolved_at IS NULL)
        OR (state = 'FAILED_REVIEW' AND review_resolution IS NOT NULL AND review_resolved_by IS NOT NULL AND review_resolved_at IS NOT NULL))
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
    actor TEXT NOT NULL CHECK (actor <> '' AND length(actor) <= 100),
    evidence TEXT NOT NULL DEFAULT '' CHECK (length(evidence) <= 1024),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_shop_delivery_attempt_events_attempt ON shop_delivery_attempt_events(attempt_row_id, id);

CREATE OR REPLACE FUNCTION shop_delivery_attempt_events_immutable() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    RAISE EXCEPTION 'shop attempt history is append-only' USING ERRCODE = 'SA422';
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_events_immutable ON shop_delivery_attempt_events;
CREATE TRIGGER trg_shop_delivery_attempt_events_immutable BEFORE UPDATE ON shop_delivery_attempt_events
    FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_events_immutable();

-- Attempt guard: creation rules, immutable identity and evidence, the state machine, the event log.
CREATE OR REPLACE FUNCTION shop_delivery_attempt_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
    d_status TEXT; d_policy TEXT; p_status TEXT; actor TEXT; note TEXT;
BEGIN
    actor := COALESCE(current_setting('champion.actor', true), '');
    note := left(COALESCE(current_setting('champion.evidence', true), ''), 1024);
    IF actor = '' THEN
        RAISE EXCEPTION 'shop attempt change without champion.actor' USING ERRCODE = 'SA422';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'PLAN_CREATED' THEN
            RAISE EXCEPTION 'a shop attempt starts at PLAN_CREATED' USING ERRCODE = 'SA422';
        END IF;
        SELECT sd.status, sd.delivery_policy, sp.status INTO d_status, d_policy, p_status
          FROM shop_deliveries sd JOIN shop_purchases sp ON sp.id = sd.purchase_id AND sp.installation_id = sd.installation_id
         WHERE sd.id = NEW.delivery_id AND sd.organization_id = NEW.organization_id AND sd.installation_id = NEW.installation_id
         FOR UPDATE OF sd;
        IF NOT FOUND OR d_status <> 'MANUAL_READY' OR d_policy <> 'MANUAL_COORDINATE' OR p_status <> 'PENDING_FULFILLMENT' THEN
            RAISE EXCEPTION 'the delivery is not open for an automatic attempt' USING ERRCODE = 'SA411';
        END IF;
        IF EXISTS (SELECT 1 FROM shop_delivery_attempts a WHERE a.delivery_id = NEW.delivery_id AND a.state NOT IN ('ABANDONED','UNSTAGED')) THEN
            RAISE EXCEPTION 'an earlier attempt is open, fulfilled or under review' USING ERRCODE = 'SA410';
        END IF;
        IF NEW.attempt <> (SELECT COUNT(*) + 1 FROM shop_delivery_attempts a WHERE a.delivery_id = NEW.delivery_id) THEN
            RAISE EXCEPTION 'the attempt number does not follow the delivery history' USING ERRCODE = 'SA412';
        END IF;
        IF NEW.before_sha256 IS NOT NULL OR NEW.staged_sha256 IS NOT NULL OR NEW.staged_at IS NOT NULL OR NEW.unstage_verified_at IS NOT NULL
           OR NEW.fulfilled_at IS NOT NULL OR NEW.review_resolution IS NOT NULL THEN
            RAISE EXCEPTION 'a new shop attempt carries no evidence' USING ERRCODE = 'SA422';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.id <> OLD.id OR NEW.organization_id <> OLD.organization_id OR NEW.installation_id <> OLD.installation_id OR NEW.delivery_id <> OLD.delivery_id
       OR NEW.attempt <> OLD.attempt OR NEW.attempt_id <> OLD.attempt_id OR NEW.fingerprint <> OLD.fingerprint
       OR NEW.artifact_path <> OLD.artifact_path OR NEW.class_name <> OLD.class_name OR NEW.quantity <> OLD.quantity
       OR NEW.pos_x <> OLD.pos_x OR NEW.pos_y <> OLD.pos_y OR NEW.pos_z <> OLD.pos_z
       OR NEW.drop_source_file <> OLD.drop_source_file OR NEW.drop_source_offset <> OLD.drop_source_offset
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a shop attempt identity is immutable' USING ERRCODE = 'SA422';
    END IF;
    -- Evidence is write-once: a recorded digest, boot or observation is never changed.
    IF (OLD.before_sha256 IS NOT NULL AND NEW.before_sha256 IS DISTINCT FROM OLD.before_sha256)
       OR (OLD.staged_sha256 IS NOT NULL AND NEW.staged_sha256 IS DISTINCT FROM OLD.staged_sha256)
       OR (OLD.unstaged_sha256 IS NOT NULL AND NEW.unstaged_sha256 IS DISTINCT FROM OLD.unstaged_sha256)
       OR (OLD.staged_at IS NOT NULL AND NEW.staged_at IS DISTINCT FROM OLD.staged_at)
       OR (OLD.staged_boot_file IS NOT NULL AND NEW.staged_boot_file IS DISTINCT FROM OLD.staged_boot_file)
       OR (OLD.restart_boot_file IS NOT NULL AND NEW.restart_boot_file IS DISTINCT FROM OLD.restart_boot_file)
       OR (OLD.restart_observed_at IS NOT NULL AND NEW.restart_observed_at IS DISTINCT FROM OLD.restart_observed_at)
       OR (OLD.unstage_verified_at IS NOT NULL AND NEW.unstage_verified_at IS DISTINCT FROM OLD.unstage_verified_at)
       OR (OLD.item_observed_at IS NOT NULL AND NEW.item_observed_at IS DISTINCT FROM OLD.item_observed_at)
       OR (OLD.pickup_observed_at IS NOT NULL AND NEW.pickup_observed_at IS DISTINCT FROM OLD.pickup_observed_at)
       OR (OLD.second_boot_file IS NOT NULL AND NEW.second_boot_file IS DISTINCT FROM OLD.second_boot_file)
       OR (OLD.second_boot_started_at IS NOT NULL AND NEW.second_boot_started_at IS DISTINCT FROM OLD.second_boot_started_at)
       OR (OLD.no_respawn_checked_at IS NOT NULL AND NEW.no_respawn_checked_at IS DISTINCT FROM OLD.no_respawn_checked_at)
       OR (OLD.failure_reason IS NOT NULL AND NEW.failure_reason IS DISTINCT FROM OLD.failure_reason) THEN
        RAISE EXCEPTION 'shop attempt evidence is write-once' USING ERRCODE = 'SA422';
    END IF;
    IF NEW.state = OLD.state THEN
        -- The only change to a terminal attempt: resolving a FAILED_REVIEW once.
        IF OLD.state = 'FAILED_REVIEW' AND OLD.review_resolution IS NULL AND NEW.review_resolution IS NOT NULL THEN
            INSERT INTO shop_delivery_attempt_events(attempt_row_id, from_state, to_state, actor, evidence)
            VALUES (OLD.id, OLD.state, NEW.state, actor, left('review resolved: ' || NEW.review_resolution || CASE WHEN note <> '' THEN ' - ' || note ELSE '' END, 1024));
            NEW.updated_at := NOW();
            RETURN NEW;
        END IF;
        IF OLD.state IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW') OR NEW.review_resolution IS DISTINCT FROM OLD.review_resolution
           OR NEW.review_resolved_by IS DISTINCT FROM OLD.review_resolved_by OR NEW.review_resolved_at IS DISTINCT FROM OLD.review_resolved_at THEN
            RAISE EXCEPTION 'a terminal shop attempt is immutable' USING ERRCODE = 'SA422';
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
        RAISE EXCEPTION 'invalid shop attempt transition % -> %', OLD.state, NEW.state USING ERRCODE = 'SA422';
    END IF;
    IF NEW.review_resolution IS NOT NULL THEN
        RAISE EXCEPTION 'a review is resolved only after FAILED_REVIEW' USING ERRCODE = 'SA422';
    END IF;
    IF NEW.state = 'FILE_PREPARED' THEN
        SELECT sd.status, sp.status INTO d_status, p_status
          FROM shop_deliveries sd JOIN shop_purchases sp ON sp.id = sd.purchase_id AND sp.installation_id = sd.installation_id
         WHERE sd.id = NEW.delivery_id FOR UPDATE OF sd;
        IF d_status <> 'MANUAL_READY' OR p_status <> 'PENDING_FULFILLMENT' THEN
            RAISE EXCEPTION 'the delivery was refunded or closed: abandon this attempt' USING ERRCODE = 'SA411';
        END IF;
    END IF;
    INSERT INTO shop_delivery_attempt_events(attempt_row_id, from_state, to_state, actor, evidence)
    VALUES (OLD.id, OLD.state, NEW.state, actor, note);
    NEW.updated_at := NOW();
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_guard ON shop_delivery_attempts;
CREATE TRIGGER trg_shop_delivery_attempt_guard BEFORE INSERT OR UPDATE ON shop_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_guard();

CREATE OR REPLACE FUNCTION shop_delivery_attempt_created_event() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    INSERT INTO shop_delivery_attempt_events(attempt_row_id, from_state, to_state, actor, evidence)
    VALUES (NEW.id, '', NEW.state, current_setting('champion.actor', true), left(COALESCE(current_setting('champion.evidence', true), ''), 1024));
    RETURN NULL;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_created ON shop_delivery_attempts;
CREATE TRIGGER trg_shop_delivery_attempt_created AFTER INSERT ON shop_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_created_event();

-- Refund / manual-fulfilment guard on the delivery. While an attempt may have put the item on the
-- server (FILE_PREPARED .. VERIFICATION_REQUIRED) the delivery can be neither cancelled nor fulfilled
-- outside the attempt. A FAILED_REVIEW attempt blocks a refund unless a human resolved it NOT_SPAWNED,
-- and blocks a manual fulfilment until it is resolved at all.
CREATE OR REPLACE FUNCTION shop_delivery_exposure_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status OR NEW.status NOT IN ('FULFILLED','CANCELLED') THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM shop_delivery_attempts a WHERE a.delivery_id = OLD.id AND (
           a.state IN ('FILE_PREPARED','FILE_STAGED','AWAITING_RESTART','RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED')
        OR (a.state = 'FAILED_REVIEW' AND NEW.status = 'CANCELLED' AND a.review_resolution IS DISTINCT FROM 'NOT_SPAWNED')
        OR (a.state = 'FAILED_REVIEW' AND NEW.status = 'FULFILLED' AND a.review_resolution IS NULL))) THEN
        RAISE EXCEPTION 'an automatic delivery attempt may have put the item on the server: resolve it before refunding or fulfilling'
            USING ERRCODE = 'SA409';
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
        RAISE EXCEPTION 'a FULFILLED attempt requires its delivery to be FULFILLED in the same transaction' USING ERRCODE = 'SA422';
    END IF;
    RETURN NULL;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_fulfilled ON shop_delivery_attempts;
CREATE CONSTRAINT TRIGGER trg_shop_delivery_attempt_fulfilled AFTER UPDATE ON shop_delivery_attempts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_fulfilled_check();
`
