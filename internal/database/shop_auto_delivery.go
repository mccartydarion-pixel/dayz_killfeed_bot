package database

// ShopAttemptBuyerFulfilmentSQL (migration 0091, docs/SHOP_DELIVERY_WORKER_DESIGN.md "What delivered
// means") adds the delivery worker's fulfilment path to the attempt ledger.
//
// Until now an attempt became FULFILLED only with evidence a person recorded in game: a sighting, a
// pickup, a second boot and a no-respawn check. The worker has nobody in game, so its attempts carry
// fulfilment_mode = 'BUYER' and become FULFILLED only with the buyer's own answer to the delivered
// order (RECEIVED), or the answer deadline passing (AUTO_COMPLETED). The database checks that the
// buyer's confirmation really is in that state, so no code path can fulfil a worker attempt without
// it. Attempts in the default mode 'OBSERVED' (the manual canary path) keep every existing rule.
//
// Additive and forward-only: three columns with defaults, the FULFILLED check replaced by one that
// accepts either mode, and one new trigger. No existing row is rewritten; the one existing FULFILLED
// attempt is OBSERVED and satisfies the unchanged half of the check.
const ShopAttemptBuyerFulfilmentSQL = `
ALTER TABLE shop_delivery_attempts
    ADD COLUMN IF NOT EXISTS fulfilment_mode TEXT NOT NULL DEFAULT 'OBSERVED' CHECK (fulfilment_mode IN ('OBSERVED','BUYER')),
    ADD COLUMN IF NOT EXISTS buyer_answer TEXT CHECK (buyer_answer IS NULL OR buyer_answer IN ('RECEIVED','AUTO_COMPLETED')),
    ADD COLUMN IF NOT EXISTS buyer_answered_at TIMESTAMPTZ;

ALTER TABLE shop_delivery_attempts DROP CONSTRAINT IF EXISTS shop_delivery_attempts_fulfilled;
ALTER TABLE shop_delivery_attempts ADD CONSTRAINT shop_delivery_attempts_fulfilled CHECK (state <> 'FULFILLED' OR (
    verified_by IS NOT NULL AND fulfilled_at IS NOT NULL AND (
        (fulfilment_mode = 'OBSERVED' AND buyer_answer IS NULL
            AND item_observed_by IS NOT NULL AND item_observed_at IS NOT NULL AND item_observed_at >= staged_at
            AND picked_up_by IS NOT NULL AND pickup_observed_at IS NOT NULL AND pickup_observed_at >= item_observed_at
            AND second_boot_file IS NOT NULL AND second_boot_file <> restart_boot_file AND second_boot_file <> staged_boot_file
            AND second_boot_started_at IS NOT NULL AND second_boot_started_at > unstage_verified_at
            AND no_respawn_checked_at IS NOT NULL AND no_respawn_checked_at >= second_boot_started_at)
        OR (fulfilment_mode = 'BUYER' AND buyer_answer IS NOT NULL AND buyer_answered_at IS NOT NULL
            AND buyer_answered_at >= unstage_verified_at))));
ALTER TABLE shop_delivery_attempts DROP CONSTRAINT IF EXISTS shop_delivery_attempts_buyer_answer;
ALTER TABLE shop_delivery_attempts ADD CONSTRAINT shop_delivery_attempts_buyer_answer CHECK (
    (buyer_answer IS NULL AND buyer_answered_at IS NULL)
    OR (buyer_answer IS NOT NULL AND buyer_answered_at IS NOT NULL AND fulfilment_mode = 'BUYER' AND state = 'FULFILLED'));

-- The mode is fixed when the attempt is created; the buyer's answer is recorded once, at the moment
-- the attempt is fulfilled, and must be the answer the buyer's confirmation actually holds.
CREATE OR REPLACE FUNCTION shop_delivery_attempt_buyer_guard() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE
    confirmed TEXT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.buyer_answer IS NOT NULL OR NEW.buyer_answered_at IS NOT NULL THEN
            RAISE EXCEPTION 'a new shop attempt carries no buyer answer' USING ERRCODE = 'SA422';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.fulfilment_mode <> OLD.fulfilment_mode THEN
        RAISE EXCEPTION 'a shop attempt fulfilment mode is immutable' USING ERRCODE = 'SA422';
    END IF;
    IF OLD.buyer_answer IS NOT NULL AND (NEW.buyer_answer IS DISTINCT FROM OLD.buyer_answer
       OR NEW.buyer_answered_at IS DISTINCT FROM OLD.buyer_answered_at) THEN
        RAISE EXCEPTION 'shop attempt evidence is write-once' USING ERRCODE = 'SA422';
    END IF;
    IF NEW.state = 'FULFILLED' AND OLD.state <> 'FULFILLED' AND NEW.fulfilment_mode = 'BUYER' THEN
        SELECT c.state INTO confirmed
          FROM shop_deliveries sd JOIN shop_order_confirmations c ON c.purchase_id = sd.purchase_id
         WHERE sd.id = NEW.delivery_id;
        IF NEW.buyer_answer IS NULL OR confirmed IS DISTINCT FROM NEW.buyer_answer THEN
            RAISE EXCEPTION 'a worker attempt is fulfilled only by the buyer answer on record' USING ERRCODE = 'SA422';
        END IF;
    END IF;
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS trg_shop_delivery_attempt_buyer_guard ON shop_delivery_attempts;
CREATE TRIGGER trg_shop_delivery_attempt_buyer_guard BEFORE INSERT OR UPDATE ON shop_delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION shop_delivery_attempt_buyer_guard();
`

