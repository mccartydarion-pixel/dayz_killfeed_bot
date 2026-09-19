# Faction Hub - backend (Phase 1 foundation, Phase 4 logos / leadership transfer / self-leave, Phase 5 competitive stats)

The web-first, installation-scoped faction directory, membership and recruitment API.
**Backend only**: no website UI here (the website consumes this API). Phase 1 (migration `0032_faction_hub`) is the
foundation - directory, membership, recruitment. Phase 4 (migration `0033_faction_logo_assets`) adds secure logo storage,
leadership transfer and self-leave; see "Phase 4" below. Code: `internal/factionhub` (pure rules and catalogs),
`internal/repository/faction_hub_repository.go` and `faction_hub_phase4_repository.go` (SQL + transactions),
`internal/app/saas_api_factions.go` and `saas_api_faction_phase4.go` (HTTP).

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
* Nobody may remove or demote the LEADER (`403`); leadership changes hands only through the explicit transfer endpoint (Phase 4, below).
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
* **Visual keys**: `flagKey` and `armbandKey` are writable only as approved catalog keys (Phase 4, below); the logo has its own
  upload endpoints. `logoKey` is a legacy always-`null` field. Nothing accepts an external URL anywhere - only a validated
  multipart upload.
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
| `POST .../{factionID}/logo` | LEADER | Phase 4: multipart `file` (PNG/JPEG/WebP, <= 5 MiB, 128-2048 px); `201`/`200` |
| `DELETE .../{factionID}/logo` | LEADER | Phase 4: idempotent; back to the default logo |
| `POST .../{factionID}/transfer-leadership` | LEADER | Phase 4: `{"memberId"}`; atomic; the old leader becomes OFFICER |
| `POST .../{factionID}/leave` | the member | Phase 4: MEMBER/OFFICER only; the LEADER gets `409 LEADERSHIP_TRANSFER_REQUIRED` |
| `GET /assets/faction-logos/{publicId}.{ext}` | nobody (public) | Phase 4: the public logo image |

Status codes: `400` invalid input, `401` auth, `403` faction role does not allow it (or the leader is protected),
`404` not found in this tenant, `409` state conflicts (name/tag taken, already in a faction, not recruiting,
duplicate or non-pending application, invalid role change, no DayZ server, suspended), `413` body too large,
`429` rate limited.

Member and applicant objects carry `userId`, `discordUserId`, `username`, `displayName`, `avatar`, and, for members,
`gamertag` - the linked DayZ player's name when the user has a **verified** gamertag link on the guild
(`player_links.status = 'VERIFIED'`; `player_id` is recorded on the membership), otherwise `null`. It is never invented.

### Rate limits

In-memory, per acting user, using the existing `saasRateLimiter`: faction creation 1 per 10 seconds and 10 per day;
join applications 5 per minute and 40 per day. Logo uploads (Phase 4): 3 per minute and 20 per day. Every attempt counts, including one that fails validation.

### Audit events

Structured `slog` lines (`component=saas_api event=...`) with identifiers only - organization, installation,
acting user, faction, application, member and target user ids, and the new/removed role. **Never** names,
descriptions, application messages or any user-written text: `faction_created`, `faction_updated`,
`faction_application_created`, `faction_application_accepted`, `faction_application_denied`,
`faction_application_withdrawn`, `faction_member_promoted`, `faction_member_demoted`, `faction_member_removed`, and (Phase 4)
`faction_logo_uploaded`, `faction_logo_replaced`, `faction_logo_deleted`, `faction_leadership_transferred`, `faction_member_left`.
Logo events carry the asset id, content type and size only - never the image bytes or the client file name.
No organization-admin mutation override exists in Phase 1, so there is no override to audit; if an emergency
moderation override is added later it must be audited.

## Phase 4: logo storage, leadership transfer, self-leave

Backend only; the website designer (Phase 3) is wired to these routes in a later pass. Migration `0033_faction_logo_assets`
is additive. Code: `internal/logoimage` (image validation), `internal/assetstore` (storage abstraction),
`internal/factionassets` (upload/replace/delete/serve/sweep orchestration), `internal/repository/faction_hub_phase4_repository.go`,
`internal/app/saas_api_faction_phase4.go`.

