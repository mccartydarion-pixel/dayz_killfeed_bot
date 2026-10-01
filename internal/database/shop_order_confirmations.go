package database

// ShopOrderConfirmationsSQL (migration 0087, docs/SHOP_ORDER_CONFIRMATION.md): the buyer's
// confirmation of a delivered Shop order and the support tickets it can open.
//
// Additive and forward-only. No existing row is read or rewritten: the purchase and delivery status
// machines, their CHECK constraints and the delivery-attempt ledger are untouched. An order that is
// already FULFILLED when this migration runs gets no confirmation row (it predates the feature).
//
//   - shop_order_confirmations: one row per purchase, opened AUTOMATICALLY by the trigger below at the
//     moment the purchase becomes FULFILLED (manual fulfilment and the canary/automatic path alike).
//     AWAITING_BUYER -> RECEIVED (buyer) | ISSUE_REPORTED (buyer, opens a ticket) | AUTO_COMPLETED
//     (deadline passed) | VOID (the purchase was refunded while still awaiting).
//     AUTO_COMPLETED -> ISSUE_REPORTED is also allowed: silence is not consent.
//   - shop_order_tickets: one OPEN ticket per purchase at most; resolved by staff as COMPLETED,
//     REFUNDED or OTHER. discord_channel_id is filled by the Discord side when it creates the channel.
//
// The confirmation window (48 hours) is the DEFAULT of deadline_at so the trigger carries no policy
// of its own; ShopConfirmationWindow in the repository mirrors it for display.
const ShopOrderConfirmationsSQL = `
CREATE TABLE IF NOT EXISTS shop_order_confirmations (
    purchase_id BIGINT PRIMARY KEY REFERENCES shop_purchases(id) ON DELETE CASCADE,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    player_id BIGINT NOT NULL,
    state TEXT NOT NULL DEFAULT 'AWAITING_BUYER'
        CHECK (state IN ('AWAITING_BUYER','RECEIVED','ISSUE_REPORTED','AUTO_COMPLETED','VOID')),
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deadline_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '48 hours'),
    responded_at TIMESTAMPTZ,
    response_source TEXT CHECK (response_source IS NULL OR response_source IN ('SITE','DISCORD','AUTO','SYSTEM')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT shop_order_confirmations_response CHECK ((state = 'AWAITING_BUYER') = (responded_at IS NULL)),
    CONSTRAINT shop_order_confirmations_deadline CHECK (deadline_at > delivered_at)
);

CREATE INDEX IF NOT EXISTS idx_shop_order_confirmations_due
    ON shop_order_confirmations (deadline_at) WHERE state = 'AWAITING_BUYER';
CREATE INDEX IF NOT EXISTS idx_shop_order_confirmations_installation
    ON shop_order_confirmations (installation_id, state);

CREATE TABLE IF NOT EXISTS shop_order_tickets (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    purchase_id BIGINT NOT NULL REFERENCES shop_purchases(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL,
    opened_by_discord_id TEXT NOT NULL CHECK (char_length(opened_by_discord_id) BETWEEN 1 AND 32),
    opened_via TEXT NOT NULL CHECK (opened_via IN ('SITE','DISCORD')),
    reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    resolution TEXT CHECK (resolution IS NULL OR resolution IN ('COMPLETED','REFUNDED','OTHER')),
    resolution_note TEXT CHECK (resolution_note IS NULL OR char_length(resolution_note) <= 1000),
    discord_channel_id TEXT CHECK (discord_channel_id IS NULL OR char_length(discord_channel_id) BETWEEN 1 AND 32),
    opened_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    resolved_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT shop_order_tickets_resolved CHECK ((status = 'RESOLVED') = (resolved_at IS NOT NULL AND resolution IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_order_tickets_open
    ON shop_order_tickets (purchase_id) WHERE status = 'OPEN';
CREATE INDEX IF NOT EXISTS idx_shop_order_tickets_installation
    ON shop_order_tickets (installation_id, status, id DESC);

-- Opens the confirmation when a purchase becomes FULFILLED and voids a still-open one when the
-- purchase is REFUNDED. It only ever writes shop_order_confirmations.
CREATE OR REPLACE FUNCTION shop_order_confirmation_sync() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.status = 'FULFILLED' AND OLD.status IS DISTINCT FROM 'FULFILLED' THEN
        INSERT INTO shop_order_confirmations(purchase_id, organization_id, installation_id, player_id)
        VALUES (NEW.id, NEW.organization_id, NEW.installation_id, NEW.player_id)
        ON CONFLICT (purchase_id) DO NOTHING;
    ELSIF NEW.status = 'REFUNDED' AND OLD.status IS DISTINCT FROM 'REFUNDED' THEN
        UPDATE shop_order_confirmations
           SET state = 'VOID', responded_at = NOW(), response_source = 'SYSTEM', updated_at = NOW()
         WHERE purchase_id = NEW.id AND state = 'AWAITING_BUYER';
    END IF;
    RETURN NEW;
END
$fn$;

DROP TRIGGER IF EXISTS trg_shop_order_confirmation_sync ON shop_purchases;
CREATE TRIGGER trg_shop_order_confirmation_sync
    AFTER UPDATE OF status ON shop_purchases
    FOR EACH ROW EXECUTE FUNCTION shop_order_confirmation_sync();
`
