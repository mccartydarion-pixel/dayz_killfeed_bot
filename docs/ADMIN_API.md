# Champion platform-admin (founder) API

The one HTTP surface that reads **across organizations**. It exists so the
Owner Hub website (`/admin`: Overview, Customers, Subscriptions, Installations,
Health) can show real data. Phase 1 was read-only; Phase 2 adds the **Owner
controls** below - the only `POST` routes under `/api/admin`, each requiring a
reason and written to `platform_audit_log` before the response is sent.
Credential access remains out of scope. The operations pages, the fleet monitor, broadcasts and
the read-only "view as customer" session are documented in [`OWNER_OPS.md`](OWNER_OPS.md).

Machine-readable contract: [`admin-openapi.yaml`](admin-openapi.yaml). The customer
API (`/api/saas/...`, [`SAAS_HTTP_API.md`](SAAS_HTTP_API.md)) is unchanged and stays
strictly tenant-scoped.

## Authentication and authorization

Every `/api/admin/*` request must pass these checks, in this order, in one place
(`requirePlatformAdmin` and `adminRoute`, `internal/app/admin_api.go`; every route is
registered through `adminHandle`, which wraps it in `adminRoute`, so a handler cannot run
without them):

| # | Check | Failure |
|---|---|---|
| 1 | `Authorization: Bearer <secret>` equals the bot's `WEBSITE_API_SECRET` (the same internal service secret every `/api/saas` route uses; constant-time compare). No secret configured = nothing authenticates. | `401 UNAUTHORIZED` |
| 2 | `X-Champion-Acting-User: <discord user id>` present (the website sets it from its verified session; browsers can never reach these routes because the secret is server-side only) | `401 UNAUTHORIZED` |
| 3 | that Discord user id is a **platform owner** (on the allowlist) or **platform staff** (on the staff list) | `403 FORBIDDEN`, message `platform admin access required` |
| 4 | the request is a read (`GET`), or the caller is a platform owner | `403 FORBIDDEN`, message `platform staff can view but not change this` |

## Roles

There are two kinds of privileged person, and only two.

| Role | Who | May do |
|---|---|---|
| `OWNER` | A Discord account on `CHAMPION_ADMIN_DISCORD_IDS`. This env var is the only source of owners. | Everything under `/api/admin`. |
| `STAFF` | A Discord account on the staff list (table `platform_staff`, migration `0125_platform_staff`), which an owner manages from the Owner Hub. | Every read under `/api/admin`. No write. |

* **View only is enforced in one place.** `adminRoute` lets a staff identity through for
  `GET` (and the `HEAD` the router answers for a `GET` route) and refuses every other
  method before the handler runs. A handler never has to remember the rule, and a write
  route added later is owner-only from the moment it is registered. No `GET` route under
  `/api/admin` changes anything or returns a secret.
* **Proved by a test that walks the routes.** `adminHandle` records every pattern it
  registers; `TestEveryAdminWriteRouteRefusesPlatformStaff` sends each one as staff and
  requires `403` with the message above for every non-`GET` route, and
  `TestAdminRoutesAreRegisteredOnlyThroughAdminHandle` fails if any `/api/admin` route is
  registered another way.
* **An owner never depends on the staff list.** The allowlist is checked first and does
  not touch the database. Being on both lists is still `OWNER`. If the staff list cannot
  be read, owners keep working and everybody else is refused.
* **Staff get nothing else.** Being staff grants no access to `/api/saas`, cannot start a
  "view as customer" session (that is a `POST`), and unlocks nothing on any
  organization. Staff access ends on the first request after removal.

### `GET /api/admin/me`

Who the caller is to the platform. Owner and staff: `200`

```json
{ "role": "OWNER", "discordId": "111111111111111111" }
```

`role` is `OWNER` or `STAFF`. Anyone else: `403 FORBIDDEN`.

### The staff list

| Route | Who | Body | Returns |
|---|---|---|---|
| `GET /api/admin/staff` | owner, staff | - | the list |
| `POST /api/admin/staff` | owner | `discordId`, optional `note`, `reason` | `200` with the list |
| `DELETE /api/admin/staff/{discordId}` | owner | `reason` | `200` with the list |

Every one of them answers with the whole list in the same shape, oldest first:

```json
{ "staff": [ { "discordId": "222222222222222222", "note": "Support", "addedBy": "111111111111111111", "addedAt": "2026-10-04T09:30:00Z" } ] }
```