### Storage audit (what already existed)

Champion had **no object storage** to reuse: no S3/R2/MinIO client (`go.mod` has no storage SDK), no bucket, no asset
abstraction, no storage variable on the Railway bot service, and the bot service has **no volume** - its filesystem is not durable
(every deploy replaces it), so it is never used for customer assets. Railway offers buckets (`railway bucket`) and Cloudflare R2
exists, but none is provisioned, and provisioning billable production infrastructure was not part of this phase. The only
durable store Champion has is its PostgreSQL database (a Railway volume). So:

* `assetstore.Store` is the abstraction (`Put`, `Get`, `Delete`, `List`, `URL`). Faction code depends only on it.
* The production implementation is `repository.PostgresAssetStore` (table `hub_asset_blobs`: `storage_key`, `content_type`,
  `data BYTEA`). Logos are small (<= 5 MiB, typically tens of KiB), the bytes live in their own table, and **no faction
  row ever holds image data** - `hub_factions` only has `logo_asset_id`, a pointer to a metadata row.
* `assetstore.MemoryStore` is the fake used by tests (no credentials, no network).
* An S3-compatible bucket (R2, Railway Bucket) can be added later as another `Store` implementation - `List` supports the orphan
  sweep and `URL` lets a public-bucket store return a direct URL instead of the Champion endpoint - with no change to the
  metadata model, the API or the website contract. Object storage becomes worthwhile when logo volume or serving load grows; the
  decision to provision it is the operator's.

### Data model

* `hub_faction_assets`: `id`, `public_id` (random UUID, the unguessable id in the public URL), `organization_id`,
  `installation_id`, `faction_id`, `asset_type` (`LOGO`), `storage_key` (server-generated), `content_type`
  (`image/png|jpeg|webp`), `size_bytes` (<= 5 MiB), `width`, `height`, `original_filename` (sanitized, display/debug only, never a
  path), `created_by_user_id`, `created_at`, `updated_at`. `(faction_id, installation_id)` is a composite FK to the faction and
  `(installation_id, organization_id)` to `installations`; the `storage_key` is `CHECK`ed to be a safe relative key.
* `hub_factions.logo_asset_id` with the composite FK `(logo_asset_id, id) -> hub_faction_assets(id, faction_id) ON DELETE SET NULL
  (logo_asset_id)`: a faction can only point at **one of its own assets** (enforced by the database; tested). Replacing or deleting the
  logo deletes the old metadata row, so a replaced logo's URL stops resolving.
* `hub_asset_blobs`: the PostgreSQL store's bytes, keyed by `storage_key`.
* Storage key: `factions/{organizationID}/{installationID}/{factionID}/{assetUUID}.{png|jpg|webp}` - built by the server from numeric
  ids, a fresh random UUID and a fixed extension map; **never** from the uploaded file name. `assetstore.ValidKey` (and the column
  `CHECK`) reject traversal (`..`), absolute paths, backslashes, empty segments and anything outside `[A-Za-z0-9/._-]`.

### Upload