// ShopAutoDeliverySQL (migration 0092, docs/SHOP_DELIVERY_WORKER_DESIGN.md) is what the delivery
// worker needs outside the ledger. Everything defaults to off: no product is automatic, no
// installation has automatic delivery enabled, and nothing here starts a worker.
//
//   - shop_products.auto_delivery / class_name: the owner's per-product switch and the DayZ class the
//     worker spawns. The worker only ever picks coordinate deliveries, so the flag has no effect on
//     a pickup product.
//   - shop_auto_delivery_settings: the owner's per-installation switch, the pause (set by the worker
//     when something is unclear, cleared only by a person), the configuration the worker last accepted
//     and the lease that keeps two bot instances from working the same server.
//   - shop_delivery_writes: the journal of every write to the Champion spawner file. A row is
//     inserted BEFORE the write and completed after the read-back; an unfinished or UNCERTAIN row
//     blocks further writes to that server until a person resolves it.
//   - shop_order_tickets.opened_via gains SYSTEM, for tickets the worker opens when a delivery needs a
//     person.
const ShopAutoDeliverySQL = `
ALTER TABLE shop_products
    ADD COLUMN IF NOT EXISTS auto_delivery BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS class_name TEXT CHECK (class_name IS NULL OR class_name ~ '^[A-Za-z][A-Za-z0-9_]{1,63}$');
ALTER TABLE shop_products DROP CONSTRAINT IF EXISTS shop_products_auto_delivery;
ALTER TABLE shop_products ADD CONSTRAINT shop_products_auto_delivery
    CHECK (NOT auto_delivery OR class_name IS NOT NULL);

CREATE TABLE IF NOT EXISTS shop_auto_delivery_settings (
    installation_id BIGINT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    paused_at TIMESTAMPTZ,
    paused_reason TEXT CHECK (paused_reason IS NULL OR char_length(paused_reason) BETWEEN 1 AND 500),
    config_sha256 TEXT CHECK (config_sha256 IS NULL OR config_sha256 ~ '^[0-9a-f]{64}$'),
    mission_path TEXT,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    updated_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT shop_auto_delivery_settings_pause CHECK ((paused_at IS NULL) = (paused_reason IS NULL))
);

CREATE TABLE IF NOT EXISTS shop_delivery_writes (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('STAGE','UNSTAGE')),
    attempt_ids TEXT[] NOT NULL CHECK (cardinality(attempt_ids) BETWEEN 1 AND 50),
    before_sha256 TEXT NOT NULL CHECK (before_sha256 ~ '^[0-9a-f]{64}$'),
    payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    boot_file TEXT NOT NULL CHECK (boot_file LIKE '%.ADM'),
    outcome TEXT NOT NULL DEFAULT 'STARTED' CHECK (outcome IN ('STARTED','WRITTEN_VERIFIED','NOT_WRITTEN','UNCERTAIN')),
    after_sha256 TEXT CHECK (after_sha256 IS NULL OR after_sha256 ~ '^[0-9a-f]{64}$'),
    detail TEXT NOT NULL DEFAULT '' CHECK (char_length(detail) <= 500),
    worker TEXT NOT NULL CHECK (char_length(worker) BETWEEN 1 AND 100),
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    resolved_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT shop_delivery_writes_payload CHECK (payload_sha256 <> before_sha256),
    CONSTRAINT shop_delivery_writes_finished CHECK ((outcome = 'STARTED') = (finished_at IS NULL))
);
-- At most one unresolved write per server: a second one cannot even be journaled.
CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_delivery_writes_unresolved
    ON shop_delivery_writes (installation_id) WHERE outcome IN ('STARTED','UNCERTAIN') AND resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_shop_delivery_writes_installation ON shop_delivery_writes (installation_id, id DESC);

DO $$
DECLARE c RECORD;
BEGIN
    FOR c IN
        SELECT con.conname FROM pg_constraint con
         WHERE con.conrelid = 'shop_order_tickets'::regclass AND con.contype = 'c'
           AND pg_get_constraintdef(con.oid) LIKE '%opened_via%'
    LOOP
        EXECUTE format('ALTER TABLE shop_order_tickets DROP CONSTRAINT %I', c.conname);
    END LOOP;
END $$;
ALTER TABLE shop_order_tickets ADD CONSTRAINT shop_order_tickets_opened_via_allowed
    CHECK (opened_via IN ('SITE','DISCORD','SYSTEM'));
`
