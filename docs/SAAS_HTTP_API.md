# Champion SaaS customer API

Server-to-server HTTP bridge between the Champion website and the
authoritative Go SaaS repositories (see [docs/SAAS_SCHEMA.md](SAAS_SCHEMA.md)
for the underlying schema). Runs on the bot's existing HTTP server/port -
there is no separate SaaS API service or port. All routes are under
`/api/saas/...`.

## Authentication model

Every request goes through, in order:

1. **Service authentication** - `Authorization: Bearer <WEBSITE_API_SECRET>`,
   the same secret and constant-time comparison the existing
   [runtime status API](runtime-status-api.md) uses. Missing or wrong token
   -> `401 UNAUTHORIZED`. **`WEBSITE_API_SECRET` must never be shipped to
   client JS** - only the website's own server-side code calls this API.
2. **Acting user identity** - `X-Champion-Acting-User: <discord_user_id>`.
   The website has already authenticated this person through Auth.js/Discord
   OAuth on its own side; this header is only ever trusted because the
   request already passed service authentication - a browser can never reach
   these handlers directly, so it can never forge this header on its own.
   The Go backend resolves it to an `app_users` row (call
   `POST /api/saas/users/sync` at least once per user first). Missing header
   or unsynced user -> `401 UNAUTHORIZED`.
3. **Organization membership validation** - every organization-scoped route
   verifies the acting user belongs to `{organizationID}` before touching
   anything else. Not a member -> `403 FORBIDDEN`.
4. **Resource authorization** - some routes additionally require the OWNER
   or ADMIN role (noted per-route below); MEMBER gets `403 FORBIDDEN`.
   Organization-scoped resource lookups (installations, guild connections,
   servers) are always additionally filtered by `organizationID` at the
   database layer, so a guessed ID from another organization resolves as
   `404 NOT_FOUND`, never leaking that resource's existence or data.

`POST /api/saas/users/sync` is the one exception to step 2 - it establishes
the acting user's row, so it only requires service auth.

## Error contract

Every non-2xx response is:

```json
{ "error": { "code": "...", "message": "..." } }
```

`message` is always a fixed, safe, human string - never a raw SQL error or
stack trace.

| Code | HTTP status | Meaning |
|---|---|---|
| `UNAUTHORIZED` | 401 | Missing/invalid service auth, or acting user not synced |
| `FORBIDDEN` | 403 | Not a member of the organization, or lacks OWNER/ADMIN for a restricted action |
| `NOT_FOUND` | 404 | Resource doesn't exist, or doesn't belong to the acting organization |
| `CONFLICT` | 409 | Slug/guild already claimed |
| `INVALID_REQUEST` | 400 | Malformed body, invalid ID, or an invalid setup-progress transition |
| `DISCORD_UNAVAILABLE` | 503 | The bot's Discord session isn't connected |
| `INSTALLATION_NOT_VERIFIED` | 422 | An action requires Discord to be verified first (e.g. permission check before installation check) |
| `INTERNAL_ERROR` | 500 | Unexpected server-side failure |

A `429` (not one of the codes above; shape is `{"error":{"code":"RATE_LIMITED","message":"..."}}`) means the caller hit a rate limit - see below.

## Rate limits

In-memory, per acting-Discord-user-ID, per process (this codebase's existing
style - no external dependency, matches `killfeed.Deduplicator`'s in-memory
pattern):

| Route | Limit |
|---|---|
| `POST /api/saas/users/sync` | 10 / minute |
| `POST /api/saas/organizations` | 5 / hour |
| `POST .../discord/verify-installation`, `POST .../discord/verify-permissions` | 10 / minute (shared budget) |

Ordinary reads (organizations list/get, dashboard, installations,
setup progress GET) are never rate limited.

## Routes

### `POST /api/saas/users/sync`
Syncs safe Discord identity fields. **Never accepts or stores an OAuth
token.** Auth: service only.