`POST /api/saas/organizations/{organizationID}/installations/{installationID}/factions/{factionID}/logo` - **faction LEADER only**
(officer, member, outsider, and organization OWNER/ADMIN without the faction role get `403`; another tenant's ids are `404`). The
LEADER check runs **before the body is read**, so an unauthorized caller costs one query, not a 5 MiB upload. Then the per-user
rate limit (3 per minute, 20 per day; `429`).

* `multipart/form-data` with **exactly one part named `file`**. Any other field (`externalUrl`, `url`, a base64 text field, raw SVG
  in a text field) or a second file is `400`; a non-multipart body (JSON with a base64 blob or an external URL, a raw image) is `415`
  `UNSUPPORTED_MEDIA_TYPE`. Nothing ever fetches an external URL.
* Accepted formats: **PNG, JPEG, WebP only**. Rejected: SVG, GIF, HTML, PDF, animated WebP and every other type (`415`).
* The type is decided from the **bytes** (signature + a real parse), not the file name or the `Content-Type` header. If the part declares
  a type that contradicts the bytes (`image/svg+xml` on a PNG), it is `415`; `application/octet-stream` or no declared type is fine.
* Limits: at most **5 MiB** (`413`, also for a body that streams past the limit); each side **128-2048 px** (`400`). Dimensions are
  read from the header **before any pixel decoding**, so a small file that claims a huge canvas (a decompression bomb) is rejected
  without allocating for it. PNG and JPEG are then fully decoded within that bounded size to prove they are not corrupt (`400`).
  WebP has no decoder in the Go standard library, so it is validated structurally (RIFF framing, exact chunk sizes, a well-formed
  VP8/VP8L/VP8X header, matching canvas, in-range dimensions) rather than pixel-decoded. An empty file is `400`.
* The original image is stored as uploaded (not re-encoded, so image metadata such as EXIF is preserved; a later phase may add
  metadata stripping and thumbnails). Squareness is not required: the website may crop or contain.
* Response `201` (first logo) or `200` (replacement): `{"logo": {...}, "replaced": false|true}` (see the contract below).
* The client file name is sanitized (base name only, `[A-Za-z0-9._ -]`, <= 100 chars) and kept for display only.

### Replacement, deletion and orphan safety

Upload order is what makes failures safe (`factionassets.Service.UploadLogo`):

1. validate the bytes (nothing is stored for a bad image);
2. write the **new** bytes to the store (the old logo is still stored and referenced);
3. **one transaction**: LEADER check, insert the asset row, repoint the faction, delete the old row - if it fails, the new bytes are
   deleted (if even that fails they are left as an orphan);
4. only **after the commit**, delete the old bytes (failure leaves an orphan).

If the byte store fails at step 2 nothing is recorded and the current logo is untouched (`500`, no vendor detail). Orphans (bytes with
no metadata row: a failed delete, a rolled-back upload whose cleanup failed, a deleted faction) are removed by
`SweepOrphans`, which runs hourly from `App.Run`: it lists objects under `factions/` older than **two hours** (so an upload that has
written bytes but not yet committed is never touched) and deletes those no asset row references.

`DELETE .../factions/{factionID}/logo` - LEADER only, **idempotent**: `200 {"logo": null, "deleted": true|false}`; the faction returns to
the Champion default logo, the metadata row and the bytes are removed, and the old URL is `404`.

### Public logo serving

Logos are public profile assets. `GET /assets/faction-logos/{publicId}.{png|jpg|webp}` on the bot's public origin needs **no
credentials**: the unguessable UUID is the only address, the extension must match the stored format, and a replaced/deleted logo does
not resolve. Responses carry `Content-Type` (the validated type), `X-Content-Type-Options: nosniff`, `Content-Security-Policy:
default-src 'none'; sandbox`, `Cross-Origin-Resource-Policy: cross-origin`, `Cache-Control: public, max-age=31536000, immutable` and an
`ETag` (`304` on `If-None-Match`). Every faction summary/profile/`me` response embeds the logo object, so the public Hub renders it with
no privileged call and no storage credential ever reaches a client.

The absolute URL uses `CHAMPION_PUBLIC_BASE_URL` (a bare `https://host` origin), falling back to Railway's `RAILWAY_PUBLIC_DOMAIN`;
with neither, `logo.url` is root-relative (`/assets/faction-logos/...`) and the website must prefix the bot's origin. If profile
responses are cached by a client, refresh them after a logo change: a new upload always has a new URL, and the upload/delete
responses carry the new state. The backend itself caches nothing.

### Flag, armband and color validation (backend is authoritative)

Before Phase 4 `flagKey`/`armbandKey` were not writable at all (the website Phase 3 designer needs them). `PUT .../factions/{factionID}`
(LEADER only) now accepts them, validated **server-side** against approved catalogs (`factionhub.DayzFlags`, `factionhub.Armbands`),
normalized to upper-case; `""` clears; anything else is `400`:

| Catalog | Approved keys |
|---|---|
| `flagKey` (DayZ flags) | `BLACK`, `BLUE`, `GREEN`, `RED` |
| `armbandKey` (armbands) | `BLACK`, `BLUE`, `GREEN`, `ORANGE`, `PINK`, `RED`, `WHITE`, `YELLOW` |

These match the website Phase 3 catalog (`lib/factions/dayzBranding.ts`). Keys are stable identifiers, never URLs. Add a key to the Go
catalog (and here) before the website offers it. `primaryColor`/`secondaryColor` must be exactly `#RRGGBB` (normalized to upper-case);
`rgb()`, `url()`, `var()`, `expression()`, named colors, short hex and any `;`, quote, brace or comment injection are `400`.
`logoKey` is a legacy always-`null` field; the logo is `logo`, and `logoKey`/`logoUrl`/`logoAssetId` in a PUT body are rejected.

### Leadership transfer

`POST .../factions/{factionID}/transfer-leadership` with `{"memberId": <target membership id>}` - **current LEADER only** (officer,
member, outsider and organization OWNER/ADMIN get `403`). The target must be another member of **this** faction (a member id from
another faction or tenant is `404`; the leader's own id is `409`); a MEMBER or an OFFICER may be chosen.

Atomic: one transaction locks the faction row and the member rows, demotes the old leader to `OFFICER` and promotes the target to
`LEADER` (the one-leader unique index is the backstop), so the faction never has zero or two leaders. Two simultaneous transfers
serialize: exactly one wins (`200`), the other finds its actor is no longer LEADER (`403`). `created_by_user_id` keeps recording the
founder. Response: `{"leader": <member>, "previousLeader": <member>}`.

### Self-leave

`POST .../factions/{factionID}/leave` (no body): the acting user leaves **their own** membership. `MEMBER` and `OFFICER` may. The
`LEADER` may not while leading: `409` with code `LEADERSHIP_TRANSFER_REQUIRED` - transfer first. A caller who is not a member
(including a repeated leave) gets `403`. Application history is untouched (an `ACCEPTED` application stays `ACCEPTED`; leaving never
creates or revives one), and the user may immediately apply elsewhere, re-apply, or found a faction on the installation. A sole leader
therefore cannot leave; disbanding is a later feature. Response: `{"left": true, "member": <the ended membership>}`.

### Website handoff contract (exact JSON)

`logo` (on **FactionSummary**, and therefore on the directory rows, `GET .../factions/me`'s `faction`, and **FactionProfile**) is either
`null` (Champion default logo) or:

```json
{ "id": 123, "url": "https://<public-origin>/assets/faction-logos/<uuid>.png", "contentType": "image/png", "width": 512, "height": 512 }
```

`contentType` is one of `image/png`, `image/jpeg`, `image/webp`; the file extension in `url` is `png`, `jpg` or `webp`. There is no other
logo field to trust (`logoKey` stays `null`).

| Call | Success | Body |
|---|---|---|
| `POST .../logo` (multipart, field `file`) | `201` first logo / `200` replacement | `{"logo": {...}, "replaced": false}` |
| `DELETE .../logo` | `200` (always, idempotent) | `{"logo": null, "deleted": true}` (`false` when there was no logo) |
| `PUT .../factions/{id}` (`flagKey`, `armbandKey`, colors) | `200` | the updated FactionProfile |
| `POST .../transfer-leadership` `{"memberId"}` | `200` | `{"leader": FactionMember, "previousLeader": FactionMember}` |
| `POST .../leave` | `200` | `{"left": true, "member": FactionMember}` |

Errors use the standard `{"error":{"code","message"}}` envelope. New in Phase 4: `415 UNSUPPORTED_MEDIA_TYPE` (not PNG/JPEG/WebP, a
declared type that contradicts the bytes, or a non-multipart body), `409 LEADERSHIP_TRANSFER_REQUIRED`; upload also uses `400 INVALID_REQUEST` (empty,
corrupt, wrong dimensions, wrong form fields), `413 PAYLOAD_TOO_LARGE` (over 5 MiB), `403 FORBIDDEN`, `404 NOT_FOUND`, `429 RATE_LIMITED`.
The upload must be sent by the website **server-side** (it holds the service secret): forward the browser's multipart file to the route
with `Authorization: Bearer <secret>` and `X-Champion-Acting-User`. Browsers never call it directly.

## Phase 5: competitive stats, achievements and activity

Faction profiles now carry real competitive data, all **derived from Champion's runtime data** (kills, deaths, bounties, server records) joined to
**membership periods** - a kill counts for a faction only while its player was a member, and only through a **verified DayZ link** (never a name match).
Full model, attribution rules, SQL semantics, cache, performance and the exact website DTOs: **`docs/FACTION_STATS.md`**. Migration `0034_faction_stats_history`
adds `hub_faction_membership_history` (one row per membership period, written in the same transaction as every join/leave/removal, current members backfilled),
`hub_faction_activity` (public-safe hub events, no free text) and `hub_faction_achievement_unlocks` (unique per faction + achievement).

| Route | Who | Notes |
|---|---|---|
| `GET .../{factionID}/stats` | any synced user | `{summary, memberContributions, updatedAt}`; cached 45 s, invalidated by kills, deaths, bounty claims and membership changes |
| `GET .../{factionID}/activity` | any synced user | public feed, newest first; `limit` (default 20, max 100), `cursor` |
| `GET .../{factionID}/achievements` | any synced user | 10 system-defined achievements with `unlocked`, `unlockedAt`, `progress`/`target` |
| `GET .../factions/{factionID}` | any synced user | the profile now has `stats` (the summary, or `null`) |

Public activity is **not** the audit log: audit events (`faction_*`, ids only) stay in the logs; the feed shows only public-safe events (a removal reads as
`MEMBER_LEFT`; no application messages, reviewers or internal ids). Unlinked members are listed with all figures zero and `identity: "UNLINKED"`.

## Tests

Real PostgreSQL 16 (`-tags integration`, throwaway database only; `TEST_DATABASE_URL` +
`ALLOW_INTEGRATION_DB_TESTS=true`): `internal/repository/faction_hub_repository_integration_test.go` (schema
guarantees, creation and slugs, directory paging/search/counts, update, application rules, transactional accept,
member matrix, verified-link identity, isolation of **every** repository method across organizations and
installations, and the concurrency scenarios) and `internal/app/saas_api_factions_integration_test.go` (the real
routes over HTTP: authentication, route precedence for `/factions/me`, the full player flow, validation and plain-text
storage, pagination, organization role vs faction role, tenant isolation, rate limits, audit-log safety, suspended and
server-less installations). Pure rules: `internal/factionhub/factionhub_test.go`. Phase 4: `internal/repository/faction_hub_phase4_integration_test.go` (asset metadata lifecycle, schema guarantees, isolation of every new method, transfer matrix and races, self-leave, the PostgreSQL asset store), `internal/app/saas_api_faction_phase4_integration_test.go` (upload/serve/replace/delete over HTTP, the security rejections, storage-failure and orphan handling, catalog validation, transfer, leave, audit safety), and unit tests in `internal/logoimage`, `internal/assetstore`, `internal/factionassets` and `internal/factionhub`. Storage tests use the in-memory store; no bucket credentials are needed.

## Not in Phase 1 / future

* Disbanding a faction (a sole leader can neither leave nor be removed).
* A faction leaderboard (Phase 5 exposes comparable figures but no ranking endpoint), playtime and hit statistics.
* Invite flow for `INVITE_ONLY`; leader-defined application questions (`answers_json`).
* Custom faction roles and per-role permissions.
* Logo thumbnails and metadata stripping; banner/profile presentation; an S3-compatible bucket implementation of `assetstore.Store`
  when volume warrants it (the interface and the API do not change).
* The bridge from a Hub faction to the Discord-side faction (`legacy_faction_id`) for kill/war statistics.
* The public website UI (Faction Hub, Discord Activity).
