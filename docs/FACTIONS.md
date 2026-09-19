# Faction Hub - Phase 1 (backend foundation)

The web-first, installation-scoped faction directory, membership and recruitment API.
**Backend only**: no website UI, no visual designer, no image upload, no flag/armband
catalogs yet. Code: `internal/factionhub` (pure rules), `internal/repository/faction_hub_repository.go`
(SQL + transactions), `internal/app/saas_api_factions.go` (HTTP). Migration `0032_faction_hub`.

## Not the Discord-side faction system

The database already has `factions` / `faction_members` / `faction_invites` / `faction_wars` (migration
0005 onward). Those are keyed by **Discord guild + DayZ `player_id`**, have a per-guild unique name and
tag, and are referenced by kills, wars, events and seasons (`internal/factions`, the `/faction`
commands). The Hub is keyed by **organization + installation (+ DayZ server) + website user
(`app_users`)** and needs the same name to be usable on different installations. So Phase 1 adds
**parallel `hub_*` tables** and does not alter the old ones:

| Hub table | Purpose |
|---|---|
| `hub_factions` | the faction: profile, recruitment status, colors, visual-key placeholders, tenant scope |
| `hub_faction_settings` | recruitment requirements (display only) |
| `hub_faction_members` | membership, primary role, optional verified DayZ identity |
| `hub_faction_applications` | join requests and their history |
| `hub_faction_roles` | role catalog: the three built-in roles (seeded), room for custom roles later |
| `hub_faction_role_memberships` | extra role grants for a later custom-roles phase (unused, not exposed) |

`hub_factions.legacy_faction_id` is a nullable, unused column reserved for a later bridge to the Discord-side
faction (so kill/war statistics can be attached to a Hub faction). Until then the two systems are independent.

## Tenant model

