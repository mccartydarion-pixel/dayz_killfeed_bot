package database

// migrations0036to0048 holds migrations 0036 through 0048, in execution order. It is one fragment of the
// registry assembled in migrations.go; never edit an applied migration.
var migrations0036to0048 = []Migration{
	{
		// Champion Shop Phase 1 (docs/SHOP.md): an installation-scoped product catalog and purchase
		// records paid with Champion Points. The Points themselves are NOT stored here: a purchase's
		// debit is a row of the existing ledger (point_transactions, type SHOP_PURCHASE, source_key
		// 'purchase:<id>'), written in the same transaction as the purchase, and a refund is a
		// compensating SHOP_REFUND credit. Everything is scoped by organization + installation
		// (composite FKs), so a product or purchase can never belong to another tenant.
		//
		// Products are never hard-deleted (is_active=false disables them); purchase items snapshot the
		// product name and price, so editing or disabling a product never rewrites history.
		Name: "0036_shop_foundation",
		SQL: `
CREATE TABLE IF NOT EXISTS shop_categories (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 60),
    slug TEXT NOT NULL CHECK (char_length(slug) BETWEEN 1 AND 60),
    description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_categories_installation_slug UNIQUE (installation_id, slug),
    CONSTRAINT uq_shop_categories_id_installation UNIQUE (id, installation_id)
);

CREATE TABLE IF NOT EXISTS shop_products (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    category_id BIGINT,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    slug TEXT NOT NULL CHECK (char_length(slug) BETWEEN 1 AND 60),
    description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 1000),
    price_points BIGINT NOT NULL CHECK (price_points > 0 AND price_points <= 1000000000),
    product_type TEXT NOT NULL CHECK (product_type IN ('ITEM','LOADOUT','VEHICLE','SERVICE','CUSTOM')),
    delivery_type TEXT NOT NULL DEFAULT 'MANUAL' CHECK (delivery_type IN ('MANUAL','DISCORD_ROLE','IN_GAME_FUTURE')),
    image_key TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    is_featured BOOLEAN NOT NULL DEFAULT FALSE,
    stock_mode TEXT NOT NULL DEFAULT 'UNLIMITED' CHECK (stock_mode IN ('UNLIMITED','FINITE')),
    stock_quantity BIGINT,
    purchase_limit INTEGER CHECK (purchase_limit IS NULL OR purchase_limit BETWEEN 1 AND 1000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    FOREIGN KEY (category_id, installation_id) REFERENCES shop_categories(id, installation_id),
    CONSTRAINT uq_shop_products_installation_slug UNIQUE (installation_id, slug),
    CONSTRAINT uq_shop_products_id_installation UNIQUE (id, installation_id),
    CONSTRAINT shop_products_stock_consistent CHECK (
        (stock_mode = 'UNLIMITED' AND stock_quantity IS NULL)
        OR (stock_mode = 'FINITE' AND stock_quantity IS NOT NULL AND stock_quantity >= 0 AND stock_quantity <= 1000000000))
);
CREATE INDEX IF NOT EXISTS idx_shop_products_catalog ON shop_products(installation_id, is_active, is_featured DESC, sort_order, id);
CREATE INDEX IF NOT EXISTS idx_shop_products_category ON shop_products(installation_id, category_id);

CREATE TABLE IF NOT EXISTS shop_purchases (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL,
    user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('PENDING','PAID','PENDING_FULFILLMENT','FULFILLED','CANCELLED','REFUNDED','FAILED')),
    total_points BIGINT NOT NULL CHECK (total_points > 0),
    delivery_type TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK (char_length(idempotency_key) BETWEEN 8 AND 64),
    paid_at TIMESTAMPTZ,
    fulfilled_at TIMESTAMPTZ,
    fulfilled_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    cancelled_at TIMESTAMPTZ,
    refunded_at TIMESTAMPTZ,
    refunded_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    refund_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_purchases_id_installation UNIQUE (id, installation_id),
    CONSTRAINT uq_shop_purchases_idempotency UNIQUE (installation_id, user_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_shop_purchases_installation ON shop_purchases(installation_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_purchases_player ON shop_purchases(installation_id, player_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_purchases_status ON shop_purchases(installation_id, status, id DESC);

CREATE TABLE IF NOT EXISTS shop_purchase_items (
    id BIGSERIAL PRIMARY KEY,
    purchase_id BIGINT NOT NULL REFERENCES shop_purchases(id) ON DELETE CASCADE,
    product_id BIGINT REFERENCES shop_products(id) ON DELETE SET NULL,
    product_name TEXT NOT NULL,
    unit_price_points BIGINT NOT NULL CHECK (unit_price_points > 0),
    quantity INTEGER NOT NULL CHECK (quantity > 0 AND quantity <= 1000),
    line_total_points BIGINT NOT NULL CHECK (line_total_points = unit_price_points * quantity)
);
CREATE INDEX IF NOT EXISTS idx_shop_purchase_items_purchase ON shop_purchase_items(purchase_id);
CREATE INDEX IF NOT EXISTS idx_shop_purchase_items_product ON shop_purchase_items(product_id);
`,
	},
	{
		// Champion Billing Phase 1 (docs/BILLING.md): Stripe fields on the existing subscriptions row
		// (still one row per organization - no parallel customer/subscription table) plus a webhook
		// dedupe log. provider/provider_customer_id/provider_subscription_id already existed
		// (0024_saas_foundation) and stay NULL until an organization actually checks out.
		Name: "0037_billing_stripe",
		SQL: `
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS provider_price_id TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS billing_interval TEXT;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS current_period_start TIMESTAMPTZ;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS canceled_at TIMESTAMPTZ;
-- trial_consumed prevents an organization from getting a fresh Stripe trial on every checkout
-- (cancel, wait, check out again): once a Stripe subscription has ever been recorded for the
-- organization, no later checkout is given subscription_data.trial_period_days.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS trial_consumed BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS idx_subscriptions_provider_customer ON subscriptions(provider_customer_id) WHERE provider_customer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_subscriptions_provider_subscription ON subscriptions(provider_subscription_id) WHERE provider_subscription_id IS NOT NULL;

-- One row per processed Stripe event id: webhook handling starts with an INSERT ... ON CONFLICT DO
-- NOTHING against this table, so a duplicated delivery (Stripe retries on anything but a 2xx) is
-- detected before any subscription row is touched, race-safe under the UNIQUE constraint.
CREATE TABLE IF NOT EXISTS billing_webhook_events (
    id BIGSERIAL PRIMARY KEY,
    provider TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    organization_id BIGINT REFERENCES organizations(id) ON DELETE SET NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_webhook_events UNIQUE (provider, event_id)
);
CREATE INDEX IF NOT EXISTS idx_billing_webhook_events_org ON billing_webhook_events(organization_id, received_at DESC);
`,
	},
	{
		Name: "0038_billing_transactions",
		SQL: `
-- Normalized payment/invoice history (Champion Access Model Phase 2, Part D). One row per
-- processed invoice.paid/invoice.payment_failed webhook event - never fabricated, never backfilled
-- from anything but the webhook itself. stripe_event_id ties a row 1:1 to the already-deduped
-- billing_webhook_events row (UNIQUE(provider, event_id) there), so redelivery of the same Stripe
-- event can never create a second transaction row even without re-checking that table.
CREATE TABLE IF NOT EXISTS billing_transactions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_invoice_id TEXT NOT NULL,
    provider_payment_intent_id TEXT,
    provider_subscription_id TEXT,
    -- Only PAID and FAILED are ever written: Champion only subscribes to invoice.paid and
    -- invoice.payment_failed (docs/BILLING.md "Webhook events"). OPEN/VOID/REFUNDED are not
    -- fabricated states - they would need Champion to also handle invoice.voided/charge.refunded,
    -- which it does not yet.
    status TEXT NOT NULL CHECK (status IN ('PAID','FAILED')),
    amount_cents BIGINT NOT NULL,
    currency TEXT NOT NULL,
    period_start TIMESTAMPTZ,
    period_end TIMESTAMPTZ,
    paid_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    stripe_event_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_billing_transactions_event UNIQUE (provider, stripe_event_id)
);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_org ON billing_transactions(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_status ON billing_transactions(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_transactions_invoice ON billing_transactions(provider, provider_invoice_id);
`,
	},
	{
		Name: "0039_player_api_lookup_index",
		SQL: `
-- Champion Access Model Phase 2 Part A/B (docs/PLAYER_API.md "Performance"): the player-facing
-- API resolves FROM a Discord user id TO their player_links row before it knows a guild id, so
-- neither existing UNIQUE constraint (guild_id, discord_user_id) or (guild_id, player_id) - both
-- guild_id-first - can be used as an index for that direction. EXPLAIN ANALYZE against this exact
-- query (SELECT ... FROM player_links WHERE discord_user_id=$1 AND status='VERIFIED') showed a
-- sequential scan without this index, and an index scan with it.
CREATE INDEX IF NOT EXISTS idx_player_links_discord_user ON player_links(discord_user_id, status);
`,
	},
	{
		Name: "0040_client_admin_control_plane",
		SQL: `
-- Champion Client Admin Control Plane Phase 1 (docs/CLIENT_ADMIN.md): Discord-role -> Champion
-- tenant-permission-level mapping. This is deliberately separate from organization_members' flat
-- OWNER/ADMIN/MEMBER role and from Champion's own platform-admin allowlist (admin_api.go) - see
-- docs/CLIENT_ADMIN.md "Permission model" for how the three relate. permission_level is enforced
-- at the app layer (internal/permissions) against a fixed, known set; the CHECK constraint is a
-- second, redundant guard against a bad direct write.
CREATE TABLE IF NOT EXISTS installation_role_permissions (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    discord_role_id TEXT NOT NULL,
    permission_level TEXT NOT NULL CHECK (permission_level IN ('OWNER','ADMINISTRATOR','MODERATOR','GATEKEEPER')),
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, discord_role_id)
);
CREATE INDEX IF NOT EXISTS idx_installation_role_permissions_installation ON installation_role_permissions(installation_id);

-- Tenant-level admin audit log (task's "ADMIN AUDIT LOG" section). No persisted audit table
-- existed anywhere before this migration (confirmed by a full-repo audit ahead of Access Model
-- Phase 2, PR #60, and reconfirmed here) - every website admin action from this phase on must
-- write one row here. before_state/after_state are small sanitized JSON snapshots, never secrets.
CREATE TABLE IF NOT EXISTS admin_audit_log (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    installation_id BIGINT REFERENCES installations(id) ON DELETE SET NULL,
    actor_user_id BIGINT REFERENCES app_users(id),
    actor_discord_id TEXT,
    action TEXT NOT NULL,
    target TEXT,
    reason TEXT,
    before_state JSONB,
    after_state JSONB,
    result TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_org_time ON admin_audit_log(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_installation_time ON admin_audit_log(installation_id, created_at DESC);

-- Player warnings (task's "DISCORD MODERATION" / warnings section). A clear preserves the row
-- (audit trail) rather than deleting it - only cleared/clearedBy/clearedAt are set.
CREATE TABLE IF NOT EXISTS player_warnings (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    issued_by_user_id BIGINT REFERENCES app_users(id),
    issued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cleared BOOLEAN NOT NULL DEFAULT FALSE,
    cleared_by_user_id BIGINT REFERENCES app_users(id),
    cleared_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_player_warnings_player ON player_warnings(guild_id, player_id, cleared);

-- Per-route location-field visibility (task's "location" capability) and maintenance mode /
-- autostart monitor state (task's "SERVER OPERATIONS" / "SERVER MAINTENANCE MODE" sections).
ALTER TABLE installation_channel_routes ADD COLUMN IF NOT EXISTS show_location BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS maintenance_mode BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_offline_minutes INTEGER NOT NULL DEFAULT 10 CHECK (autostart_offline_minutes > 0);
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_cooldown_minutes INTEGER NOT NULL DEFAULT 30 CHECK (autostart_cooldown_minutes > 0);
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_last_attempt_at TIMESTAMPTZ;
ALTER TABLE server_configs ADD COLUMN IF NOT EXISTS autostart_attempt_count INTEGER NOT NULL DEFAULT 0;

-- Champion-side whitelist/ban-list records (task's "ACCESS CONTROL"). Nitrado's own whitelist/
-- banlist API (services/{id}/gameservers/games/{whitelist|banlist}, add/remove by identifier only)
-- carries no reason/notes/expiry metadata, so this table is the authoritative source for those
-- fields; the Nitrado call is the enforcement side effect on add/remove. A row is soft-removed
-- (removed_at set) rather than deleted, preserving history; the partial unique index allows an
-- identifier to be re-added after a prior removal without colliding with its own history.
CREATE TABLE IF NOT EXISTS installation_access_entries (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    list_type TEXT NOT NULL CHECK (list_type IN ('WHITELIST','BANLIST')),
    identifier TEXT NOT NULL,
    reason TEXT,
    notes TEXT,
    expires_at TIMESTAMPTZ,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    removed_at TIMESTAMPTZ,
    removed_by_user_id BIGINT REFERENCES app_users(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_installation_access_entries_active ON installation_access_entries(installation_id, list_type, identifier) WHERE removed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_installation_access_entries_lookup ON installation_access_entries(installation_id, list_type, removed_at);
`,
	},
	{
		Name: "0041_player_location_events",
		SQL: `
-- Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): persisted ADM position history. ADM parsing
-- already extracts Position{X,Y,Z} on any event whose metadata block includes "pos=<...>"
-- (internal/killfeed/parser.go's posRe) but it was previously discarded immediately after use
-- (only ever read transiently for kill-distance/embed rendering) - this table is the first
-- durable store for it. guild_id/server_id (not organization_id/installation_id) match every
-- other bot-native, guild-scoped table in this schema (kills, deaths, player_warnings,
-- player_server_activity); the SaaS API resolves organization/installation -> guild/server the
-- same way every other Client Admin route already does (ClientAdminRepository.Scope), so no
-- second identity needs to be stored per row.
--
-- "Current location" is deliberately NOT a second table: it is derived at query time (SELECT ...
-- ORDER BY observed_at DESC LIMIT 1, served by this table's own leading index), so there is only
-- ever one truth for a player's location - never a second, potentially-stale copy to reconcile.
--
-- UNIQUE(player_id, server_id, event_type, observed_at) is the durable backstop against a
-- duplicate ADM replay creating a duplicate location row (task section 14's "no duplicate
-- location events after ADM replay") - the same pattern kills/deaths already use
-- (UNIQUE(guild_id, event_fingerprint)), at ADM's own timestamp resolution (whole seconds); the
-- writer inserts with ON CONFLICT DO NOTHING.
CREATE TABLE IF NOT EXISTS player_location_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    gamertag TEXT NOT NULL,
    x DOUBLE PRECISION NOT NULL,
    z DOUBLE PRECISION NOT NULL,
    y DOUBLE PRECISION,
    event_type TEXT NOT NULL CHECK (event_type IN ('CONNECT','DISCONNECT','HIT','KILL','DEATH','RESPAWN','UNCONSCIOUS','OTHER_ADM')),
    observed_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL DEFAULT 'ADM',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_player_location_events_dedupe UNIQUE (player_id, server_id, event_type, observed_at)
);
CREATE INDEX IF NOT EXISTS idx_player_location_events_player_time ON player_location_events(player_id, observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_player_location_events_server_time ON player_location_events(server_id, observed_at DESC);
-- Backs the retention cleanup job's DELETE ... WHERE created_at < cutoff.
CREATE INDEX IF NOT EXISTS idx_player_location_events_retention ON player_location_events(created_at);
`,
	},
	{
		Name: "0042_zones_uav_base_radar",
		SQL: `
-- Champion Phase 4 (docs/ZONES_UAV_RADAR.md): installation-scoped geographic zones plus a stateful
-- intrusion engine consuming Phase 3's player_location_events. installation_zones carries both
-- installation_id (tenant-CRUD identity, matching every other Client Admin table) AND guild_id/
-- server_id, resolved once at creation time from ClientAdminRepository.Scope and denormalized here
-- so the intrusion engine - which lives in internal/killfeed and only ever knows guild_id/server_id,
-- never organization_id/installation_id - can list a server's active zones without joining through
-- installations on every location event (the per-server zone cache still avoids even this lookup
-- on the hot path; see internal/killfeed/zone_cache.go).
--
-- zone_type's UAV/BASE_RADAR values share this exact same table and the exact same intrusion
-- engine as every other zone type (task: "do NOT duplicate intrusion logic") - they only change
-- alert presentation and (internal/permissions) which capability is required to manage them.
CREATE TABLE IF NOT EXISTS installation_zones (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    zone_type TEXT NOT NULL CHECK (zone_type IN ('SAFEZONE','PVP','RESTRICTED','EVENT','UAV','BASE_RADAR','CUSTOM')),
    center_x DOUBLE PRECISION NOT NULL,
    center_z DOUBLE PRECISION NOT NULL,
    radius DOUBLE PRECISION NOT NULL CHECK (radius > 0),
    -- alert_channel_id is the ONLY Discord destination the intrusion engine ever sends to (task
    -- section 17: "never invent a fallback channel"). NULL means alerting is silently disabled for
    -- this zone - intrusions/presence/audit are still tracked, nothing is ever posted to Discord.
    alert_channel_id TEXT,
    cooldown_seconds INTEGER NOT NULL DEFAULT 300 CHECK (cooldown_seconds >= 0),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_installation_zones_installation ON installation_zones(installation_id);
-- Backs the zone cache's refresh query (the hot lookup: "every enabled zone on this server").
CREATE INDEX IF NOT EXISTS idx_installation_zones_server_enabled ON installation_zones(server_id, enabled);

-- Entities fully excluded from intrusion detection for a zone (no presence tracking, no intrusion,
-- no alert - e.g. server admins patrolling a restricted zone). Applies to every zone type.
CREATE TABLE IF NOT EXISTS zone_ignore_entries (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    entry_type TEXT NOT NULL CHECK (entry_type IN ('PLAYER','FACTION','DISCORD_ROLE')),
    entry_value TEXT NOT NULL,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(zone_id, entry_type, entry_value)
);
CREATE INDEX IF NOT EXISTS idx_zone_ignore_entries_zone ON zone_ignore_entries(zone_id);

-- Entities authorized to be inside a zone without triggering an ALERT (presence/intrusion history
-- is still recorded - only Discord alerting is suppressed). Only meaningful for UAV/BASE_RADAR
-- zones (task: "authorized entities never trigger intrusion alerts for UAV/Base Radar") - the
-- intrusion engine ignores this list entirely for every other zone type.
CREATE TABLE IF NOT EXISTS zone_authorized_entries (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    entry_type TEXT NOT NULL CHECK (entry_type IN ('PLAYER','FACTION')),
    entry_value TEXT NOT NULL,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(zone_id, entry_type, entry_value)
);
CREATE INDEX IF NOT EXISTS idx_zone_authorized_entries_zone ON zone_authorized_entries(zone_id);

-- Zone bans are explicitly NOT server bans (task: "never auto-adds to the DayZ banlist") - a
-- banned player entering a zone is tracked and flagged (zone_intrusions.banned, a ZONE_BAN_VIOLATION
-- operational event) exactly like any other intrusion, purely for admin awareness/history.
CREATE TABLE IF NOT EXISTS zone_bans (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    reason TEXT,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_by_user_id BIGINT REFERENCES app_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lifted_at TIMESTAMPTZ,
    lifted_by_user_id BIGINT REFERENCES app_users(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_zone_bans_active ON zone_bans(zone_id, player_id) WHERE active;
CREATE INDEX IF NOT EXISTS idx_zone_bans_zone ON zone_bans(zone_id);
CREATE INDEX IF NOT EXISTS idx_zone_bans_player ON zone_bans(player_id);

-- zone_presence is a deliberate exception to the "no second truth" principle Phase 3 established
-- for location data: this is not duplicated location data, it is the intrusion engine's own
-- persisted STATE MACHINE result (task section 19/24: "persist current presence state...use this
-- for efficient transition detection" and "restore presence from persisted state on restart, never
-- generate fake entry alerts for players already inside"). last_alert_at is the cooldown anchor -
-- it survives an exit/re-entry cycle for the SAME zone+player pair so a boundary-jitter flap can
-- never bypass the cooldown by technically closing and reopening an intrusion.
CREATE TABLE IF NOT EXISTS zone_presence (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('INSIDE','OUTSIDE')),
    entered_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ NOT NULL,
    last_location_event_id BIGINT,
    last_alert_at TIMESTAMPTZ,
    UNIQUE(zone_id, player_id)
);
CREATE INDEX IF NOT EXISTS idx_zone_presence_zone_status ON zone_presence(zone_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_presence_player ON zone_presence(player_id);

-- zone_intrusions is the durable, append-mostly history: a row is created on every OUTSIDE->INSIDE
-- transition and NEVER deleted on exit (task section 8), only updated (exited_at/status). status is
-- a 3-stage lifecycle: ACTIVE (ongoing, unacknowledged) -> ACKNOWLEDGED (ongoing, an admin has seen
-- it - task section 16) -> EXITED (set whenever the player leaves, from either ACTIVE or
-- ACKNOWLEDGED; acknowledged_at/acknowledged_by are preserved as history either way). The partial
-- unique index enforces "at most one OPEN (non-EXITED) intrusion per zone+player", which is also
-- the intrusion engine's own hot lookup for "is this player already tracked as inside this zone".
CREATE TABLE IF NOT EXISTS zone_intrusions (
    id BIGSERIAL PRIMARY KEY,
    zone_id BIGINT NOT NULL REFERENCES installation_zones(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    gamertag TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE','EXITED','ACKNOWLEDGED')),
    banned BOOLEAN NOT NULL DEFAULT FALSE,
    entered_at TIMESTAMPTZ NOT NULL,
    exited_at TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by_user_id BIGINT REFERENCES app_users(id),
    last_alert_at TIMESTAMPTZ,
    alert_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_installation_status ON zone_intrusions(installation_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_zone_status ON zone_intrusions(zone_id, status);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_player ON zone_intrusions(player_id);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_entered ON zone_intrusions(entered_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_zone_intrusions_open ON zone_intrusions(zone_id, player_id) WHERE status <> 'EXITED';
`,
	},
	{
		Name: "0043_heatmap_indexes",
		SQL: `
-- Champion Phase 5 (docs/HEATMAPS.md): heatmap aggregation queries never scan kills/deaths/
-- player_location_events/zone_intrusions without a covering index - audited against the existing
-- index set first (task section 17), adding only what's actually missing:
--   - kills already has idx_kills_server(guild_id,server_id,created_at DESC), but heatmap queries
--     filter by event_time (the column that matches player_location_events.observed_at exactly -
--     both are set from the same ev.Timestamp during the same processLine call), not created_at.
--   - deaths already has idx_deaths_server(guild_id,server_id,event_time DESC) - covers heatmap
--     death queries as-is, nothing to add.
--   - player_location_events has (player_id,observed_at) and (server_id,observed_at), but every
--     heatmap join additionally filters by event_type ('KILL'/'DEATH') or needs it implicitly - a
--     composite (server_id,event_type,observed_at) index serves both the kill/death coordinate
--     joins and the player-activity aggregation's own time-range scan.
--   - zone_intrusions has (installation_id,status) and (entered_at DESC) separately, but heatmap
--     intrusion queries filter by (installation_id, entered_at range) together.
CREATE INDEX IF NOT EXISTS idx_kills_server_event_time ON kills(guild_id, server_id, event_time DESC);
CREATE INDEX IF NOT EXISTS idx_player_location_events_server_type_time ON player_location_events(server_id, event_type, observed_at);
CREATE INDEX IF NOT EXISTS idx_zone_intrusions_installation_entered ON zone_intrusions(installation_id, entered_at DESC);
`,
	},
	{
		Name: "0044_remove_casino_route",
		SQL: `
-- Champion Channel System V2: CASINO no longer exists as a feature or route key. Stored
-- routes and embed templates for it are removed; the Discord channels themselves are never
-- touched (auto-setup reports them as retirable instead).
DELETE FROM installation_channel_routes WHERE route_key = 'CASINO';
DELETE FROM installation_embed_templates WHERE route_key = 'CASINO';
`,
	},
	{
		Name: "0045_player_location_events_adm_axis_fix",
		SQL:  admLocationAxisFixSQL,
	},
	{
		Name: "0046_installation_retired_channels",
		SQL: `
-- Champion Channel System V2: Champion-managed Discord channels/categories that setup or repair
-- stopped routing to. Recorded so a later, explicit, customer-confirmed cleanup can prove a
-- channel is Champion-owned without ever deciding by name. source is ROUTE (a managed route used
-- to point at it) or LEGACY_SETUP (created by the legacy /setup command).
CREATE TABLE IF NOT EXISTS installation_retired_channels (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('CHANNEL', 'CATEGORY')),
    source TEXT NOT NULL CHECK (source IN ('ROUTE', 'LEGACY_SETUP')),
    former_routes TEXT[] NOT NULL DEFAULT '{}',
    legacy_field TEXT NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, channel_id)
);
`,
	},
	{
		Name: "0047_no_card_trial_grants",
		SQL: `
-- Champion Customer Onboarding V2: one no-card 14-day trial per Discord account.
-- trial_grants records the one trial a user has started (user_id is the key, so a second
-- organization never grants a second trial) and the organization it went to (UNIQUE, so one
-- organization is never trialed twice). intended_plan is the plan the customer picked during the
-- trial - it is never used as the paid plan; Stripe webhooks alone set subscriptions.plan.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS intended_plan TEXT;
CREATE TABLE IF NOT EXISTS trial_grants (
    user_id BIGINT PRIMARY KEY REFERENCES app_users(id) ON DELETE CASCADE,
    organization_id BIGINT NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
` + TrialGrantBackfillSQL,
	},
	{
		Name: "0048_case_evidence_observation",
		SQL: `
-- C.A.S.E. Phase 2B: immutable, source-addressed ADM evidence.
-- No client input, no scoring, no bans, no speculative event timestamp.
CREATE TABLE IF NOT EXISTS case_evidence_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    source_end_offset BIGINT NOT NULL CHECK (source_end_offset >= 0),
    line_sha256 CHAR(64) NOT NULL,
    event_type TEXT NOT NULL,
    adm_clock TEXT NOT NULL DEFAULT '',
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    subject_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    actor_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    target_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    subject_name TEXT NOT NULL DEFAULT '',
    actor_name TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',
    actor_x DOUBLE PRECISION,
    actor_z DOUBLE PRECISION,
    actor_altitude DOUBLE PRECISION,
    target_x DOUBLE PRECISION,
    target_z DOUBLE PRECISION,
    target_altitude DOUBLE PRECISION,
    subject_x DOUBLE PRECISION,
    subject_z DOUBLE PRECISION,
    subject_altitude DOUBLE PRECISION,
    weapon TEXT NOT NULL DEFAULT '',
    ammo TEXT NOT NULL DEFAULT '',
    hit_zone TEXT NOT NULL DEFAULT '',
    hit_zone_id TEXT NOT NULL DEFAULT '',
    damage DOUBLE PRECISION,
    hp DOUBLE PRECISION,
    distance_meters DOUBLE PRECISION,
    boundary_kind TEXT NOT NULL DEFAULT '' CHECK (
        boundary_kind IN ('','CONNECT','DISCONNECT','RESPAWN','DEATH','SUICIDE')
    ),
    CONSTRAINT uq_case_evidence_source UNIQUE (guild_id, server_id, source_id, source_end_offset)
);
CREATE INDEX IF NOT EXISTS idx_case_evidence_server_id ON case_evidence_events(guild_id,server_id,id DESC);
CREATE INDEX IF NOT EXISTS idx_case_evidence_actor_id ON case_evidence_events(guild_id,server_id,actor_player_id,id DESC) WHERE actor_player_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_evidence_target_id ON case_evidence_events(guild_id,server_id,target_player_id,id DESC) WHERE target_player_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_evidence_subject_id ON case_evidence_events(guild_id,server_id,subject_player_id,id DESC) WHERE subject_player_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_evidence_hit_time ON case_evidence_events(guild_id,server_id,ingested_at DESC) WHERE event_type='PLAYER_HIT';
`,
	},
}
