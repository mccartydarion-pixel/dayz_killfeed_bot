package database

// migrations0024to0035 holds migrations 0024 through 0035, in execution order. It is one fragment of the
// registry assembled in migrations.go; never edit an applied migration.
var migrations0024to0035 = []Migration{
	{
		// SaaS Phase 1 / Stage B2: the Go backend is the authoritative owner of
		// the shared Champion production schema (see the ownership rule
		// documented in docs/SAAS_SCHEMA.md) - the website reads/writes these
		// tables through server-side services but never runs its own
		// migrations against them. Purely additive: no existing table is
		// renamed, no existing column is dropped or retyped, and every new
		// foreign key back into gameplay tables (game_servers, guilds) uses
		// SET NULL/CASCADE only where the cascaded row is SaaS metadata, never
		// kill/death/player history (section 19/20 of the task).
		//
		// organization_id is added directly to the two existing tables that
		// already represent "DayZ server connection" (game_servers) and
		// "encrypted Nitrado credential" (nitrado_connections) rather than
		// creating parallel dayz_server_connections/nitrado_credentials
		// tables - see docs/SAAS_SCHEMA.md for the full reuse mapping.
		Name: "0024_saas_foundation",
		SQL: `
CREATE TABLE IF NOT EXISTS app_users (
    id BIGSERIAL PRIMARY KEY,
    discord_user_id TEXT UNIQUE NOT NULL,
    discord_username TEXT NOT NULL,
    discord_global_name TEXT,
    avatar TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS organizations (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    slug TEXT UNIQUE NOT NULL,
    owner_user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_organizations_owner ON organizations(owner_user_id);

CREATE TABLE IF NOT EXISTS organization_members (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organization_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_organization_members_user ON organization_members(user_id);

CREATE TABLE IF NOT EXISTS discord_guild_connections (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL UNIQUE REFERENCES guilds(id) ON DELETE CASCADE,
    guild_name TEXT,
    guild_icon TEXT,
    bot_installed BOOLEAN NOT NULL DEFAULT TRUE,
    permissions_verified BOOLEAN NOT NULL DEFAULT FALSE,
    connected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_discord_guild_connections_org ON discord_guild_connections(organization_id);

-- game_servers already represents "a customer's DayZ/Nitrado server
-- connection" (see 0012_phase48_multitenant_servers_credentials) - extended
-- rather than duplicated under a dayz_server_connections table.
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS organization_id BIGINT REFERENCES organizations(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_game_servers_org ON game_servers(organization_id);

-- nitrado_connections already represents the encrypted Nitrado credential
-- envelope (ciphertext/nonce/key_version) - extended rather than duplicated
-- under a nitrado_credentials table.
ALTER TABLE nitrado_connections ADD COLUMN IF NOT EXISTS organization_id BIGINT REFERENCES organizations(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_nitrado_connections_org ON nitrado_connections(organization_id);

CREATE TABLE IF NOT EXISTS installations (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    discord_guild_connection_id BIGINT NOT NULL REFERENCES discord_guild_connections(id) ON DELETE CASCADE,
    game_server_id BIGINT REFERENCES game_servers(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'NOT_STARTED',
    plan TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    setup_completed_at TIMESTAMPTZ,
    last_health_check_at TIMESTAMPTZ,
    UNIQUE(discord_guild_connection_id, game_server_id)
);
CREATE INDEX IF NOT EXISTS idx_installations_org ON installations(organization_id);
CREATE INDEX IF NOT EXISTS idx_installations_status ON installations(status);
CREATE INDEX IF NOT EXISTS idx_installations_guild_connection ON installations(discord_guild_connection_id);
CREATE INDEX IF NOT EXISTS idx_installations_server ON installations(game_server_id);

CREATE TABLE IF NOT EXISTS installation_setup_progress (
    installation_id BIGINT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    current_step TEXT NOT NULL DEFAULT 'DISCORD',
    discord_completed BOOLEAN NOT NULL DEFAULT FALSE,
    nitrado_completed BOOLEAN NOT NULL DEFAULT FALSE,
    server_selected BOOLEAN NOT NULL DEFAULT FALSE,
    channels_completed BOOLEAN NOT NULL DEFAULT FALSE,
    validation_completed BOOLEAN NOT NULL DEFAULT FALSE,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS installation_settings (
    installation_id BIGINT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    killfeed_channel_id TEXT,
    leaderboard_channel_id TEXT,
    player_status_channel_id TEXT,
    admin_log_channel_id TEXT,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    distance_unit TEXT NOT NULL DEFAULT 'METERS',
    online_display_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    leaderboard_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS subscriptions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL UNIQUE REFERENCES organizations(id) ON DELETE CASCADE,
    provider TEXT,
    provider_customer_id TEXT,
    provider_subscription_id TEXT,
    plan TEXT NOT NULL DEFAULT 'TRIAL',
    status TEXT NOT NULL DEFAULT 'TRIAL',
    trial_ends_at TIMESTAMPTZ,
    current_period_end TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
	},
	{
		// SaaS Phase 1 / console DayZ backend: the "connect Nitrado" flow is
		// organization-scoped (one Nitrado account credential per
		// organization, POST /api/saas/organizations/{id}/nitrado/connect -
		// no guild/installation in that path), unlike the original
		// guild-scoped bot /setup flow that created nitrado_connections.
		// guild_id NOT NULL. Relaxing it to nullable (rather than adding a
		// parallel nitrado_credentials table - reuse, not duplicate, per
		// this task) lets a SaaS-created row exist with organization_id set
		// and guild_id NULL; existing guild-scoped rows are untouched.
		// UNIQUE(organization_id) is safe alongside the existing
		// UNIQUE(guild_id): Postgres never treats NULLs as conflicting, so
		// legacy guild_id-only rows (organization_id NULL) never collide
		// with each other or with SaaS rows.
		Name: "0025_saas_nitrado_console",
		SQL: `
ALTER TABLE nitrado_connections ALTER COLUMN guild_id DROP NOT NULL;
ALTER TABLE nitrado_connections ADD CONSTRAINT nitrado_connections_organization_id_key UNIQUE(organization_id);
`,
	},
	{
		// SaaS Step 5 one-click channel auto-setup: channel_setup_source
		// distinguishes an explicit customer PUT (.../channels, "MANUAL")
		// from the one-click auto-setup endpoint ("AUTO") so a repeat
		// auto-setup call can safely reuse its own prior work while never
		// silently overwriting a customer's manual customization -
		// installation_settings' existing killfeed/leaderboard/player-status/
		// admin-log channel ID columns remain the single source of truth for
		// which channels are actually configured (section 4/13 - "once
		// created, those IDs become authoritative"). champion_category_id
		// remembers the Champion-managed category so it can be looked up by
		// ID (authoritative) instead of by name on every repeat run - name
		// matching is only a recovery fallback (section 4).
		Name: "0026_saas_channel_auto_setup",
		SQL: `
ALTER TABLE installation_settings ADD COLUMN IF NOT EXISTS channel_setup_source TEXT NOT NULL DEFAULT '';
ALTER TABLE installation_settings ADD COLUMN IF NOT EXISTS champion_category_id TEXT;
`,
	},
	{
		// SaaS Step 5 full channel routing: replaces the four-field
		// installation_settings model with a scalable feature -> Discord
		// channel table, one row per (installation, route_key). Additive
		// only - installation_settings' four legacy columns are neither
		// dropped nor stop being read (section 20/5): GET/PUT
		// .../installations/{id}/channels keeps working exactly as before
		// for any consumer that hasn't migrated to
		// GET/PUT .../channel-routes yet.
		//
		// The one-time backfill below seeds the new table from whatever
		// legacy columns are already set, so an existing installation's
		// configuration is visible under the new system immediately -
		// route_key choices per section 5's explicit mapping:
		//   killfeed_channel_id      -> KILLFEED
		//   leaderboard_channel_id   -> STATS_LEADERBOARDS (explicit instruction)
		//   player_status_channel_id -> STATS_LEADERBOARDS, but only as a
		//     fallback when leaderboard_channel_id is unset - audited via
		//     docs/SAAS_SCHEMA.md's own doc comment, which decouples
		//     player_status_channel_id from guilds.player_stats_channel_id
		//     (internal/discord/setup_store.go's PlayerStatsChannelID: an
		//     on-demand "my stats / search player" panel - the same
		//     "manually viewed... statistics" purpose STATS_LEADERBOARDS
		//     describes, not a connections/join-leave log). CONNECTIONS has
		//     no legacy source: its closest runtime analog,
		//     OnlinePlayersChannelID, drives a voice-channel-name counter,
		//     not a text channel, so backfilling it here would be wrong.
		//   admin_log_channel_id     -> ADMIN_LOGS (explicit instruction)
		// managed_by_champion is backfilled from channel_setup_source
		// ('AUTO' -> true), which already distinguishes exactly this.
		Name: "0027_saas_channel_routes",
		SQL: `
CREATE TABLE IF NOT EXISTS installation_channel_routes (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    managed_by_champion BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, route_key)
);
CREATE INDEX IF NOT EXISTS idx_installation_channel_routes_installation ON installation_channel_routes(installation_id);

INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT installation_id, 'KILLFEED', killfeed_channel_id, (channel_setup_source = 'AUTO')
FROM installation_settings WHERE killfeed_channel_id IS NOT NULL
ON CONFLICT (installation_id, route_key) DO NOTHING;

INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT installation_id, 'STATS_LEADERBOARDS', COALESCE(leaderboard_channel_id, player_status_channel_id), (channel_setup_source = 'AUTO')
FROM installation_settings WHERE COALESCE(leaderboard_channel_id, player_status_channel_id) IS NOT NULL
ON CONFLICT (installation_id, route_key) DO NOTHING;

INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT installation_id, 'ADMIN_LOGS', admin_log_channel_id, (channel_setup_source = 'AUTO')
FROM installation_settings WHERE admin_log_channel_id IS NOT NULL
ON CONFLICT (installation_id, route_key) DO NOTHING;
`,
	},
	{
		Name: "0028_guild_route_panels",
		SQL: `
-- Which persistent panel message lives in which routed channel, per guild and
-- route key (LINK_GAMERTAG, STATS_LEADERBOARDS, AUTO_LEADERBOARD). Keyed by
-- channel so several servers routing to the same channel share one message and
-- a route change can never leave two live panels behind.
CREATE TABLE IF NOT EXISTS guild_route_panels (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (guild_id, route_key, channel_id)
);
`,
	},
	{
		Name: "0029_bounties_server_scope",
		SQL: `
-- Server-scoped bounties (Phase 1). server_id NULL keeps every pre-existing row
-- guild-wide (claimable on any server of the guild, exactly as before); a set
-- server_id makes the bounty claimable only by a kill on that server.
ALTER TABLE bounties ADD COLUMN IF NOT EXISTS server_id BIGINT REFERENCES game_servers(id) ON DELETE CASCADE;

-- Several manual bounties may now be active on one target (stacking). The
-- streak-driven AUTOMATIC bounty keeps its "at most one active per target"
-- rule, so the old blanket index is replaced by one scoped to AUTOMATIC.
DROP INDEX IF EXISTS uq_active_bounty_target;
CREATE UNIQUE INDEX IF NOT EXISTS uq_active_automatic_bounty ON bounties(guild_id,target_player_id) WHERE status='ACTIVE' AND created_by_type='AUTOMATIC';

CREATE INDEX IF NOT EXISTS idx_bounties_server_status ON bounties(guild_id,server_id,status);
CREATE INDEX IF NOT EXISTS idx_bounties_target_player ON bounties(target_player_id);
CREATE INDEX IF NOT EXISTS idx_bounties_created_at ON bounties(created_at);
CREATE INDEX IF NOT EXISTS idx_bounties_claimed_at ON bounties(claimed_at) WHERE claimed_at IS NOT NULL;
`,
	},
	{
		Name: "0030_economy_ledger",
		SQL: `
-- Economy Phase 1: the existing Champion Points tables become the economy's
-- ledger and balance store (no second currency). Everything stays guild-wide.

-- The ledger amount is now signed (debits are negative) and 64-bit: economy
-- totals can exceed what a 32-bit column holds. (Rewrites the table once.)
ALTER TABLE point_transactions ALTER COLUMN amount TYPE BIGINT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS balance_after BIGINT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS created_by TEXT;
ALTER TABLE point_transactions ADD COLUMN IF NOT EXISTS server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL;

-- The spendable balance. lifetime_points/season_points stay earn-only leaderboard
-- scores (season_points resets each season; balance never does).
ALTER TABLE player_points ADD COLUMN IF NOT EXISTS balance BIGINT NOT NULL DEFAULT 0;
` + EconomyBackfillSQL + `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'player_points_balance_nonneg') THEN
        ALTER TABLE player_points ADD CONSTRAINT player_points_balance_nonneg CHECK (balance >= 0);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_point_transactions_history ON point_transactions(guild_id, player_id, id DESC);

