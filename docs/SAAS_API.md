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
creates a 14-day TRIAL subscription.

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
connection already has an installation.

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
One-click setup: creates (or reuses) Champion's default category and its
four default channels in the installation's Discord guild, persists the
resulting IDs, and advances setup exactly like `#20` does. OWNER/ADMIN only.

Request (optional body):
```json
{ "force": false }
```

Behavior:
1. Verifies the bot is actually installed in the guild (`503
   DISCORD_UNAVAILABLE` if not).
2. **Unless `force=true`**, refuses if the installation already has a
   customer-configured (manual) channel selection - see "Custom
   configuration protection" below.
3. Verifies the bot holds **Manage Channels** in the guild - see "Missing
   permission" below.
4. Resolves (or creates) the `CHAMPION KILLFEED` category: prefers the
   previously-persisted category ID (authoritative); if that's gone (or
   this is the first run), falls back to a case-insensitive name scan for
   recovery; only creates a new one if neither is found.
5. Resolves (or creates) each of the four default channels the same way -
   ID first, then a name scan **scoped to the resolved category** (so
   recovery never adopts an unrelated same-named channel elsewhere in the
   guild), then create.
6. Persists the four resulting channel IDs plus the category ID, advances
   setup progress, and moves the installation to `CONFIGURING` (same
   one-directional rule as `#20`).

Default blueprint (section 1):

| Channel | Settings field | Purpose |
|---|---|---|
| `#champion-killfeed` | `killfeedChannelId` | kills, deaths, special kill events |
| `#champion-leaderboard` | `leaderboardChannelId` | leaderboards, rankings, seasonal/competitive standings |
| `#champion-players` | `playerStatusChannelId` | online players, player-status panels |
| `#champion-admin` | `adminLogChannelId` | admin/log/diagnostic output for server staff |

All four are created under a `CHAMPION KILLFEED` category.

Response `200` on success ([`AutoSetupChannelsResponse`](#autosetupchannelsresponse)):
```json
{
  "configured": true,
  "category": { "id": "999", "name": "CHAMPION KILLFEED" },
  "channels": {
    "killfeedChannelId": "111",
    "leaderboardChannelId": "112",
    "playerStatusChannelId": "113",
    "adminLogChannelId": "114"
  }
}
```

**Idempotent by design**: calling this route again with the same
installation reuses the exact same category and channel IDs - it never
creates `#champion-killfeed-1`/`#champion-killfeed-2` duplicates, because
step 4/5 above always check the persisted ID (and then existing names)
before ever creating anything.

**Custom configuration protection** (never destroys manual customization):
if the installation already has a channel selection that wasn't itself
produced by a prior auto-setup call, this route responds `200` with a
safe, structured "did not configure" result instead of overwriting it:
```json
{ "configured": false, "reason": "CUSTOM_CONFIGURATION_EXISTS" }
```
Pass `{ "force": true }` to override this and run auto-setup anyway
(an explicit, deliberate customer action - never the default).

**Missing permission**: if the bot lacks Manage Channels in the guild,
this route also responds `200` with a safe result rather than a raw
Discord error or a 5xx:
```json
{ "configured": false, "reason": "MISSING_MANAGE_CHANNELS" }
```

`configured: false` responses are deliberately **not** the
`{"error":{"code","message"}}` envelope (see `## Error contract` below) -
they're expected, UI-renderable outcomes, not failures to retry blindly.

Auto-setup never replaces `#13` (verify-permissions) - Step 6 on the
website still runs permission verification against whichever channel IDs
ended up configured, whether by `#20` or `#21`.

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

#### `InstallationChannelSettings` (request of `#20`, response of `#19`/`#20`, embedded in `#21`)
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

#### `AutoSetupChannelsResponse` (response of `#21`)
| field | type |
|---|---|
| `configured` | boolean |
| `reason` | string? - `"MISSING_MANAGE_CHANNELS"` \| `"CUSTOM_CONFIGURATION_EXISTS"`, present only when `configured=false` |
| `category` | [`ChannelCategorySummary`](#channelcategorysummary)? - present only when `configured=true` |
| `channels` | [`InstallationChannelSettings`](#installationchannelsettings)? - present only when `configured=true` |

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
| `INSTALLATION_NOT_VERIFIED` | 422 | Action requires Discord to be verified first |
| `NITRADO_UNAVAILABLE` | 503 | Nitrado rejected the token, or is unreachable |
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

Ordinary reads (`#2`, `#4`, `#5`, `#7`, `#8`, `#15`, `#17`, `#18`, `#19`) are
never rate limited. `#20`/`#21`/`#22` (channel writes) aren't separately
rate limited either, matching `#16`'s precedent - all four are already
gated to OWNER/ADMIN and organization-scoped.

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
| Manual channel configuration (Customize mode) | READY |
| One-click channel auto-setup | READY |
| Optional custom channel creation | READY |

Nothing is BLOCKED. Out of scope for this handoff (a later task):
billing/checkout, entitlement enforcement, Step 6 permission-verification
website UI (the backend route `#13` already exists from an earlier task).
DayZ PC and non-console Nitrado services are intentionally unsupported, not
missing - see the platform contract note above.