Request:
```json
{ "discord_user_id": "...", "discord_username": "...", "discord_global_name": "...", "avatar": "..." }
```
`discord_user_id` and `discord_username` are required. Response `200`: [`UserSummary`](#usersummary).

### `GET /api/saas/organizations`
Every organization the acting user belongs to, each with their role folded
in. Auth: member (of each returned org, implicitly). Response `200`:
[`OrganizationSummary`](#organizationsummary)`[]`.

### `POST /api/saas/organizations`
Creates the organization and the acting user's OWNER membership atomically.
Also creates a 14-day TRIAL subscription. Auth: any synced user (they become
OWNER of what they create).

Request:
```json
{ "name": "...", "slug": "..." }
```
Response `201`: [`OrganizationSummary`](#organizationsummary). `409 CONFLICT` if the slug is taken.

### `GET /api/saas/organizations/{organizationID}`
Auth: member. Response `200`: [`OrganizationSummary`](#organizationsummary).

### `GET /api/saas/organizations/{organizationID}/dashboard`
Auth: member. Response `200`: [`DashboardSummary`](#dashboardsummary) - organization, role, subscription, and every installation's full summary in one call.

### `POST /api/saas/organizations/{organizationID}/installations`
Auth: **OWNER/ADMIN**. Creates an installation (with its setup-progress and
settings child rows) referencing an already-connected guild connection.
`game_server_id` starts unset - server selection is a later phase.

Request:
```json
{ "discordGuildConnectionId": 123 }
```
Response `201`: [`InstallationSummary`](#installationsummary). `404` if the guild connection isn't this organization's. `409 CONFLICT` if that guild connection already has an installation.

### `GET /api/saas/organizations/{organizationID}/installations/{installationID}`
Auth: member. Response `200`: [`InstallationSummary`](#installationsummary).

### `GET /api/saas/organizations/{organizationID}/installations/{installationID}/setup`
Auth: member. Response `200`: [`SetupProgress`](#setupprogress).

### `PATCH /api/saas/organizations/{organizationID}/installations/{installationID}/setup`
Auth: **OWNER/ADMIN** (MEMBER is read-only). True PATCH semantics - only
fields present in the body are changed. Every field is optional.

Request:
```json
{
  "currentStep": "NITRADO",
  "discordCompleted": true,
  "nitradoCompleted": false,
  "serverSelected": false,
  "channelsCompleted": false,
  "validationCompleted": false
}
```
`currentStep` must be one of `DISCORD`, `NITRADO`, `SERVER`, `CHANNELS`, `VALIDATION`, `COMPLETE` (`400 INVALID_REQUEST` otherwise).

**Step order is enforced server-side**: the resulting merged state must
satisfy `discord -> nitrado -> server -> channels -> validation` - a later
flag can only be `true` if every earlier flag in that chain is also `true`
in the same resulting state. An invalid transition (e.g. marking
`validationCompleted` while `discordCompleted` is still `false`) is rejected
whole with `400 INVALID_REQUEST` and nothing is written.

Response `200`: [`SetupProgress`](#setupprogress).

### `POST /api/saas/organizations/{organizationID}/discord/guilds/eligible`
Auth: member. The website supplies Discord-OAuth-verified guild candidates
(from Auth.js's `identify guilds` scope, which includes Discord's own
per-guild permission bitfield for the acting user); the Go API independently
re-derives eligibility from that bitfield (**never** trusts a pre-computed
`eligible` boolean from the browser/website) and cross-checks bot presence
via the bot's own live Discord session. A guild the bot happens to already
be in says nothing about whether *this user* can administer it, so bot
membership is checked separately from - never as a substitute for -
permission eligibility.

Request:
```json
{
  "guilds": [
    { "discordGuildId": "...", "guildName": "...", "guildIcon": "...", "permissions": "8" }
  ]
}
```
`permissions` is the raw Discord permission bitfield for this user in this guild, as a decimal string (Discord's own API shape - large enough to exceed safe JS integer range). Eligible = `ADMINISTRATOR` or `MANAGE_GUILD` bit set.

Response `200`: [`DiscordGuildSummary`](#discordguildsummary)`[]`.

### `POST /api/saas/organizations/{organizationID}/discord/connection`
Auth: **OWNER/ADMIN**. Persists a verified guild selection. Re-verifies
eligibility from the submitted `permissions` bitfield (never trusts an
earlier eligible-list response) and checks live bot presence.

Request:
```json
{ "discordGuildId": "...", "guildName": "...", "guildIcon": "...", "permissions": "8" }
```
Response `200`: [`DiscordGuildConnectionSummary`](#discordguildconnectionsummary).

- `403 FORBIDDEN` if the permission bitfield doesn't grant ADMINISTRATOR/MANAGE_GUILD.
- `409 CONFLICT` if this guild is already connected to a **different** organization (never silently reassigned). Reconnecting a guild your own organization already owns succeeds idempotently.
- `503 DISCORD_UNAVAILABLE` if the bot session isn't connected.

### `POST /api/saas/organizations/{organizationID}/installations/{installationID}/discord/verify-installation`
Auth: member. Verifies bot presence using the **live** Discord session -
never trusts a redirect/query-string success. On success, if the
installation is still `NOT_STARTED`, transitions it to `DISCORD_CONNECTED`
(never further, and never regresses an installation already past that
point). Rate limited (shared budget with verify-permissions).

Response `200`: [`DiscordVerificationResult`](#discordverificationresult).

### `POST /api/saas/organizations/{organizationID}/installations/{installationID}/discord/verify-permissions`
Auth: member. Checks the bot's actual permissions in a specific channel.
Requires the installation to already be past `NOT_STARTED` (`422
INSTALLATION_NOT_VERIFIED` otherwise - connect Discord first). Rate limited
(shared budget with verify-installation).

Request:
```json
{ "channelId": "..." }
```
Response `200`: [`PermissionVerificationResult`](#permissionverificationresult). Never returns a raw Discord permission bitfield - only the mapped PASS/WARNING/FAIL per capability.

**PASS/WARNING/FAIL mapping** (a Champion product decision, not something
Discord itself defines): `View Channel` and `Send Messages` are *blocking* -
missing either is `FAIL` and fails the whole check (a killfeed literally
cannot post without them). `Embed Links` and `Read Message History` are
*degrading* - missing either is `WARNING` (the bot can still post, just with
reduced presentation). If the guild or channel isn't reachable at all, every
capability is `FAIL`.

## Response DTOs

None of these ever include a Nitrado ciphertext/IV/auth tag, a Discord
bot/OAuth token, `WEBSITE_API_SECRET`, a database URL, or any other internal
credential (section 18) - see `TestNoSensitiveFieldsInAPIResponses` in
`internal/app/saas_api_test.go` for the regression guard.

#### `UserSummary`
```ts
{ id: number, discordUserId: string, discordUsername: string, discordGlobalName?: string, avatar?: string }
```

#### `OrganizationSummary`
```ts
{ id: number, name: string, slug: string, role: "OWNER" | "ADMIN" | "MEMBER" }
```
`role` is always the **acting user's own** role - never another member's.

#### `DashboardSummary`
```ts
{
  organization: OrganizationSummary,
  subscription?: SubscriptionSummary,
  installations: InstallationSummary[]
}
```

#### `SubscriptionSummary`
```ts
{ plan: string, status: "TRIAL"|"ACTIVE"|"PAST_DUE"|"CANCELED"|"SUSPENDED", trialEndsAt?: string, currentPeriodEnd?: string }
```

#### `InstallationSummary`
```ts
{
  id: number,
  status: "NOT_STARTED"|"DISCORD_CONNECTED"|"NITRADO_CONNECTED"|"CONFIGURING"|"READY"|"DEGRADED"|"DISCONNECTED"|"SUSPENDED",
  plan?: string,
  health: "SETTING_UP"|"HEALTHY"|"DEGRADED"|"OFFLINE",  // derived from status, not a stored field
  setupProgress?: SetupProgress,
  discordConnection?: DiscordGuildConnectionSummary,
  dayzServer?: DayZServerSummary,   // present only once a server is selected (a later phase)
  createdAt: string,
  setupCompletedAt?: string,        // stamped once, the first time status reaches READY
  lastHealthCheckAt?: string
}
```

#### `SetupProgress`
```ts
{
  currentStep: "DISCORD"|"NITRADO"|"SERVER"|"CHANNELS"|"VALIDATION"|"COMPLETE",
  discordCompleted: boolean, nitradoCompleted: boolean, serverSelected: boolean,
  channelsCompleted: boolean, validationCompleted: boolean,
  completedAt?: string   // stamped once, the first time validationCompleted becomes true
}
```

#### `DiscordGuildConnectionSummary`
```ts
{ id: number, guildName?: string, guildIcon?: string, botInstalled: boolean, permissionsVerified: boolean, connectedAt: string }
```

#### `DayZServerSummary`
```ts
{ id: number, displayName?: string, game: string, platform: string, status: string }
```

#### `DiscordGuildSummary`
```ts
{ discordGuildId: string, guildName?: string, guildIcon?: string, eligible: boolean, botInstalled: boolean }
```

#### `DiscordVerificationResult`
```ts
{ installed: boolean, guildReachable: boolean, verifiedAt: string }
```

#### `PermissionVerificationResult`
```ts
{ capabilities: { capability: string, result: "PASS"|"WARNING"|"FAIL" }[], overallPass: boolean }
```
`capabilities` is always exactly the 4 entries: `View Channel`, `Send Messages`, `Embed Links`, `Read Message History`, in that order.

## Explicitly out of scope for this API (see docs/SAAS_SCHEMA.md)

Nitrado credential submission/decryption, server selection, channel
configuration writes, billing/checkout, and entitlement enforcement are not
part of this API yet - `installations` intentionally stops at
`DISCORD_CONNECTED` here. Nitrado setup is the next task.