A faction belongs to exactly one **installation** (an organization's Discord guild + DayZ server pair):

* `hub_factions(organization_id, installation_id, game_server_id)`. `game_server_id` is the DayZ server the
  installation had when the faction was created; **`installation_id` is authoritative** for isolation.
* `(installation_id, organization_id)` is a composite FK to `installations(id, organization_id)`, so a faction
  can never claim an organization that does not own its installation.
* Members and applications carry `(faction_id, installation_id)` as a composite FK to the faction, so they can
  never sit in a different installation than their faction.
* Name, tag and slug are unique **per installation** (case-insensitive for name and tag), never globally and
  never per guild. The same name works on another installation, even of the same organization.
* A faction can only be created on an installation that has a selected DayZ server and is not `SUSPENDED`
  (`409 CONFLICT`). Reads still work on both.
* Every repository query is scoped by organization + installation (+ faction). An id from another tenant
  answers **`404 NOT_FOUND`**, identical to an id that does not exist - nothing is inferable.

## Roles

Built-in, primary role per member (`hub_faction_members.role_key`):

| Role | May |
|---|---|
| `LEADER` | everything: edit profile/recruitment/requirements, review applications, promote/demote, remove OFFICER and MEMBER |
| `OFFICER` | list, accept and deny applications; remove a MEMBER |
| `MEMBER` | normal access (read) |

* Exactly one `LEADER` per faction: a partial unique index (`role_key = 'LEADER'`) plus the creation
  transaction (faction + leader membership together). The creator becomes the leader; `created_by_user_id`
  records the founder explicitly (`ON DELETE RESTRICT`, so a faction is never left without its founding account).
* Nobody may remove or demote the LEADER in Phase 1 (`403`); there is no leader-transfer endpoint yet.
* Promote is `MEMBER -> OFFICER` and demote is `OFFICER -> MEMBER`, LEADER only. An officer can never promote
  another officer or a leader.
* A user with no membership in the faction has no faction rights at all.
* **Future custom roles**: `hub_faction_roles` already has `faction_id` (NULL = built-in system role),
  `role_key`, `rank`, `is_system`, and `hub_faction_role_memberships` can grant extra roles to a member. Neither is
  exposed; there is no arbitrary RBAC in this phase.

### Faction role vs organization role vs platform admin

Three separate things; none implies another:

* **Organization role** (`OWNER`/`ADMIN`/`MEMBER` of the customer organization): the only faction privilege is a
  **read-only moderation view** - `GET .../applications` and `viewer.moderationView=true` on a profile. It
  grants no mutation (accept, deny, edit, promote, demote and remove all answer `403`).
* **Faction role**: decides every faction mutation, read from `hub_faction_members` **inside the mutating
  transaction**. An organization admin who wants to lead a faction founds or joins one like any player.
* **Platform founder admin** (`CHAMPION_ADMIN_DISCORD_IDS`): not consulted by any Hub route.

Players are not organization members. Reads and the player actions (found, apply, withdraw) are open to **any
synced Champion user** on any installation whose organization id matches the path.

## Recruitment

`recruitmentStatus` is one of:

* `OPEN` - players can submit applications.
* `INVITE_ONLY` - applications cannot be freely submitted (`409`; a future invite flow will allow them).
* `CLOSED` - no recruitment (`409`). This is the default for a new faction.

### Requirements (display only)

`hub_faction_settings` / the `requirements` object: `minimumHours` (nullable, 0-100000), `minimumAge` (nullable,
13-99), `pvpRequired`, `builderNeeded`, `micRequired` (booleans), `customRequirements` (plain text, 500).
They are shown to applicants and **never enforced**, and nothing verifies real-world identity.

## Applications

`hub_faction_applications`: `status` is `PENDING`, `ACCEPTED`, `DENIED`, `WITHDRAWN` or `CANCELLED`
(closed by the system when the applicant joined or founded a faction elsewhere on the installation).
**Rows are never deleted**; every outcome is a status and a user may apply again after a denial or withdrawal.

Applying is refused (`409`) when the faction is not `OPEN`, the applicant is already in **any** faction on the
installation (including this one), or a `PENDING` application to this faction already exists. A suspended
installation refuses it too.

* **Accept** (LEADER/OFFICER) is **one transaction**: authorize the actor, lock the applicant, re-validate the
  application is `PENDING`, re-validate the applicant is still eligible, create the membership (`MEMBER`),
  mark the application `ACCEPTED` with reviewer and time, and cancel the applicant's other pending applications
  on that installation. Any failure leaves nothing behind.
* **Deny** (LEADER/OFFICER): `DENIED` with `reviewed_by_user_id` / `reviewed_at`.
* **Withdraw**: only the applicant, only while `PENDING`; someone else's application looks like it does not exist.
* `answers_json` exists in the schema for leader-defined questions but the API keeps it **disabled**: an
  `answers` key is rejected as an unknown field, and no form logic is executed.

## One active faction per installation

Chosen for DayZ: a user is in at most one faction **per installation** (they may be in one on another server).
It is a database guarantee (`UNIQUE (installation_id, user_id)` on the membership) as well as a service check
(`409 CONFLICT`). Founding a faction also cancels the founder's pending applications there.

### Concurrency

Every operation that creates or changes a user's membership or applications on an installation first takes a
transaction-scoped **advisory lock on `(installation, user)`**, before any row lock. That serializes two
simultaneous founds by one user, two accepts of the same application, and two leaders accepting the same
applicant into different factions. Without the lock the last case deadlocks (verified while developing the
tests). Member-management actions additionally lock the faction row so a demotion cannot race a removal.
Losing requests get a typed `409` (`ErrAlreadyInFaction` / `ErrNotPending`), never a raw database error.

## Validation and moderation safety

* `name`: 3-32 characters; letters, digits, spaces and `- _ . ' & !` only (no mentions, markup or control
  characters); whitespace collapsed; at least two letters/digits. `tag`: 2-5 ASCII letters/digits, stored upper-case.
* `description` and application `message`: plain text, at most 500 characters. Control characters are removed and
  line endings normalized; **nothing is HTML-escaped or stripped** - text is stored as plain content and a client
  renders it escaped (a `<script>` in a description is stored and returned as inert text).
* Colors are `#RRGGBB` (stored upper-case); `""` clears one.
* **Visual keys** (`logoKey`, `flagKey`, `armbandKey`) exist in the schema and every response (always `null` for
  now) as placeholders for approved catalogs. They are **not writable**: the update route rejects them, and any URL
  key, with `400`. Nothing accepts a URL or an upload.
* Request bodies are limited to 16 KiB (`413`), must be exactly one JSON object, and reject unknown keys.
* Slugs are generated from the name (`UNIT ZERO` -> `unit-zero`; only `[a-z0-9]`, hyphen-separated, at most 40
  characters, `faction` as a fallback) and stay stable when a faction is renamed. A collision on the installation
  gets `-2`, `-3`, ... (a database `ON CONFLICT` on the slug index, so racing creates cannot pick the same one).

## API

All routes live under `/api/saas/organizations/{organizationID}/installations/{installationID}/factions` and use
the standard customer-API chain (service bearer auth, `X-Champion-Acting-User`, JSON errors
`{"error":{"code","message"}}`); see `docs/SAAS_API.md` and `docs/saas-openapi.yaml`.

| Method and path | Who | Notes |
|---|---|---|
| `GET /factions` | any synced user | directory; `recruiting=true`, `q` (name or tag, case-insensitive, max 50), `limit` (default 25, max 100), `cursor`; newest first; member counts come from the same query |
| `POST /factions` | any synced user | found a faction; body `name`, `tag`, `description`, `recruitmentStatus` (default `CLOSED`); creator becomes LEADER; `201` |
| `GET /factions/me` | any synced user | `{faction, membership, role, pendingApplications}` |
| `GET /factions/{factionID}` | any synced user | public profile: requirements, leader, officers, members (max 100), visual keys, `viewer` block; never application data |
| `PUT /factions/{factionID}` | LEADER | every field optional: `name`, `tag`, `description`, `recruitmentStatus`, `primaryColor`, `secondaryColor`, `requirements` |
| `POST .../{factionID}/applications` | any synced user | body `{"message"}` (optional); `201` |
| `GET .../{factionID}/applications` | LEADER/OFFICER, or org OWNER/ADMIN (read-only) | `status`, `limit`, `cursor`; includes the applicant's message |
| `POST .../applications/{applicationID}/accept` | LEADER/OFFICER | `{application, member}` |
| `POST .../applications/{applicationID}/deny` | LEADER/OFFICER | |
| `POST .../applications/{applicationID}/withdraw` | the applicant | |
| `GET .../{factionID}/members` | any synced user | most senior first; `limit` (default 100, max 200); `total` is the real count |
| `POST .../members/{memberID}/promote` | LEADER | MEMBER -> OFFICER |
| `POST .../members/{memberID}/demote` | LEADER | OFFICER -> MEMBER |
| `DELETE .../members/{memberID}` | LEADER (MEMBER, OFFICER) / OFFICER (MEMBER) | the LEADER can never be removed |

Status codes: `400` invalid input, `401` auth, `403` faction role does not allow it (or the leader is protected),
`404` not found in this tenant, `409` state conflicts (name/tag taken, already in a faction, not recruiting,
duplicate or non-pending application, invalid role change, no DayZ server, suspended), `413` body too large,
`429` rate limited.

Member and applicant objects carry `userId`, `discordUserId`, `username`, `displayName`, `avatar`, and, for members,
`gamertag` - the linked DayZ player's name when the user has a **verified** gamertag link on the guild
(`player_links.status = 'VERIFIED'`; `player_id` is recorded on the membership), otherwise `null`. It is never invented.

### Rate limits

In-memory, per acting user, using the existing `saasRateLimiter`: faction creation 1 per 10 seconds and 10 per day;
join applications 5 per minute and 40 per day. Every attempt counts, including one that fails validation.

### Audit events

Structured `slog` lines (`component=saas_api event=...`) with identifiers only - organization, installation,
acting user, faction, application, member and target user ids, and the new/removed role. **Never** names,
descriptions, application messages or any user-written text: `faction_created`, `faction_updated`,
`faction_application_created`, `faction_application_accepted`, `faction_application_denied`,
`faction_application_withdrawn`, `faction_member_promoted`, `faction_member_demoted`, `faction_member_removed`.
No organization-admin mutation override exists in Phase 1, so there is no override to audit; if an emergency
moderation override is added later it must be audited.

## Tests

Real PostgreSQL 16 (`-tags integration`, throwaway database only; `TEST_DATABASE_URL` +
`ALLOW_INTEGRATION_DB_TESTS=true`): `internal/repository/faction_hub_repository_integration_test.go` (schema
guarantees, creation and slugs, directory paging/search/counts, update, application rules, transactional accept,
member matrix, verified-link identity, isolation of **every** repository method across organizations and
installations, and the concurrency scenarios) and `internal/app/saas_api_factions_integration_test.go` (the real
routes over HTTP: authentication, route precedence for `/factions/me`, the full player flow, validation and plain-text
storage, pagination, organization role vs faction role, tenant isolation, rate limits, audit-log safety, suspended and
server-less installations). Pure rules: `internal/factionhub/factionhub_test.go`.

## Not in Phase 1 / future

* Leader transfer, leaving a faction (a member cannot leave on their own yet - the leader removes them), disbanding.
* Invite flow for `INVITE_ONLY`; leader-defined application questions (`answers_json`).
* Custom faction roles and per-role permissions.
* Visual customization: approved logo, DayZ flag and armband catalogs (`logoKey`/`flagKey`/`armbandKey` are
  validated against them when added), faction colors and banner/profile presentation, and any image upload.
* The bridge from a Hub faction to the Discord-side faction (`legacy_faction_id`) for kill/war statistics.
* The public website UI (Faction Hub, Discord Activity).
