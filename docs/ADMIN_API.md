# Champion platform-admin (founder) API - Phase 1, read-only

The one HTTP surface that reads **across organizations**. It exists so the
website's `/admin` pages (Overview, Customers, Subscriptions, Installations,
Health) can show real data. It is **read-only**: only `GET` routes exist, so any
other method is answered `405` by the router. Mutations (plan changes, trial
extensions, suspensions, deletions, impersonation, billing) are Phase 2 and will
require audit logging first.

Machine-readable contract: [`admin-openapi.yaml`](admin-openapi.yaml). The
customer API (`/api/saas/...`, [`SAAS_HTTP_API.md`](SAAS_HTTP_API.md)) is unchanged
and stays strictly tenant-scoped.

## Authentication and authorization

Every `/api/admin/*` request must pass **both** checks, in this order, in one place
(`requirePlatformAdmin`, `internal/app/admin_api.go`; every route is registered
through `adminRoute`, which calls it, so a handler cannot run without it):

| # | Check | Failure |
|---|---|---|
| 1 | `Authorization: Bearer <secret>` equals the bot's `WEBSITE_API_SECRET` (the same internal service secret every `/api/saas` route uses; constant-time compare). No secret configured = nothing authenticates. | `401 UNAUTHORIZED` |
| 2 | `X-Champion-Acting-User: <discord user id>` present (set by the website from its verified session; browsers can never reach these routes because the secret is server-side only) | `401 UNAUTHORIZED` |
| 3 | that Discord user id is on the **platform-admin allowlist** | `403 FORBIDDEN` |

### The platform-admin allowlist

`CHAMPION_ADMIN_DISCORD_IDS` on the bot service: a comma-separated list of Discord
user ids, e.g. `CHAMPION_ADMIN_DISCORD_IDS=111111111111111111,222222222222222222`.

* This is **explicit and separate** from every existing role. An organization
  `OWNER`/`ADMIN`/`MEMBER`, a Discord guild administrator or a `Manage Server`
  permission does **not** make anyone a platform admin - none of them is consulted.
* **Fails closed:** unset or empty = nobody is an admin (every request is `403`).
  Entries that are not plain numeric Discord ids (typos, names, `*`) are dropped, so
  a mistake can never widen access.
* The allowlist is checked against the header id directly; the founder does not need
  a `POST /api/saas/users/sync` row. Being on the allowlist grants **no** access to
  `/api/saas` (customer routes still require organization membership).
* Rotate by changing the env var and redeploying.

Existing audit of what was (not) reusable: `internal/admin` is the runtime
diagnostics service behind the Discord `/admin` command, which authorizes by Discord
guild permission (per-guild, not platform-wide); `app_users`/`organizations`/
`organization_members` model *customers*. Neither identifies a founder, hence the
allowlist.

## Conventions

* Errors use the customer API's envelope: `{"error":{"code","message"}}` with codes
  `UNAUTHORIZED` (401), `FORBIDDEN` (403), `NOT_FOUND` (404), `INVALID_REQUEST` (400),
  `INTERNAL_ERROR` (500). Messages are fixed strings; database errors are logged,
  never returned.
* Responses are `application/json` with `Cache-Control: no-store`.
* Timestamps are RFC 3339 UTC strings; absent values are `null`.
* IDs are the database ids (integers), except Discord ids, which are strings.

### Pagination (all list endpoints)

Keyset, newest first (`id DESC`), bounded:

| Param | Meaning |
|---|---|
| `limit` | default **50**, maximum **100** (larger values are clamped to 100). `0`, negative or non-numeric = `400` - there is no "unlimited". |
| `cursor` | opaque; pass the previous response's `nextCursor`. Invalid = `400`. |

```json
{ "items": [ ... ], "nextCursor": "YWRtMTo0Mg" , "limit": 50 }
```

`nextCursor` is `null` on the last page. Pages never duplicate or skip a row, even
while rows are being added (a new row is newer than every cursor and appears on
the first page instead).

### Search