-- Append-only: a ledger row's facts can never be rewritten. (balance_after may be
-- filled in once for legacy rows, and the ON DELETE SET NULL foreign keys may
-- null season_id/server_id; row deletion by cascade is unaffected.)
CREATE OR REPLACE FUNCTION point_transactions_append_only() RETURNS trigger AS $fn$
BEGIN
    IF NEW.guild_id IS DISTINCT FROM OLD.guild_id
       OR NEW.player_id IS DISTINCT FROM OLD.player_id
       OR NEW.amount IS DISTINCT FROM OLD.amount
       OR NEW.reason_type IS DISTINCT FROM OLD.reason_type
       OR NEW.source_key IS DISTINCT FROM OLD.source_key
       OR NEW.description IS DISTINCT FROM OLD.description
       OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (OLD.balance_after IS NOT NULL AND NEW.balance_after IS DISTINCT FROM OLD.balance_after) THEN
        RAISE EXCEPTION 'point_transactions is append-only';
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_point_transactions_append_only ON point_transactions;
CREATE TRIGGER trg_point_transactions_append_only BEFORE UPDATE ON point_transactions FOR EACH ROW EXECUTE FUNCTION point_transactions_append_only();
`,
	},
	{
		// Embed Designer Phase 2: durable custom embed templates. One row per
		// (installation, route). Storage only - no Discord publisher reads this yet.
		// route_key is TEXT validated in Go against the fixed route set (this schema's
		// convention: no CHECK for enumerated values); config_json is the typed,
		// server-validated template (never a raw client blob). Deleting an installation
		// deletes its templates (same as its other child rows).
		Name: "0031_installation_embed_templates",
		SQL: `
