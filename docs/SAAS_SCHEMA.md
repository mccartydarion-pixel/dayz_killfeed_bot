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
| `PVE_FEED` | `pvefeed` | Infected/environment/PvE events | OPTIONAL (not implemented yet) |
| `LINK_GAMERTAG` | `link-gamertag` | Player linking / gamertag linking panel | FEATURE_DEPENDENT |
| `STATS_LEADERBOARDS` | `stats-leaderboards` | Manually viewed general statistics and leaderboards | OPTIONAL |
| `AUTO_LEADERBOARD` | `auto-leaderboard` | Automatically refreshed leaderboard panel | OPTIONAL |
| `HITFEED` | `hitfeed` | Hit/damage event feed | OPTIONAL (not implemented yet) |
| `BOUNTY` | `bounty` | Public bounty board/events | OPTIONAL (not implemented yet) |
| `BOUNTY_TRACKING` | `bounty-tracking` | Bounty progression/tracking | OPTIONAL (not implemented yet) |
| `HEATMAPS` | `heatmaps` | Heatmap/activity output | OPTIONAL (not implemented yet) |
| `ECONOMY` | `economy` | Economy/credits information | OPTIONAL (not implemented yet) |
| `CASINO` | `casino` | Casino commands/results | OPTIONAL (not implemented yet) |
| `SHOP` | `shop` | Store/shop output | OPTIONAL (not implemented yet) |
| `CONNECTIONS` | `connections` | Connect/disconnect/player connection events | OPTIONAL (not implemented yet) |
| `BUILD_FEED` | `build-feed` | Building/base-related feed | OPTIONAL (not implemented yet) |
| `ADMIN_ALERTS` | `admin-alerts` | Important moderation/server alerts | OPTIONAL (not implemented yet) |
| `ADMIN_LOGS` | `admin-logs` | Detailed administrative/diagnostic logging | FEATURE_DEPENDENT |

See `docs/SAAS_API.md`'s "Channel routing" section for the full runtime
publisher audit behind each requirement level, and the exact backward-
compatibility backfill migration 0027 performs from `installation_settings`.

Repository: `internal/repository/saas_channel_routes_repository.go`
(`ChannelRouteRepository`) - `ListForInstallation`, `UpsertRoute`,
`DeleteRoute`, `ListDistinctChannelIDs` (Step 6 permission-verification
readiness - several routes sharing one channel only need to be verified
once).

### `subscriptions`
Billing-ready state only - **no billing provider is integrated yet**.
Provider fields stay `NULL` until it is.

| Column | Notes |
|---|---|
| `id` | BIGSERIAL PK |
| `organization_id` | FK `organizations(id)`, **UNIQUE**, `ON DELETE CASCADE` - one subscription per organization |
| `provider`, `provider_customer_id`, `provider_subscription_id` | TEXT, nullable |
| `plan` | TEXT, default `'TRIAL'` |
| `status` | TEXT, one of `TRIAL` \| `ACTIVE` \| `PAST_DUE` \| `CANCELED` \| `SUSPENDED` (Go constants `repository.Subscription*`) |
| `trial_ends_at`, `current_period_end` | TIMESTAMPTZ, nullable |
| `created_at`, `updated_at` | TIMESTAMPTZ |

Repository: `SubscriptionRepository` (`saas_subscriptions_repository.go`) - `EnsureTrial` (idempotent), `GetForOrganization`.

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