* `discordId` must be a Discord user id: digits only, 15 to 20 long. Otherwise `400`.
* `note` is optional, at most 120 characters. Longer: `400`.
* `reason` is required and validated like every other owner action (max 500 characters).
* Adding someone who is already staff updates the note and keeps `addedBy` and `addedAt`.
* Adding a platform owner's id is refused with `409 CONFLICT` and the message
  `this account is a platform owner and already has full access; it cannot be added as staff`.
* Removing an id that is not on the list is `404 NOT_FOUND`.
* Each change writes one `platform_audit_log` row with the owner's reason: action
  `staff.added`, `staff.note_updated` or `staff.removed`, `target_type` `platform_staff`,
  and the member (`discordId`, `note`, `addedBy`, `addedAt`) in the before/after state.
  `target_id` is left empty because a Discord id does not fit a JSON number safely; filter
  the audit log with `targetType=platform_staff`.

The website also sends `X-Champion-Admin: true`. The backend **ignores** it - a header
a caller controls can never grant access; only the allowlist does.

### The platform-owner allowlist

`CHAMPION_ADMIN_DISCORD_IDS` on the bot service: a comma-separated list of Discord
user ids, e.g. `CHAMPION_ADMIN_DISCORD_IDS=111111111111111111,222222222222222222`.
(The website's own `session.user.isAdmin` should be driven by the same list; the
backend re-checks it independently.)

* **Explicit and separate** from every existing role. An organization
  `OWNER`/`ADMIN`/`MEMBER`, a Discord guild administrator or a `Manage Server`
  permission does **not** make anyone a platform admin - none is consulted.
* **Fails closed:** unset or empty = nobody is an admin (every request is `403`).
  Entries that are not plain numeric Discord ids (typos, names, `*`) are dropped, so a
  mistake can never widen access.
* Checked against the header id directly; the founder does not need a
  `POST /api/saas/users/sync` row. Being on the allowlist grants **no** access to
  `/api/saas` (customer routes still require organization membership).
* Rotate by changing the env var and redeploying.
* Being on the allowlist also gives **platform owner access** to the organizations that
  account owns: see "Platform owner access" below.

Audit of what was (not) reusable: `internal/admin` is the runtime diagnostics service
behind the Discord `/admin` command, which authorizes by Discord guild permission
(per guild, not platform-wide); `app_users`/`organizations`/`organization_members`
model *customers*. Neither identifies a founder, hence the allowlist.

## Conventions

* Errors use the customer API's envelope: `{"error":{"code","message"}}` with codes
  `UNAUTHORIZED` (401), `FORBIDDEN` (403), `NOT_FOUND` (404), `INVALID_REQUEST` (400),
  `INTERNAL_ERROR` (500). Messages are fixed strings; database errors are logged,
  never returned.
* `application/json`, `Cache-Control: no-store`. Timestamps are RFC 3339 UTC strings;
  absent values are `null`. Ids are integers, except Discord snowflakes and member
  ids, which are strings.
* **Website-first shapes.** The base shape of every response is the one the Founder
  Hub already consumes (`lib/admin/types.ts` in the website repo): flat
  `discordGuild` / `dayzServer` *names*, `owner` as a display name, `installations`
  embedded in organizations, etc. Richer authoritative detail is added under
  **additional keys** (`ownerUser`, `discord`, `server`, `generalSettings`, ...), so
  the website needs no transformation and no per-row follow-up requests. A contract
  test mirrors the website's types (required keys and kinds) against both fakes and
  real PostgreSQL rows.

### Pagination (all list endpoints)

Keyset, newest first (`id DESC`), bounded:

| Param | Meaning |
|---|---|
| `limit` | default **50**, maximum **100** (larger values are clamped to 100). `0`, negative or non-numeric = `400` - there is no "unlimited". |
| `cursor` | opaque; pass the previous response's `nextCursor`. Invalid = `400`. |

```json
{ "items": [ ... ], "nextCursor": "YWRtMTo0Mg", "limit": 50 }
```

`nextCursor` is `null` on the last page (an empty result is `items: []`, `nextCursor: null`).
Pages never duplicate or skip a row, even while rows are added (a new row is newer
than every cursor and appears on the first page instead).

### Search