CREATE TABLE IF NOT EXISTS installation_embed_templates (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    route_key TEXT NOT NULL,
    config_json JSONB NOT NULL CHECK (jsonb_typeof(config_json) = 'object' AND octet_length(config_json::text) <= 65536),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(installation_id, route_key)
);
`,
	},
	{
		// Faction Hub Phase 1: the web-first, installation-scoped faction directory,
		// membership and recruitment model. Deliberately PARALLEL to the Discord-side
		// factions/faction_members tables (migration 0005): those are keyed by guild and
		// DayZ player_id and are referenced by kills, wars, events and seasons, while the
		// Hub is keyed by organization + installation (+ DayZ server) and by website
		// user (app_users). Hub tables carry the hub_ prefix; legacy_faction_id is a
		// nullable, unused bridge column for a later phase. Nothing here alters the
		// existing faction tables.
		//
		// Tenant integrity is enforced by the schema, not only by handlers:
		//   * (installation_id, organization_id) is a composite FK to installations, so a
		//     faction can never claim an organization that does not own its installation;
		//   * children carry (faction_id, installation_id) as a composite FK to the
		//     faction, so a member/application can never sit in a different installation
		//     than its faction;
		//   * uq_hub_members_installation_user makes "one active faction per user per
		//     installation" a database guarantee (the service also checks it, for a
		//     friendly error and inside the accept-application transaction).
		// Unlike most status text in this schema, the few columns whose corruption
		// would break the membership rules also carry a CHECK.
		Name: "0032_faction_hub",
		SQL: `
