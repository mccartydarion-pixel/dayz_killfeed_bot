# Champion SaaS schema (production-owned by the Go backend)

## Schema ownership

**The Go backend (this repository) is the authoritative owner of the shared
Champion production PostgreSQL schema**, including every table documented
here. `internal/database/migrations.go` is the single migration history for
the whole database - the website repository has no migration system and
must never gain one against these tables.

The website may read and write the tables below through its own
server-side services (never client-side, never with a shared/static
credential), but it must never run its own migrations against them, add
columns directly, or introduce a conflicting schema for the same concepts.
If the website needs a new field or table for these SaaS concerns, that
change belongs in this repository's `internal/database/migrations.go`, in a
PR reviewed the same way as any other Champion backend change.

This split exists because the Go backend already owns the entire
production migration history (schema_migrations, 24+ migrations covering
kills/players/factions/seasons/links/servers/etc. - see
`internal/database/migrations.go`) and the live runtime (ADM ingestion,
Discord bot, presence) that several of these tables also back. Splitting
schema ownership between two migration systems against the same database
would risk migration-order conflicts and duplicate-concept drift; centralizing
it here avoids that entirely.

## Reused vs. new tables

Some SaaS concepts already existed in Champion's schema from earlier
multi-tenant work (`0012_phase48_multitenant_servers_credentials`) and were
**extended, not duplicated**:

| SaaS concept | Table | Notes |
|---|---|---|
| DayZ/Nitrado server connection | `game_servers` (existing) | Added `organization_id` (nullable FK to `organizations`, `ON DELETE SET NULL`). The guild-scoped bot runtime (`ServerRepository`) is untouched; SaaS-scoped access goes through `SaaSServerRepository` (`internal/repository/saas_server_connections_repository.go`). |
| Encrypted Nitrado credential envelope | `nitrado_connections` (existing) | Added `organization_id` (nullable FK, `ON DELETE SET NULL`). `credential_nonce` is the AES-GCM IV; the auth tag is embedded in `credential_ciphertext` (standard Go `crypto/cipher` `AEAD.Seal` output); the algorithm is a fixed constant (`AES-256-GCM`, `repository.CredentialAlgorithm`), not a column, since only one is supported. Guild-scoped access is unchanged (`ServerRepository.SaveConnection`/`GetConnection`); SaaS-scoped access goes through `CredentialRepository` (`internal/repository/saas_credentials_repository.go`). |

Everything else below is new, added in `0024_saas_foundation`.

## New tables

### `app_users`
A durable mapping for a Discord-authenticated website user. **Never stores
a Discord OAuth access/refresh token** - that belongs to the website's own
session store.

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `discord_user_id` | TEXT, **UNIQUE NOT NULL** |
| `discord_username`, `discord_global_name`, `avatar` | TEXT |
| `created_at`, `updated_at`, `last_login_at` | TIMESTAMPTZ |

Repository: `UserRepository` (`saas_users_repository.go`) - `UpsertDiscordUser`, `GetByDiscordID`.