`search=<text>` (max 100 characters, else `400`) is a case-insensitive substring
match, always sent to the database as a **bound parameter** (never spliced into SQL);
`%`, `_` and `\` in the text are literal characters.

| Endpoint | Searched fields |
|---|---|
| organizations | organization name, slug, Discord guild name, DayZ server name (any of its installations) |
| subscriptions | organization name, slug |
| installations | organization name, slug, Discord guild name, DayZ server name |

### Status vocabulary (real values only)

| Backend value | Where | Not to be confused with |
|---|---|---|
| Installation `status`: `NOT_STARTED`, `DISCORD_CONNECTED`, `NITRADO_CONNECTED`, `CONFIGURING`, `READY`, `DEGRADED`, `DISCONNECTED`, `SUSPENDED` | installations | there is no `OFFLINE` *status*; `DISCONNECTED` is the real one |
| Installation `health` (derived from `status`, identical to the customer API): `HEALTHY` (READY), `DEGRADED` (DEGRADED), `OFFLINE` (DISCONNECTED, SUSPENDED), `SETTING_UP` (all others) | installations | `OFFLINE` here is a *health* value |
| Subscription `status`: `TRIAL`, `ACTIVE`, `PAST_DUE`, `CANCELED`, `SUSPENDED` | subscriptions | there is no `trialing`/`expired` status (see `trialExpired`) |

Filters are case-insensitive. A value **outside** these lists cannot match any row, so
it returns a well-formed **empty page** (200, and no database query) - not a 400.
That keeps a typo in the website's free-text filter boxes from turning a whole
page into "temporarily unavailable". A malformed *identifier* (`organizationId`,
path ids, `limit`, `cursor`) is still a `400`.

## Endpoints

### `GET /api/admin/overview`

Authoritative counts straight from the tables; the website's flat fields first, then
the structured breakdown.

```json
{
  "totalOrganizations": 12, "totalUsers": 31,
  "activeInstallations": 9, "readyInstallations": 8, "configuringInstallations": 2, "degradedInstallations": 1,
  "trials": 9, "activeSubscriptions": 2, "suspendedSubscriptions": 1,
  "backendStatus": "HEALTHY",

  "organizations": 12, "users": 31,
  "installations": { "total": 14, "notStarted": 1, "discordConnected": 0, "nitradoConnected": 1,
      "configuring": 2, "ready": 8, "degraded": 1, "disconnected": 1, "suspended": 0, "other": 0 },
  "subscriptions": { "total": 12, "trial": 9, "active": 2, "pastDue": 0, "canceled": 0, "suspended": 1,
      "other": 0, "trialExpired": 3 }
}
```

* `activeInstallations` = `READY` + `DEGRADED` (set up and not disconnected/suspended).
  There is no stored "active" status, so this definition is documented, not invented.
* `trials` = subscriptions with status `TRIAL`; `trialExpired` (structured block) is
  derived: `TRIAL` whose `trial_ends_at` has passed.
* `backendStatus` = the runtime health registry's overall state (`HEALTHY`,
  `DEGRADED`, `UNHEALTHY`), or `UNKNOWN` if no registry is available.
* `other` counts values outside the documented vocabulary (should stay 0).
  Organizations without a subscription row are not counted in `subscriptions`.
* The request's wording `offline` / `trialing` / `expired` are not backend statuses and
  are therefore not keys.

### `GET /api/admin/organizations`

Params: `limit`, `cursor`, `search`, `plan`, `subscriptionStatus`, `installationStatus`
(an organization matches when *any* of its installations has that status). The
website's customers filter sends `status`; on this endpoint it is an alias for
`installationStatus` (an explicit `installationStatus` wins).

```json
{ "items": [ {
  "id": 7, "name": "Alpha", "slug": "alpha", "createdAt": "...",
  "owner": "Alice",
  "ownerUser": { "id": 3, "displayName": "Alice", "discordId": "111..." },
  "memberCount": 3, "installationCount": 1,
  "discordGuild": "Alpha Guild", "dayzServer": "Alpha Server",
  "subscription": { "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "...", "currentPeriodEnd": null,
      "entitlements": ["killfeed", "..."], "createdAt": "...", "updatedAt": "..." },
  "installations": [ <installation summary> ]
} ], "nextCursor": null, "limit": 50 }
```

* In a **list**, `installations` holds only the organization's *primary* installation
  (its `READY` one if any, otherwise its newest), first and only; `installationCount`
  is the total. `discordGuild`/`dayzServer` are that primary installation's names.
* `subscription` is `null` with no subscription row. `owner` is the owner's display
  name (the website's contract); `ownerUser` is the full reference.
* One page = two statements (organizations, then the page's primary installations
  in one query) regardless of page size.

### `GET /api/admin/organizations/{organizationID}`

The same object as a list row, plus `members`, and `installations` holding **all**
of the organization's installations (primary first, at most 200).

```json
{ "id": 7, "name": "Alpha", "...": "...",
  "members": [ { "id": "3", "displayName": "Alice", "discordId": "111...", "role": "OWNER", "joinedAt": "..." } ],
  "installations": [ <installation summary>, ... ],
  "recentPayments": [ { "id": 41, "organizationId": 7, "organizationName": "Alpha", "status": "PAID",
      "amountCents": 1999, "currency": "usd", "periodStart": "...", "periodEnd": "...", "paidAt": "...",
      "failedAt": null, "invoiceReference": "in_...", "createdAt": "..." } ] }