CREATE UNIQUE INDEX IF NOT EXISTS uq_installations_id_org ON installations(id, organization_id);

CREATE TABLE IF NOT EXISTS hub_factions (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    tag TEXT NOT NULL,
    slug TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    recruitment_status TEXT NOT NULL DEFAULT 'CLOSED' CHECK (recruitment_status IN ('OPEN','INVITE_ONLY','CLOSED')),
    -- Visual keys: placeholders for the approved catalogs (never URLs; not writable yet).
    logo_key TEXT,
    flag_key TEXT,
    armband_key TEXT,
    primary_color TEXT CHECK (primary_color IS NULL OR primary_color ~ '^#[0-9A-Fa-f]{6}$'),
    secondary_color TEXT CHECK (secondary_color IS NULL OR secondary_color ~ '^#[0-9A-Fa-f]{6}$'),
    -- The founder. RESTRICT: a faction is never left without its founding account.
    created_by_user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE RESTRICT,
    -- Unused bridge to the Discord-side faction (a later phase).
    legacy_faction_id BIGINT REFERENCES factions(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    UNIQUE (id, installation_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_slug ON hub_factions(installation_id, slug);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_name ON hub_factions(installation_id, LOWER(name));
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_factions_installation_tag ON hub_factions(installation_id, LOWER(tag));
CREATE INDEX IF NOT EXISTS idx_hub_factions_installation_recruiting ON hub_factions(installation_id, recruitment_status);
CREATE INDEX IF NOT EXISTS idx_hub_factions_org ON hub_factions(organization_id);

CREATE TABLE IF NOT EXISTS hub_faction_settings (
    faction_id BIGINT PRIMARY KEY REFERENCES hub_factions(id) ON DELETE CASCADE,
    minimum_hours INTEGER CHECK (minimum_hours IS NULL OR minimum_hours >= 0),
    minimum_age INTEGER CHECK (minimum_age IS NULL OR minimum_age BETWEEN 0 AND 120),
    pvp_required BOOLEAN NOT NULL DEFAULT FALSE,
    builder_needed BOOLEAN NOT NULL DEFAULT FALSE,
    mic_required BOOLEAN NOT NULL DEFAULT FALSE,
    custom_requirements TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Role catalog. Phase 1 has only the three built-in (system) roles, seeded below with
-- faction_id NULL; a later phase can add per-faction custom roles as rows with a
-- faction_id and grant them through hub_faction_role_memberships. Neither is exposed
-- through the API yet.
CREATE TABLE IF NOT EXISTS hub_faction_roles (
    id BIGSERIAL PRIMARY KEY,
    faction_id BIGINT REFERENCES hub_factions(id) ON DELETE CASCADE,
    role_key TEXT NOT NULL,
    display_name TEXT NOT NULL,
    rank INTEGER NOT NULL,
    is_system BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_faction_roles_system_key ON hub_faction_roles(role_key) WHERE faction_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_faction_roles_faction_key ON hub_faction_roles(faction_id, role_key) WHERE faction_id IS NOT NULL;
INSERT INTO hub_faction_roles(faction_id, role_key, display_name, rank, is_system) VALUES
    (NULL, 'LEADER', 'Leader', 0, TRUE),
    (NULL, 'OFFICER', 'Officer', 1, TRUE),
    (NULL, 'MEMBER', 'Member', 2, TRUE)
ON CONFLICT (role_key) WHERE faction_id IS NULL DO NOTHING;

CREATE TABLE IF NOT EXISTS hub_faction_members (
    id BIGSERIAL PRIMARY KEY,
    faction_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    -- The DayZ identity, when the user has a verified gamertag link on the guild.
    player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    -- The primary (built-in) role: LEADER, OFFICER or MEMBER.
    role_key TEXT NOT NULL CHECK (role_key IN ('LEADER','OFFICER','MEMBER')),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    CONSTRAINT uq_hub_members_faction_user UNIQUE (faction_id, user_id),
    CONSTRAINT uq_hub_members_installation_user UNIQUE (installation_id, user_id)
);
-- At most one LEADER per faction (the creation transaction supplies the first one).
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_members_one_leader ON hub_faction_members(faction_id) WHERE role_key = 'LEADER';
CREATE INDEX IF NOT EXISTS idx_hub_members_faction ON hub_faction_members(faction_id, role_key);
CREATE INDEX IF NOT EXISTS idx_hub_members_user ON hub_faction_members(user_id);

CREATE TABLE IF NOT EXISTS hub_faction_role_memberships (
    id BIGSERIAL PRIMARY KEY,
    member_id BIGINT NOT NULL REFERENCES hub_faction_members(id) ON DELETE CASCADE,
    role_id BIGINT NOT NULL REFERENCES hub_faction_roles(id) ON DELETE CASCADE,
    granted_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (member_id, role_id)
);

-- Application history is never deleted: every outcome is a status.
CREATE TABLE IF NOT EXISTS hub_faction_applications (
    id BIGSERIAL PRIMARY KEY,
    faction_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','ACCEPTED','DENIED','WITHDRAWN','CANCELLED')),
    message TEXT NOT NULL DEFAULT '',
    -- Reserved for leader-defined questions; the API keeps it disabled in Phase 1.
    answers_json JSONB CHECK (answers_json IS NULL OR jsonb_typeof(answers_json) = 'object'),
    reviewed_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_applications_pending ON hub_faction_applications(faction_id, user_id) WHERE status = 'PENDING';
CREATE INDEX IF NOT EXISTS idx_hub_applications_faction_status ON hub_faction_applications(faction_id, status, id DESC);
CREATE INDEX IF NOT EXISTS idx_hub_applications_user ON hub_faction_applications(installation_id, user_id, status);
`,
	},
	{
		// Faction Hub Phase 4: faction logo storage (docs/FACTIONS.md). Metadata lives in
		// hub_faction_assets; the bytes live behind the assetstore.Store abstraction (in
		// production the durable PostgreSQL-backed store below - Champion has no object
		// storage, and the Railway service filesystem is not durable). A faction never holds
		// image bytes itself: hub_factions.logo_asset_id only points at a metadata row.
		//
		// Tenant integrity is in the schema: an asset carries (faction_id, installation_id)
		// as a composite FK to its faction, and the faction's logo pointer is a composite FK
		// (logo_asset_id, id) -> (asset id, faction_id), so a faction can only ever point at
		// one of its OWN assets. The pointer clears (SET NULL on the one column) if the asset
		// row goes; the asset rows cascade with the faction. storage_key is generated by the
		// server (never derived from an uploaded name); public_id is the unguessable id used
		// in the public URL. Additive only: no existing column or row changes.
		Name: "0033_faction_logo_assets",
		SQL: `
CREATE TABLE IF NOT EXISTS hub_faction_assets (
    id BIGSERIAL PRIMARY KEY,
    public_id UUID NOT NULL UNIQUE,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    asset_type TEXT NOT NULL CHECK (asset_type IN ('LOGO')),
    storage_key TEXT NOT NULL UNIQUE CHECK (length(storage_key) <= 200 AND storage_key !~ '\.\.' AND storage_key ~ '^[A-Za-z0-9][A-Za-z0-9/._-]*$'),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png','image/jpeg','image/webp')),
    size_bytes INTEGER NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 5242880),
    width INTEGER NOT NULL CHECK (width BETWEEN 1 AND 8192),
    height INTEGER NOT NULL CHECK (height BETWEEN 1 AND 8192),
    -- Sanitized display/debug name only; never used as a path or executable input.
    original_filename TEXT NOT NULL DEFAULT '' CHECK (length(original_filename) <= 120),
    created_by_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    UNIQUE (id, faction_id)
);
CREATE INDEX IF NOT EXISTS idx_hub_faction_assets_faction ON hub_faction_assets(faction_id, asset_type);

ALTER TABLE hub_factions ADD COLUMN IF NOT EXISTS logo_asset_id BIGINT;
ALTER TABLE hub_factions ADD CONSTRAINT fk_hub_factions_logo_asset
    FOREIGN KEY (logo_asset_id, id) REFERENCES hub_faction_assets(id, faction_id) ON DELETE SET NULL (logo_asset_id);

-- Bytes for the PostgreSQL-backed assetstore.Store. One row per stored object; the metadata
-- above says what it is. Kept in its own table so listing/serving faction rows never reads
-- image data, and a different store (an S3-compatible bucket) can replace it without touching
-- the metadata model.
CREATE TABLE IF NOT EXISTS hub_asset_blobs (
    storage_key TEXT PRIMARY KEY CHECK (length(storage_key) <= 200),
    content_type TEXT NOT NULL,
    data BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_hub_asset_blobs_created ON hub_asset_blobs(created_at);
`,
	},
	{
		// Faction Hub Phase 5: competitive stats, achievements and activity (docs/FACTION_STATS.md).
		// Nothing here duplicates killfeed data: kills, deaths and bounties stay authoritative in
		// their existing tables and the Hub only DERIVES faction figures from them. What the Hub
		// must add is TIME: a kill counts for a faction only while its player was a member, so
		// membership needs history (hub_faction_members holds only the current state and its rows
		// are deleted on leave/removal).
		//
		//   * hub_faction_membership_history: one row per membership period [joined_at, left_at);
		//     open while left_at IS NULL (at most one open period per user per installation, the
		//     same rule as the live membership). player_identity_id is an informational snapshot
		//     of the verified DayZ identity at join time - attribution always uses the live
		//     verified link (player_links), never this column.
		//   * hub_faction_activity: public-safe, non-combat events the Hub itself produces
		//     (joins, leaves, role changes, leadership transfers, branding). Combat events are
		//     derived from kills/bounties at read time, achievements from the unlock table; this is
		//     NOT the internal audit log (slog) and holds no free text.
		//   * hub_faction_achievement_unlocks: one row per (faction, achievement) - the unique key
		//     is what makes concurrent evaluation unlock exactly once.
		//
		// Current members are backfilled into history with their real joined_at, so their kills
		// since joining are attributed correctly. Members who left BEFORE this migration have no
		// recorded period and cannot be reconstructed: their earlier kills are not credited.
		Name: "0034_faction_stats_history",
		SQL: `
CREATE TABLE IF NOT EXISTS hub_faction_membership_history (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    player_identity_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    joined_at TIMESTAMPTZ NOT NULL,
    left_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (left_at IS NULL OR left_at >= joined_at),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_hub_history_open ON hub_faction_membership_history(installation_id, user_id) WHERE left_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_hub_history_faction ON hub_faction_membership_history(faction_id, joined_at);
CREATE INDEX IF NOT EXISTS idx_hub_history_user ON hub_faction_membership_history(user_id, installation_id);

INSERT INTO hub_faction_membership_history(organization_id, installation_id, faction_id, user_id, player_identity_id, joined_at)
SELECT f.organization_id, m.installation_id, m.faction_id, m.user_id, m.player_id, m.joined_at
FROM hub_faction_members m
JOIN hub_factions f ON f.id = m.faction_id
WHERE NOT EXISTS (SELECT 1 FROM hub_faction_membership_history h WHERE h.faction_id = m.faction_id AND h.user_id = m.user_id AND h.left_at IS NULL);

CREATE TABLE IF NOT EXISTS hub_faction_activity (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    event_type TEXT NOT NULL CHECK (event_type IN ('FACTION_CREATED','MEMBER_JOINED','MEMBER_LEFT','MEMBER_PROMOTED','MEMBER_DEMOTED','LEADERSHIP_TRANSFERRED','FACTION_UPDATED','FACTION_LOGO_CHANGED')),
    -- The member the event is about, and who caused it (leadership transfer: new / previous leader).
    subject_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    actor_user_id BIGINT REFERENCES app_users(id) ON DELETE SET NULL,
    -- A short fixed value (a role key), never free text.
    detail TEXT CHECK (detail IS NULL OR detail IN ('LEADER','OFFICER','MEMBER')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_hub_activity_faction ON hub_faction_activity(faction_id, occurred_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS hub_faction_achievement_unlocks (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    faction_id BIGINT NOT NULL,
    achievement_key TEXT NOT NULL,
    unlocked_at TIMESTAMPTZ NOT NULL,
    metadata_json JSONB CHECK (metadata_json IS NULL OR jsonb_typeof(metadata_json) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (faction_id, installation_id) REFERENCES hub_factions(id, installation_id) ON DELETE CASCADE,
    FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE,
    CONSTRAINT uq_hub_achievement_unlock UNIQUE (faction_id, achievement_key)
);
CREATE INDEX IF NOT EXISTS idx_hub_unlocks_faction ON hub_faction_achievement_unlocks(faction_id, unlocked_at DESC, id DESC);
`,
	},
	{
		// Economy web foundation: the ledger can no longer be edited (0030) OR deleted directly.
		// A correction is a compensating transaction, never a removal. Deletion caused by a
		// foreign-key cascade (a guild or player being deleted) still works: cascade actions run
		// inside an internal trigger, so pg_trigger_depth() is > 1 there, while a direct
		// DELETE statement reaches this trigger at depth 1. No table or column changes.
		Name: "0035_economy_ledger_no_delete",
		SQL: `
CREATE OR REPLACE FUNCTION point_transactions_no_delete() RETURNS trigger AS $fn$
BEGIN
    IF pg_trigger_depth() <= 1 THEN
        RAISE EXCEPTION 'point_transactions is append-only: correct a mistake with a compensating transaction';
    END IF;
    RETURN OLD;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_point_transactions_no_delete ON point_transactions;
CREATE TRIGGER trg_point_transactions_no_delete BEFORE DELETE ON point_transactions FOR EACH ROW EXECUTE FUNCTION point_transactions_no_delete();
`,
	},
}
