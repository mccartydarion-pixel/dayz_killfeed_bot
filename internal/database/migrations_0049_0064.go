package database

// migrations0049to0064 holds migrations 0049 through 0064, in execution order. It is one fragment of the
// registry assembled in migrations.go; never edit an applied migration.
var migrations0049to0064 = []Migration{
	{
		Name: "0049_shop_delivery_engine",
		SQL: `
-- Champion Shop Delivery Engine 2.0 (docs/SHOP_DELIVERY.md). Additive: a delivery policy on
-- products, a per-installation delivery map setting, and one shop_deliveries row per purchase.
-- Every delivery is still fulfilled by staff (delivery_type MANUAL); nothing here spawns items,
-- writes server files or restarts servers. No Nitrado credential is ever stored here.
ALTER TABLE shop_products ADD COLUMN IF NOT EXISTS delivery_policy TEXT NOT NULL DEFAULT 'MANUAL_PICKUP';
DO $$ BEGIN
    ALTER TABLE shop_products ADD CONSTRAINT shop_products_delivery_policy_check CHECK (delivery_policy IN ('MANUAL_PICKUP','MANUAL_COORDINATE'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS shop_delivery_settings (
    installation_id BIGINT PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    map_key TEXT CHECK (map_key IS NULL OR char_length(map_key) BETWEEN 1 AND 40),
    updated_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS shop_deliveries (
    id BIGSERIAL PRIMARY KEY,
    purchase_id BIGINT NOT NULL,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    delivery_type TEXT NOT NULL DEFAULT 'MANUAL' CHECK (delivery_type = 'MANUAL'),
    delivery_policy TEXT NOT NULL CHECK (delivery_policy IN ('MANUAL_PICKUP','MANUAL_COORDINATE')),
    map_key TEXT,
    coord_x DOUBLE PRECISION,
    coord_z DOUBLE PRECISION,
    -- Only the Phase 2A states are allowed; the reserved automatic-delivery states need a later migration.
    status TEXT NOT NULL CHECK (status IN ('MANUAL_READY','FULFILLED','CANCELLED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    fulfilled_at TIMESTAMPTZ,
    fulfilled_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    cancelled_at TIMESTAMPTZ,
    cancelled_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    cancel_reason TEXT CHECK (cancel_reason IS NULL OR cancel_reason IN ('REFUNDED','PURCHASE_CANCELLED','PURCHASE_FAILED')),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    FOREIGN KEY (purchase_id, installation_id) REFERENCES shop_purchases(id, installation_id) ON DELETE CASCADE,
    CONSTRAINT uq_shop_deliveries_purchase UNIQUE (purchase_id),
    -- A coordinate delivery carries a map and a finite ground position (NaN and +-Infinity fail the
    -- range comparison); a pickup carries none. There is no y/altitude column on purpose.
    CONSTRAINT shop_deliveries_coordinates CHECK (
        (delivery_policy = 'MANUAL_PICKUP' AND map_key IS NULL AND coord_x IS NULL AND coord_z IS NULL)
        OR (delivery_policy = 'MANUAL_COORDINATE' AND map_key IS NOT NULL
            AND coord_x >= 0 AND coord_x <= 100000 AND coord_z >= 0 AND coord_z <= 100000)),
    CONSTRAINT shop_deliveries_fulfilled CHECK (status <> 'FULFILLED' OR fulfilled_at IS NOT NULL),
    CONSTRAINT shop_deliveries_cancelled CHECK (status <> 'CANCELLED' OR (cancelled_at IS NOT NULL AND cancel_reason IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_shop_deliveries_queue ON shop_deliveries(installation_id, status, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_deliveries_installation ON shop_deliveries(installation_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_shop_deliveries_player ON shop_deliveries(installation_id, player_id, id DESC);
` + ShopDeliveryBackfillSQL,
	},
	{
		Name: "0050_live_sync_player_list_and_sessions",
		SQL: `
-- Champion Live Sync phase 1 (docs/CHAMPION_LIVE_SYNC.md). Additive.
-- 1. Every ADM location observation carries its physical source: the canonical ADM file, the byte
--    offset at the end of the line, its server-local time (DayZ writes zone-less local time, so it is
--    stored without a zone) and, for player lists, the snapshot identity.
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS source_file TEXT;
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS source_offset BIGINT;
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS source_local_time TIMESTAMP;
ALTER TABLE player_location_events ADD COLUMN IF NOT EXISTS snapshot_ref TEXT;
-- 2. PLAYER_LIST: the routine five-minute ADM player-list observation.
DO $$
DECLARE c TEXT;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'player_location_events'::regclass AND contype = 'c'
               AND pg_get_constraintdef(oid) LIKE '%event_type%'
    LOOP
        EXECUTE format('ALTER TABLE player_location_events DROP CONSTRAINT %I', c);
    END LOOP;
END $$;
ALTER TABLE player_location_events ADD CONSTRAINT player_location_events_event_type_check
    CHECK (event_type IN ('CONNECT','DISCONNECT','HIT','KILL','DEATH','RESPAWN','UNCONSCIOUS','OTHER_ADM','PLAYER_LIST'));
ALTER TABLE player_location_events ADD CONSTRAINT player_location_events_source_complete
    CHECK ((source_file IS NULL AND source_offset IS NULL) OR (source_file IS NOT NULL AND source_offset IS NOT NULL AND source_offset >= 0));
-- 3. Dedupe: a sourced row is unique by its physical source (an exact replay guard that also keeps
--    every five-minute observation of a stationary player); legacy unsourced rows keep the old key,
--    which previously deduplicated on Champion's ingestion time.
ALTER TABLE player_location_events DROP CONSTRAINT IF EXISTS uq_player_location_events_dedupe;
CREATE UNIQUE INDEX IF NOT EXISTS uq_player_location_events_legacy
    ON player_location_events(player_id, server_id, event_type, observed_at) WHERE source_file IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_player_location_events_source
    ON player_location_events(server_id, source_file, source_offset, player_id, event_type) WHERE source_file IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_player_location_events_session
    ON player_location_events(server_id, player_id, source_file, source_offset DESC) WHERE source_file IS NOT NULL;
-- 4. The server's current boot session = the ADM file the ingestion engine currently reads.
CREATE TABLE IF NOT EXISTS server_adm_sessions (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    adm_file TEXT NOT NULL,
    session_local_start TIMESTAMP,
    selected_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
	},
	{
		Name: "0051_live_sync_sources_and_records",
		SQL: `
-- Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md). Additive.
-- 1. One durable checkpoint per watched non-ADM source file (RPT, script, crash, restart.log). The
--    checkpoint is the end of the last complete line whose records are committed; it advances in the
--    same transaction as those records.
CREATE TABLE IF NOT EXISTS live_sync_sources (
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    family TEXT NOT NULL,
    source_file TEXT NOT NULL,
    remote_path TEXT NOT NULL,
    file_local_start TIMESTAMP,
    checkpoint_offset BIGINT NOT NULL DEFAULT 0 CHECK (checkpoint_offset >= 0),
    -- bytes that existed when the source was first attached; records at or before it are BACKFILL
    backfill_until BIGINT NOT NULL DEFAULT 0,
    read_size BIGINT NOT NULL DEFAULT 0,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    attached_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_read_at TIMESTAMPTZ,
    last_growth_at TIMESTAMPTZ,
    records BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (server_id, family, source_file)
);
-- 2. Every complete record read from those sources, recognized or UNKNOWN (sanitized), with its
--    physical identity. event_id is deterministic (scope + source + offset + category), so a replay
--    is a no-op. delivery separates bytes observed live from history read late.
CREATE TABLE IF NOT EXISTS live_sync_records (
    id BIGSERIAL PRIMARY KEY,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    event_id TEXT NOT NULL,
    family TEXT NOT NULL,
    source_file TEXT NOT NULL,
    source_offset BIGINT NOT NULL CHECK (source_offset > 0),
    category TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PARSED','PARTIAL','UNKNOWN')),
    delivery TEXT NOT NULL CHECK (delivery IN ('LIVE','BACKFILL')),
    boot_id TEXT NOT NULL DEFAULT '',
    source_local_time TIMESTAMP,
    source_utc TIMESTAMPTZ,
    visible_after TIMESTAMPTZ,
    detected_at TIMESTAMPTZ NOT NULL,
    persisted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    evidence TEXT NOT NULL DEFAULT '' CHECK (length(evidence) <= 1024),
    parser TEXT NOT NULL,
    UNIQUE (server_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_live_sync_records_recent ON live_sync_records(server_id, family, id DESC);
CREATE INDEX IF NOT EXISTS idx_live_sync_records_category ON live_sync_records(server_id, category, id DESC);
-- 3. A boot session ends on DayZ-written evidence (RPT shutdown completed, a restart.log restart or
--    pre-start line, a newer boot's file). An ended session is never CURRENT.
ALTER TABLE server_adm_sessions ADD COLUMN IF NOT EXISTS ended_at TIMESTAMPTZ;
ALTER TABLE server_adm_sessions ADD COLUMN IF NOT EXISTS ended_reason TEXT;
ALTER TABLE server_adm_sessions ADD COLUMN IF NOT EXISTS ended_evidence TEXT;
-- 4. The server's UTC offset, learned only from a restart.log line that states it.
CREATE TABLE IF NOT EXISTS live_sync_server_clock (
    server_id BIGINT PRIMARY KEY REFERENCES game_servers(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL,
    utc_offset_minutes INT NOT NULL CHECK (utc_offset_minutes BETWEEN -840 AND 840),
    learned_from TEXT NOT NULL,
    learned_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- 5. Kills and deaths carry the ADM line's physical source, so heatmaps join them to the location
--    row written from the same line (the old join on event_time never matched: event_time is NULL).
ALTER TABLE kills ADD COLUMN IF NOT EXISTS source_file TEXT;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS source_offset BIGINT;
ALTER TABLE kills ADD COLUMN IF NOT EXISTS source_local_time TIMESTAMP;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS source_file TEXT;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS source_offset BIGINT;
ALTER TABLE deaths ADD COLUMN IF NOT EXISTS source_local_time TIMESTAMP;
CREATE INDEX IF NOT EXISTS idx_kills_source ON kills(server_id, source_file, source_offset) WHERE source_file IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_deaths_source ON deaths(server_id, source_file, source_offset) WHERE source_file IS NOT NULL;
`,
	},
	{
		Name: "0052_live_sync_rpt_command_line_cleanup",
		SQL:  LiveSyncCommandLineCleanupSQL,
	},
	{
		Name: "0053_installation_embed_activation",
		SQL: `
-- Embed Designer runtime activation (docs/EMBED_RUNTIME.md "Activation"). Additive.
-- One row per installation and route: which presentation the installation SELECTED for that route.
-- DEFAULT (or no row) = the Champion default card; CUSTOM = the installation's saved template. A
-- custom template renders only when (1) the operator rollout switch CHAMPION_CUSTOM_EMBEDS_ENABLED
-- is on, (2) the route supports runtime rendering, (3) this row says CUSTOM and (4) the saved
-- template is valid and enabled. Scoped by installation: one installation never affects another.
CREATE TABLE IF NOT EXISTS installation_embed_activation (
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('DEFAULT', 'CUSTOM')),
    updated_by_user_id BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (installation_id, route_key)
);
-- Before this migration a saved, enabled template was meant to render whenever the rollout switch
-- was on. That intent is carried over as an explicit CUSTOM selection, for the runtime routes only;
-- every other route starts at DEFAULT. Templates themselves are untouched.
INSERT INTO installation_embed_activation (installation_id, route_key, mode)
SELECT installation_id, route_key, 'CUSTOM' FROM installation_embed_templates
WHERE route_key IN ('KILLFEED','HITFEED','PVE_FEED','BOUNTY_TRACKING','CONNECTIONS','ECONOMY')
  AND COALESCE((config_json->>'enabled')::boolean, false)
ON CONFLICT (installation_id, route_key) DO NOTHING;
`,
	},
	{
		// Champion Shop Phase 2C.3 durable delivery-attempt ledger (docs/SHOP_DELIVERY_PHASE2C3.md).
		// Additive; inert until something creates an attempt (automatic delivery stays disabled).
		Name: "0054_shop_delivery_attempts",
		SQL:  ShopDeliveryAttemptsSQL,
	},
	{
		// Champion Shop Phase 2C.4 structured canary evidence (docs/SHOP_DELIVERY_PHASE2C4.md). Additive,
		// append-only; depends on 0054. Inert until the canary execution gate is enabled.
		Name: "0055_shop_delivery_attempt_evidence",
		SQL:  ShopAttemptEvidenceSQL,
	},
	{
		// C.A.S.E. 2G.2: inert, source-linked shadow-evaluation ledger.
		Name: "0056_case_shadow_evaluations",
		SQL:  CaseShadowLedgerSQL,
	},
	{
		Name: "0056_case_addon_subscriptions",
		SQL: `
-- Phase 6.1: C.A.S.E. is an ADDITIVE per-server purchase, never a new base plan.
-- An installation may be repointed to a different game server; the purchased
-- game_server_id remains bound, and runtime access checks require equality.
-- RESTRICT deletion of bound installation/server: paid Stripe subscriptions
-- must be cancelled/reconciled before removing their local binding.
CREATE TABLE IF NOT EXISTS case_addon_subscriptions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    tier TEXT NOT NULL CHECK (tier IN ('CASE_WATCH','CASE_PRO','CASE_COMMAND')),
    status TEXT NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','TRIAL','ACTIVE','PAST_DUE','CANCELED','SUSPENDED')),
    provider TEXT CHECK (provider IS NULL OR provider = 'stripe'),
    provider_customer_id TEXT,
    provider_subscription_id TEXT,
    provider_price_id TEXT,
    current_period_start TIMESTAMPTZ,
    current_period_end TIMESTAMPTZ,
    trial_started_at TIMESTAMPTZ,
    trial_ends_at TIMESTAMPTZ,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT case_addon_installation_scope
        FOREIGN KEY (installation_id, organization_id)
        REFERENCES installations(id, organization_id) ON DELETE RESTRICT,
    CONSTRAINT uq_case_addon_org_installation UNIQUE (organization_id, installation_id),
    CONSTRAINT uq_case_addon_org_server UNIQUE (organization_id, game_server_id),
    CONSTRAINT case_addon_period_order CHECK (
        current_period_start IS NULL OR current_period_end IS NULL OR
        current_period_start < current_period_end
    ),
    CONSTRAINT case_addon_trial_order CHECK (
        trial_started_at IS NULL OR trial_ends_at IS NULL OR
        trial_started_at < trial_ends_at
    ),
    CONSTRAINT case_addon_subscription_id_nonempty CHECK (
        provider_subscription_id IS NULL OR LENGTH(BTRIM(provider_subscription_id)) > 0
    ),
    CONSTRAINT case_addon_active_provider CHECK (
        status NOT IN ('ACTIVE','TRIAL') OR (
            COALESCE(provider,'') = 'stripe' AND
            NULLIF(BTRIM(COALESCE(provider_subscription_id,'')),'') IS NOT NULL AND
            NULLIF(BTRIM(COALESCE(provider_price_id,'')),'') IS NOT NULL AND
            current_period_end IS NOT NULL
        )
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_addon_provider_subscription
    ON case_addon_subscriptions(provider, provider_subscription_id)
    WHERE provider_subscription_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_case_addon_org_status
    ON case_addon_subscriptions(organization_id, status, installation_id);
-- No backfill and no write/API route in this milestone. All packages start
-- with zero rows; only the later verified add-on webhook may grant access.
`,
	},
	{
		Name: "0057_case_checkout_reconciliation",
		SQL: `
-- Phase 6.2: retain a single per-server pending checkout and its Stripe
-- idempotency identity; do not create a new subscription when a retry races.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_session_id TEXT;
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_url TEXT;
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_reserved_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_checkout_session
    ON case_addon_subscriptions(checkout_session_id)
    WHERE checkout_session_id IS NOT NULL;
-- Checkout and subscription webhooks are recorded only in the same
-- transaction that successfully applies the event. An error rolls both back,
-- so Stripe retries can never be silently ignored.
CREATE TABLE IF NOT EXISTS case_addon_webhook_events (
    provider TEXT NOT NULL CHECK (provider = 'stripe'),
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    addon_id BIGINT NOT NULL REFERENCES case_addon_subscriptions(id) ON DELETE RESTRICT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider,event_id)
);
CREATE INDEX IF NOT EXISTS idx_case_addon_webhook_addon
    ON case_addon_webhook_events(addon_id, received_at DESC);
`,
	},
	{
		Name: "0058_case_payment_confirmation",
		SQL: `
-- A Stripe subscription can appear ACTIVE before asynchronous payment
-- succeeds. Preserve an independent invoice-paid proof for paid access.
-- C.A.S.E. ACTIVE access is withheld until a signed invoice.paid webhook.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS paid_through TIMESTAMPTZ;
`,
	},
	{
		Name: "0059_case_founder_trial_ledger",
		SQL: `
-- Additive, immutable one-time founder trial identity. No grants/backfill.
-- Future code must write a grant only after verifying an eligible existing
-- base customer and a real Stripe Pro trial on this exact bound game server.
CREATE TABLE IF NOT EXISTS case_addon_trial_grants (
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    installation_id BIGINT NOT NULL,
    addon_id BIGINT NOT NULL REFERENCES case_addon_subscriptions(id) ON DELETE RESTRICT,
    provider_subscription_id TEXT NOT NULL CHECK (LENGTH(BTRIM(provider_subscription_id)) > 0),
    tier TEXT NOT NULL DEFAULT 'CASE_PRO' CHECK (tier = 'CASE_PRO'),
    trial_started_at TIMESTAMPTZ NOT NULL,
    trial_ends_at TIMESTAMPTZ NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT case_founder_trial_scope FOREIGN KEY (installation_id, organization_id)
        REFERENCES installations(id, organization_id) ON DELETE RESTRICT,
    CONSTRAINT case_founder_trial_duration CHECK (
        trial_ends_at > trial_started_at AND
        trial_ends_at <= trial_started_at + INTERVAL '7 days'
    ),
    PRIMARY KEY (organization_id, game_server_id),
    CONSTRAINT uq_case_founder_trial_subscription UNIQUE (provider_subscription_id),
    CONSTRAINT uq_case_founder_trial_addon UNIQUE (addon_id)
);
-- The unique (organization_id, game_server_id) key survives a change of
-- installation or cancellation. A new trial for the same server is impossible.
`,
	},
	{
		Name: "0060_case_checkout_attempt",
		SQL: `
-- Recovery after a Stripe-confirmed expired Checkout Session must never
-- reuse the old Stripe idempotency identity.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS checkout_attempt BIGINT NOT NULL DEFAULT 1
        CHECK (checkout_attempt > 0);
`,
	},
	{
		Name: "0061_case_watch_digest_outbox",
		SQL: `
-- Durable per-server paid staff digest. A pre-send claim can expire and be
-- retried safely, but a SENDING row must NEVER be automatically resent:
-- a crash or network error may occur after Discord accepted a message.
CREATE TABLE IF NOT EXISTS case_watch_digest_outbox (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    installation_id BIGINT NOT NULL,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE RESTRICT,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    source_lines BIGINT NOT NULL CHECK (source_lines >= 0),
    hit_lines BIGINT NOT NULL CHECK (hit_lines >= 0),
    kill_lines BIGINT NOT NULL CHECK (kill_lines >= 0),
    collector_enabled BOOLEAN NOT NULL,
    status TEXT NOT NULL DEFAULT 'READY'
        CHECK (status IN ('READY','CLAIMED','SENDING','SENT','UNKNOWN','BLOCKED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 3),
    claim_version INTEGER NOT NULL DEFAULT 0 CHECK (claim_version >= 0),
    claim_expires_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    discord_channel_id TEXT,
    discord_message_id TEXT,
    reason_code TEXT,
    sent_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT case_digest_installation_scope FOREIGN KEY (installation_id,organization_id)
        REFERENCES installations(id,organization_id) ON DELETE RESTRICT,
    CONSTRAINT case_digest_window CHECK (window_start < window_end),
    CONSTRAINT case_digest_sent_receipt CHECK (
        status <> 'SENT' OR
        (NULLIF(BTRIM(COALESCE(discord_message_id,'')),'') IS NOT NULL
         AND NULLIF(BTRIM(COALESCE(discord_channel_id,'')),'') IS NOT NULL
         AND sent_at IS NOT NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_case_digest_claim
    ON case_watch_digest_outbox(status,next_attempt_at,id)
    WHERE status IN ('READY','CLAIMED');
CREATE INDEX IF NOT EXISTS idx_case_digest_scope
    ON case_watch_digest_outbox(organization_id,installation_id,game_server_id,requested_at DESC);
`,
	},
	{
		Name: "0062_case_watch_requester",
		SQL: `
-- Old rows from pre-release 0059 have NULL and are blocked by the worker.
-- New paid messages must retain the authenticated requester, so a role
-- revocation before delivery can be checked against fresh Discord roles.
ALTER TABLE case_watch_digest_outbox
    ADD COLUMN IF NOT EXISTS requested_by_user_id BIGINT REFERENCES app_users(id) ON DELETE RESTRICT;
`,
	},
	{
		// Inert C.A.S.E. core review/outbox schema; no production writer or sender.
		// 0057-0062 are reserved in an independent older billing candidate.
		Name: "0063_case_review_outbox_skeleton",
		SQL:  CASEReviewSkeletonSQL,
	},
	{
		Name: "0063_case_plan_changes",
		SQL: `
-- Phase 6.10: Watch <-> Pro changes and re-subscription after cancellation.
-- paid_tier is the tier a signed invoice.paid actually covered through
-- paid_through. tier/provider_price_id is what Stripe will bill next. An
-- upgrade unlocks only when its proration invoice is paid; a downgrade keeps
-- the already-paid tier until paid_through.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS paid_tier TEXT
        CHECK (paid_tier IS NULL OR paid_tier IN ('CASE_WATCH','CASE_PRO','CASE_COMMAND'));
UPDATE case_addon_subscriptions SET paid_tier=tier
    WHERE paid_through IS NOT NULL AND paid_tier IS NULL;
ALTER TABLE case_addon_subscriptions DROP CONSTRAINT IF EXISTS case_addon_paid_tier_required;
ALTER TABLE case_addon_subscriptions ADD CONSTRAINT case_addon_paid_tier_required
    CHECK (paid_through IS NULL OR paid_tier IS NOT NULL);
-- One CURRENT add-on per installation and per game server. A CANCELED row is
-- retained as history (its Stripe subscription id stays unique) so the server
-- can purchase again without deleting records or reusing an old identity.
ALTER TABLE case_addon_subscriptions DROP CONSTRAINT IF EXISTS uq_case_addon_org_installation;
ALTER TABLE case_addon_subscriptions DROP CONSTRAINT IF EXISTS uq_case_addon_org_server;
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_addon_current_installation
    ON case_addon_subscriptions(organization_id, installation_id) WHERE status <> 'CANCELED';
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_addon_current_server
    ON case_addon_subscriptions(organization_id, game_server_id) WHERE status <> 'CANCELED';
`,
	},
	{
		// Inert C.A.S.E. build-action evidence; no detector or alert activation.
		Name: "0064_case_build_evidence",
		SQL:  CASEBuildEvidenceSQL,
	},
	{
		Name: "0064_case_invoice_coverage",
		SQL: `
-- Phase 6.26B: invoice-level C.A.S.E. coverage so refunds, disputes and voids can revoke exactly
-- the coverage they reverse. Additive; touches no base billing table.
-- One row per paid C.A.S.E. invoice. Coverage in good standing (PAID, PARTIALLY_REFUNDED,
-- DISPUTE_WON) grants its tier until period_end; REFUNDED, DISPUTED, DISPUTE_LOST and VOIDED grant
-- nothing. paid_through/paid_tier on the add-on are recomputed from these rows.
CREATE TABLE IF NOT EXISTS case_addon_invoice_coverage (
    id BIGSERIAL PRIMARY KEY,
    addon_id BIGINT NOT NULL REFERENCES case_addon_subscriptions(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL CHECK (provider = 'stripe'),
    provider_invoice_id TEXT NOT NULL CHECK (LENGTH(BTRIM(provider_invoice_id)) > 0),
    provider_subscription_id TEXT NOT NULL CHECK (LENGTH(BTRIM(provider_subscription_id)) > 0),
    provider_payment_intent_id TEXT,
    tier TEXT NOT NULL CHECK (tier IN ('CASE_WATCH','CASE_PRO','CASE_COMMAND')),
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    amount_paid_cents BIGINT NOT NULL DEFAULT 0 CHECK (amount_paid_cents >= 0),
    amount_refunded_cents BIGINT NOT NULL DEFAULT 0 CHECK (amount_refunded_cents >= 0),
    currency TEXT,
    status TEXT NOT NULL DEFAULT 'PAID'
        CHECK (status IN ('PAID','PARTIALLY_REFUNDED','REFUNDED','DISPUTED','DISPUTE_WON','DISPUTE_LOST','VOIDED')),
    dispute_status TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_case_invoice_coverage UNIQUE (provider, provider_invoice_id),
    CONSTRAINT case_invoice_coverage_period CHECK (period_start < period_end)
);
CREATE INDEX IF NOT EXISTS idx_case_invoice_coverage_addon
    ON case_addon_invoice_coverage(addon_id, period_end DESC);
-- Append-only audit history of every coverage change (never updated or deleted by the app).
CREATE TABLE IF NOT EXISTS case_addon_coverage_events (
    id BIGSERIAL PRIMARY KEY,
    addon_id BIGINT NOT NULL REFERENCES case_addon_subscriptions(id) ON DELETE RESTRICT,
    provider_invoice_id TEXT NOT NULL,
    stripe_event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    previous_status TEXT,
    new_status TEXT NOT NULL,
    amount_refunded_cents BIGINT,
    dispute_status TEXT,
    paid_through_before TIMESTAMPTZ,
    paid_through_after TIMESTAMPTZ,
    paid_tier_before TEXT,
    paid_tier_after TEXT,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_case_coverage_events_addon
    ON case_addon_coverage_events(addon_id, recorded_at DESC);
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS coverage_state TEXT NOT NULL DEFAULT 'OK'
        CHECK (coverage_state IN ('OK','PARTIALLY_REFUNDED','REFUNDED','DISPUTED','DISPUTE_LOST'));
-- New add-ons start with a complete (empty) ledger. Pre-existing paid rows were paid before the
-- ledger existed: their invoices are reconstructed from Stripe before the first recalculation.
ALTER TABLE case_addon_subscriptions
    ADD COLUMN IF NOT EXISTS coverage_backfilled BOOLEAN NOT NULL DEFAULT TRUE;
UPDATE case_addon_subscriptions SET coverage_backfilled = FALSE
    WHERE paid_through IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM case_addon_invoice_coverage c WHERE c.addon_id = case_addon_subscriptions.id);
`,
	},
}