```

Members are ordered OWNER, ADMIN, then the rest (at most 200); `id` is the user id as
a string. `404 NOT_FOUND` for an unknown id, `400` for a non-positive/non-numeric one.
`recentPayments` (Champion Access Model Phase 2) is this organization's newest 10
`billing_transactions` rows, newest first, `[]` if none - see `docs/BILLING.md` section 27.

### `GET /api/admin/subscriptions`

Params: `limit`, `cursor`, `search`, `status`, `plan`.

```json
{ "items": [ { "id": 5, "organization": "Alpha", "organizationId": 7, "organizationName": "Alpha",
  "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "...", "currentPeriodEnd": null,
  "entitlements": ["killfeed", "..."], "installationCount": 1,
  "createdAt": "...", "updatedAt": "..." } ], "nextCursor": null, "limit": 50 }
```

Only stored data. Stripe billing IS integrated (`docs/BILLING.md`) and `billingInterval`/
`cancelAtPeriodEnd` are populated once a subscription has reconciled, but invoice/payment
history is a **separate** surface (`GET /api/admin/billing/payments` below) - the provider
columns (`provider`, `provider_customer_id`, `provider_subscription_id`) are still **never**
returned here or anywhere else. `entitlements` is the plan's resolved feature keys (today
every plan resolves to the full set - see `internal/entitlements`). `organization` and
`organizationName` are the same value (the website reads `organization`).

### `GET /api/admin/installations`

Params: `limit`, `cursor`, `search`, `status`, `health`, `organizationId`.

```json
{ "items": [ {
  "id": 9, "installationId": 9, "organizationId": 7, "organization": "Alpha",
  "discordGuild": "Alpha Guild", "dayzServer": "Alpha Server", "platform": "PLAYSTATION",
  "plan": "TRIAL", "status": "READY", "health": "HEALTHY",
  "currentSetupStep": "CHANNELS", "setupCompletedAt": null, "lastHealthCheckAt": null, "createdAt": "...",
  "discord": { "guildId": "123...", "guildName": "Alpha Guild", "botInstalled": true, "permissionsVerified": true },
  "server": { "id": 4, "serviceId": "1234567", "displayName": "Alpha Server", "platform": "PLAYSTATION", "status": "ACTIVE", "active": true }
} ], "nextCursor": null, "limit": 50 }
```

`organization` is the organization *name* (website contract). The structured
`discord` / `server` objects carry the ids and flags; `server` and the flat
`dayzServer` / `platform` are `null` until a server is selected; `discordGuild` is
`null` for an unnamed guild. `plan` is the installation's own plan when set,
otherwise its organization's subscription plan.

### `GET /api/admin/installations/{installationID}`

The installation summary above (flat, including `organization`, `organizationId`)
plus:

```json
{ "organizationSlug": "alpha", "updatedAt": "...",
  "subscription": { "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "...", "entitlements": [...], "createdAt": "...", "updatedAt": "..." },
  "setupProgress": { "currentStep": "CHANNELS", "discordCompleted": true, "nitradoCompleted": false,
      "serverSelected": false, "channelsCompleted": false, "validationCompleted": false, "completedAt": null },
  "settings": { "timezone": "UTC", "distanceUnit": "METERS", "onlineDisplayEnabled": true, "leaderboardEnabled": true },
  "generalSettings": { "...": "same object as settings" },
  "channelRoutes": [ { "routeKey": "KILLFEED", "channelId": "123...", "channelName": "killfeed", "managedByChampion": true } ],
  "nitradoConnection": { "connected": true, "status": "ACTIVE", "lastValidatedAt": "...", "lastSuccessAt": null, "lastFailureAt": null } }
```

* `settings` is the website's name and `generalSettings` the requested one; both carry
  the same object (`null` if the settings row does not exist, as do `setupProgress` and
  `nitradoConnection`).
* `channelRoutes` are read by this installation's id only - never another
  installation's. **`channelName` is not stored** (the table holds the channel id); it
  is filled from the bot's in-memory Discord cache when the bot already knows the
  channel and is **`null`** otherwise (the website then shows the id) - never
  fetched live, never invented. This is a documented limitation of the data model.
* `nitradoConnection` is *status only* (the organization's Nitrado link); the credential
  is never read.

### `GET /api/admin/billing/plans` (Champion Access Model Phase 2)

The full plan catalog - public **and** private plans, unlike the customer-facing
`GET /api/saas/billing/plans` (which filters to `isPublic`). Still never a Stripe price
id (`docs/BILLING.md` section 25's own values are enough for an Owner Hub display).

```json
{ "items": [ { "key": "PRO", "name": "Pro", "description": "...", "features": ["killfeed", "..."],
  "limits": { "installations": 5 }, "monthly": { "amountCents": 1999, "currency": "usd" },
  "yearly": null, "isPublic": true, "popular": true, "trialDays": 0, "sortOrder": 2 } ],
  "nextCursor": null, "limit": 3 }
```

### `GET /api/admin/billing/payments` (Champion Access Model Phase 2)

Params: `limit`, `cursor`, `organizationId`, `status` (`PAID`/`FAILED` only - an unrecognized
status yields an empty page, not an error, the same convention `status`/`plan` already use on
`GET /api/admin/subscriptions`). Newest first.

```json
{ "items": [ { "id": 41, "organizationId": 7, "organizationName": "Alpha", "status": "PAID",
  "amountCents": 1999, "currency": "usd", "periodStart": "...", "periodEnd": "...",
  "paidAt": "...", "failedAt": null, "invoiceReference": "in_...", "createdAt": "..." } ],
  "nextCursor": null, "limit": 50 }
```

Sourced entirely from `billing_transactions` (`docs/BILLING.md` section 27) - never a live
Stripe call. No `plan` filter (not captured per-transaction) and no date-range filter (cursor
pagination is already a stable ordering).

### `GET /api/admin/live-sync`

Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md section 7.5). For every running server worker:
per-family watcher freshness (`FRESH` / `LAGGING` / `FAILING` / `NO_SOURCE`), canonical source file,
checkpoint, read vs listed size, last read / growth / failure, safe error class, whether the file
was given up (`gaveUp`, `gaveUpAt`, `gaveUpReason`, `nextRetryAt`), record counts
(live / backfill / unknown), rotations and a measured latency summary; the learned UTC offset;
session-end evidence; the recorded ADM session; stored-record statistics and ADM latency for the
last 6 hours. In-memory and stored state only - no live Nitrado call, no token, signed URL,
physical path or raw log line.

### `GET /api/admin/health`

Stored and in-memory state only: no live Nitrado call, no per-row Discord call, no
invented uptime percentages.

```json
{ "backendStatus": "HEALTHY",
  "installations": [ { "id": 9, "organizationId": 7, "organization": "Alpha", "status": "DEGRADED", "health": "DEGRADED",
      "discordBotInstalled": true, "permissionsVerified": true, "serverStatus": "ACTIVE", "lastHealthCheckAt": null } ],
  "generatedAt": "...",
  "backend": { "overall": "HEALTHY", "uptimeSeconds": 86400,
      "components": [ { "name": "database", "state": "HEALTHY", "critical": true, "consecutiveFailures": 0, "lastSuccessAt": "...", "lastFailureAt": null } ],
      "workers": [ { "name": "adm-...", "state": "HEALTHY", "running": true, "startedAt": "...", "lastHeartbeatAt": "...", "lastSuccessAt": null, "lastErrorAt": null } ],
      "workerTotal": 3 },
  "database": { "ok": true },
  "discord": { "configured": true, "ready": true },
  "summary": { "total": 14, "byStatus": { "READY": 8 }, "byHealth": { "HEALTHY": 8 }, "serversByStatus": { "ACTIVE": 12 },
      "botNotInstalled": 0, "permissionsUnverified": 2, "readyNeverChecked": 1 },
  "embedRender": { "enabled": false, "templateCustomRender": 0, "templateDefaultRender": 120, "templateFallbackRender": 0, "templateRenderError": 0 },
  "performance": {
      "database": { "poolTotalConns": 3, "poolIdleConns": 2, "poolMaxConns": 25, "poolAcquiredConns": 1, "poolAcquireCount": 1042,
          "poolEmptyAcquireCount": 0, "poolAcquireDurationMs": 12, "queryTotal": 8841, "querySlow": 0, "queryAvgDurationMs": 0.31 },
      "routing": { "cacheHits": 512, "cacheMisses": 9, "cacheHitRate": 0.9826, "cacheEntries": 9 },
      "tableSizes": [ { "table": "kills", "totalBytes": 734003200, "tableBytes": 402653184, "indexBytes": 331350016, "rowsEstimate": 1830211 } ] } }
```

`installations` (the website's list) holds at most 50 installations needing attention:
status `DEGRADED`, `DISCONNECTED` or `SUSPENDED`, never-checked first. Free-text error
messages from components and workers are intentionally not exposed. `state`/`overall`
use the runtime's `HEALTHY | DEGRADED | UNHEALTHY | UNKNOWN`.

`performance` is the in-process counter snapshot (docs/PERFORMANCE.md section 11) plus
`tableSizes`: the 15 largest tables on disk, biggest first, read from `pg_class` only -
`totalBytes` = `tableBytes` (heap + TOAST) + `indexBytes`; `rowsEstimate` is the planner's
estimate from the last ANALYZE, `null` when there has been none, never a `COUNT(*)`. No
table is scanned and no row content is read; when the catalog read fails the list is
empty and the response still succeeds. The Owner Hub can chart it to watch data growth
(docs/PERFORMANCE.md section 17).

## Platform owner access

For an organization **owned by a platform owner**, every plan feature is unlocked and every
feature switch is on, with no plan check and no per-server switch needed.

**Which organizations.** The organization's owner (`organizations.owner_user_id`) is an
account whose Discord id is on `CHAMPION_ADMIN_DISCORD_IDS`. A platform owner who is only
an `ADMIN` or `MEMBER` of somebody else's organization does **not** unlock that
organization. Platform staff unlock nothing. No customer organization gets anything it did
not have before.

**How it is resolved.** `internal/owneraccess` keeps the set of such organizations and
their installations in memory and reloads it in the background every 60 seconds, so hot
paths (a feed route per event, an embed per card) add no query. Creating an organization
or an installation as a platform owner reloads it at once. Taking an id off the allowlist
needs a redeploy anyway, after which the organization is an ordinary customer again.

**Plan features.** `internal/entitlements` answers every question about a `Plan`, and a
`Plan` can only be built with `entitlements.ForOrganization(organizationID, planKey)`. For
a platform owner's organization that plan has every feature, whatever the plan key and
whether or not `CHAMPION_PLAN_GATING_ENABLED` is on. Because `Has`, `Resolve`,
`RouteAllowed` and `FactionLimit` no longer accept a plan string, a gate cannot be written
without saying whose plan it is; a test fails on `ForOrganization` with a literal id.
This covers the HTTP plan gate (`PLAN_FEATURE_REQUIRED`), the faction cap, fight replay,
the shop order buttons, map rotation, feed channel routes, custom embed templates, hot
zones and the feed identity, and the entitlement lists the billing and admin APIs return.

**Billing state.** The organization's subscription row is left alone and reported as it
is (plan, status, trial dates). What is overridden is the lock-out: `billingRequired` is
`false` and the installation capacity is 1000, so an expired trial, a canceled or
suspended subscription or a missing row never blocks the owner's own organization from
setting up a service. The subscription and trial responses carry
`platformOwnerAccess: true` for such an organization, and the admin read model's
`subscription.platformOwnerAccess` says the same. Not covered: the separately billed
C.A.S.E. add-on (`internal/casebilling`), which keeps its own access rules.

**Feature switches.** For an installation of such an organization, a flag marked
`ownerDefaultOn` is on when it has no override. An explicit override still wins, so the
owner can switch a feature **off** for their own server from the Owner Hub.

| Flag | On by owner access | Why |
|---|---|---|
| `custom_embeds` | yes | Only lets the installation's own saved templates render. Still needs the embed renderer to exist on the deployment. |
| `map_rotation` | yes | Only makes the Map rotation page usable. Nothing is written to a game server until the owner saves a rotation and enables it there. |
| `shop_canary` | no | Lets real shop deliveries change files on the game server, and the canary needs its own preparation. |
| `case_evidence` | no | Starts collecting evidence by itself with high write volume, and only takes effect after a worker restart. |
| `case_build_evidence` | no | Depends on `case_evidence`, adds more writes, and needs a worker restart. |

The three "no" flags behave for an owner's installation exactly as for anyone else: the
environment default, or an override set in the Owner Hub.

**What it never does.** A suspended installation stays suspended. Nothing is enabled on a
game server by itself. Platform owner access is not a subscription: no Stripe object and
no subscription row is created or changed.

## Owner controls (Phase 2, writes)

Every write below is owner only (see "Roles"), goes through the same `adminRoute` gate as a read, takes a JSON body
with a required `reason` (max 500 characters), returns `409 CONFLICT` when the target is
already in the requested state, and records one `platform_audit_log` row (actor, action,
target, reason, sanitized before/after). Nothing here reads or writes a Stripe id.

| Route | Body | Effect |
| --- | --- | --- |
| `POST /organizations/{id}/trial` | `days` (1-365) or `until` | Start or extend a trial (`TRIAL`/`TRIAL`). Refused (`409`) when the organization is billed through Stripe. |
| `POST /organizations/{id}/grant` | `plan`, optional `days` (max 3650) or `until` | Activate a plan without Stripe (`ACTIVE`/`plan`), open-ended or dated; a dated grant lapses on its own (`billingRequired`). Refused when billed through Stripe. |
| `POST /organizations/{id}/revoke` | - | End a trial or grant: `INACTIVE`/`NONE`, billing required. Refused when billed through Stripe. |
| `POST /organizations/{id}/billing/cancel` | - | Stripe-backed: cancel at period end (`billing.Service.Cancel`). |
| `POST /organizations/{id}/billing/reactivate` | - | Stripe-backed: undo a scheduled cancellation. |
| `POST /organizations/{id}/billing/reconcile` | - | Stripe-backed: re-pull the subscription from Stripe and overwrite the row. |
| `POST /installations/{id}/suspend` | - | Status `SUSPENDED` (previous status remembered), the game server deactivated and its worker stopped. Data is kept. |
| `POST /installations/{id}/reinstate` | - | Previous status restored, game server reactivated, worker restarted. |
| `POST /installations/{id}/restart-worker` | - | Stop and start the ADM worker (no tail-start). Refused while suspended. |
| `POST /users/{id}/ban` | - | `banned_at` set; every `/api/saas` route that resolves the acting user answers `403` from then on. Allowlisted platform admins cannot be banned. |
| `POST /users/{id}/unban` | - | Clears the ban. |
| `GET /audit` | query `organizationId`, `targetType`, `targetId`, `action`, `cursor`, `limit` | The platform audit log, newest first, cursor-paginated like every other list. |

Responses carry the sanitized target: `{"subscription": {plan, status, trialEndsAt,
currentPeriodEnd, cancelAtPeriodEnd, externallyBilled, ownerGrantUntil, ownerGrantReason,
billingRequired, trialStatus}}`, `{"installation": {id, organizationId, status, suspendedAt,
suspendedReason, statusBeforeSuspend, workerRunning}}` or `{"user": {id, discordUserId,
username, globalName, avatar, createdAt, lastLoginAt, bannedAt, banReason}}`.

The read model exposes the same state: `subscription.externallyBilled`,
`subscription.ownerGrantUntil/ownerGrantReason` on organizations and installations,
`installation.suspension` on the installation detail, and `members[].bannedAt`.

## Operations console

| Route | Body | Effect |
| --- | --- | --- |
| `GET /ops/status` | - | What the Discord `/admin status` and `/admin diagnostics` subcommands show: `runtime`, `health`, `workers`, `link_diagnostics`, `presence_diagnostics`, `pipeline_diagnostics`, plus `workerManager` (the ADM workers running per active game server). |
| `POST /ops/leaderboard-refresh` | `reason` | The manual leaderboard refresh (`/admin leaderboard-refresh`). Audited as `ops.leaderboard_refreshed`. |
| `POST /ops/adm-source-scan` | `reason` | The slow live scan of Nitrado ADM candidates (`/admin adm-source-scan`, ~30 s). Audited as `ops.adm_source_scanned`; the scan result is returned. |

`GET /live-sync` (per-server watcher freshness, ADM session, boot authority, stored stats and
latency) now goes through the same secret guard as every other admin response.

## Feature flags

Per-installation overrides of the environment rollout switches (internal/featureflags). A
missing override means the environment default applies, exactly as before; an override
wins. Consumers: custom embed rendering (`CHAMPION_CUSTOM_EMBEDS_ENABLED`), the shop canary
gate (`CHAMPION_SHOP_CANARY_*`), C.A.S.E. evidence and build evidence (`CASE_*`; read when a
worker starts, so a change takes effect on the installation's next worker restart) and map
rotation (`map_rotation`, default `CHAMPION_MAP_ROTATION_ENABLED` = off, docs/MAP_ROTATION.md).

| Route | Body | Effect |
| --- | --- | --- |
| `GET /flags` | - | The catalog: key, label, description, env var, restartRequired, the env value on this deployment, `ownerDefaultOn` and (when it is false) `ownerDefaultNote` saying why. |
| `GET /installations/{id}/flags` | - | Each flag's `default` (env answer for this installation), `override` (stored decision or null), `effective`, `source`, `sourceLabel`, `ownerAccess`, reason, updatedBy/At. |
| `PUT /installations/{id}/flags/{flag}` | `reason`, `enabled: true\|false` | Sets the override (audited `installation.flag_set`); without `enabled` clears it (`installation.flag_cleared`). Unknown flag: `404`. |

`source` says where `effective` comes from: `override` (the stored decision),
`owner_access` (no override, and the installation belongs to a platform owner's own
organization, see "Platform owner access") or `default` (the environment). `sourceLabel`
is the same in plain words for the page to show. `ownerAccess` is `true` whenever owner
access applies to that flag for that installation, also while an override hides it, so
the page can say what clearing the override would do.

## Users

| Route | Query | Returns |
| --- | --- | --- |
| `GET /users` | `search` (username / global name / Discord id), `banned=true\|false`, `cursor`, `limit` | Website accounts newest first: id, Discord id, username, global name, avatar, createdAt, lastLoginAt, bannedAt/banReason, trialUsed, organizations[{id,name,slug,role}]; plus `counts{total, banned, activeLast30d}`. |
| `GET /users/{id}` | - | One account in the same shape; `404` when unknown. |

## Fields that are never returned

By construction (every query names its columns; the response types have no such
fields - asserted by a test that walks every DTO) and by a backstop (a response
containing any configured secret is withheld with a `500` instead of being sent):

* Nitrado access tokens, the encrypted credential blob, its nonce and key version
* the Discord bot token, OAuth secrets
* `WEBSITE_API_SECRET` / `CHAMPION_SAAS_API_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`,
  `DATABASE_URL` and database passwords, Railway credentials
* billing-provider identifiers (`provider_customer_id`, `provider_subscription_id`)
* free-text runtime error strings

## Access logging

Each admin read logs one line (`component=admin_api event=admin_read`) with
`acting_admin_discord_id`, `role` (`OWNER` or `STAFF`), `route` (the route pattern, not
the URL with its query), and `organization_id` / `installation_id` when the path has
them. A write logs the same line as `event=admin_write`. A write refused because the
caller is staff logs `event=admin_write_denied` with the id, role and route. A rejected
platform-admin check logs `event=admin_denied` with the presented id and the route.
Never logged: the `Authorization` header or any token, query strings (search text),
request or response bodies.

## Efficiency

Organization lists are two statements per page; subscription and installation lists
are one; detail endpoints are a fixed handful - none grow with the number of rows
(asserted with a statement counter against real PostgreSQL). Search is a bounded
`ILIKE` scan over the (small) SaaS tables - add `pg_trgm` indexes if the customer
count ever makes that measurable.

## Website contract notes

Where the website's types cannot be met authoritatively:

| Website field | Backend behavior |
|---|---|
| `AdminOverview.activeInstallations` | no stored "active" status: defined as `READY` + `DEGRADED` |
| `AdminOverview.backendStatus` | runtime health registry overall (`UNKNOWN` if unavailable) |
| `AdminChannelRoute.channelName` | not stored; Discord-cache name or `null` |
| `AdminOrganization.installations` (list) | primary installation only; `installationCount` gives the total; the detail has all |
| `status` filter on `/organizations` | alias for `installationStatus` |
| statuses in the request text: `offline`, `trialing`, `expired` | not backend statuses; the real values are documented above |

## Configuration checklist

1. Bot service: `WEBSITE_API_SECRET` (already set for the runtime/SaaS APIs) and
   `CHAMPION_ADMIN_DISCORD_IDS=<founder discord ids>`.
2. Website: `CHAMPION_SAAS_API_URL` / `CHAMPION_SAAS_API_SECRET` (already used). Ask
   `GET /api/admin/me` for the role instead of keeping a second copy of the ids.
3. Platform staff need no configuration: an owner adds them in the Owner Hub.
