# Champion SaaS API - website integration handoff

This is the **authoritative** contract for the website's integration
against the Go backend's SaaS customer API. It supersedes
[docs/SAAS_HTTP_API.md](SAAS_HTTP_API.md) as the integration source of
truth (that file remains as supplementary implementation notes and stays in
sync with this one). See [docs/SAAS_SCHEMA.md](SAAS_SCHEMA.md) for the
underlying PostgreSQL schema these endpoints read and write.

**The routes and DTO names below are the actual, tested, shipped API.**
Where an earlier planning draft used different names, this document says so
explicitly rather than silently renaming a working, already-integration-tested
route to match a guess.

## Base URL

The SaaS API runs on the bot's existing HTTP server - the same Railway
service and port as [`GET /api/runtime/status`](runtime-status-api.md), just
a different path prefix (`/api/saas/...` vs `/api/runtime/...`). There is no
separate SaaS service, port, or deployment.

Website environment variables:

```
CHAMPION_SAAS_API_URL=<same Railway URL as CHAMPION_RUNTIME_API_URL>
CHAMPION_SAAS_API_SECRET=<same value as the bot's WEBSITE_API_SECRET>
```

`CHAMPION_SAAS_API_URL` is the bot's base origin (e.g.
`https://dayzkillfeedbot-production.up.railway.app`) - append the paths
below to it. `CHAMPION_SAAS_API_SECRET` is **not a new/separate secret**:
the bot has exactly one server-to-server secret
(`WEBSITE_API_SECRET`, set on the bot's Railway service), already reused for
the runtime status API under the name `CHAMPION_RUNTIME_API_SECRET` on the
website side. Set `CHAMPION_SAAS_API_SECRET` to that same value. This
follows section 2's explicit instruction to prefer the existing trusted
server-to-server secret mechanism rather than mint a new one.

## Authentication

```
Authorization: Bearer <CHAMPION_SAAS_API_SECRET>
```

- **Header**: `Authorization`, standard `Bearer <token>` format.
- **Env var (website side)**: `CHAMPION_SAAS_API_SECRET`.
- **Env var (bot side)**: `WEBSITE_API_SECRET` (same value - see above).
- **On missing/invalid token**: every route returns `401` with body
  `{"error":{"code":"UNAUTHORIZED","message":"missing or invalid service authentication"}}`,
  compared in constant time (`crypto/subtle.ConstantTimeCompare`) so a
  wrong-length or wrong-value token can't be timed to guess the secret.
- **Never expose this secret to browser code.** Only the website's own
  server-side code (Next.js API routes / server components / server
  actions) may call this API. There is no CORS configuration - it is not
  meant to be reachable directly from a browser.

## Acting user identity

```
X-Champion-Acting-User: <discord_user_id>
```

The website has already authenticated this person through Auth.js/Discord
OAuth on its own side. This header is a **trusted assertion**, not a claim
the Go backend independently re-verifies against Discord - it is only safe
to trust because:

1. The request already passed `Authorization` service authentication (a
   browser can never reach this API directly, so it can never forge this
   header on its own - it can only be set by the website's own trusted
   server-side code, which already knows who the visitor is).
2. The Go backend still resolves this Discord ID to its own `app_users` row
   and performs its own organization-membership and role checks for every
   subsequent action - **the header identifies who is acting, it does not
   grant them anything by itself.**

The website must call `POST /api/saas/users/sync` at least once per user
(e.g. right after sign-in) before using their Discord ID in this header on
any other route - an unsynced Discord ID gets `401 UNAUTHORIZED` ("acting
user has not synced") everywhere else.

`POST /api/saas/users/sync` is the one route that does not require this
header (it's what establishes the synced row this header will later
resolve).

## Authorization chain (every route)

1. **Service auth** (`Authorization` header) → `401` if missing/wrong.
2. **Acting user resolution** (`X-Champion-Acting-User` header → `app_users`
   row) → `401` if missing header or the Discord ID never synced.
3. **Organization membership** (for every `{organizationID}`-scoped route) →
   `403 FORBIDDEN` if the acting user isn't a member of that organization.
4. **Role authorization** (routes marked "OWNER/ADMIN" below) → `403
   FORBIDDEN` if the acting user's role is `MEMBER`.

Every organization-scoped resource lookup (installations, guild
connections, servers) is *additionally* filtered by `organizationID` at the
database query itself - a correctly-authenticated member of organization A
who guesses an ID belonging to organization B gets `404 NOT_FOUND` for that
resource, never a peek at its existence or data.

## Routes

Base path for every route below: prepend `CHAMPION_SAAS_API_URL`.

| # | Method | Path | Role required | Notes |
|---|---|---|---|---|
| 1 | POST | `/api/saas/users/sync` | (any synced-or-not user) | |
| 2 | GET | `/api/saas/organizations` | member (per org returned) | |
| 3 | POST | `/api/saas/organizations` | (any synced user) | |
| 4 | GET | `/api/saas/organizations/{organizationID}` | member | |
| 5 | GET | `/api/saas/organizations/{organizationID}/dashboard` | member | |
| 6 | POST | `/api/saas/organizations/{organizationID}/installations` | OWNER/ADMIN | |
| 7 | GET | `/api/saas/organizations/{organizationID}/installations/{installationID}` | member | |
| 8 | GET | `/api/saas/organizations/{organizationID}/installations/{installationID}/setup` | member | |
| 9 | PATCH | `/api/saas/organizations/{organizationID}/installations/{installationID}/setup` | OWNER/ADMIN | |
| 10 | POST | `/api/saas/organizations/{organizationID}/discord/guilds/eligible` | member | see naming note below |
| 11 | POST | `/api/saas/organizations/{organizationID}/discord/connection` | OWNER/ADMIN | see naming note below |
| 12 | POST | `/api/saas/organizations/{organizationID}/installations/{installationID}/discord/verify-installation` | member | |
| 13 | POST | `/api/saas/organizations/{organizationID}/installations/{installationID}/discord/verify-permissions` | member | see naming note below |
| 14 | POST | `/api/saas/organizations/{organizationID}/nitrado/connect` | OWNER/ADMIN | |
| 15 | GET | `/api/saas/organizations/{organizationID}/nitrado/services` | member | |
| 16 | POST | `/api/saas/organizations/{organizationID}/installations/{installationID}/dayz-server` | OWNER/ADMIN | |
| 17 | POST | `/api/saas/organizations/{organizationID}/installations/{installationID}/dayz-server/validate` | member | |

### Naming differences from earlier planning drafts

An earlier internal draft of this contract used slightly different route
names for three of these. Per this task's own instruction ("do not rename
working routes merely to match this prompt"), the shipped, integration-tested
names above are authoritative:

- **#10/#11**: planned as a single `POST .../discord-guild`. Shipped as
  **two** routes instead, because they're genuinely different operations
  with different authorization levels: `#10` (`.../discord/guilds/eligible`,
  member-level) lets the website ask "which of these OAuth-verified
  candidate guilds can this organization actually connect," while `#11`
  (`.../discord/connection`, OWNER/ADMIN-level) is the one that persists a
  selection. Splitting them means a MEMBER can browse eligible guilds
  without being able to commit one.
- **#13**: planned as `GET .../discord/permissions`. Shipped as **`POST
  .../discord/verify-permissions`**, because the check requires a request
  body (`channelId` - which channel to check permissions against), which a
  `GET` request isn't a good fit for, and because it triggers a live
  Discord API call each time rather than reading cached state (matching the
  "verify-" naming already used for `#12`).

### 1. `POST /api/saas/users/sync`
Upserts safe Discord identity fields. **Never accepts or stores a Discord
OAuth token.**

Request:
```json
{
  "discord_user_id": "111222333444",
  "discord_username": "PlayerOne",
  "discord_global_name": "Player One",
  "avatar": "a1b2c3d4e5f6"
}
```
`discord_user_id` and `discord_username` are required.

Response `200` ([`UserSummary`](#usersummary)):
```json
{ "id": 1, "discordUserId": "111222333444", "discordUsername": "PlayerOne", "discordGlobalName": "Player One", "avatar": "a1b2c3d4e5f6" }
```

### 2. `GET /api/saas/organizations`
Response `200` (`[`[`OrganizationSummary`](#organizationsummary)`]`):
```json
[{ "id": 5, "name": "Champions", "slug": "champions", "role": "OWNER" }]
```

### 3. `POST /api/saas/organizations`
Creates the organization and the caller's OWNER membership atomically. Also
starts the caller's one no-card 14-day trial if they have not used it yet;
a caller who already used theirs gets an `INACTIVE` subscription (billing
required) - creating more organizations never grants more trials
(`docs/BILLING.md` section 28).

Request:
```json
{ "name": "Champions", "slug": "champions" }
```
Response `201` ([`OrganizationSummary`](#organizationsummary)):
```json
{ "id": 5, "name": "Champions", "slug": "champions", "role": "OWNER" }
```
`409 CONFLICT` if the slug is taken.

### 4. `GET /api/saas/organizations/{organizationID}`
Response `200`: [`OrganizationSummary`](#organizationsummary) (same shape as above).

### 5. `GET /api/saas/organizations/{organizationID}/dashboard`
Response `200`: [`DashboardSummary`](#dashboardsummary) - see full example below.

### 6. `POST /api/saas/organizations/{organizationID}/installations`
Request:
```json
{ "discordGuildConnectionId": 12 }
```
Response `201`: [`InstallationSummary`](#installationsummary). `404` if the
guild connection isn't this organization's; `409 CONFLICT` if that
connection already has an unconfigured installation.

This is the "initial setup / new service" operation, gated by the
subscription (Onboarding V2, `docs/BILLING.md` section 28):
`402 BILLING_REQUIRED` when there is no running trial or paid plan (trial
expired, account already used its trial, subscription canceled/suspended);
`409 INSTALLATION_LIMIT_REACHED` when the organization already has as many
installations as its plan allows (the trial allows 1). It never modifies or
replaces an existing installation - reconfigure one through its own routes
(`#16`, `#20`, `#21`).

### 7. `GET /api/saas/organizations/{organizationID}/installations/{installationID}`
Response `200`: [`InstallationSummary`](#installationsummary).

### 8. `GET .../installations/{installationID}/setup`
Response `200`: [`SetupProgress`](#setupprogress).

### 9. `PATCH .../installations/{installationID}/setup`
True PATCH - only fields present in the body are changed.

Request:
```json
{ "currentStep": "NITRADO", "nitradoCompleted": true }
```
`currentStep` must be one of `DISCORD`, `NITRADO`, `SERVER`, `CHANNELS`,
`VALIDATION`, `COMPLETE`. **Step order is enforced server-side**: the
resulting merged state must satisfy
`discord → nitrado → server → channels → validation` (a later flag can only
be `true` if every earlier one already is, in the same resulting state).
An invalid transition is rejected whole with `400 INVALID_REQUEST` and
nothing is written.

Response `200`: [`SetupProgress`](#setupprogress).

### 10. `POST /api/saas/organizations/{organizationID}/discord/guilds/eligible`
The website supplies Discord-OAuth-verified candidates (Auth.js `identify
guilds` scope, which returns a per-guild permission bitfield for the acting
user); the Go API independently re-derives eligibility from that bitfield
(never trusts a pre-computed boolean) and cross-checks bot presence against
the bot's **local gateway state cache** - a pure in-memory lookup, never a
live Discord API call per candidate. This is intentional and load-bearing:
a website OAuth session can list dozens of guilds for one user, and this
route is called with all of them at once, so it must not scale with a live
network round trip per guild (that was the original cause of this route
timing out for users in many servers). `botInstalled` therefore reflects
the bot's state as of its last gateway event for that guild (effectively
real-time - populated from `GUILD_CREATE`/`GUILD_DELETE` - not a periodic
poll), not a request-time live check. The final, single guild the customer
actually selects is still authoritatively, live-verified by `#11` and `#12`
below - this endpoint only ever produces a list, never persists anything.

Request:
```json
{
  "guilds": [
    { "discordGuildId": "555666777888", "guildName": "My DayZ Server", "guildIcon": "abc123", "permissions": "8" }
  ]
}
```
`permissions` is the raw Discord permission bitfield for this user in this
guild, as a **decimal string** (Discord's own API shape). Eligible =
`ADMINISTRATOR` (`8`) or `MANAGE_GUILD` (`32`) bit set.

Response `200` (`[`[`DiscordGuildConnectionCandidate`](#discordguildsummary)`]`, named `DiscordGuildSummary` in code):
```json
[{ "discordGuildId": "555666777888", "guildName": "My DayZ Server", "guildIcon": "abc123", "eligible": true, "botInstalled": false }]
```

### 11. `POST /api/saas/organizations/{organizationID}/discord/connection`
Persists a verified guild selection. Re-verifies eligibility from the
submitted `permissions` (never trusts an earlier `#10` response) and checks
live bot presence.

Request:
```json
{ "discordGuildId": "555666777888", "guildName": "My DayZ Server", "guildIcon": "abc123", "permissions": "8" }
```
Response `200`: [`DiscordGuildConnectionSummary`](#discordguildconnectionsummary).

- `403 FORBIDDEN` - permission bitfield doesn't grant ADMINISTRATOR/MANAGE_GUILD.
- `409 CONFLICT` - this guild is already connected to a **different**
  organization (never silently reassigned). Reconnecting a guild your own
  organization already owns succeeds idempotently.
- `503 DISCORD_UNAVAILABLE` - the bot's Discord session isn't connected.

### 12. `POST .../installations/{installationID}/discord/verify-installation`
Verifies bot presence using the **live** Discord session - never trusts a
redirect/query-string success. On success, if the installation is still
`NOT_STARTED`, transitions it to `DISCORD_CONNECTED` (never further; never
regresses an installation already past that point). No request body.

Response `200`: [`DiscordInstallationVerification`](#discordinstallationverification) (named `DiscordVerificationResult` in code):
```json
{ "installed": true, "guildReachable": true, "verifiedAt": "2026-09-18T02:00:00Z" }
```

### 13. `POST .../installations/{installationID}/discord/verify-permissions`
Checks the bot's actual permissions in a specific channel. Requires the
installation to already be past `NOT_STARTED` (`422
INSTALLATION_NOT_VERIFIED` otherwise - call `#12` first).

Request:
```json
{ "channelId": "999888777666" }
```
Response `200`: [`DiscordPermissionVerification`](#discordpermissionverification) (named `PermissionVerificationResult` in code):
```json
{
  "capabilities": [
    { "capability": "View Channel", "result": "PASS" },
    { "capability": "Send Messages", "result": "PASS" },
    { "capability": "Embed Links", "result": "WARNING" },
    { "capability": "Read Message History", "result": "PASS" }
  ],
  "overallPass": true
}
```
`View Channel`/`Send Messages` missing → `FAIL` (blocking - a killfeed
literally cannot post). `Embed Links`/`Read Message History` missing →
`WARNING` (degraded, not blocking). Guild/channel unreachable → every
capability `FAIL`. Never returns a raw Discord permission bitfield.

### 14. `POST /api/saas/organizations/{organizationID}/nitrado/connect`
Validates a customer's Nitrado account token live (never persisted unless
valid), discovers accessible services, and stores the encrypted credential
envelope. **The token is never returned in the response, logged, or stored
in plaintext anywhere.** One Nitrado connection per organization
(`UNIQUE(organization_id)`) - connecting again replaces it (token rotation).

Request:
```json
{ "token": "<customer Nitrado token>" }
```
Response `200`:
```json
{ "connected": true, "servicesFound": 3 }
```
`servicesFound` counts only **supported** DayZ console services (PlayStation
+ Xbox - see `#15`), not the customer's raw total Nitrado service count.
`503 NITRADO_UNAVAILABLE` if Nitrado rejects the token or is unreachable -
nothing is persisted in that case. On success, advances setup progress
(`nitradoCompleted=true`, `currentStep` advances to `SERVER`) for every
installation under the organization still mid-setup - the Nitrado
connection is organization-scoped, not installation-scoped, so it can
unblock more than one installation's Step 4 at once.

### 15. `GET /api/saas/organizations/{organizationID}/nitrado/services`
Discovers the connected account's DayZ console services live. Returns only
supported platforms (section 1) - PC DayZ and unrelated games are dropped
entirely, never included in the response. `404 NOT_FOUND` if no Nitrado
connection exists yet for this organization (call `#14` first).

Response `200`: [`NitradoServiceSummary`](#nitradoservicesummary)`[]`:
```json
[
  { "serviceId": 123456, "name": "Champions PS", "game": "DayZ", "platform": "PLAYSTATION", "status": "ONLINE" },
  { "serviceId": 987654, "name": "Champions Xbox", "game": "DayZ", "platform": "XBOX", "status": "ONLINE" }
]
```

### 16. `POST .../installations/{installationID}/dayz-server`
Selects one discovered service as the installation's DayZ server.
**Re-verifies live** against the connected Nitrado account (never trusts an
earlier `#15` response or a client-supplied platform/name) before
persisting - rejects a service that has since disappeared, and rejects
anything that isn't a supported console platform (PC included) even if it's
a real service on the account.

Request:
```json
{ "serviceId": 123456 }
```
Response `200` ([`SelectDayZServerResponse`](#selectdayzserverresponse)):
```json
{
  "server": { "id": 42, "serviceId": 123456, "displayName": "Champions PS", "game": "DayZ", "platform": "PLAYSTATION", "status": "ONLINE" },
  "installationId": 4,
  "reusedInstallation": true
}
```
`400 INVALID_REQUEST` if the service isn't a supported DayZ console platform.
`404 NOT_FOUND` if the service ID isn't on the connected account.
`409 CONFLICT` if this exact game_servers row is already owned by a
**different** organization (never silently reassigned - mirrors `#11`'s
guild-connection conflict rule).

**Same-guild reuse**: `discord_guild_connections` and `game_servers` share
one `UNIQUE(discord_guild_connection_id, game_server_id)` pair per
installation. If the Discord guild + DayZ server pair you're selecting is
already owned by a **different existing installation** in your
organization (e.g. a customer re-running the setup wizard for a guild they
already fully set up under an older installation), this endpoint does
**not** create a duplicate or 500 - it resolves to that existing
installation instead: `installationId` in the response is that existing
installation's ID (not necessarily the one in the request path), and
`reusedInstallation` is `true`. Setup progress (`serverSelected=true`,
`currentStep=CHANNELS`) advances on the **resolved** installation. If the
originally-requested installation had no other progress or settings, it is
safely deleted as redundant; if it had any real progress/settings, it's
left alone (never deleted without proof it was empty). Selecting a
**different** DayZ server under the same guild connection is always a
legitimate new pairing (`reusedInstallation=false`) - multi-server
customers are unaffected.

### 17. `POST .../installations/{installationID}/dayz-server/validate`
A safe, read-only live check that the installation's already-selected DayZ
server is still reachable and still a supported console platform. Any
member may run it (it never mutates anything). `400 INVALID_REQUEST` if no
server has been selected yet (call `#16` first).

Response `200`:
```json
{ "reachable": true, "supported": true, "platform": "XBOX", "message": "DayZ Xbox server connected." }
```
An unreachable/unsupported server still responds `200` with
`reachable`/`supported` reflecting reality (this is a status check, not an
error condition) - e.g. `{ "reachable": false, "supported": false, "message": "this DayZ server was not found on the connected Nitrado account" }`.

### 18. `GET .../installations/{installationID}/discord/channels`
Lists the installation's Discord guild's Champion-selectable channels - only
text and announcement channels the bot can currently **view** are ever
returned (voice/stage/category/forum/thread channels never appear). Any
member may call it.

Response `200` ([`DiscordChannelSummary[]`](#discordchannelsummary)):
```json
[
  { "id": "111", "name": "champion-killfeed", "type": "TEXT", "position": 0, "canSend": true },
  { "id": "222", "name": "announcements", "type": "ANNOUNCEMENT", "position": 1, "canSend": false }
]
```
`canSend` is a safe, best-effort hint (the bot can currently post there) -
it is **not** a substitute for `#13`'s full permission verification, which
remains the authoritative check before Champion actually relies on a
channel.

### 19. `GET .../installations/{installationID}/channels`
Returns the installation's currently persisted channel selection. Any
member may call it - this is what a page refresh in Customize mode uses to
restore the form.

Response `200` ([`InstallationChannelSettings`](#installationchannelsettings)):
```json
{ "killfeedChannelId": "111", "leaderboardChannelId": "111", "playerStatusChannelId": "", "adminLogChannelId": "222" }
```
An empty string means "not configured yet." The same channel ID may appear
under multiple purposes (shown above: `111` serves both killfeed and
leaderboard) - Champion never requires four distinct channels.

### 20. `PUT .../installations/{installationID}/channels`
Saves a manual (Customize mode) channel selection. OWNER/ADMIN only.

Request (same shape as the response above):
```json
{ "killfeedChannelId": "111", "leaderboardChannelId": "111", "playerStatusChannelId": "", "adminLogChannelId": "222" }
```
`killfeedChannelId` is the only required field - `400 INVALID_REQUEST` if
missing. Every non-empty channel ID is **re-verified live** against the
installation's own Discord guild (`#18`'s own channel list) before anything
is persisted - `400 INVALID_REQUEST` if any of them don't belong to this
guild or aren't a supported text-capable channel; a channel ID from a
different guild is never accepted, even if it's a syntactically valid
Discord snowflake. Every other settings field (timezone, distance unit,
online-display/leaderboard toggles) is preserved untouched - only the four
channel columns are ever mutated by this route.

On success: `channelsCompleted=true`, `currentStep=VALIDATION` (never
`validationCompleted` - `#13` remains the only route that sets that), and
the installation's channel configuration is marked customer-owned - a later
`#21` one-click auto-setup call will never silently overwrite it without
`force=true`. If the installation's status was still `DISCORD_CONNECTED` or
`NITRADO_CONNECTED`, it advances to `CONFIGURING`.

### 21. `POST .../installations/{installationID}/channels/auto-setup`
One-click setup: applies Champion's **Channel System V2 layout** (see "Channel
routing" below) in the installation's Discord guild - three categories and
one channel per destination that has a working producer - persists the
resulting route IDs, verifies every created channel, and advances setup
exactly like `#20`/`#24` do. OWNER/ADMIN only.

Request (optional body):
```json
{ "force": false }
```

Behavior:
1. Verifies the bot is actually installed in the guild (`503
   DISCORD_UNAVAILABLE` if not).
2. **Unless `force=true`**, refuses if the installation already has
   customer-owned routing - either a legacy manual save (`#20`) or any
   route saved via `#24` with `managedByChampion=false` - see "Custom
   configuration protection" below.
3. Verifies the bot holds **Manage Channels** in the guild - see "Missing
   permission" below.
4. Decides which destinations get a channel (**no blank channel policy**):
   a destination is created only when one of its anchor routes has a
   working producer in this process. Everything else is skipped, reported
   with its state, and its Champion-managed routes are unmapped. If the
   combat feed itself cannot work, nothing is created (`KILLFEED_UNAVAILABLE`).
5. Resolves (or creates) the categories those destinations need
   (`🏆 CHAMPION • LIVE`, `🏆 CHAMPION • HUB`, and `🔒 CHAMPION • STAFF`,
   which is created hidden from `@everyone`): the parent of an already-reused
   V2 channel first, then a case-insensitive name scan, then create.
6. Resolves (or creates) each destination channel: a persisted route channel
   that already carries the V2 name first, then a name scan **scoped to its
   category** (recovery never adopts a same-named channel elsewhere), then
   create. Every route of the destination is mapped to it
   (`managedByChampion=true`), including a route whose source is not proven
   yet (`BUILD_FEED` before any build line was parsed) so it is ready.
7. Posts/restores persistent panels immediately (the same serialized sync
   the runtime uses, so it never posts a second copy), then verifies every
   created channel: exists, route mapped, producer connected, bot can send,
   visible Champion content. A live-feed channel with no Champion message
   yet gets **one** small starter card; a channel that already has one never
   gets another. A channel failing any check is reported `BROKEN`.
8. Mirrors `KILLFEED`/`STATS_LEADERBOARDS`/`ADMIN_LOGS` back onto the legacy
   `installation_settings` fields (the LIVE category is the persisted
   category ID) so `#19` keeps reflecting reality, advances setup progress,
   and moves the installation to `CONFIGURING` (same one-directional rule as
   `#20`).

Champion **never deletes** a Discord channel. Champion-managed channels and
categories that no route uses any more (for example a pre-V2 `casino`,
`shop`, `build-feed` or the old `CHAMPION KILLFEED` category) are returned
in `retirable` for the customer to remove.

Response `200` on success ([`AutoSetupChannelsResponse`](#autosetupchannelsresponse)):
```json
{
  "configured": true,
  "category": { "id": "900", "name": "🏆 CHAMPION • LIVE", "key": "LIVE" },
  "categories": [
    { "id": "900", "name": "🏆 CHAMPION • LIVE", "key": "LIVE" },
    { "id": "901", "name": "🏆 CHAMPION • HUB", "key": "HUB" },
    { "id": "902", "name": "🔒 CHAMPION • STAFF", "key": "STAFF" }
  ],
  "routes": {
    "KILLFEED": { "channelId": "111", "channelName": "🔫・combat-feed", "managedByChampion": true },
    "PVE_FEED": { "channelId": "111", "channelName": "🔫・combat-feed", "managedByChampion": true },
    "...": "... every mapped route ..."
  },
  "destinations": [
    {
      "key": "ADMIN_LOGS", "label": "Admin Logs", "category": "STAFF",
      "channelId": "130", "channelName": "🛡️・admin-logs",
      "health": "ACTIVE", "created": true, "starterSent": true,
      "routes": [
        { "routeKey": "ADMIN_LOGS", "health": "ACTIVE", "detail": "ADMMonitorPublisher ADM health (per server worker)" },
        { "routeKey": "ADMIN_ALERTS", "health": "ACTIVE", "detail": "AdminAlertPublisher: ADM stale, Nitrado download failures, zone/UAV/base radar intrusions" },
        { "routeKey": "BUILD_FEED", "health": "BLOCKED", "detail": "SOURCE_BLOCKED: no build/placement line parsed yet - enable adminLogPlacement / adminLogBuildActions in the server config" }
      ],
      "checks": { "channelExists": true, "routeMapped": true, "producerConnected": true, "botCanSend": true, "visibleContent": true }
    },
    {
      "key": "BOUNTIES", "label": "Bounties", "category": "LIVE", "channelName": "💀・bounties",
      "health": "BROKEN", "detail": "bounty board is not running",
      "created": false, "starterSent": false,
      "routes": [ { "routeKey": "BOUNTY", "health": "BROKEN", "detail": "bounty board is not running" }, { "routeKey": "BOUNTY_TRACKING", "health": "BROKEN", "detail": "bounty board is not running" } ]
    }
  ],
  "retirable": [
    { "channelId": "77", "channelName": "casino", "kind": "CHANNEL", "formerRoutes": ["CASINO"] },
    { "channelId": "70", "kind": "CATEGORY" }
  ]
}
```

Health states: `ACTIVE` (real publisher/panel connected), `DISABLED`
(customer turned it off - no per-installation toggles exist yet),
`NOT_REQUIRED`, `BLOCKED` (backend/source not implemented), `BROKEN` (should
work, but the producer is not running or verification failed).

**Idempotent by design**: calling this route again with the same
installation reuses the exact same categories and channel IDs for every
route and posts no second starter card - it never creates
`combat-feed-1`/`combat-feed-2` duplicates, because steps 5/6 above always
check persisted IDs (and then existing names) before creating anything.

**Custom configuration protection** (never destroys customer customization):
if the installation already has routing that wasn't itself produced by a
prior auto-setup call, this route responds `200` with a safe, structured
"did not configure" result instead of overwriting it:
```json
{ "configured": false, "reason": "CUSTOM_CONFIGURATION_EXISTS" }
```
Pass `{ "force": true }` to override this and restore Champion's defaults
anyway (an explicit, deliberate customer action - never the default).

**Missing permission**: if the bot lacks Manage Channels in the guild,
this route also responds `200` with a safe result rather than a raw
Discord error or a 5xx:
```json
{ "configured": false, "reason": "MISSING_MANAGE_CHANNELS" }
```

**Killfeed unavailable**: if the killfeed producer is not running in this
process, nothing is created:
```json
{ "configured": false, "reason": "KILLFEED_UNAVAILABLE" }
```

`configured: false` responses are deliberately **not** the
`{"error":{"code","message"}}` envelope (see `## Error contract` below) -
they're expected, UI-renderable outcomes, not failures to retry blindly.

Auto-setup never replaces `#13` (verify-permissions) - Step 6 on the
website still runs permission verification against whichever channel IDs
ended up configured, whether by `#20` or `#24`/`#21`. Step 6 should verify
every **unique** configured channel ID once, not once per route -
`ChannelRouteRepository.ListDistinctChannelIDs` exists specifically for
this (see "Channel routing" below).

### 23. `GET .../installations/{installationID}/channel-routes`
The full routing model's read side (section 11) - returns every currently
configured route, keyed by `route_key`. Any member may call it. Required for
refresh persistence, same as `#19`.

Response `200` ([`ChannelRoutesResponse`](#channelroutesresponse)):
```json
{
  "routes": {
    "KILLFEED": { "channelId": "111", "channelName": "killfeed", "managedByChampion": true },
    "BOUNTY": { "channelId": "117", "channelName": "bounty", "managedByChampion": false }
  }
}
```
A route absent from `routes` means it isn't configured (disabled/never set)
- never a placeholder empty entry. `channelName` is a best-effort live
lookup and may be omitted if the channel was deleted in Discord;
`channelId` is always the persisted, authoritative value regardless.

### 24. `PUT .../installations/{installationID}/channel-routes`
Saves a manual (Customize mode) routing selection (section 9/11/12).
OWNER/ADMIN only. This is the full-scale successor to `#20` - `#19`/`#20`
remain available unchanged for any consumer that hasn't migrated.

Request - a **partial merge** keyed by `route_key`, never requiring every
route (section 9/10):
```json
{
  "routes": {
    "KILLFEED": "111",
    "BOUNTY": "117",
    "PVE_FEED": ""
  }
}
```
- A key present with a **non-empty** channel ID upserts that route,
  always as `managedByChampion=false` (an explicit customer selection is
  never re-labeled Champion-managed, even if it happens to match a
  channel Champion previously created for a *different* route).
- A key present with an **empty string** explicitly disables/removes that
  route (section 9 "disable optional routes").
- A key **absent** from the request is left completely untouched - you
  never have to resend every route to change one.

The **same channel ID may serve multiple routes** (section 9) - `KILLFEED`
and `BOUNTY` may both point at the same Discord channel.

Every supplied non-empty channel ID is **re-verified live** against the
installation's own Discord guild before anything is persisted (section 12)
- `400 INVALID_REQUEST` if it doesn't belong to this guild or isn't a
supported text-capable channel; unknown `route_key`s are also rejected the
same way.

`KILLFEED` remains required (section 10): a request whose *resulting*
merged state would leave `KILLFEED` unset is rejected with
`400 INVALID_REQUEST` before anything is written - nothing is partially
applied.

Response `200`: the same shape as `#23`'s GET, reflecting the full,
now-updated route set. On success, `channelsCompleted=true` and
`currentStep=VALIDATION` advance exactly like `#20`, and the installation
advances to `CONFIGURING` under the same one-directional rule.

## Channel routing

Champion routes each feature independently via a stable `route_key` - never a
display name (`installation_channel_routes`, migration 0027,
`docs/SAAS_SCHEMA.md`). **A feature route is not a Discord channel**: several
routes share one destination channel (Channel System V2). `#19`/`#20` (the
legacy four-field surface) still work exactly as before.

### Default layout (Channel System V2)

| Category | Channel | Routes |
|---|---|---|
| `🏆 CHAMPION • LIVE` | `🔫・combat-feed` | `KILLFEED`, `PVE_FEED` |
| | `🎯・hitfeed` | `HITFEED` |
| | `💀・bounties` | `BOUNTY`, `BOUNTY_TRACKING` |
| | `🟢・connections` | `CONNECTIONS` |
| | `🗺️・heatmaps` | `HEATMAPS` (PvP heatmap summary) |
| `🏆 CHAMPION • HUB` | `📡・server-status` | `SERVER_STATUS` (+ season/war/event results) |
| | `📊・leaderboards` | `AUTO_LEADERBOARD`, `STATS_LEADERBOARDS` |
| | `🔗・player-link` | `LINK_GAMERTAG` |
| | `💰・economy` | `ECONOMY`, `SHOP` |
| | `🟢・Online Players: N` (voice) | `ONLINE_COUNTER` |
| `🔒 CHAMPION • STAFF` (private) | `🛡️・admin-logs` | `ADMIN_LOGS`, `ADMIN_ALERTS`, `BUILD_FEED` |

There is no separate `casino`, `shop`, `build-feed`, `admin-alerts`,
`bounty-tracking`, `stats-leaderboards`, `auto-leaderboard`, `death-feed` or
`pvefeed` channel. `CASINO` does not exist (migration 0044 removed stored
`CASINO` routes and templates). The legacy death feed shares the combat feed:
with a `KILLFEED` route the death feed posts there, and the legacy death
channel is only the fallback for guilds without routes.

**One setup engine.** One-click setup (`#21`), Repair (`#34`) and the Discord
`/setup run|repair` command all run the same planner
(`championDestinations`). `/setup` applies it, with repair semantics, to
every installation connected to the guild; a guild with no installation is
told to connect on the website. The legacy `SetupManager` never creates a
channel any more - it only re-posts a missing legacy panel inside a legacy
channel that still exists, for guilds whose feature is not routed.

### Route producers

Audited against the runtime (`routeProducerAudit` in
`internal/app/saas_channel_layout.go`); at setup, a producer that is not
instantiated in the process (routing disabled, service absent) is reported
`BROKEN` instead.

| `route_key` | Producer | State |
|---|---|---|
| `KILLFEED` | `KillfeedPublisher` (per server worker) | ACTIVE - **required** |
| `PVE_FEED` | `PveFeedPublisher` - explicit suicides only; the ADM parser cannot yet tell infected/animal/environment causes apart | ACTIVE |
| `HITFEED` | `HitfeedPublisher` - aggregated, rate-capped | ACTIVE |
| `BOUNTY` | `BountyBoard` persistent board (`docs/BOUNTY_SYSTEM.md`) | ACTIVE |
| `BOUNTY_TRACKING` | `BountyTracker` lifecycle feed | ACTIVE |
| `CONNECTIONS` | `ConnectionsPublisher` - bounded, batched | ACTIVE |
| `HEATMAPS` | `HeatmapBoard` persistent PvP summary from the Phase 5 aggregates (`docs/HEATMAPS.md`) | ACTIVE |
| `SERVER_STATUS` | `ServerStatusBoard` - one persistent message per routed channel, edited in place: Champion's ADM link (CONNECTED / LOCATING LOG / DEGRADED / WAITING FOR FIRST POLL), ADM log freshness and the tracked online count. No uptime, latency or game-server power state (not measured) | ACTIVE |
| `ONLINE_COUNTER` | `VoiceChannelCounter` - a display-only voice channel renamed to the online count; created/reused by setup (recognized by its name prefix, never duplicated) | ACTIVE |
| `AUTO_LEADERBOARD` | `LeaderboardScheduler` persistent leaderboard | ACTIVE |
| `STATS_LEADERBOARDS` | `RouteSyncer` "My Stats / Search Player" panel | ACTIVE |
| `LINK_GAMERTAG` | `RouteSyncer` link panel | ACTIVE |
| `ECONOMY` | `EconomyFeed` | ACTIVE |
| `SHOP` | shop purchase/refund events, published by `EconomyFeed` on the `ECONOMY` route | ACTIVE |
| `ADMIN_LOGS` | `ADMMonitorPublisher` ADM health | ACTIVE |
| `ADMIN_ALERTS` | `AdminAlertPublisher` - ADM stale, repeated Nitrado download failures, zone/UAV/base radar intrusions (`docs/ADMIN_LOGS.md`) | ACTIVE |
| `BUILD_FEED` | `BuildFeedPublisher` - ADM placement/build lines (`docs/ADMIN_LOGS.md`) | ACTIVE once a build line has been parsed in this process; otherwise BLOCKED - `SOURCE_BLOCKED` (the server must enable `adminLogPlacement` / `adminLogBuildActions`) |

The runtime panel owners (`RouteSyncer`, `BountyBoard`, `HeatmapBoard`,
`ServerStatusBoard`, `LeaderboardScheduler`, `EconomyFeed`, the voice counter) serve the bot's configured guild. An
installation in any other guild will see its panel channels reported
`BROKEN` (no visible panel) by auto-setup's verification, never silently
passed.

**Backward-compatible backfill** (migration 0027, one-time, additive): any
existing installation's legacy `installation_settings` values were copied
into the new table so they're visible under `#23` immediately -
`killfeed_channel_id` -> `KILLFEED`, `leaderboard_channel_id` ->
`STATS_LEADERBOARDS`, `admin_log_channel_id` -> `ADMIN_LOGS`.
`player_status_channel_id` also backfills into `STATS_LEADERBOARDS`, but
only when `leaderboard_channel_id` is unset - audited, not guessed:
`player_status_channel_id` decouples from `guilds.player_stats_channel_id`
(`internal/discord/setup_store.go`'s `PlayerStatsChannelID`), which is the
same on-demand "general statistics" panel `STATS_LEADERBOARDS` describes,
not a connections/join-leave log. `CONNECTIONS` has no legacy source at all
for exactly that reason.

### 33. `GET .../installations/{installationID}/channels/layout`
Read-only Channel System V2 status (any member): every destination with its
plan, the channel its routes point at, and the same checks setup runs
(channel exists, route mapped, producer connected, bot can view/send/embed,
visible Champion content). Nothing is created or posted.

```json
{ "routes": { "...": "..." }, "destinations": [ "...ChannelDestinationReport..." ], "retirable": [ "...RetirableChannel..." ] }
```

A destination that should exist but has no route is `BROKEN` with
`"not set up yet - run Repair Champion Discord Layout"`; a routed channel
deleted in Discord is `BROKEN` with `"the Discord channel no longer exists"`.

### 34. `POST .../installations/{installationID}/channels/repair`
**Repair Champion Discord Layout** (OWNER/ADMIN). The same engine as `#21`,
but it runs over customer routing and **preserves** every customer-owned
route untouched; a destination served entirely by a customer channel gets
no Champion channel. Same response as `#21`, including `summary`:

```json
{ "created": ["📡・server-status"], "reused": ["🔫・combat-feed"], "remapped": [], "panelsRepaired": ["LEADERBOARDS"],
  "startersSent": [], "preserved": ["KILLFEED"], "broken": [], "blocked": [], "retirable": 3 }
```

A second run changes nothing (no creates, remaps, starter cards or panels).

### 35. `POST .../installations/{installationID}/channels/cleanup`
**Clean Up Retired Champion Channels** (OWNER/ADMIN). Request
`{ "channelIds": ["..."] }` - the channels the customer confirmed from the
`retirable` list. Deletes a channel only when **all** hold:

- it is recorded in `installation_retired_channels` for this installation
  (setup/repair recorded it: a Champion-managed route used to point at it,
  it is the pre-V2 Champion category, or the legacy `/setup` created it and
  a V2 route replaced it) - never decided by name;
- no route of any installation references it;
- it still exists with the recorded kind;
- a category is empty (channels are deleted before categories).

Everything else is returned in `skipped` with a reason (`NOT_RETIRABLE`,
`REFERENCED`, `NOT_EMPTY`, `GONE`, `DISCORD_ERROR`). Deleting a legacy
`/setup` channel also clears its legacy `GuildSetup` pointer.

```json
{ "deleted": [{ "channelId": "77", "channelName": "casino", "kind": "CHANNEL", "source": "ROUTE", "managedByChampion": true }], "skipped": [{ "channelId": "88", "reason": "NOT_RETIRABLE" }] }
```

### 22. `POST .../installations/{installationID}/discord/channels`
Optional, explicit "create a new channel" action for Customize mode -
lets a customer create a channel Champion doesn't already know about
without leaving the setup flow. OWNER/ADMIN only.

Request:
```json
{ "name": "PvP Feed", "categoryId": "999" }
```
`categoryId` is optional; when supplied it must already belong to **this
installation's own Discord guild** (`400 INVALID_REQUEST` otherwise - never
trusted blindly, and structurally impossible to target another guild since
the guild always comes from the installation, never from the request).
`name` is normalized into a Discord-safe channel name (lowercased,
whitespace collapsed to single hyphens, unsupported characters dropped) -
e.g. `"PvP Feed"` becomes `"pvp-feed"`. `400 INVALID_REQUEST` if the name
normalizes to empty or exceeds 100 characters. `503 DISCORD_UNAVAILABLE` if
the bot lacks Manage Channels.

Response `201` ([`DiscordChannelSummary`](#discordchannelsummary)):
```json
{ "id": "333", "name": "pvp-feed", "type": "TEXT", "position": 0, "canSend": true }
```

## Setup completion, customer hub, and revalidation

**The Go backend is the sole authority on whether setup is complete** - the
website may request finalization, but every prerequisite below is
independently re-derived from persisted backend state (plus one live,
multi-channel Discord permission check). A client-supplied
`{"discordCompleted": true}`-style claim is never trusted for this.

### 25. `POST .../installations/{installationID}/setup/complete`
Finalizes onboarding. OWNER/ADMIN only.

Checks, in order, all from persisted backend state:
1. Installation belongs to the organization (`404 NOT_FOUND` otherwise).
2. **Idempotent short-circuit**: if the installation is already `READY`,
   returns success immediately - none of the checks below re-run, and
   nothing is re-stamped (section 7).
3. Discord connection exists and Champion is actually installed in the
   guild (live check, same as `#12`).
4. Nitrado is connected for the organization.
5. A DayZ server is selected.
6. A `KILLFEED` channel route is configured (the route model - only
   `KILLFEED` is globally required; no other route blocks finalization
   merely because it exists in the default blueprint).
7. The persisted setup-progress flags (`discordCompleted`, `nitradoCompleted`,
   `serverSelected`, `channelsCompleted`) are all true.
8. **Multi-channel permission check**: every *unique* Discord channel ID
   across all configured routes (`ChannelRouteRepository.
   ListDistinctChannelIDs` - a channel shared by several routes is verified
   exactly once) must pass the same blocking-capability check `#13`
   (verify-permissions) uses - **View Channel** and **Send Messages** block;
   **Embed Links** and **Read Message History** remain warnings and never
   block finalization.

Any failed check returns `422 INSTALLATION_NOT_VERIFIED` with a message
naming the specific unmet prerequisite - never a raw Discord error, never a
5xx for an ordinary "not ready yet" state.

On success: `installation_setup_progress.validation_completed=true`,
`current_step='COMPLETE'`, `completed_at=NOW()` (only if it was still
`NULL`); `installations.status='READY'`, `setup_completed_at=NOW()` (only
if it was still `NULL`) - both via the existing, already-idempotent
`UpdateSetupProgress`/`UpdateStatus` repository methods, which have carried
this "stamp once" behavior since the setup-progress table was first
introduced.

Response `200` ([`FinalizeSetupResponse`](#finalizesetupresponse)):
```json
{
  "completed": true,
  "installation": { "id": 4, "status": "READY", "health": "HEALTHY", "...": "..." }
}
```

### 26. `GET .../installations/{installationID}/hub`
Everything the customer landing page needs in one call. Any member may
read. Reuses existing DTOs/repositories throughout - never a duplicate
installation/organization model.

Response `200` ([`HubSummary`](#hubsummary)):
```json
{
  "organization": { "id": 1, "name": "...", "slug": "...", "role": "OWNER" },
  "subscription": { "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "..." },
  "installation": { "id": 4, "status": "READY", "health": "HEALTHY", "...": "..." },
  "discord": { "guildName": "...", "guildIcon": "...", "botInstalled": true },
  "dayzServer": { "id": 7, "serviceId": 111111, "displayName": "...", "platform": "PLAYSTATION", "status": "ONLINE" },
  "channelRoutes": { "KILLFEED": { "channelId": "111", "channelName": "killfeed", "managedByChampion": true } },
  "settings": { "timezone": "UTC", "distanceUnit": "METERS", "onlineDisplayEnabled": true, "leaderboardEnabled": true }
}
```
`discord`/`dayzServer`/`subscription` are best-effort and omitted (never a
hard failure) if that piece isn't configured yet - a not-yet-READY
installation can still open its hub without a 404, though the website is
expected to route a non-READY installation back to onboarding (see
"Completed installation detection" below), not rely on the hub to enforce
that. Never returns a Nitrado token, encrypted credential, Discord bot
token, `WEBSITE_API_SECRET`, or database detail.

### 27. `GET .../installations/{installationID}/settings`
### 28. `PUT .../installations/{installationID}/settings`
The customer-editable general settings surface (sections 10/11) -
deliberately separate from the channel-specific endpoints (`#19`/`#20`/
`#23`/`#24`), never overloaded onto them. Any member may `GET`; OWNER/ADMIN
only for `PUT`.

Request/response `200` ([`InstallationGeneralSettings`](#installationgeneralsettings)):
```json
{ "timezone": "America/New_York", "distanceUnit": "METERS", "onlineDisplayEnabled": true, "leaderboardEnabled": true }
```
`timezone` is required (non-empty); `distanceUnit` must be `METERS` or
`FEET` (case-insensitive on input, normalized to uppercase) -
`400 INVALID_REQUEST` otherwise. This column exists but isn't yet wired
into killfeed embed rendering (which computes distance in meters natively -
`internal/presentation/story_engine.go`), so it's forward-looking, not yet
runtime-authoritative.

### Completed installation detection
An installation is complete when, and only when, **`status == "READY"`**
(`#7`/`#8`'s `InstallationSummary.status`, or `#26`'s
`installation.status`) - this already implies persisted setup completion
is valid, since `status` only ever reaches `READY` via `#25`'s checks
above. The website should never infer completion from UI history or from
having seen a completed step earlier in the same session - always check
the current `status`, freshly read.

### Reconfigure flow (never a reset)
A `READY` installation should never automatically return to the onboarding
wizard. "Reconfigure" is simply opening the existing, already read-only-safe
GET endpoints for an individual section - Discord (`#12`), DayZ server
(`#17`'s validate, or re-select via `#16`), Channels (`#19`/`#23`), General
Settings (`#27`) - none of which touch `validationCompleted`, `completedAt`,
or `status` merely by being read, or (for non-critical writes) by being
saved. There is no separate "enter reconfigure mode" endpoint or flag -
GET-then-optionally-PUT on the relevant section is the entire model.

### Revalidation rules (critical vs. non-critical changes)
A `READY` installation's already-passed permission verification is only
ever trusted for the **exact channels it was verified against**. Changing
one of those channels means the old verification result no longer applies,
so these specific changes **downgrade** an already-`READY` installation
back to `validationCompleted=false`, `currentStep=VALIDATION`,
`status=CONFIGURING` (never all the way back to step 1, and never erasing
`completedAt`, which is only ever stamped the first time - see `#25`):

| Change | Endpoint | Critical? |
|---|---|---|
| The `KILLFEED` route's channel changes | `#20` (legacy) or `#24` (routes) | **Yes** |
| `#21` (auto-setup, `force=true`) resolves a different `KILLFEED` channel than before | `#21` | **Yes** |
| A different DayZ server is selected | `#16` | **Yes** |
| Any **other** route's channel changes (e.g. `BOUNTY`, `ADMIN_LOGS`) | `#24` | No |
| Timezone, distance unit, online-display/leaderboard toggles | `#28` | No |
| Re-saving the SAME `KILLFEED` channel/server (no actual change) | any of the above | No (no-op) |

Only `KILLFEED` and the selected DayZ server are wired to this downgrade
today, matching this task's explicit critical-change examples and its own
test requirements. A **Discord guild** change has no corresponding
operation in this API at all - an installation's `discord_guild_connection_id`
is never repointed after creation (the duplicate-installation reuse flow,
`#16`'s "Same-guild reuse", creates or reuses a *different* installation
instead) - so there is nothing to invalidate for that case; it's called out
here for completeness, not left silently unhandled.

## Embed templates (persistence only)

Embed Designer Phase 2: durable, tenant-safe storage of custom embed templates, one per
`(installation, routeKey)`.

> **CUSTOM TEMPLATE PERSISTENCE: LIVE**
> **CUSTOM TEMPLATE RUNTIME RENDERING: behind `CHAMPION_CUSTOM_EMBEDS_ENABLED` (default OFF)**
>
> With the flag off no Discord publisher reads these rows and saving, changing or deleting a
> template never changes any Discord message. With it on, the routes listed in `runtimeRoutes`
> (`KILLFEED`, `HITFEED`, `PVE_FEED`, `BOUNTY_TRACKING`, `ECONOMY`, `CONNECTIONS` single-event
> cards) render the saved template - the next event after a save/reset - and fall back to the
> existing card on any problem. Every response carries `runtimeRendering`: `ENABLED` only when
> the flag is on and the route is one of those, else `NOT_ENABLED`, so a client never presents a
> saved template as live when it is not. See `docs/EMBED_RUNTIME.md`.

Authorization (same chain as every customer route: service auth -> acting user ->
organization membership -> installation ownership):

| Endpoint | Who |
|---|---|
| `GET` list / one | any member (OWNER, ADMIN, MEMBER) - the same read convention as `channel-routes` and `settings` |
| `PUT`, `DELETE` | OWNER or ADMIN only (MEMBER is `403`) |

The installation must belong to the organization in the path: an installation of another
organization, and an unknown installation, are the same `404 NOT_FOUND` (the installation id
alone never selects a row). Templates belong to one installation only - two installations of
one guild (two DayZ servers) are fully independent.

### 29. `GET .../installations/{installationID}/embed-templates`

Only the **customized** routes are stored, so only those are returned (Champion defaults are
never copied into the database).

```json
{
  "installationId": 9,
  "templates": [ { "routeKey": "KILLFEED", "customized": true, "template": { "...": "..." },
                   "variables": ["killer", "victim"], "createdAt": "...", "updatedAt": "...",
                   "runtimeRendering": "NOT_ENABLED" } ],
  "customizedRoutes": ["KILLFEED"],
  "variables": { "KILLFEED": ["killer", "victim", "weapon", "distance", "ammo", "streak", "server_name", "timestamp"], "...": [] },
  "limits": { "title": 256, "description": 4096, "fields": 25, "fieldLabel": 256, "fieldValue": 1024, "footerText": 2048, "authorName": 256, "totalText": 6000 },
  "runtimeRendering": "NOT_ENABLED"
}
```

`variables` (approved placeholders per route) and `limits` are authoritative and shared with
the validator, so the designer can stay in step with the server.

### 30. `GET .../installations/{installationID}/embed-templates/{routeKey}`

The stored template, or the not-customized state (the backend does not fabricate a default):

```json
{ "routeKey": "KILLFEED", "customized": false, "template": null, "variables": ["killer", "..."],
  "createdAt": null, "updatedAt": null, "runtimeRendering": "NOT_ENABLED" }
```

An unknown `routeKey` is `400 INVALID_REQUEST`.

### 31. `PUT .../installations/{installationID}/embed-templates/{routeKey}`

OWNER/ADMIN. Body = the website's `EmbedTemplate` (see the model below). Validated, normalized
and upserted (`(installation_id, route_key)` is unique; concurrent saves converge on one row;
`created_at` is kept, `updated_at` is refreshed). The response is the **normalized stored**
template in the same shape as #30 with `customized: true`.

Errors: `400 INVALID_REQUEST` with a message listing up to five `path: problem` issues (never
echoing the input), `413 PAYLOAD_TOO_LARGE` (body over 64 KiB), `403`, `404`.

### 32. `DELETE .../installations/{installationID}/embed-templates/{routeKey}`

OWNER/ADMIN. Resets that one route to the Champion default by deleting only that
installation+route row. **Idempotent**: `200` with the not-customized state (as in #30)
whether or not a custom template existed.

**Variable metadata (V2.1).** `#29`/`#30` also return `variableDefinitions` (per route / per
variable: `name`, `label`, `description`, `category`, `example`, `optional`, `availability`,
`format`, `source`) beside the unchanged `variables` name lists. Template text supports line
breaks (`\n`, real newlines; `\\n` is literal). Full tables: `docs/EMBED_DESIGNER_V2.md`.

### 36. `POST .../installations/{installationID}/embed-templates/{routeKey}/preview`

Embed Designer V2 (`docs/EMBED_DESIGNER_V2.md`). Any member. Renders an **unsaved** draft
`{ "template": EmbedTemplate, "variables": { name: sample } }` with the production renderer
(`embedrender.RenderEvent`, the same function live events use) and returns the rendered embed DTO,
`metrics` (`fieldCount`, `totalText`), `warnings`, `renderable` (+ `reason`:
`CUSTOM_TEMPLATE_DISABLED` \| `TEMPLATE_PRODUCED_NO_CONTENT`), `runtimeRendering`,
`customRenderingSupported` and the route's `destination` preflight. Saves nothing, sends nothing.
Unknown body keys (`channelId` ...) are rejected with `EMBED_TEMPLATE_INVALID`.

### 37. `POST .../installations/{installationID}/embed-templates/{routeKey}/test`

OWNER/ADMIN. Renders the same **unsaved** draft and sends it, with a "design preview - not a live
server event" notice and mentions disabled, to the channel the installation's own route resolves
to (never a channel from the request). Only `embedrender.SupportedRoutes()`; allowed while the
rollout flag is off (the response says `runtimeRendering: NOT_ENABLED`). Rate limited to one send
per 3 s per actor and 10 per minute per installation. Returns `{ sent, routeKey, runtimeRendering,
channel: {id, name}, messageId, sentAt }`. Error codes: `EMBED_TEMPLATE_INVALID` (400),
`EMBED_TEMPLATE_NOT_RENDERABLE` / `EMBED_CUSTOM_RENDERING_NOT_SUPPORTED` (422),
`EMBED_ROUTE_NOT_CONFIGURED` / `EMBED_TEST_CHANNEL_UNAVAILABLE` / `EMBED_TEST_SEND_FORBIDDEN` /
`EMBED_TEST_EMBED_LINKS_REQUIRED` (409), `EMBED_TEST_RATE_LIMITED` (429), `EMBED_TEST_SEND_FAILED`
(502). Writes a safe `embed_test_sent` audit row (ids, route, channel, message - never template
text or sample values). No gameplay effects of any kind.

### Template model (`EmbedTemplate`)

```json
{
  "routeKey": "KILLFEED", "enabled": true, "color": "#D4AF37",
  "title":       { "enabled": true, "template": "{{killer}} eliminated {{victim}}" },
  "description": { "enabled": true, "template": "A {{weapon}} kill from {{distance}} away." },
  "author":      { "enabled": false, "name": "Champion", "iconUrl": "" },
  "thumbnail":   { "enabled": false, "url": "" },
  "image":       { "enabled": false, "url": "" },
  "footer":      { "enabled": true, "text": "Champion Killfeed", "iconUrl": "" },
  "timestamp": true,
  "fields": [ { "key": "killer", "label": "Killer", "enabled": true, "template": "{{killer}}", "inline": true, "order": 0 } ]
}
```

The server adds `"version": 1` (the stored schema version; ignored if sent). It is a fixed,
typed structure: an unknown key anywhere is rejected (`400`), never stored. Omitted sections
decode as disabled/empty.

### Validation (server-side; the website's checks are not trusted)

| Rule | Limit |
|---|---|
| `title.template` | <= 256 characters |
| `description.template` | <= 4096 |
| `fields` | <= 25; `key` 1-64 of `A-Za-z0-9_-`, unique (case-insensitive); `order` 0-10000 (fields are sorted by `order`, stable, and renumbered `0..n-1`) |
| field `label` / `template` | <= 256 / <= 1024 |
| `footer.text` / `author.name` | <= 2048 / <= 256 |
| combined enabled text | <= 6000 (title + description + field labels/values + footer + author name, counted as written; the runtime must also truncate rendered output when it is enabled) |
| lengths | counted in characters (runes), inclusive |
| `color` | exactly `#RRGGBB` (any case; stored upper-case). `0x...`, decimal, names, 3/8-digit hex are rejected, so a value can never wrap or overflow a Discord color |
| URLs (`author.iconUrl`, `thumbnail.url`, `image.url`, `footer.iconUrl`) | `http`/`https` only, with a host, <= 2048 chars, no whitespace/control characters, no embedded credentials, no placeholders. `javascript:`, `data:`, `file:` and malformed URLs are rejected. Required when the section is enabled (icons are optional) |
| enabled sections | `title`, `description`, `author.name`, `footer.text`, field label and template must be non-empty when enabled; an enabled embed needs at least one enabled title/description/field/author/footer/image |
| control characters | rejected (newline, tab and carriage return are allowed) |
| request body | <= 64 KiB (`413`) |

### Variables and template safety

Text is plain text with `{{name}}` placeholders (inner spaces are accepted and canonicalized
to `{{name}}`). Only the approved variables of the route in the URL are accepted:

| Route | Variables |
|---|---|
| `KILLFEED` | `killer` `victim` `weapon` `distance` `ammo` `streak` `special_kill` `bounty_amount` `server_name` `timestamp` |
| `PVE_FEED` | `victim` `cause` `server_name` `timestamp` |
| `HITFEED` | `killer` `attacker` `victim` `weapon` `ammo` `distance` `hit_zone` `damage` `hits` `server_name` |
| `BOUNTY` | `victim` `server_name` `timestamp` |
| `BOUNTY_TRACKING` | `killer` `victim` `target` `hunter` `amount` `total` `count` `weapon` `distance` `status` `server_name` |
| `ECONOMY` | `player` `amount` `balance` `transaction_type` `server_name` |
| `SHOP` | `player` `item` `amount` `balance` |
| `CONNECTIONS` | `player` `event` `event_type` `session` `server_name` `timestamp` |
| `BUILD_FEED` | `player` `structure` `server_name` |
| `ADMIN_ALERTS`, `ADMIN_LOGS` | `event` `player` `server_name` `timestamp` |
| `HEATMAPS`, `LINK_GAMERTAG`, `STATS_LEADERBOARDS`, `AUTO_LEADERBOARD` | `server_name` `timestamp` (no event vocabulary yet) |

An unknown variable, a variable that belongs to another route, or a wrong-case name is
rejected. **Every other brace is reserved**: `{{killer}`, `{killer}`, `{{ .Field }}`,
`{{killer | upper}}`, `{{killer()}}`, `${killer}`, `{% ... %}` are all `400`. There is no
expression language - no functions, conditionals, HTML execution or evaluation of any kind;
the text is only ever substituted. Route keys are the fixed channel-route set (the same list
as `championDestinations`; a test keeps them identical) - arbitrary strings are rejected.

### Website compatibility

The wire model is the website's Phase 1 `EmbedTemplate` (`lib/saas/embedTypes.ts`); a test
feeds all 12 of the designer's default templates through the strict decoder and validator
and checks every designer variable is approved. Differences, documented rather than
accepted unsafely:

| Website behavior | Backend |
|---|---|
| `{{ killer }}` with inner spaces | accepted, stored as `{{killer}}` |
| URL check is `^https?://` only | also requires a host, no credentials/whitespace/placeholders, <= 2048 |
| total-text check uses sample values | counted on the template as written (up to 6000) |
| `order` values may repeat or have gaps | sorted (stable) and renumbered `0..n-1` |
| routes: 12 | also `HEATMAPS`, `LINK_GAMERTAG`, `STATS_LEADERBOARDS`, `AUTO_LEADERBOARD` (the rest of the channel-route set) |
| variables | additionally `ammo`, `streak` (KILLFEED) and `transaction_type` (ECONOMY) - values the runtime events carry |

Audit events (`embed_template_saved`, `embed_template_deleted`) log `organization_id`,
`installation_id`, `route_key` and `acting_user_id` only - never template contents or headers.

## Faction Hub (Phase 1, backend only)

The installation-scoped faction directory, membership and recruitment API. Full model, rules and
rationale: `docs/FACTIONS.md`; machine-readable contract: `docs/saas-openapi.yaml`.
All routes are under `/api/saas/organizations/{organizationID}/installations/{installationID}/factions`.

Authorization differs from the rest of this API on purpose: **players are not organization members**, so
reads and player actions (found, apply, withdraw) need only service auth and a synced acting user - not
organization membership. The installation must belong to the path's organization (otherwise `404`).
Faction mutations are decided **only by the acting user's faction role** (LEADER/OFFICER), read inside the
mutating transaction; an organization OWNER/ADMIN gets a read-only view of a faction's applications and
nothing more, and the platform-admin allowlist is not consulted.

| Route | Who |
|---|---|
| `GET .../factions` (`recruiting`, `q`, `limit`, `cursor`) | any synced user |
| `POST .../factions` (found; `201`) | any synced user |
| `GET .../factions/me` | any synced user |
| `GET .../factions/{factionID}` | any synced user |
| `PUT .../factions/{factionID}` | faction LEADER |
| `POST .../factions/{factionID}/applications` (`201`) | any synced user |
| `GET .../factions/{factionID}/applications` (`status`, `limit`, `cursor`) | LEADER/OFFICER; org OWNER/ADMIN read-only |
| `POST .../applications/{applicationID}/accept` \| `deny` | LEADER/OFFICER |
| `POST .../applications/{applicationID}/withdraw` | the applicant |
| `GET .../factions/{factionID}/members` | any synced user |
| `POST .../members/{memberID}/promote` \| `demote` | LEADER |
| `DELETE .../members/{memberID}` | LEADER (MEMBER, OFFICER); OFFICER (MEMBER) |
| `POST .../factions/{factionID}/logo` (multipart `file`; `201` new, `200` replaced) | faction LEADER |
| `DELETE .../factions/{factionID}/logo` (idempotent) | faction LEADER |
| `POST .../factions/{factionID}/transfer-leadership` (`{"memberId"}`) | faction LEADER |
| `POST .../factions/{factionID}/leave` | the member (MEMBER/OFFICER; LEADER gets `409 LEADERSHIP_TRANSFER_REQUIRED`) |
| `GET /assets/faction-logos/{publicId}.{png\|jpg\|webp}` (public, no auth) | anyone |

**Phase 4 (logo storage, leadership transfer, self-leave)** - full contract in `docs/FACTIONS.md`:

* Every faction summary/profile/`me` object carries `logo`: `null` (Champion default) or
  `{ "id", "url", "contentType", "width", "height" }`. The upload returns `{ "logo": {...}, "replaced": bool }`, the delete
  `{ "logo": null, "deleted": bool }`, the transfer `{ "leader": member, "previousLeader": member }`, the leave
  `{ "left": true, "member": member }`.
* Upload is `multipart/form-data` with one part named `file`: PNG, JPEG or WebP only, at most 5 MiB, 128-2048 px per side, decided from
  the bytes (not the name or declared type). Anything else is `415 UNSUPPORTED_MEDIA_TYPE` (format, contradicting declared type, or a
  non-multipart body), `413` (too large) or `400` (empty, corrupt, bad dimensions, extra fields). Faction LEADER only, checked before the
  body is read. Rate limited: 3 / minute and 20 / day per user. The website must send it server-side with the service secret.
* `PUT .../factions/{factionID}` now accepts `flagKey` (`BLACK BLUE GREEN RED`) and `armbandKey` (`BLACK BLUE GREEN ORANGE PINK RED
  WHITE YELLOW`), validated server-side and normalized to upper-case (`""` clears); colors must be exactly `#RRGGBB`. `logoKey`,
  `logoUrl`, `logoAssetId` and any URL are still rejected.
* New error codes: `UNSUPPORTED_MEDIA_TYPE` (415), `LEADERSHIP_TRANSFER_REQUIRED` (409).

**Phase 5 (competitive stats, achievements, activity)** - full contract, attribution rules and DTOs in `docs/FACTION_STATS.md`. Three read-only routes, open to any
synced user and scoped by organization + installation + faction (another tenant's ids are `404`):

| Route | Returns |
|---|---|
| `GET .../factions/{factionID}/stats` | `{ "summary": {kills, deaths, kdRatio, headshots, longshots, currentKillStreak, bestKillStreak, bountiesClaimed, bountyValueClaimed, memberCount, linkedMemberCount, achievementsUnlocked, trackingSince}, "memberContributions": [...], "updatedAt" }` |
| `GET .../factions/{factionID}/activity?limit=&cursor=` | `{ "items": [event...], "nextCursor": string\|null, "limit" }` - public-safe events, newest first, `limit` default 20 / max 100 |
| `GET .../factions/{factionID}/achievements` | `{ "items": [{key, name, description, unlocked, unlockedAt, progress, target, unit}...], "unlockedCount", "total" }` |

The faction profile (`GET .../factions/{factionID}` and the create/update responses) now includes `stats` - the same summary, or `null` if it could not be computed.
Figures come only from real kills, deaths and bounties, attributed through a member's **verified** DayZ link and only while they were a member; unlinked members are
listed with zero figures and `identity: "UNLINKED"`.

**Phase 6 (faction leaderboards)** - full contract, tie-breaks, tracking semantics and DTOs in `docs/FACTION_LEADERBOARDS.md`. One read-only route, open to any synced
user and scoped by organization + installation (a mismatched pair is `404`):

| Route | Returns |
|---|---|
| `GET .../factions/leaderboard?metric=&limit=&cursor=&q=` | `FactionLeaderboardResponse`: `{ "metric", "direction": "DESC"\|"ASC", "items": [FactionLeaderboardEntry...], "nextCursor": string\|null, "limit", "total", "updatedAt" }` |

`metric` is one of `KILLS` (default), `DEATHS` (ascending - fewest first), `KD`, `HEADSHOTS`, `LONGSHOTS`, `BEST_STREAK`, `BOUNTIES_CLAIMED`, `BOUNTY_VALUE`, `ACHIEVEMENTS`;
any other value, a bad `limit` (default 25, max 100, clamped), a bad or other-metric `cursor`, an over-long `q` or a malformed query string is `400`. `q` matches the faction
name or tag case-insensitively and never changes a faction's `rank`. Each entry is `{ rank, factionId, name, tag, slug, logo, flagKey, armbandKey, primaryColor,
secondaryColor, memberCount, value, trackingSince, hasTrackedActivity, stats }` - `value` is an integer except for `KD`; `stats` repeats every figure using the profile's
names. Factions with no tracked activity are listed with zeros (`hasTrackedActivity: false`). No overall score and no season fields exist. The result is cached 45 s per
installation; the literal `/leaderboard` route takes precedence over `/{factionID}`.

Errors use the standard envelope. `409 CONFLICT` covers state conflicts (name or tag taken on the
installation, already in a faction there, not recruiting, duplicate/non-pending application, invalid role
change, no DayZ server selected, suspended installation); `403` means the faction role does not allow it or
the leader is protected; `404` also covers another tenant's ids; `413` a body over 16 KiB; `429` `RATE_LIMITED`.
Lists use the keyset envelope `{ "items": [], "nextCursor": null, "limit": 25 }` (newest first).
JSON request bodies must be one JSON object with **no unknown keys**: image URLs, `logoKey`, ids and
`answers` are rejected with `400` (`flagKey`/`armbandKey` are accepted only as approved catalog keys - Phase 4).

Audit events (`faction_created`, `faction_updated`, `faction_application_created|accepted|denied|withdrawn`,
`faction_member_promoted|demoted|removed`, and Phase 4: `faction_logo_uploaded|replaced|deleted`,
`faction_leadership_transferred`, `faction_member_left`) log ids only - never names, descriptions, application
messages, file names or image bytes.

## Champion Economy (web foundation)

Installation-scoped views of the existing **Champion Points** economy (currency `{code:"CHAMPION_POINTS", name:"Champion Points", symbol:"pts"}`), all under
`/api/saas/organizations/{organizationID}/installations/{installationID}/economy`. Full contract, guarantees and DTOs: `docs/ECONOMY.md`. Standard chain (service auth, acting user,
organization + installation scope; another tenant's ids are `404`).

| Route | Who | Returns |
|---|---|---|
| `GET .../economy/me` | synced user with a **verified** DayZ link | `{ currency, account: {accountId, gamertag, balance, updatedAt}, installationId, gameServerId }` |
| `GET .../economy/me/transactions?limit=&cursor=&type=` | same | `{ currency, items: [{id, type, direction, amount, balanceAfter, description, referenceType, createdAt}], nextCursor, limit }`, newest first (default 25, max 100) |
| `GET .../economy/accounts?q=&limit=` | org OWNER/ADMIN | `{ currency, items: AdminEconomyAccount[], limit }` - lookup by gamertag or verified Discord name (`q` 2-50 chars, max 25 results) |
| `GET .../economy/accounts/{accountID}` | org OWNER/ADMIN | `{ currency, account: AdminEconomyAccount }` |
| `GET .../economy/accounts/{accountID}/transactions?limit=&cursor=&type=` | org OWNER/ADMIN | as `/me/transactions` plus `reason`, `actorDiscordUserId`, `isSystem`, `referenceId`, `gameServerId` |
| `POST .../economy/accounts/{accountID}/grant` | org OWNER/ADMIN | `{ currency, account, transaction, duplicate }` |
| `POST .../economy/accounts/{accountID}/debit` | org OWNER/ADMIN | `{ currency, account, transaction, duplicate }` |

`grant`/`debit` body: `{ "amount": 1..1000000000 (whole number), "reason": required, max 200 chars, "idempotencyKey": optional 8-64 chars of A-Za-z0-9._:- }`; unknown keys are `400`. A retry with
the same key returns the original transaction with `duplicate: true` (HTTP 200); the same key with another amount is `409 DUPLICATE_TRANSACTION`. `accountId` is the DayZ player id inside
the installation's Discord guild. Players only ever see their own account (`/me` has no player parameter); a faction role or organization MEMBER gets `403 ECONOMY_FORBIDDEN` on admin routes.

New error codes: `INSUFFICIENT_FUNDS` (409), `INVALID_AMOUNT` (400), `PLAYER_IDENTITY_REQUIRED` (409), `ECONOMY_ACCOUNT_NOT_FOUND` (404), `DUPLICATE_TRANSACTION` (409), `ECONOMY_FORBIDDEN` (403).
Rate limits: admin grant/debit 30 per minute, history 120 per minute (per acting user). Balances are guild-wide (two installations of one Discord guild show the same account).

## Champion Shop (Phase 1)

Installation-scoped product catalog bought with Champion Points; full contract, transaction semantics and DTOs in `docs/SHOP.md`. Base
`/api/saas/organizations/{organizationID}/installations/{installationID}/shop`. Standard chain; another tenant's ids are `404`. Player routes need a synced user (purchases and history additionally a **verified** DayZ
link: `409 PLAYER_IDENTITY_REQUIRED`); admin routes need an organization **OWNER/ADMIN** (`403 SHOP_FORBIDDEN`; faction roles grant nothing).

| Route | Who | Returns |
|---|---|---|
| `GET .../shop/categories` | player | `{ currency, items: ShopCategory[] }` (active, with product counts) |
| `GET .../shop/products?category=&q=&limit=&cursor=` | player | `{ currency, items: ShopProduct[], nextCursor, limit }` - active products, featured first |
| `GET .../shop/products/{productID}` | player | `{ currency, product: ShopProduct }` (a disabled product is `404 SHOP_PRODUCT_NOT_FOUND`) |
| `POST .../shop/purchases` | player | body `{ productId, quantity 1..100, idempotencyKey }` -> `201`/`200` `{ currency, purchase, remainingBalance, duplicate }` |
| `GET .../shop/me/purchases?status=&productId=&limit=&cursor=` | player | `{ currency, items: ShopPurchase[], nextCursor, limit }`, newest first |
| `GET .../shop/me/purchases/{purchaseID}` | player | `{ currency, purchase }` (another player's is `404 PURCHASE_NOT_FOUND`) |
| `GET .../shop/admin/categories`, `POST .../shop/categories`, `PUT .../shop/categories/{categoryID}` | admin | manage categories |
| `GET .../shop/admin/products`, `GET .../shop/admin/products/{productID}` | admin | products incl. inactive and `stockQuantity` |
| `POST .../shop/products`, `PUT .../shop/products/{productID}` | admin | create / partial update (`categoryId`, `purchaseLimit` accept `null`); no delete (disable with `isActive:false`) |
| `GET .../shop/admin/purchases?status=&playerId=&productId=&limit=&cursor=`, `GET .../shop/admin/purchases/{purchaseID}` | admin | `AdminShopPurchase` (player, refund reason, actors) |
| `POST .../shop/purchases/{purchaseID}/fulfill` | admin | `PENDING_FULFILLMENT` -> `FULFILLED` |
| `POST .../shop/purchases/{purchaseID}/refund` | admin | body `{ reason }` -> compensating `SHOP_REFUND` credit, status `REFUNDED`, once per purchase |

A purchase is one database transaction: product lock, stock, purchase limit, the Champion Points debit through the economy ledger (`SHOP_PURCHASE`, reference `purchase:<id>`), the purchase and its price/name snapshot; any failure
(including `INSUFFICIENT_FUNDS`) leaves nothing behind. Same `idempotencyKey` returns the original purchase (`duplicate: true`); the same key with different data is `409 DUPLICATE_PURCHASE`. Fulfillment is manual; `deliveryType` `MANUAL` is the only
accepted value (`DISCORD_ROLE`, `IN_GAME_FUTURE` are reserved). Two installations of one Discord guild share the Champion Points balance (catalog and history stay per installation).

New error codes: `SHOP_PRODUCT_NOT_FOUND` (404), `SHOP_PRODUCT_DISABLED` (409), `SHOP_CATEGORY_NOT_FOUND` (404), `OUT_OF_STOCK` (409), `PURCHASE_LIMIT_REACHED` (409), `INVALID_QUANTITY` (400), `DUPLICATE_PURCHASE` (409), `PURCHASE_NOT_FOUND` (404),
`INVALID_PURCHASE_STATUS` (409), `SHOP_FORBIDDEN` (403); `INSUFFICIENT_FUNDS` (409) is shared with the economy. Rate limits (per acting user): purchase 10/min, admin mutations (create/update, fulfill, refund) 30/min.

### Shop Delivery Engine 2.0 (Phase 2A)

Every purchase has one persistent delivery record; full contract in `docs/SHOP_DELIVERY.md`. Products have `deliveryPolicy` `MANUAL_PICKUP` (default) or `MANUAL_COORDINATE`
(the purchase body then needs `delivery: { x, z }`, validated against the installation's admin-configured map: `chernarusplus` 0..15360 or `enoch` 0..12800). The purchase, the
Points debit, the stock change, the item snapshot and the delivery commit together or not at all; the delivery coordinates are part of the idempotency identity. Delivery is by
staff only: the existing fulfill/refund routes move the delivery to `FULFILLED` / `CANCELLED` atomically (a delivered order keeps its fulfillment when refunded). No automatic
spawning, no Nitrado file write, no restart.

| Route | Who | Returns |
|---|---|---|
| `GET .../shop/delivery-settings` | player | `{ settings: { map, coordinateDeliveryAvailable, automaticDelivery: false } }` |
| `GET .../shop/admin/delivery-settings`, `PUT .../shop/delivery-settings` | admin | `{ settings }` with `mapKey`, `supportedMaps`; PUT body `{ mapKey: "chernarusplus" \| "enoch" \| null }` |
| `GET .../shop/admin/deliveries?status=&policy=&playerId=&limit=&cursor=` | admin | `{ currency, items: AdminShopDelivery[], nextCursor, limit }` |
| `GET .../shop/admin/deliveries/{deliveryID}` | admin | `{ delivery: AdminShopDelivery }` |
| `GET .../shop/me/deliveries/{deliveryID}` | player | `{ delivery: ShopDelivery }` - own deliveries only |

Purchases (player and admin views) and products gain `delivery` / `deliveryPolicy`. New error codes: `DELIVERY_COORDINATES_REQUIRED`, `DELIVERY_COORDINATES_INVALID`,
`DELIVERY_COORDINATES_OUT_OF_BOUNDS`, `DELIVERY_COORDINATES_NOT_ACCEPTED`, `UNSUPPORTED_MAP` (400), `DELIVERY_MAP_UNRESOLVED` (409), `DELIVERY_NOT_FOUND` (404).

## Champion Billing (Phase 1)

Stripe subscriptions, hosted checkout, the Customer Portal and webhook reconciliation on top of the existing `subscriptions` row (one per organization). Full contract, status mapping, trial
handling, security review and the exact DTOs in `docs/BILLING.md`. **The LOW/MEDIUM/HIGH catalog is approved (Phase 1.2, test-mode Stripe Price ids)** - see `docs/BILLING.md` section 25 for the
exact catalog and the commercial decisions still outstanding (annual pricing, payment-failure grace period, downgrade timing). When `CHAMPION_BILLING_PLANS_JSON` is unset on an environment, the
plans API returns an empty list and checkout refuses every plan key instead of failing to start.

| Route | Who | Notes |
|---|---|---|
| `GET /api/saas/billing/plans` | any synced user | public plan catalog; no organization context, no Stripe price id |
| `GET .../organizations/{organizationID}/billing/subscription` | any member | plan, status, billing interval, trial/period, `cancelAtPeriodEnd`, entitlements, `canManageBilling` |
| `POST .../billing/checkout` | OWNER/ADMIN | `{ planKey, interval, returnPath? }` -> `{ checkoutUrl }` |
| `POST .../billing/portal` | OWNER/ADMIN | `{ returnPath? }` -> `{ portalUrl }`; `409 NO_BILLING_CUSTOMER` before any checkout |
| `POST .../billing/plan` | OWNER/ADMIN | `{ planKey, interval }` -> the summary; applies immediately with Stripe's default proration (both upgrade and downgrade) |
| `POST .../billing/cancel` | OWNER/ADMIN | schedules cancellation at period end; the subscription stays active until then |
| `POST .../billing/reactivate` | OWNER/ADMIN | undoes a scheduled cancellation while still active |
| `POST /api/saas/billing/webhook` | **Stripe only** | not behind the website's bearer token - authenticates via `Stripe-Signature` (`STRIPE_WEBHOOK_SECRET`); idempotent by Stripe event id |

`planKey`/`interval` are always resolved to a Stripe price **server-side** from the catalog - a client never supplies a price. Checkout/portal return URLs are never a client-supplied host: only a
root-relative `returnPath` is accepted, combined with a server-chosen, allowlisted origin (`docs/BILLING.md` "Safe return URLs"; production default `https://championshp.vip`).

New error codes: `INVALID_PLAN` (400), `NO_ACTIVE_SUBSCRIPTION` (409), `NO_BILLING_CUSTOMER` (409), `BILLING_UNAVAILABLE` (503, Stripe not configured on this environment). Rate limit: 20 billing
actions per minute per acting user (checkout/portal/plan/cancel/reactivate share the budget); every read is unlimited.

### Trial (Customer Onboarding V2)

Start Free Trial -> 14 days -> configure the first service -> activate a paid plan when ready. One no-card trial per Discord account; no Stripe object is created to start it, and checkout
never adds a Stripe trial (`trialDays` is always 0). Full policy: `docs/BILLING.md` section 28.

| Route | Who | Notes |
|---|---|---|
| `GET .../organizations/{organizationID}/trial` | any member | read-only [`TrialState`](#trialstate); never starts a trial |
| `POST .../organizations/{organizationID}/trial/start` | OWNER/ADMIN | `{ planKey? }` (LOW/MEDIUM/HIGH, validated against the public catalog) -> `200` [`TrialState`](#trialstate). Idempotent: starts the trial if the account and organization are still eligible, otherwise returns the persisted state unchanged. It never restarts or extends a trial and never overwrites a paid subscription. `planKey` is stored as the intended plan; it is not a purchase. |

`GET .../billing/subscription` also returns `intendedPlan`, `trialStatus`, `trialDaysRemaining` and `billingRequired`; `status` may be `INACTIVE` (no trial, no paid plan).

#### TrialState
```json
{
  "trialStatus": "ACTIVE",
  "subscriptionStatus": "TRIAL",
  "selectedPlan": "MEDIUM",
  "trialStartedAt": "2026-09-23T12:00:00Z",
  "trialEndsAt": "2026-10-07T12:00:00Z",
  "daysRemaining": 14,
  "billingRequired": false,
  "paymentMethodRequired": false,
  "started": true,
  "installationLimit": 1,
  "installationCount": 0
}
```
`trialStatus`: `NOT_STARTED` (no subscription row yet), `ACTIVE`, `EXPIRED` (billing required, data kept), `NOT_ELIGIBLE` (this account already used its trial; billing required),
`CONVERTED` (a Stripe subscription exists; `selectedPlan` is then the paid plan). `billingRequired` is also true for a canceled or suspended subscription.

## Champion Player API (Access Model Phase 2, Part A/B)

Full contract in `docs/PLAYER_API.md`. A Player is never an organization member (same principle as the economy/shop player routes above) - these two routes take no `organizationID` and check no
organization role at all; authorization comes entirely from a `VERIFIED` `player_links` row plus observed activity on the specific installation.

| Route | Who | Notes |
|---|---|---|
| `GET /api/saas/player/servers` | any synced user | installations the acting user is a proven player on, most-recently-active first; empty list (not an error) if unverified/never observed |
| `GET /api/saas/player/servers/{installationID}/stats` | any synced user, if associated | kills/deaths/kd/headshots/longshots/playtime/bounties/faction/lastSeenAt, scoped strictly to this installation's own server |

New error codes: `PLAYER_IDENTITY_REQUIRED` (409, reused from the economy's `Me` endpoint - no `VERIFIED` link for this installation's guild). An unknown installation id AND a verified-but-never-
played-here server both return `404 NOT_FOUND` - deliberately the same code, to avoid letting a caller enumerate which installations of a guild they belong to.

## Client Admin Control Plane (Phase 1)

Full contract in `docs/CLIENT_ADMIN.md`. A capability-based tenant administration system: Clients
map their own Discord roles to one of four Levels (OWNER/ADMINISTRATOR/MODERATOR/GATEKEEPER), and
every route below is gated by a specific capability's minimum Level rather than the organization
OWNER/ADMIN/MEMBER role used elsewhere in this document (the organization owner is always
bootstrapped to Level OWNER; every other Level requires an explicit role mapping).

All routes are under `/api/saas/organizations/{organizationID}/installations/{installationID}/admin`:
**`GET /me`** (current-actor permission introspection, see below), permission mapping CRUD, the
audit log, warnings, bounty reset, faction admin, last-online, an economy-balance-search alias,
server name/feed-location/maintenance-mode, server restart/stop (real Nitrado endpoints,
typed-confirmation-gated), whitelist/banlist management (real Nitrado endpoints, Champion-side
metadata), and stat reset (streak per-player, season-based guild-wide). See `docs/CLIENT_ADMIN.md`
for the full route table, the DayZ/Nitrado capability audit (what's a real Nitrado API call vs.
deferred), and the explicit list of deferred capabilities (priority list, gameplay toggles,
zones/UAV/heatmap, auto payments) with reasoning.

**`GET /me`** reports the acting user's own resolved Level and exact capability set for the
selected installation - the mechanism the website's Client Server Admin UI uses to decide which
controls to render, instead of inferring visibility from the organization OWNER/ADMIN/MEMBER role
or any website-side session flag. Unlike every other route here, it requires no specific
capability - an actor with no mapped Level still gets a normal `403 ADMIN_FORBIDDEN`, not a
fabricated 200. Response: `{"level":"MODERATOR","capabilities":["...",...],"discordRoleIds":["..."]}`.
`capabilities` is derived from the same fixed Level table every other route's authorization check
reads (`permissions.CapabilitiesForLevel`), sorted for stable output - never a second
hand-maintained list. See `docs/CLIENT_ADMIN.md` "Current Actor Client Admin Permissions".

New error codes: `ADMIN_FORBIDDEN` (403), `ADMIN_ESCALATION_DENIED` (403),
`ADMIN_CONFIRMATION_REQUIRED` (409), `ADMIN_DISCORD_UNAVAILABLE` (503).

### Player Intelligence (Phase 3)

Full contract in `docs/PLAYER_INTELLIGENCE.md`. The authoritative player directory and persisted
ADM location-event history - **not** built on the economy account search. New capabilities
`PLAYER_DIRECTORY_VIEW` (Moderator), `PLAYER_LAST_LOCATION_VIEW`/`PLAYER_LOCATION_VIEW`
(Administrator - location access is privileged, one level above the plain directory).

```
GET .../admin/players                          ?q=&online=&linked=&cursor=&limit=
GET .../admin/players/online                   currently-connected players + latest known location each
GET .../admin/players/{playerID}/locations/latest
GET .../admin/players/{playerID}/locations/current   { status: CURRENT | UNKNOWN, location } (Live Sync phase 1)
GET .../admin/players/{playerID}/locations     ?from=&to=&eventType=&cursor=&limit=, newest first
```

**Live Sync phase 1 (docs/CHAMPION_LIVE_SYNC.md).** Directory `currentLocation` is session-scoped (absent =
unknown) with `currentLocationStatus` and a separate `lastKnownLocation`; every location carries
`sessionScope` (`CURRENT_SESSION`/`HISTORICAL`) and `sourceLocalTime` (DayZ server-local time, no zone);
`eventType` may be `PLAYER_LIST` (five-minute ADM player-list observations).

**Live Sync phase 2.** Every location also carries `occurredAt` (DayZ's own time in UTC - present only
when the server's UTC offset is known from restart.log) and `timeBasis` (`SOURCE` | `INGESTION`);
`ageSeconds`/`freshness` are measured from `occurredAt` when present, so a line ingested late is never
presented as fresh. `currentLocation` also becomes unknown as soon as written evidence (RPT shutdown
completed, restart.log pre-start check, a newer boot's file) has ended the boot session.

Every returned location carries `observedAt`/`ageSeconds`/`freshness` (`LIVE_RECENT`≤60s /
`RECENT`≤5min / `STALE`>5min) - ADM has no continuous GPS, so "live" is never claimed unless the
age genuinely qualifies. Viewing a player's location history or online-player locations writes an
`admin_audit_log` row (location access is privileged, task's own instruction).

### Zones + UAV / Base Radar (Phase 4)

Full contract in `docs/ZONES_UAV_RADAR.md`. Installation-scoped geographic zones plus a stateful
intrusion engine consuming Phase 3's location events - one shared engine for every zone type,
including UAV/Base Radar (no duplicated logic). New capabilities `ZONE_VIEW` (Moderator),
`ZONE_MANAGE`/`ZONE_IGNORE_MANAGE` (Administrator), `UAV_MANAGE` (Owner - required **in addition
to** `ZONE_MANAGE` for any UAV/BASE_RADAR zone), `INTRUSION_ACK` (Moderator).

```
GET    .../admin/zones                                                          ZONE_VIEW
POST   .../admin/zones                                                          ZONE_MANAGE (+UAV_MANAGE for UAV/BASE_RADAR)
GET    .../admin/zones/{zoneID}                                                 ZONE_VIEW
PUT    .../admin/zones/{zoneID}                                                 ZONE_MANAGE (+UAV_MANAGE if already UAV/BASE_RADAR)
DELETE .../admin/zones/{zoneID}                                                 ZONE_MANAGE (+UAV_MANAGE if already UAV/BASE_RADAR)
GET    .../admin/zones/{zoneID}/ignore                                          ZONE_VIEW
POST   .../admin/zones/{zoneID}/ignore          {"entryType":"PLAYER|FACTION|DISCORD_ROLE","entryValue":"..."}  ZONE_IGNORE_MANAGE
DELETE .../admin/zones/ignore/{entryID}                                         ZONE_IGNORE_MANAGE
GET    .../admin/zones/{zoneID}/authorized                                      ZONE_VIEW
POST   .../admin/zones/{zoneID}/authorized      {"entryType":"PLAYER|FACTION","entryValue":"..."}               ZONE_MANAGE
DELETE .../admin/zones/authorized/{entryID}                                     ZONE_MANAGE
GET    .../admin/zones/{zoneID}/bans                                            ZONE_VIEW
POST   .../admin/zones/{zoneID}/bans            {"playerId":123,"reason":"..."}                                 ZONE_MANAGE
DELETE .../admin/zones/bans/{banID}                                             ZONE_MANAGE (lifts the ban)
GET    .../admin/zones/{zoneID}/active-intruders                                ZONE_VIEW
GET    .../admin/intrusions/active              ?zoneId=&zoneType=&playerId=&acknowledged=                     ZONE_VIEW
GET    .../admin/intrusions/history              ?zoneId=&playerId=&from=&to=&status=&cursor=&limit=            ZONE_VIEW
POST   .../admin/intrusions/{intrusionID}/acknowledge                           INTRUSION_ACK
```

Every active-intruder/active-intrusion response carries a `presenceStatus` (`PRESENT_RECENT` or
`PRESENCE_UNCERTAIN`, computed at read time from the same freshness thresholds above) - a stale
last-known-location never auto-closes an intrusion, it is only ever flagged uncertain. Zone bans
are explicitly not server bans - they never call the Nitrado banlist API. Every zone/ignore/
authorized/ban mutation and every acknowledgement writes an `admin_audit_log` row.

### Heatmaps (Phase 5)

Full contract in `docs/HEATMAPS.md`. Aggregate PvP kill/death, player-activity, and zone-intrusion
heatmap datasets from data Phase 3/4 already persist - no new Nitrado polling, no fabricated
coordinates, no frontend rendering. New capability `HEATMAP_VIEW` (Moderator).

```
GET .../admin/heatmap  ?type=PVP_KILLS|PVP_DEATHS|PLAYER_ACTIVITY|ZONE_INTRUSIONS&from=&to=&resolution=&zoneId=
```

`type` is required; `resolution` is one of `50/100/250/500/1000` (default 250); `from`/`to` are
RFC3339 (default: last 24h), max 30-day span; `zoneId` is only honored for `ZONE_INTRUSIONS` and is
tenant-checked against the caller's own installation. Response is always an aggregate grid - never
a player ID, gamertag, Discord ID, or faction identity:

```json
{"type":"PVP_KILLS","from":"...","to":"...","resolution":250,"totalEvents":452,
 "cells":[{"cellX":10,"cellZ":18,"centerX":2625,"centerZ":4625,"count":27,"intensity":0.82}]}
```

No data is a valid `200` (`{"totalEvents":0,"cells":[]}`), never `404`. A query that would return
more than 5000 cells is rejected with `400` and an actionable message - the resolution is never
silently mutated. Results are cached server-side for 45 seconds, keyed by every query dimension.

## Request/response DTOs

None of these ever include a Nitrado ciphertext/IV/auth tag, a Discord
bot/OAuth token, `WEBSITE_API_SECRET`, a database URL, or any other internal
credential - enforced by `TestNoSensitiveFieldsInAPIResponses` in
`internal/app/saas_api_test.go` (section 9).

#### `UserSummary`
| field | type |
|---|---|
| `id` | number |
| `discordUserId` | string |
| `discordUsername` | string |
| `discordGlobalName` | string? |
| `avatar` | string? |

#### `OrganizationSummary`
| field | type |
|---|---|
| `id` | number |
| `name` | string |
| `slug` | string |
| `role` | `"OWNER"` \| `"ADMIN"` \| `"MEMBER"` - always the **acting user's own** role |

#### `OrganizationMembership`
Conceptual shape (`organization_id`, `user_id`, `role`, `created_at`) backing
`OrganizationRepository`'s internal authorization checks
(`internal/repository/saas_organizations_repository.go`). **Not returned as
a standalone object by any current route** - the acting user's own
membership is folded directly into `OrganizationSummary.role` everywhere.
There is no "list organization members" endpoint yet; add one if/when the
website needs a team-management UI (not required by this handoff - see
section 8's feature list, which does not include it).

#### `DashboardSummary`
| field | type |
|---|---|
| `organization` | [`OrganizationSummary`](#organizationsummary) |
| `subscription` | [`SubscriptionSummary`](#subscriptionsummary)? |
| `installations` | [`InstallationSummary`](#installationsummary)[] |

Example:
```json
{
  "organization": { "id": 5, "name": "Champions", "slug": "champions", "role": "OWNER" },
  "subscription": { "plan": "TRIAL", "status": "TRIAL", "trialEndsAt": "2026-10-02T00:00:00Z" },
  "installations": [
    {
      "id": 9,
      "status": "DISCORD_CONNECTED",
      "health": "SETTING_UP",
      "setupProgress": {
        "currentStep": "NITRADO",
        "discordCompleted": true, "nitradoCompleted": false, "serverSelected": false,
        "channelsCompleted": false, "validationCompleted": false
      },
      "discordConnection": {
        "id": 12, "guildName": "My DayZ Server", "guildIcon": "abc123",
        "botInstalled": true, "permissionsVerified": true, "connectedAt": "2026-09-18T01:00:00Z"
      },
      "createdAt": "2026-09-18T01:00:00Z"
    }
  ]
}
```

#### `InstallationSummary`
| field | type |
|---|---|
| `id` | number |
| `status` | `"NOT_STARTED"` \| `"DISCORD_CONNECTED"` \| `"NITRADO_CONNECTED"` \| `"CONFIGURING"` \| `"READY"` \| `"DEGRADED"` \| `"DISCONNECTED"` \| `"SUSPENDED"` |
| `plan` | string? |
| `health` | `"SETTING_UP"` \| `"HEALTHY"` \| `"DEGRADED"` \| `"OFFLINE"` - derived from `status`, not a stored field |
| `setupProgress` | [`SetupProgress`](#setupprogress)? |
| `discordConnection` | [`DiscordGuildConnectionSummary`](#discordguildconnectionsummary)? |
| `dayzServer` | [`DayZServerSummary`](#dayzserversummary)? - present once a server is selected via `#16` |
| `createdAt` | string (RFC3339) |
| `setupCompletedAt` | string? - stamped once, the first time `status` reaches `READY` |
| `lastHealthCheckAt` | string? |

#### `SetupProgress`
| field | type |
|---|---|
| `currentStep` | `"DISCORD"` \| `"NITRADO"` \| `"SERVER"` \| `"CHANNELS"` \| `"VALIDATION"` \| `"COMPLETE"` |
| `discordCompleted` | boolean |
| `nitradoCompleted` | boolean |
| `serverSelected` | boolean |
| `channelsCompleted` | boolean |
| `validationCompleted` | boolean |
| `completedAt` | string? - stamped once, the first time `validationCompleted` becomes true |

#### `DiscordGuildConnectionSummary`
| field | type |
|---|---|
| `id` | number |
| `guildName` | string? |
| `guildIcon` | string? |
| `botInstalled` | boolean |
| `permissionsVerified` | boolean |
| `connectedAt` | string (RFC3339) |

#### `DiscordGuildSummary` (eligibility candidate result)
| field | type |
|---|---|
| `discordGuildId` | string |
| `guildName` | string? |
| `guildIcon` | string? |
| `eligible` | boolean |
| `botInstalled` | boolean |

#### `DiscordInstallationVerification` (code name: `DiscordVerificationResult`)
| field | type |
|---|---|
| `installed` | boolean |
| `guildReachable` | boolean |
| `verifiedAt` | string (RFC3339) |

#### `DiscordPermissionVerification` (code name: `PermissionVerificationResult`)
| field | type |
|---|---|
| `capabilities` | `{ capability: string, result: "PASS"\|"WARNING"\|"FAIL" }[]` - always exactly the 4 entries `View Channel`, `Send Messages`, `Embed Links`, `Read Message History`, in that order |
| `overallPass` | boolean |

#### `SubscriptionSummary`
Used unchanged in the dashboard (`GET .../dashboard`) and hub (`GET .../installations/{id}/hub`) responses. The dedicated billing endpoints below return a richer `SubscriptionSummary` (billing
interval, cancellation, entitlements, `canManageBilling`) - see `docs/BILLING.md` section 24 for its exact shape.

| field | type |
|---|---|
| `plan` | string |
| `status` | `"TRIAL"` \| `"ACTIVE"` \| `"PAST_DUE"` \| `"CANCELED"` \| `"SUSPENDED"` |
| `trialEndsAt` | string? |
| `currentPeriodEnd` | string? |

#### `NitradoServiceSummary`
| field | type |
|---|---|
| `serviceId` | number |
| `name` | string |
| `game` | string - always `"DayZ"` (only DayZ services are ever returned) |
| `platform` | `"PLAYSTATION"` \| `"XBOX"` - the stable backend enum. The website renders its own friendly label ("PlayStation"/"Xbox") - never persist or match against a display label. |
| `status` | `"ONLINE"` \| `"OFFLINE"` |

#### `SelectDayZServerResponse` (response of `#16`)
| field | type |
|---|---|
| `server` | [`DayZServerSelection`](#dayzserverselection) |
| `installationId` | number - the **resolved** installation; may differ from the request path's `{installationID}` when an existing installation already owned this guild+server pair (see `#16`'s "Same-guild reuse") |
| `reusedInstallation` | boolean |

#### `DayZServerSelection`
| field | type |
|---|---|
| `id` | number - the `game_servers` row ID |
| `serviceId` | number |
| `displayName` | string |
| `game` | string |
| `platform` | `"PLAYSTATION"` \| `"XBOX"` |
| `status` | `"ONLINE"` \| `"OFFLINE"` |

#### `DayZServerSummary` (embedded in `InstallationSummary.dayzServer`)
Same shape as [`DayZServerSelection`](#dayzserverselection) above -
the website hydrates the selected DayZ server directly from a dashboard/
installation response (`#5`/`#7`) after a refresh, without needing to
re-select it via `#16`.
| field | type |
|---|---|
| `id` | number |
| `serviceId` | number |
| `displayName` | string? |
| `game` | string |
| `platform` | `"PLAYSTATION"` \| `"XBOX"` |
| `status` | `"ONLINE"` \| `"OFFLINE"` |

#### `DayZServerValidation` (response of `#17`)
| field | type |
|---|---|
| `reachable` | boolean |
| `supported` | boolean |
| `platform` | `"PLAYSTATION"` \| `"XBOX"`? - omitted when the server couldn't be resolved at all |
| `message` | string - human-readable, safe to display directly |

#### `DiscordChannelSummary` (response of `#18`, `#22`)
| field | type |
|---|---|
| `id` | string - Discord channel snowflake |
| `name` | string |
| `type` | `"TEXT"` \| `"ANNOUNCEMENT"` - the stable backend enum, never Discord's raw integer type |
| `position` | number |
| `canSend` | boolean - safe hint only, not a substitute for `#13` |

#### `InstallationChannelSettings` (request of `#20`, response of `#19`/`#20`)
The legacy four-field surface - still live, unaffected by the routing
expansion below.
| field | type |
|---|---|
| `killfeedChannelId` | string - required on `#20`; empty string means unset elsewhere |
| `leaderboardChannelId` | string |
| `playerStatusChannelId` | string |
| `adminLogChannelId` | string |

#### `ChannelCategorySummary` (embedded in `#21`)
| field | type |
|---|---|
| `id` | string - Discord category channel snowflake |
| `name` | string |

#### `ChannelRouteInfo` (value type of `ChannelRoutesResponse.routes` and `AutoSetupChannelsResponse.routes`)
| field | type |
|---|---|
| `channelId` | string - Discord channel snowflake |
| `channelName` | string? - best-effort live lookup, omitted if the channel was deleted in Discord |
| `managedByChampion` | boolean - `true` only for a channel Champion itself created/reused; `false` once a customer explicitly points the route at a channel |

#### `ChannelRoutesResponse` (response of `#23`/`#24`)
| field | type |
|---|---|
| `routes` | object - map of `route_key` -> [`ChannelRouteInfo`](#channelrouteinfo); a key absent from the map means that route isn't configured |

#### `AutoSetupChannelsResponse` (response of `#21`)
| field | type |
|---|---|
| `configured` | boolean |
| `reason` | string? - `"MISSING_MANAGE_CHANNELS"` \| `"CUSTOM_CONFIGURATION_EXISTS"` \| `"KILLFEED_UNAVAILABLE"`, present only when `configured=false` |
| `category` | [`ChannelCategorySummary`](#channelcategorysummary)? - the LIVE category, present only when `configured=true` |
| `categories` | [`ChannelCategorySummary`](#channelcategorysummary)[]? - every V2 category used (`key` = `LIVE` \| `HUB` \| `STAFF`) |
| `routes` | object? - map of `route_key` -> [`ChannelRouteInfo`](#channelrouteinfo) for every mapped route; routes of skipped destinations are absent |
| `destinations` | object[]? - one per V2 destination: `key`, `label`, `category`, `channelId?`, `channelName`, `health` (`ACTIVE` \| `DISABLED` \| `NOT_REQUIRED` \| `BLOCKED` \| `BROKEN`), `detail?`, `created`, `starterSent`, `routes[]` (`routeKey`, `health`, `detail?`), `checks?` (`channelExists`, `routeMapped`, `producerConnected`, `botCanSend`, `visibleContent`) |
| `retirable` | object[]? - Champion-managed channels/categories no route uses any more: `channelId`, `channelName?`, `kind` (`CHANNEL` \| `CATEGORY`), `formerRoutes?`. Never deleted by Champion |

#### `FinalizeSetupResponse` (response of `#25`)
| field | type |
|---|---|
| `completed` | boolean |
| `installation` | [`InstallationSummary`](#installationsummary) - the reused, existing DTO, never a duplicate model |

#### `HubDiscordSummary` (embedded in `HubSummary.discord`)
| field | type |
|---|---|
| `guildName` | string? |
| `guildIcon` | string? |
| `botInstalled` | boolean |

#### `HubSummary` (response of `#26`)
| field | type |
|---|---|
| `organization` | [`OrganizationSummary`](#organizationsummary) |
| `subscription` | [`SubscriptionSummary`](#subscriptionsummary)? |
| `installation` | [`InstallationSummary`](#installationsummary) |
| `discord` | [`HubDiscordSummary`](#hubdiscordsummary)? |
| `dayzServer` | [`DayZServerSummary`](#dayzserversummary)? |
| `channelRoutes` | object - map of `route_key` -> [`ChannelRouteInfo`](#channelrouteinfo) |
| `settings` | [`InstallationGeneralSettings`](#installationgeneralsettings) |

#### `InstallationGeneralSettings` (request/response of `#27`/`#28`, embedded in `HubSummary.settings`)
| field | type |
|---|---|
| `timezone` | string - required on `PUT` |
| `distanceUnit` | `"METERS"` \| `"FEET"` |
| `onlineDisplayEnabled` | boolean |
| `leaderboardEnabled` | boolean |

## Error contract

Every non-2xx response:
```json
{ "error": { "code": "FORBIDDEN", "message": "not a member of this organization" } }
```
`message` is always a fixed, safe, human string - never raw SQL or a stack trace (section 9).

| Code | HTTP | Meaning |
|---|---|---|
| `UNAUTHORIZED` | 401 | Missing/invalid service auth, or acting user not synced |
| `FORBIDDEN` | 403 | Not a member of the organization, or lacks OWNER/ADMIN |
| `NOT_FOUND` | 404 | Resource doesn't exist, or doesn't belong to the acting organization |
| `CONFLICT` | 409 | Slug or guild already claimed |
| `INVALID_REQUEST` | 400 | Malformed body, invalid ID, or an invalid setup-progress transition |
| `DISCORD_UNAVAILABLE` | 503 | The bot's Discord session isn't connected |
| `INSTALLATION_NOT_VERIFIED` | 422 | Action requires Discord to be verified first, or (`#25`) a setup prerequisite isn't met yet - the message names which one |
| `NITRADO_UNAVAILABLE` | 503 | Nitrado rejected the token, or is unreachable |
| `PAYLOAD_TOO_LARGE` | 413 | Request body over the endpoint's bound (embed templates: 64 KiB) |
| `BILLING_REQUIRED` | 402 | A new service needs a running trial or a paid plan (trial expired, trial already used, subscription ended) - `docs/BILLING.md` section 28 |
| `INSTALLATION_LIMIT_REACHED` | 409 | The organization is at its plan's installation capacity (trial: 1) - reconfigure the existing service or upgrade |
| `INTERNAL_ERROR` | 500 | Unexpected server-side failure |
| `RATE_LIMITED` (not in the code list above, still `{"error":{"code","message"}}`-shaped) | 429 | See rate limits below |

## Rate limits

In-memory, per acting Discord user ID, per process:

| Route(s) | Limit |
|---|---|
| `#1` user sync | 10 / minute |
| `#3` organization creation | 5 / hour |
| `#12`/`#13` Discord verification (shared budget) | 10 / minute |
| `#14` Nitrado connect | 10 / hour |
| Faction Hub: create a faction | 1 / 10 seconds and 10 / day |
| Faction Hub: submit a join application | 5 / minute and 40 / day |
| Faction Hub: upload a faction logo | 3 / minute and 20 / day |

Ordinary reads (`#2`, `#4`, `#5`, `#7`, `#8`, `#15`, `#17`, `#18`, `#19`,
`#23`, `#26`, `#27`) are never rate limited. `#20`/`#21`/`#22`/`#24`/`#25`/
`#28` (writes) aren't separately rate limited either, matching `#16`'s
precedent - all are already gated to OWNER/ADMIN (except `#25`, gated the
same way but effectively a no-op once already `READY`) and organization-scoped.

## Tenant-scoping rules

- Every organization-scoped repository lookup takes `organizationID`
  explicitly and filters by it at the SQL layer (`WHERE organization_id=$1
  AND id=$2`), never a bare `GetByID(id)`.
- A non-member of an organization gets `403 FORBIDDEN` for anything scoped
  to it.
- A member of organization A who supplies an ID belonging to organization B
  (installation, guild connection, etc.) gets `404 NOT_FOUND` - the lookup
  simply finds no matching row, so B's data (or even its existence) is never
  revealed to A.
- Connecting a Discord guild already claimed by a different organization is
  rejected with `409 CONFLICT`, never silently reassigned.
- See `internal/app/saas_api_integration_test.go` for the full tenant-isolation
  test suite (organization A cannot view/list/mutate organization B's data
  under any tested scenario, including ID guessing).

## Console DayZ platform contract

Champion supports exactly two DayZ console platforms in this stage:
`PLAYSTATION` and `XBOX` - both stable, persistence-safe enum values
(`internal/nitrado.ConsolePlatform`). DayZ PC and any non-DayZ Nitrado
service are classified `UNSUPPORTED` and are never exposed by `#15`
(services discovery) or accepted by `#16` (server selection), even if
they're real services on the connected account.

Platform is classified centrally, once, in
`internal/nitrado.ClassifyDayZPlatform` - from Nitrado's own
game-identifier field (`service.game`, confirmed live as `"DayZ (PS4)"` for
a real PlayStation service), never from a customer-editable display name
like the server's hostname or title. No live Xbox DayZ service was
available to confirm Nitrado's exact Xbox game string; Xbox detection
covers Nitrado's documented naming patterns as a best-effort match - see
that function's doc comment if a real Xbox payload is ever observed to
differ.

**ADM/log ingestion compatibility**: the existing ADM discovery and
download pipeline (`internal/nitrado/logs.go`, `internal/killfeed/canonical_source.go`)
was audited and found to already be platform-agnostic by construction - it
walks Nitrado's generic file-server API and the `noftp`/`ftproot` dual-mount
alias logic using only Nitrado-reported paths (`game_specific.path` from
the Gameserver Details endpoint) and a `.ADM` file-extension match, with no
PlayStation-specific literal anywhere in the actual matching logic (only in
doc comments, as an example). No platform-specific adapter was needed or
added for ingestion - the same pipeline that already runs for the
PlayStation server works unmodified for a connected Xbox server.

## Verified feature coverage

| Feature | Status |
|---|---|
| User sync | READY |
| Organization creation | READY |
| Organization listing | READY |
| Membership authorization | READY |
| Dashboard summary | READY |
| Installation creation/read | READY |
| Setup progress persistence | READY |
| Discord guild persistence | READY |
| Bot installation verification | READY |
| Permission verification | READY |
| Nitrado credential connect (PlayStation + Xbox) | READY |
| DayZ console server discovery/selection | READY |
| DayZ server reachability validation | READY |
| Discord channel discovery | READY |
| Manual channel configuration (Customize mode, legacy 4-field) | READY |
| Full per-route channel configuration (Customize mode) | READY |
| One-click channel auto-setup (Channel System V2 layout, verified) | READY |
| Optional custom channel creation | READY |
| Backend-authoritative setup finalization (`#25`) | READY |
| Multi-channel permission verification at finalize time | READY |
| Customer hub summary (`#26`) | READY |
| Customer-editable general settings (`#27`/`#28`) | READY |
| Critical-change revalidation (KILLFEED route, DayZ server) | READY |

Nothing is BLOCKED. Out of scope for this handoff (a later task):
entitlement enforcement (billing/checkout itself now exists - `docs/BILLING.md`), a dedicated Step 6
verify-permissions website UI showing per-channel PASS/WARNING/FAIL (`#13`
itself still only checks one channel per call - `#25`'s finalize check is
what actually aggregates every unique route channel today, not `#13`), and
the routes marked BLOCKED in the channel routing table above (the build
feed until the server logs build actions) - one-click setup creates no channel for
a destination without a working producer. DayZ PC and non-console Nitrado services are intentionally
unsupported, not missing - see the platform contract note above.