`search=<text>` (max 100 characters, else `400`) is a case-insensitive substring
match, always sent to the database as a **bound parameter** (never spliced into
SQL); `%`, `_` and `\` in the text are literal characters.

| Endpoint | Searched fields |
|---|---|
| organizations | organization name, slug, Discord guild name, DayZ server name (any of its installations) |
| subscriptions | organization name, slug |
| installations | organization name, slug, Discord guild name, DayZ server name |

### Status vocabulary (real values only)

The backend's own status values are reported - nothing is renamed or invented. Note
that this differs from some frontend wording:

| Backend value | Where | Not to be confused with |
|---|---|---|
| Installation `status`: `NOT_STARTED`, `DISCORD_CONNECTED`, `NITRADO_CONNECTED`, `CONFIGURING`, `READY`, `DEGRADED`, `DISCONNECTED`, `SUSPENDED` | installations | there is no `OFFLINE` *status*: use `DISCONNECTED` |
| Installation `health` (derived from `status`, identical to the customer API): `HEALTHY` (READY), `DEGRADED` (DEGRADED), `OFFLINE` (DISCONNECTED, SUSPENDED), `SETTING_UP` (all others) | installations, organizations' primary installation | `OFFLINE` here is a *health* value |
| Subscription `status`: `TRIAL`, `ACTIVE`, `PAST_DUE`, `CANCELED`, `SUSPENDED` | subscriptions | there is no `trialing` or `expired` status (see `trialExpired` below) |

Filters validate against these lists (case-insensitive) and reject anything else with
`400`.

## Endpoints

### `GET /api/admin/overview`

Authoritative counts straight from the tables.

```json
{
  "organizations": 12,
  "users": 31,
  "installations": {
    "total": 14, "notStarted": 1, "discordConnected": 0, "nitradoConnected": 1,
    "configuring": 2, "ready": 8, "degraded": 1, "disconnected": 1, "suspended": 0, "other": 0
  },
  "subscriptions": {
    "total": 12, "trial": 9, "active": 2, "pastDue": 0, "canceled": 0, "suspended": 1,
    "other": 0, "trialExpired": 3
  }
}
```

`other` counts any value outside the documented vocabulary (should stay 0).
`trialExpired` is **derived**: `TRIAL` subscriptions whose `trial_ends_at` has passed.
Organizations without a subscription row are not counted in `subscriptions`.

### `GET /api/admin/organizations`

Params: `limit`, `cursor`, `search`, `plan`, `subscriptionStatus`, `installationStatus`
(an organization matches when *any* of its installations has that status).

```json
{ "items": [ {
  "organization": { "id": 7, "name": "Alpha", "slug": "alpha", "createdAt": "..." },
  "owner": { "userId": 3, "displayName": "Alice", "discordId": "111..." },
  "memberCount": 3,
  "subscription": { "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "...", "entitlements": ["killfeed", "..."] },
  "installationCount": 1,
  "primaryInstallation": { "id": 9, "status": "READY", "health": "HEALTHY",
      "guildName": "Alpha Guild", "dayzServerName": "Alpha Server", "platform": "PLAYSTATION",
      "lastHealthCheckAt": null }
} ], "nextCursor": null, "limit": 50 }
```

`subscription` is `null` for an organization with no subscription row;
`primaryInstallation` is `null` when it has no installation. The primary installation
is the organization's `READY` one if any, otherwise its newest. Everything is one
statement (joins, `LATERAL`, counts) - no per-row queries.

### `GET /api/admin/organizations/{organizationID}`

```json
{ "organization": {...}, "owner": {...}, "memberCount": 3,
  "members": [ { "id": 3, "displayName": "Alice", "discordId": "111...", "role": "OWNER", "joinedAt": "..." } ],
  "subscription": {...}, "installationCount": 1,
  "installations": [ <installation row, see below> ] }
```

Members are ordered OWNER, ADMIN, then the rest; children are capped at 200 rows.
`404 NOT_FOUND` for an unknown id, `400` for a non-positive/non-numeric one.

### `GET /api/admin/subscriptions`

Params: `limit`, `cursor`, `search`, `status`, `plan`.

```json
{ "items": [ { "id": 5, "organizationId": 7, "organizationName": "Alpha",
  "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "...", "currentPeriodEnd": null,
  "entitlements": ["killfeed", "..."], "installationCount": 1,
  "createdAt": "...", "updatedAt": "..." } ], "nextCursor": null, "limit": 50 }
```

Only stored data. Billing is not integrated: no invoices, prices, cards or renewal
amounts exist, and the provider columns (`provider`, `provider_customer_id`,
`provider_subscription_id`) are **never** returned. `entitlements` is the plan's
resolved feature keys (today every plan resolves to the full set - see
`internal/entitlements`).

### `GET /api/admin/installations`

Params: `limit`, `cursor`, `search`, `status`, `health`, `organizationId`.

```json
{ "items": [ {
  "installationId": 9,
  "organization": { "id": 7, "name": "Alpha" },
  "plan": "TRIAL", "status": "READY", "health": "HEALTHY",
  "discord": { "guildId": "123...", "guildName": "Alpha Guild", "botInstalled": true, "permissionsVerified": true },
  "dayzServer": { "id": 4, "serviceId": "1234567", "displayName": "Alpha Server", "platform": "PLAYSTATION", "status": "ACTIVE", "active": true },
  "setup": { "currentStep": "CHANNELS", "completedAt": null },
  "lastHealthCheckAt": null, "createdAt": "..."
} ], "nextCursor": null, "limit": 50 }
```

`plan` is the installation's own plan when set, otherwise its organization's
subscription plan. `dayzServer` is `null` until a server is selected.
`discord.guildId` is the Discord guild snowflake.

### `GET /api/admin/installations/{installationID}`

```json
{ "organization": { "id": 7, "name": "Alpha", "slug": "alpha", "createdAt": "..." },
  "subscription": { "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "...", "entitlements": [...] },
  "installation": { "id": 9, "plan": "TRIAL", "status": "READY", "health": "HEALTHY",
      "setupCompletedAt": null, "lastHealthCheckAt": null, "createdAt": "...", "updatedAt": "..." },
  "discord": { ... }, "dayzServer": { ... } ,
  "setupProgress": { "currentStep": "CHANNELS", "discordCompleted": true, "nitradoCompleted": false,
      "serverSelected": false, "channelsCompleted": false, "validationCompleted": false, "completedAt": null },
  "generalSettings": { "timezone": "UTC", "distanceUnit": "METERS", "onlineDisplayEnabled": true, "leaderboardEnabled": true },
  "channelRoutes": [ { "routeKey": "KILLFEED", "channelId": "123...", "channelName": "killfeed", "managedByChampion": true } ],
  "nitradoConnection": { "connected": true, "status": "ACTIVE", "lastValidatedAt": "...", "lastSuccessAt": null, "lastFailureAt": null } }
```

* `channelRoutes` are read by this installation's id only - never another
  installation's. `channelName` is **not stored** (the table holds the channel id);
  it is filled from the bot's in-memory Discord cache when the bot already knows the
  channel, and **omitted** otherwise - never fetched live, never invented.
* `setupProgress`, `generalSettings`, `nitradoConnection` are `null` when the row does
  not exist. `nitradoConnection` is *status only* (the organization's Nitrado link);
  the credential is never read.

### `GET /api/admin/health`

Stored and in-memory state only: no live Nitrado call, no per-row Discord call, no
invented uptime percentages.

```json
{ "generatedAt": "...",
  "backend": { "overall": "HEALTHY", "uptimeSeconds": 86400,
      "components": [ { "name": "database", "state": "HEALTHY", "critical": true, "consecutiveFailures": 0, "lastSuccessAt": "...", "lastFailureAt": null } ],
      "workers": [ { "name": "adm-...", "state": "HEALTHY", "running": true, "startedAt": "...", "lastHeartbeatAt": "...", "lastSuccessAt": null, "lastErrorAt": null } ],
      "workerTotal": 3 },
  "database": { "ok": true },
  "discord": { "configured": true, "ready": true },
  "installations": {
      "total": 14,
      "byStatus": { "READY": 8 }, "byHealth": { "HEALTHY": 8 },
      "serversByStatus": { "ACTIVE": 12 },
      "botNotInstalled": 0, "permissionsUnverified": 2, "readyNeverChecked": 1,
      "needsAttention": [ <installation row> ] } }
```

`needsAttention` is at most 25 installations whose status is `DEGRADED`,
`DISCONNECTED` or `SUSPENDED` (never-checked first). Free-text error messages from
components and workers are intentionally not exposed. `backend.overall`/`state` use
the runtime's `HEALTHY | DEGRADED | UNHEALTHY | UNKNOWN`.

## Fields that are never returned

By construction (every query names its columns; the response types have no such
fields - asserted by tests that walk every DTO) and by a backstop (a response
containing any configured secret is withheld with a `500` instead of being sent):

* Nitrado access tokens, the encrypted credential blob, its nonce and key version
* the Discord bot token, OAuth secrets
* `WEBSITE_API_SECRET` / `CHAMPION_SAAS_API_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`,
  `DATABASE_URL` and database passwords, Railway credentials
* billing-provider identifiers (`provider_customer_id`, `provider_subscription_id`)
* free-text runtime error strings

## Access logging

Each admin read logs one line (`component=admin_api event=admin_read`) with
`acting_admin_discord_id`, `route` (the route pattern, not the URL with its query),
and `organization_id` / `installation_id` when the path has them. A rejected
platform-admin check logs `event=admin_denied` with the presented id and the route.
Never logged: the `Authorization` header or any token, query strings (search text),
request or response bodies.

## Efficiency

List endpoints are single joined/aggregated statements (owner, subscription, counts
and the primary installation are part of the same query); detail endpoints run a
fixed handful of statements regardless of data size. Search is a bounded
`ILIKE` scan over the (small) SaaS tables - add `pg_trgm` indexes if the customer
count ever makes that measurable.

## Configuration checklist

1. Bot service: `WEBSITE_API_SECRET` (already set for the runtime/SaaS APIs) and
   `CHAMPION_ADMIN_DISCORD_IDS=<founder discord ids>`.
2. Website: keep calling with the same secret and `X-Champion-Acting-User` = the
   verified session's Discord id (already how the customer API is called); only send
   these requests from server-side code, and only when `session.user.isAdmin`.
