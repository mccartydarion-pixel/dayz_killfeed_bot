package database

// ShopOrderConfirmationDiscordSQL (migration 0088, docs/SHOP_ORDER_CONFIRMATION.md "Discord"): what
// the Discord side of the buyer confirmation needs to remember.
//
// Additive and forward-only. It only adds columns with defaults to the two tables of migration 0087
// and one new table; no existing row is rewritten by hand.
//
//   - shop_order_confirmations.notify_*: the delivery of the "your order was delivered" DM.
//     PENDING -> SENT (the DM with its two buttons was delivered) | UNAVAILABLE (the buyer has no
//     verified Discord account here, their DMs are closed, or the retries ran out). UNAVAILABLE is
//     not an error: the buyer answers on the website instead.
//   - shop_order_tickets.channel_*: the retry state of creating the ticket's private channel.
//   - shop_ticket_discord_setup: per Discord server, the Owner and Staff roles and the Tickets
//     category the bot created (or adopted), so they are created once and reused.
const ShopOrderConfirmationDiscordSQL = `
ALTER TABLE shop_order_confirmations
    ADD COLUMN IF NOT EXISTS notify_state TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (notify_state IN ('PENDING','SENT','UNAVAILABLE')),
    ADD COLUMN IF NOT EXISTS notify_attempts INTEGER NOT NULL DEFAULT 0 CHECK (notify_attempts >= 0),
    ADD COLUMN IF NOT EXISTS notify_claimed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dm_channel_id TEXT CHECK (dm_channel_id IS NULL OR char_length(dm_channel_id) BETWEEN 1 AND 32),
    ADD COLUMN IF NOT EXISTS dm_message_id TEXT CHECK (dm_message_id IS NULL OR char_length(dm_message_id) BETWEEN 1 AND 32);

CREATE INDEX IF NOT EXISTS idx_shop_order_confirmations_notify
    ON shop_order_confirmations (purchase_id) WHERE notify_state = 'PENDING' AND state = 'AWAITING_BUYER';

ALTER TABLE shop_order_tickets
    ADD COLUMN IF NOT EXISTS channel_attempts INTEGER NOT NULL DEFAULT 0 CHECK (channel_attempts >= 0),
    ADD COLUMN IF NOT EXISTS channel_claimed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_shop_order_tickets_channel_pending
    ON shop_order_tickets (id) WHERE status = 'OPEN' AND discord_channel_id IS NULL;

CREATE TABLE IF NOT EXISTS shop_ticket_discord_setup (
    guild_id BIGINT PRIMARY KEY REFERENCES guilds(id) ON DELETE CASCADE,
    owner_role_id TEXT CHECK (owner_role_id IS NULL OR char_length(owner_role_id) BETWEEN 1 AND 32),
    staff_role_id TEXT CHECK (staff_role_id IS NULL OR char_length(staff_role_id) BETWEEN 1 AND 32),
    category_id TEXT CHECK (category_id IS NULL OR char_length(category_id) BETWEEN 1 AND 32),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