### `organizations`
The customer/account tenant boundary - the ID every other SaaS lookup must
be scoped by.

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `name`, `slug` | TEXT; `slug` UNIQUE |
| `owner_user_id` | BIGINT FK `app_users(id)`, `ON DELETE RESTRICT` (an owner can't be deleted while they still own an org) |
| `created_at`, `updated_at` | TIMESTAMPTZ |

Repository: `OrganizationRepository` (`saas_organizations_repository.go`) - `Create` (also creates the OWNER `organization_members` row, atomically), `ListForUser`, `VerifyMembership`.

### `organization_members`
| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `organization_id` | FK `organizations(id)`, `ON DELETE CASCADE` |
| `user_id` | FK `app_users(id)`, `ON DELETE CASCADE` |
| `role` | TEXT: `OWNER` \| `ADMIN` \| `MEMBER` (Go constants `repository.RoleOwner`/`RoleAdmin`/`RoleMember`; not a SQL CHECK, see below) |
| `created_at` | TIMESTAMPTZ |
| | `UNIQUE(organization_id, user_id)` |

### `discord_guild_connections`
Organization ownership of an **already-connected** `guilds` row. Does not
re-store the Discord snowflake or channel config - `guilds.discord_guild_id`
and `guilds`' channel columns stay authoritative for the bot runtime.

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `organization_id` | FK `organizations(id)`, `ON DELETE CASCADE` |
| `guild_id` | FK `guilds(id)`, **UNIQUE**, `ON DELETE CASCADE` - a guild can only ever be claimed by one organization |
| `guild_name`, `guild_icon` | TEXT (cached Discord metadata for the dashboard) |
| `bot_installed`, `permissions_verified` | BOOLEAN |
| `connected_at`, `updated_at` | TIMESTAMPTZ |

Repository: `GuildConnectionRepository` (`saas_guild_connections_repository.go`) - `Upsert`, `ListByOrganization`, `GetScoped`.

### `installations`
Connects an organization + a Discord guild connection + (once selected) a
DayZ server (`game_servers.id`).

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `organization_id` | FK `organizations(id)`, `ON DELETE CASCADE` |
| `discord_guild_connection_id` | FK `discord_guild_connections(id)`, `ON DELETE CASCADE` |
| `game_server_id` | FK `game_servers(id)`, nullable (no server selected yet), `ON DELETE CASCADE` |
| `status` | TEXT, one of `NOT_STARTED` \| `DISCORD_CONNECTED` \| `NITRADO_CONNECTED` \| `CONFIGURING` \| `READY` \| `DEGRADED` \| `DISCONNECTED` \| `SUSPENDED` (Go constants `repository.Installation*`) |
| `plan` | TEXT, nullable |
| `created_at`, `updated_at`, `setup_completed_at`, `last_health_check_at` | TIMESTAMPTZ; `setup_completed_at` is stamped once, the first time `status` reaches `READY` |
| | `UNIQUE(discord_guild_connection_id, game_server_id)` |

Repository: `InstallationRepository` (`saas_installations_repository.go`) - `Create` (also creates its `installation_setup_progress`/`installation_settings` rows, atomically), `GetScoped`, `ListByOrganization`, `UpdateStatus`, `RecordHealthCheck`.

`status` only reaches `READY` (and `validation_completed` above only
becomes `true`) via `POST .../installations/{id}/setup/complete`
(`internal/app/saas_api_setup_completion.go`, `docs/SAAS_API.md`'s "Setup
completion, customer hub, and revalidation" section) - never a client claim.
That same route also defines the handful of "critical" configuration
changes (the `KILLFEED` channel route, the selected DayZ server) that
downgrade an already-`READY` installation back to `CONFIGURING`/
`validation_completed=false` without erasing `completed_at`/
`setup_completed_at`, which are still only ever stamped the first time.

### `installation_setup_progress`
Resumable onboarding state, 1:1 with `installations`. Only durable,
non-derivable progress is stored - nothing the UI can recompute from
`installations`/`discord_guild_connections`/`game_servers` state.

| Column | Notes |
|---|---|
| `installation_id` | PK, FK `installations(id)`, `ON DELETE CASCADE` |
| `current_step` | TEXT |
| `discord_completed`, `nitrado_completed`, `server_selected`, `channels_completed`, `validation_completed` | BOOLEAN |
| `completed_at` | TIMESTAMPTZ, stamped once, the first time `validation_completed` becomes true |
| `updated_at` | TIMESTAMPTZ |

Repository methods: `InstallationRepository.GetSetupProgress`/`UpdateSetupProgress`.

### `installation_settings`
The SaaS dashboard-facing settings surface, 1:1 with `installations`.
**Intentionally decoupled** from `guilds`' own
`killfeed_channel_id`/`leaderboards_channel_id`/`player_stats_channel_id`
columns, which remain authoritative for the current single-server-per-guild
bot runtime. An installation is a *(guild, server)* pair, so this table is
the forward path to per-server channel configuration once a guild can have
multiple DayZ servers - wiring the bot runtime to read from here instead of
`guilds` is a separate, later task, not part of this schema change.

| Column | Notes |
|---|---|
| `installation_id` | PK, FK `installations(id)`, `ON DELETE CASCADE` |
| `killfeed_channel_id`, `leaderboard_channel_id`, `player_status_channel_id`, `admin_log_channel_id` | TEXT, nullable |
| `timezone` | TEXT, default `'UTC'` |
| `distance_unit` | TEXT, default `'METERS'` |
| `online_display_enabled`, `leaderboard_enabled` | BOOLEAN, default `TRUE` |
| `created_at`, `updated_at` | TIMESTAMPTZ |
| `channel_setup_source` | TEXT, default `''` - `''` \| `'AUTO'` \| `'MANUAL'`, tracks how the legacy four fields above were last set (never used by the new `installation_channel_routes` table below, which tracks ownership per-route instead) |
| `champion_category_id` | TEXT, nullable - the Champion-managed Discord category one-click setup created/reused, shared with `installation_channel_routes` (section below) |

Repository methods: `InstallationRepository.GetSettings`/`UpdateSettings`.

### `installation_channel_routes`
The scalable feature -> Discord channel routing table (migration 0027),
superseding `installation_settings`' four-field model for anything beyond
the original killfeed/leaderboard/player-status/admin-log channels.
**Additive, not a replacement**: `installation_settings`' four columns
above are neither dropped nor stop being read - see
`internal/app/saas_api_channels.go` (unchanged legacy `GET`/`PUT
.../channels` endpoints) vs. `internal/app/saas_api_channel_routes.go` (new
`GET`/`PUT .../channel-routes` and the upgraded `POST
.../channels/auto-setup`, which dual-writes into both).

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `installation_id` | FK `installations(id)`, `ON DELETE CASCADE` |
| `route_key` | TEXT - one of the sixteen stable keys below, never a display name |
| `channel_id` | TEXT - a Discord channel snowflake |
| `managed_by_champion` | BOOLEAN, default `FALSE` - `TRUE` only for a channel Champion itself created/reused via one-click auto-setup; `FALSE` once a customer explicitly points a route at a channel via a manual save. Any future "reset Champion channels" feature must only ever delete a channel where this is `TRUE`. |
| `created_at`, `updated_at` | TIMESTAMPTZ |
|  | `UNIQUE(installation_id, route_key)` - one channel per route per installation; the same `channel_id` may appear under multiple `route_key` rows (a customer may point several features at one channel) |

Index: `installation_id`.

**Stable route keys** (`internal/app/saas_api_channel_routes.go`'s
`championRouteBlueprint` - the single source of truth; never edit this list
without updating that Go slice, and vice versa):

| `route_key` | Default channel name | Purpose | Requirement |
|---|---|---|---|
| `KILLFEED` | `killfeed` | PvP kill/death/special-kill feed | REQUIRED |
| `PVE_FEED` | `pvefeed` | Infected/environment/PvE events | OPTIONAL (implemented for explicit suicides) |
| `LINK_GAMERTAG` | `link-gamertag` | Player linking / gamertag linking panel | FEATURE_DEPENDENT |
| `STATS_LEADERBOARDS` | `stats-leaderboards` | Manually viewed general statistics and leaderboards | OPTIONAL |
| `AUTO_LEADERBOARD` | `auto-leaderboard` | Automatically refreshed leaderboard panel | OPTIONAL |
| `HITFEED` | `hitfeed` | Hit/damage event feed | OPTIONAL (implemented) |
| `BOUNTY` | `bounty` | Public bounty board/events | OPTIONAL (implemented) |
| `BOUNTY_TRACKING` | `bounty-tracking` | Bounty progression/tracking | OPTIONAL (implemented) |
| `HEATMAPS` | `heatmaps` | Heatmap/activity output | OPTIONAL (not implemented yet) |
| `ECONOMY` | `economy` | Economy/credits information | OPTIONAL (not implemented yet) |
| `CASINO` | `casino` | Casino commands/results | OPTIONAL (not implemented yet) |
| `SHOP` | `shop` | Store/shop output | OPTIONAL (not implemented yet) |
| `CONNECTIONS` | `connections` | Connect/disconnect/player connection events | OPTIONAL (implemented) |
| `BUILD_FEED` | `build-feed` | Building/base-related feed | OPTIONAL (not implemented yet) |
| `ADMIN_ALERTS` | `admin-alerts` | Important moderation/server alerts | OPTIONAL (not implemented yet) |
| `ADMIN_LOGS` | `admin-logs` | Detailed administrative/diagnostic logging | FEATURE_DEPENDENT |

See `docs/SAAS_API.md`'s "Channel routing" section for the full runtime
publisher audit behind each requirement level, and the exact backward-
compatibility backfill migration 0027 performs from `installation_settings`.

Repository: `internal/repository/saas_channel_routes_repository.go`
(`ChannelRouteRepository`) - `ListForInstallation`, `UpsertRoute`,
`DeleteRoute`, `ListDistinctChannelIDs` (used by `POST .../setup/complete`'s
multi-channel permission check - `internal/app/saas_api_setup_completion.go`
- so several routes sharing one channel are verified exactly once).

### `subscriptions`
**Stripe is now integrated** (`docs/BILLING.md`, migration `0037`) - provider fields are populated once an organization checks out; they stay `NULL` until then.

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `organization_id` | FK `organizations(id)`, **UNIQUE**, `ON DELETE CASCADE` - one subscription per organization |
| `provider`, `provider_customer_id`, `provider_subscription_id`, `provider_price_id` | TEXT, nullable |
| `plan` | TEXT, default `'TRIAL'` |
| `status` | TEXT, one of `TRIAL` \| `ACTIVE` \| `PAST_DUE` \| `CANCELED` \| `SUSPENDED` (Go constants `repository.Subscription*`; Stripe's own status is mapped onto these - `docs/BILLING.md` "Subscription status mapping") |
| `billing_interval` | TEXT, `MONTHLY` \| `YEARLY`, nullable |
| `trial_ends_at`, `current_period_start`, `current_period_end` | TIMESTAMPTZ, nullable |
| `cancel_at_period_end` | BOOLEAN, default `false` |
| `canceled_at` | TIMESTAMPTZ, nullable |
| `trial_consumed` | BOOLEAN, default `false` - a Stripe trial is granted at most once per organization |
| `created_at`, `updated_at` | TIMESTAMPTZ |

Repository: `SubscriptionRepository` (`saas_subscriptions_repository.go`) - `EnsureTrial` (idempotent), `GetForOrganization`, `GetByProviderCustomerID`, `GetByProviderSubscriptionID`,
`SetProviderCustomer`, `ApplyProviderState` (the one write path Stripe reconciliation uses), `RecordWebhookEventOnce`. See "Champion Billing (migration 0037)" below and `docs/BILLING.md`.

## Entitlements

Feature gating is **not a table** - `internal/entitlements` centralizes
feature-key resolution by plan in Go code (`Resolve(plan) []Key`,
`Has(plan, key) bool`), matching the website's own centralized entitlement
design instead of duplicating per-installation boolean flags. Keys today:
`killfeed`, `leaderboards`, `live_players`, `website_dashboard`,
`discord_activity`, `special_kills`, `advanced_stats`, `multiple_servers`,
`priority_support`.

**Paid access is not enforced yet.** `Resolve` currently returns the full
key set for every plan - defining what each paid tier actually grants is a
business decision for the billing-integration task, not this one. Call
sites should key off the returned entitlement keys (not "is a plan
configured"), so tightening this later needs no call-site changes.

## Enum/status convention

No column in this schema uses a SQL `CHECK` constraint for enumerated
values (matching the existing convention throughout
`internal/database/migrations.go` - e.g. `game_servers.status`,
`competitive_events.status`). Every enumerated column is a plain `TEXT`
column with allowed values enforced at the Go application layer via typed
constants. This is deliberate: adding a new status/role/plan value later
needs a Go change only, never a migration to redefine a `CHECK`.
`game_servers.platform`/`installations.plan`/`subscriptions.provider` are
left completely unconstrained for the same reason - Champion supports
`PLAYSTATION` today, and other platforms can be added with zero migration.

## Tenant isolation

Every SaaS repository method that reads or writes organization-scoped data
takes `organizationID` as an explicit parameter and includes it in the
`WHERE` clause - never `GetByID(id)` alone. See `GetScoped`/`ListByOrganization`
methods across `saas_*_repository.go`. `OrganizationRepository.VerifyMembership`
is the check a website request handler should call before trusting any
organization-scoped request body/query param.

## ON DELETE behavior

All new cascades stay within SaaS metadata, or reuse the existing `guilds`
cascade pattern - **no new cascade reaches kill/death/player history**:

- `organizations` deletion → **cascades** `organization_members`, `discord_guild_connections`, `installations`, `subscriptions` (all pure SaaS metadata).
- `organizations` deletion → **sets NULL** on `game_servers.organization_id`/`nitrado_connections.organization_id` (never deletes the underlying server row or its kill/death history).
- `app_users` deletion → **cascades** `organization_members` (their memberships); **restricted** while they still own an `organizations` row (must transfer ownership first).
- `discord_guild_connections`/`installations` deletion → **cascades** their own child rows (`installation_setup_progress`, `installation_settings`) only.
- `guilds`/`game_servers` deletion (pre-existing behavior, unchanged by this migration) still cascades into kills/players/etc. as it always has - this task does not touch that.

## Validation status

`go build`, `go vet`, and `go test ./...` all pass with this schema
(including a build+vet pass under `-tags integration`). Live migration-chain
validation against a disposable PostgreSQL instance (as
`internal/database/database_integration_test.go` and
`internal/repository/saas_repository_integration_test.go` are written to do)
was **not run** in the environment this schema was authored in - no local
Docker/PostgreSQL and no `TEST_DATABASE_URL` were available. Run it with:

```bash
TEST_DATABASE_URL=postgres://... ALLOW_INTEGRATION_DB_TESTS=true go test -tags integration ./internal/database/... ./internal/repository/...
```

against a disposable database before this lands somewhere migrations run
automatically (Railway will run them via `DB.Migrate` on next deploy either
way, same as every prior migration in this history).

## guild_route_panels (migration 0028)

Which persistent panel message lives in which routed channel, for the
guild-level route-driven artifacts (`LINK_GAMERTAG`, `STATS_LEADERBOARDS`,
`AUTO_LEADERBOARD`). One row per `(guild_id, route_key, channel_id)`, with
`guild_id` referencing `guilds(id)` (`ON DELETE CASCADE`). Keyed by channel so a
route change, a restart, or several servers sharing a channel can never leave two
live copies of a panel. See `docs/SAAS_RUNTIME_ROUTING.md`.

## bounties (migrations 0007 + 0029)

Durable bounties (see `docs/BOUNTY_SYSTEM.md`). `guild_id` references `guilds(id)`;
`target_player_id`, `claimed_by_player_id` reference `players(id)`;
`claimed_kill_id` references `kills(id)`. `reward_points` is the amount in Champion
Points. **`server_id`** (migration 0029, nullable, `ON DELETE CASCADE`) scopes a
bounty to one `game_servers` row; NULL means guild-wide (every row created before
0029). `status` is `ACTIVE | CLAIMED | EXPIRED | CANCELLED`; `created_by_type` is
`ADMIN | AUTOMATIC`. Uniqueness: at most one ACTIVE `AUTOMATIC` bounty per
`(guild_id, target_player_id)` (`uq_active_automatic_bounty`); manual bounties stack.
Indexes: `(guild_id, server_id, status)`, `target_player_id`, `created_at`,
`claimed_at`. Awards are recorded in `point_transactions` with
`reason_type = 'BOUNTY_CLAIM'` and `source_key = 'bounty:<id>'` (unique), which makes
the payout idempotent.

## player_points / point_transactions (migrations 0001 + 0030)

The Champion Points economy (see `docs/ECONOMY_SYSTEM.md`). `player_points` is keyed
`(guild_id, player_id)` and stays guild-wide: `lifetime_points` / `season_points` are
the earn-only leaderboard scores (the season score resets each season) and
**`balance BIGINT NOT NULL DEFAULT 0`** (migration 0030, `CHECK (balance >= 0)`) is
the spendable balance, which a season reset never touches. `point_transactions` is
the append-only ledger: `amount` is a signed **BIGINT** (debits negative),
`balance_after BIGINT` is the balance right after the row, `description` and
`created_by` (the acting Discord user id, or `SYSTEM`) are the audit trail, and
`server_id` (nullable, `ON DELETE SET NULL`) records where it happened.
`UNIQUE (guild_id, player_id, reason_type, source_key)` is the idempotency key
(`BOUNTY_CLAIM`/`bounty:<id>`, `EVENT_*`/`event:<id>:<place>`, ...). A trigger
(`trg_point_transactions_append_only`) refuses any update that would rewrite a row's
facts. Index `(guild_id, player_id, id DESC)` serves history. The 0030 backfill sets
`balance = lifetime_points` and fills legacy `balance_after` running totals.

## installation_embed_templates (migration 0031)

Custom embed templates (Embed Designer Phase 2 - **storage only**: no Discord publisher reads
this table; custom template runtime rendering is not enabled). One row per customized
`(installation, route)`; a route without a row uses the Champion default, and defaults are never
copied into the table.

| Column | Type | Notes |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `installation_id` | `BIGINT NOT NULL` | `REFERENCES installations(id) ON DELETE CASCADE` - a template belongs to exactly one installation (two installations of one guild are independent), and is deleted with it, like `installation_settings` / `installation_channel_routes` |
| `route_key` | `TEXT NOT NULL` | one of the fixed channel-route keys; validated in Go (this schema's convention: no `CHECK` for enumerated values) |
| `config_json` | `JSONB NOT NULL` | the typed, server-validated and normalized template (never a raw client blob), carrying `version: 1`. `CHECK`ed to be a JSON object of at most 64 KiB |
| `created_at` | `TIMESTAMPTZ NOT NULL DEFAULT NOW()` | preserved on update |
| `updated_at` | `TIMESTAMPTZ NOT NULL DEFAULT NOW()` | refreshed by every upsert |

`UNIQUE (installation_id, route_key)` makes concurrent saves converge on a single row (the API
upserts with `INSERT ... ON CONFLICT DO UPDATE`) and doubles as the lookup index. Tenant safety is
in the queries: every read and write joins `installations` on `organization_id`, so an installation
id alone never reaches another organization's row. See `docs/SAAS_API.md` (Embed templates).

## Faction Hub (migration 0032)

The web-first, installation-scoped faction model (`docs/FACTIONS.md`). **Parallel to, and not a change of,** the
Discord-side `factions` / `faction_members` tables (migration 0005), which are keyed by guild and DayZ `player_id`
and referenced by kills, wars, events and seasons. Hub tables carry the `hub_` prefix. Migration `0032` is purely
additive: it creates the six tables below and one unique index on `installations(id, organization_id)` (needed as a
composite-FK target); it alters no existing table. Unlike most status text in this schema, a few columns whose
corruption would break the membership rules also carry a `CHECK`.

| Table | Key columns and constraints |
|---|---|
| `hub_factions` | `organization_id`, `installation_id`, `game_server_id` (`REFERENCES game_servers ON DELETE CASCADE`), `name`, `tag`, `slug`, `description`, `recruitment_status` (`CHECK IN ('OPEN','INVITE_ONLY','CLOSED')`, default `CLOSED`), `logo_key` / `flag_key` / `armband_key` (nullable placeholders, never URLs), `primary_color` / `secondary_color` (`CHECK ~ '^#[0-9A-Fa-f]{6}$'`), `created_by_user_id` (`REFERENCES app_users ON DELETE RESTRICT`), `legacy_faction_id` (unused bridge, `REFERENCES factions ON DELETE SET NULL`). **`FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE CASCADE`**; `UNIQUE (id, installation_id)`; unique indexes on `(installation_id, slug)`, `(installation_id, LOWER(name))`, `(installation_id, LOWER(tag))`; index on `(installation_id, recruitment_status)` |
| `hub_faction_settings` | one row per faction (`faction_id` PK, cascade): `minimum_hours`, `minimum_age` (nullable), `pvp_required`, `builder_needed`, `mic_required`, `custom_requirements`. Display-only requirements |
| `hub_faction_roles` | role catalog: `faction_id` (NULL = built-in system role), `role_key`, `display_name`, `rank`, `is_system`. Seeded with `LEADER`, `OFFICER`, `MEMBER`; unique on `role_key` for system roles and on `(faction_id, role_key)` for custom ones. Room for custom roles later; not exposed |
| `hub_faction_members` | `faction_id`, `installation_id`, `user_id` (`REFERENCES app_users ON DELETE CASCADE`), `player_id` (nullable, `REFERENCES players ON DELETE SET NULL`: the verified DayZ identity), `role_key` (`CHECK IN ('LEADER','OFFICER','MEMBER')`), `joined_at`. **`FOREIGN KEY (faction_id, installation_id)` to the faction**; `UNIQUE (faction_id, user_id)`; **`UNIQUE (installation_id, user_id)` = one active faction per user per installation**; partial unique index `(faction_id) WHERE role_key='LEADER'` = at most one leader |
| `hub_faction_role_memberships` | `member_id`, `role_id`, `granted_by_user_id`; `UNIQUE (member_id, role_id)`. For future custom roles; unused |
| `hub_faction_applications` | `faction_id`, `installation_id`, `user_id`, `status` (`CHECK IN ('PENDING','ACCEPTED','DENIED','WITHDRAWN','CANCELLED')`), `message`, `answers_json` (nullable object, API keeps it disabled), `reviewed_by_user_id`, `reviewed_at`. Composite FK to the faction; **partial unique index `(faction_id, user_id) WHERE status='PENDING'`**; indexes `(faction_id, status, id DESC)` and `(installation_id, user_id, status)`. Rows are never deleted |

Tenant safety is in the schema as well as the queries: a faction cannot name an organization that does not own its
installation, and a member or application cannot sit in a different installation than its faction. Every repository
method scopes by organization + installation (+ faction). See `docs/FACTIONS.md` for the concurrency design (an
advisory lock on `(installation, user)` taken before any row lock).

## Faction logo storage (migration 0033)

Phase 4 of the Faction Hub (`docs/FACTIONS.md`). Champion has no object storage and the bot service filesystem is not durable, so logo
**bytes** live behind the `assetstore.Store` abstraction (production: the PostgreSQL-backed store below) and only **metadata** is in the
faction model. Additive: one new column on `hub_factions` and two new tables; nothing existing changes.

| Object | Notes |
|---|---|
| `hub_factions.logo_asset_id BIGINT NULL` | the current logo. **`FOREIGN KEY (logo_asset_id, id) REFERENCES hub_faction_assets(id, faction_id) ON DELETE SET NULL (logo_asset_id)`** - a faction can only ever point at one of its own assets; the pointer clears if the asset row is removed (column-list `SET NULL` needs PostgreSQL 15+; production is 18). `NULL` = the Champion default logo |
| `hub_faction_assets` | `id`, `public_id UUID UNIQUE` (unguessable id in the public URL), `organization_id`, `installation_id`, `faction_id`, `asset_type` (`CHECK IN ('LOGO')`), `storage_key TEXT UNIQUE` (server-generated; `CHECK`: relative, `[A-Za-z0-9/._-]`, no `..`, <= 200), `content_type` (`CHECK IN ('image/png','image/jpeg','image/webp')`), `size_bytes` (`1..5242880`), `width`, `height`, `original_filename` (sanitized, <= 120, display only), `created_by_user_id` (`ON DELETE SET NULL`), `created_at`, `updated_at`. `FOREIGN KEY (faction_id, installation_id)` to the faction and `(installation_id, organization_id)` to `installations`, both `ON DELETE CASCADE`; `UNIQUE (id, faction_id)`; index `(faction_id, asset_type)`. A replaced or deleted logo has **no row** (its URL stops resolving) |
| `hub_asset_blobs` | the PostgreSQL store's bytes: `storage_key TEXT PRIMARY KEY`, `content_type`, `data BYTEA`, `created_at` (index on `created_at` for the orphan sweep). Deliberately separate from the faction tables so faction reads never touch image data and an S3-compatible store can replace it without touching the metadata model. Bytes with no referencing `hub_faction_assets` row are orphans, removed hourly (only if older than two hours) |

## Faction competitive layer (migration 0034)

Phase 5 of the Faction Hub (`docs/FACTION_STATS.md`). Kills, deaths and bounties are **not copied**: faction figures are derived from `kills`, `deaths`, `bounties`,
`record_events` and `player_links` joined to membership periods. Additive: three new tables, nothing existing changes.

| Table | Notes |
|---|---|
| `hub_faction_membership_history` | one row per membership **period**: `organization_id`, `installation_id`, `faction_id`, `user_id` (`ON DELETE CASCADE`), `player_identity_id` (nullable snapshot of the verified player at join time - informational; attribution uses the live `player_links`), `joined_at`, `left_at` (NULL = still a member; `CHECK left_at >= joined_at`), `created_at`. Composite FKs `(faction_id, installation_id)` -> the faction and `(installation_id, organization_id)` -> `installations`. **`UNIQUE (installation_id, user_id) WHERE left_at IS NULL`** = at most one open period, like the live membership. Written in the same transaction as every join, leave and removal; rows are never deleted. Backfilled from current members with their real `joined_at`. Indexes `(faction_id, joined_at)`, `(user_id, installation_id)` |
| `hub_faction_activity` | public-safe hub events: `event_type` (`CHECK IN` FACTION_CREATED, MEMBER_JOINED, MEMBER_LEFT, MEMBER_PROMOTED, MEMBER_DEMOTED, LEADERSHIP_TRANSFERRED, FACTION_UPDATED, FACTION_LOGO_CHANGED), `subject_user_id` and `actor_user_id` (`ON DELETE SET NULL`), `detail` (`CHECK IN ('LEADER','OFFICER','MEMBER')` - never free text), `occurred_at`. Same composite tenant FKs. Index `(faction_id, occurred_at DESC, id DESC)` serves the keyset feed. Not the audit log |
| `hub_faction_achievement_unlocks` | `achievement_key`, `unlocked_at`, `metadata_json` (nullable object). **`CONSTRAINT uq_hub_achievement_unlock UNIQUE (faction_id, achievement_key)`** makes concurrent evaluation unlock exactly once. Same composite tenant FKs; index `(faction_id, unlocked_at DESC, id DESC)`. The achievement catalog itself lives in code |

No index was added to `kills`, `deaths` or `bounties`: the faction queries are driven by the faction's linked players through the existing `idx_kills_killer`, `idx_kills_victim`,
`idx_deaths_player` indexes (measured with a 300,000-kill guild history: 22 ms).

## Faction leaderboards (Phase 6 - no schema change)

Phase 6 (`docs/FACTION_LEADERBOARDS.md`) adds **no table, column, index or migration** (the latest migration remains `0034_faction_stats_history`). The leaderboard is a
*query result*, not stored data: one grouped statement over `hub_factions`, `hub_faction_membership_history`, `player_links`, `kills`, `deaths`, `bounties` and
`hub_faction_achievement_unlocks` - the same definition as the per-faction stats, with the faction filter switched off - computed per `(organization, installation)`
and cached in memory for 45 s. Everything is scoped by `installation_id` (and its guild + game server); ranking and tie-breaking (ending in `hub_factions.id`) happen in
the service. There is deliberately no materialized leaderboard table: it could only drift from the source rows. Measured with 120 factions and a 300,000-kill history:
746 ms uncached, 47 ms with 20,000 kills, no new index needed.

## Economy web foundation (migration 0035)

The Champion Economy web layer (`docs/ECONOMY.md`) adds **no table, column or index**. An economy "account" is the existing `(guild_id, player_id)` pair: `player_points`
(`balance` BIGINT `CHECK >= 0`, plus earn-only `lifetime_points`/`season_points`) and the append-only ledger `point_transactions` (migration `0030`: signed `amount`, `balance_after`,
`reason_type` = transaction type, `source_key` = idempotency reference, `description`, `created_by`, `server_id`; `UNIQUE (guild_id, player_id, reason_type, source_key)`;
`idx_point_transactions_history (guild_id, player_id, id DESC)` serves the keyset history). The website reaches an account through `installations` -> `discord_guild_connections.guild_id`
(the balance scope) and `installations.game_server_id` (write attribution). Identity is a `VERIFIED` `player_links` row.

Migration `0035_economy_ledger_no_delete` adds one trigger: `point_transactions_no_delete` (`BEFORE DELETE ... FOR EACH ROW`) raises unless it runs inside a foreign-key cascade
(`pg_trigger_depth() > 1`), so a ledger row can no longer be removed by a direct `DELETE`, while deleting a guild or a player still cascades. Together with the `0030` update guard
the ledger is immutable; a correction is a compensating transaction.

## Champion Shop (migration 0036)

Phase 1 of the Shop (`docs/SHOP.md`). Four additive tables; **Champion Points are not stored here** - a purchase's debit and a refund's credit are rows of the existing ledger `point_transactions`
(types `SHOP_PURCHASE` / `SHOP_REFUND`, `source_key = 'purchase:<id>'`), written in the same transaction as the purchase. Everything is tenant-scoped by composite foreign keys
`(installation_id, organization_id) -> installations(id, organization_id) ON DELETE CASCADE`.

| Table | Notes |
|---|---|
| `shop_categories` | `name` (1-60), `slug`, `description`, `sort_order`, `is_active`. `UNIQUE (installation_id, slug)`, `UNIQUE (id, installation_id)`. Never deleted (only disabled) |
| `shop_products` | `category_id` (composite FK `(category_id, installation_id)` -> `shop_categories`), `name` (1-80), `slug` (`UNIQUE (installation_id, slug)`), `description` (<= 1000), `price_points BIGINT CHECK 1..1e9`, `product_type` (`ITEM`, `LOADOUT`, `VEHICLE`, `SERVICE`, `CUSTOM`), `delivery_type` (`MANUAL`, reserved `DISCORD_ROLE`, `IN_GAME_FUTURE`), `image_key` (reserved, unused), `sort_order`, `is_active`, `is_featured`, `stock_mode` (`UNLIMITED`/`FINITE`), `stock_quantity` (`CHECK`: NULL when UNLIMITED, `0..1e9` when FINITE), `purchase_limit` (`1..1000` or NULL). Indexes `(installation_id, is_active, is_featured DESC, sort_order, id)`, `(installation_id, category_id)` |
| `shop_purchases` | `game_server_id` (`ON DELETE SET NULL`), `user_id` (`app_users`, `ON DELETE SET NULL`), `player_id` (`players`, `ON DELETE CASCADE`, like the ledger), `status` (`CHECK IN` PENDING, PAID, PENDING_FULFILLMENT, FULFILLED, CANCELLED, REFUNDED, FAILED), `total_points > 0`, `delivery_type` snapshot, `idempotency_key` (8-64), `paid_at`, `fulfilled_at/_by_user_id`, `cancelled_at`, `refunded_at/_by_user_id`, `refund_reason` (admin-only). **`UNIQUE (installation_id, user_id, idempotency_key)`**. Indexes `(installation_id, id DESC)`, `(installation_id, player_id, id DESC)`, `(installation_id, status, id DESC)` |
| `shop_purchase_items` | immutable snapshot: `purchase_id` (`ON DELETE CASCADE`), `product_id` (`ON DELETE SET NULL`), `product_name`, `unit_price_points`, `quantity` (1..1000), `line_total_points` (`CHECK = unit_price_points * quantity`). Indexes by purchase and by product |

Products are never hard-deleted, so purchase history always keeps its product link and, in any case, its own name/price snapshot. Read-only reconciliation between purchases and the ledger:
`ShopRepository.ReconcileShop` (see `docs/SHOP.md` section 8).

## Champion Billing (migration 0037)

Phase 1 of billing (`docs/BILLING.md`). No new customer/subscription table - the existing `subscriptions` row (one per organization, `0024_saas_foundation`) gained Stripe fields; `provider`/
`provider_customer_id`/`provider_subscription_id` already existed and were always `NULL` before this.

| Column added | Notes |
|---|---|
| `provider_price_id` | The Stripe Price currently on the subscription |
| `billing_interval` | `MONTHLY` / `YEARLY` (free-text, same convention as `plan`/`status` - see "Enum/status convention" below) |
| `current_period_start` | Alongside the existing `current_period_end` |
| `cancel_at_period_end` | `BOOLEAN NOT NULL DEFAULT FALSE` |
| `canceled_at` | Nullable |
| `trial_consumed` | `BOOLEAN NOT NULL DEFAULT FALSE`; set once a Stripe subscription id is ever recorded, never cleared - prevents a repeated Stripe trial grant |

Indexes: `idx_subscriptions_provider_customer` / `idx_subscriptions_provider_subscription` (partial, `WHERE ... IS NOT NULL`) support webhook attribution lookups.

A new table, **`billing_webhook_events`**, is the webhook idempotency log: `provider`, `event_id`, `event_type`, a best-effort nullable `organization_id` (`ON DELETE SET NULL`), `received_at`,
`CONSTRAINT uq_billing_webhook_events UNIQUE (provider, event_id)`. Webhook processing inserts into this table (`ON CONFLICT DO NOTHING`) before any other write; no row means "already processed".

Another new table (Champion Access Model Phase 2, `0038_billing_transactions`), **`billing_transactions`**, is the normalized payment/invoice history: `organization_id`, `provider`,
`provider_invoice_id`, `provider_payment_intent_id`, `provider_subscription_id`, `status` (`PAID`/`FAILED` only - `CHECK` constraint), `amount_cents`, `currency`, `period_start`, `period_end`,
`paid_at`, `failed_at`, `stripe_event_id`, `created_at`/`updated_at`. `CONSTRAINT uq_billing_transactions_event UNIQUE (provider, stripe_event_id)` ties each row 1:1 to the already-deduped
`billing_webhook_events` row for the same event. Populated only from `invoice.paid`/`invoice.payment_failed` webhook events - see `docs/BILLING.md` section 27 for exactly what's captured and why
`plan` is not a column. `idx_billing_transactions_org`, `idx_billing_transactions_status`, `idx_billing_transactions_invoice` support the Owner payments list's filters.

The plan catalog itself (names, prices, features, Stripe price ids) is **not** stored in PostgreSQL at all - it is configuration (`CHAMPION_BILLING_PLANS_JSON`), loaded once at startup by
`internal/billing.LoadCatalog`. See `docs/BILLING.md` for the full model, status mapping and security review.

## Champion Access Model Phase 2: player identity and the `0039_player_api_lookup_index` migration

No new tables - the player-facing API (`docs/PLAYER_API.md`) reads exclusively from tables that
already existed (`player_links`, `players`, `kills`, `deaths`, `bounties`, `player_server_activity`,
`hub_faction_members`). One new index, `idx_player_links_discord_user` on
`player_links(discord_user_id, status)`, supports the one query shape nothing existing served: a
lookup FROM a Discord user id TO their `player_links` row, before a guild id is known (both existing
unique constraints on `player_links` start with `guild_id`). See `docs/PLAYER_API.md` section 7 for
the `EXPLAIN` evidence.

## Client Admin Control Plane Phase 1 (migration 0040)

See `docs/CLIENT_ADMIN.md` for the full design. New tables:

| Table | Notes |
|---|---|
| `installation_role_permissions` | `installation_id` (`ON DELETE CASCADE`), `discord_role_id`, `permission_level` (`CHECK IN` OWNER, ADMINISTRATOR, MODERATOR, GATEKEEPER), `created_by_user_id`. `UNIQUE (installation_id, discord_role_id)` - one Level per role per installation |
| `admin_audit_log` | `organization_id` (`ON DELETE CASCADE`), `installation_id` (nullable, `ON DELETE SET NULL` - an audit row outlives a deleted installation), `actor_user_id`/`actor_discord_id`, `action`, `target`, `reason`, `before_state`/`after_state` (`JSONB`, sanitized snapshots, never secrets), `result`. Indexes `(organization_id, created_at DESC)`, `(installation_id, created_at DESC)`. Rows are never deleted or edited |
| `player_warnings` | `guild_id` (`ON DELETE CASCADE`), `player_id` (`ON DELETE CASCADE`), `reason`, `issued_by_user_id`, `issued_at`, `cleared` (default false), `cleared_by_user_id`, `cleared_at`. A clear sets the three `cleared*` columns; the row itself is never deleted, preserving the audit trail |
| `installation_access_entries` | Champion-side whitelist/ban-list metadata (Nitrado's own API takes only a player `identifier`, no reason/notes/expiry): `installation_id`, `list_type` (`CHECK IN` WHITELIST, BANLIST), `identifier`, `reason`, `notes`, `expires_at`, `created_by_user_id`, `removed_at`/`removed_by_user_id` (soft-remove, preserves history). Partial unique index `(installation_id, list_type, identifier) WHERE removed_at IS NULL` allows re-adding an identifier after a prior removal |

Columns added:

| Table.Column | Notes |
|---|---|
| `installation_channel_routes.show_location` | `BOOLEAN NOT NULL DEFAULT TRUE` - per-route toggle for whether that feed's embeds include location fields |
| `server_configs.maintenance_mode` | `BOOLEAN NOT NULL DEFAULT FALSE` - Champion-side flag only, never touches the Nitrado server |
| `server_configs.autostart_enabled`, `autostart_offline_minutes`, `autostart_cooldown_minutes`, `autostart_last_attempt_at`, `autostart_attempt_count` | Monitor-loop state for a future `SERVER_AUTOSTART` scheduler (`docs/CLIENT_ADMIN.md` "Deferred" - the columns shipped, the monitor loop itself did not) |

## Player Intelligence / Location History (migration 0041)

See `docs/PLAYER_INTELLIGENCE.md` for the full design. New table:

| Table | Notes |
|---|---|
| `player_location_events` | `guild_id`/`server_id` (not `organization_id`/`installation_id` - matches every other bot-native guild-scoped table; the SaaS API resolves org/installation -> guild/server the same way every other Client Admin route does), `player_id` (`ON DELETE CASCADE`), `gamertag`, `x`/`z` (`DOUBLE PRECISION NOT NULL`), `y` (nullable), `event_type` (`CHECK IN` CONNECT, DISCONNECT, HIT, KILL, DEATH, RESPAWN, UNCONSCIOUS, OTHER_ADM), `observed_at`, `source` (default `'ADM'`), `created_at`. **`UNIQUE (player_id, server_id, event_type, observed_at)`** is the durable dedupe backstop against a duplicate ADM replay, inserted with `ON CONFLICT DO NOTHING` - the same pattern `kills`/`deaths` already use for their own fingerprint uniqueness. Indexes `(player_id, observed_at DESC)`, `(server_id, observed_at DESC)`, `(created_at)` (backs the retention sweep's `DELETE ... WHERE created_at < cutoff`). No separate "current location" table - always derived at query time (`ORDER BY observed_at DESC LIMIT 1`), per the task's own "do not store a second conflicting truth unless necessary" |
