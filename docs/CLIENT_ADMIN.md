# Client Admin Control Plane (Phase 1)

This document is the design record and website-integration reference for Champion's tenant
administration system: a capability-based permission model with Discord-role mapping, an admin
audit log, and a curated set of website-controlled DayZ/Discord/economy/faction/stats admin
actions. It follows the same evidence-first discipline as `docs/NITRADO_DELTA_READS.md`: every
capability below is reported honestly as **implemented**, **implemented on an existing primitive**,
or **deferred**, with the reasoning for each deferral - nothing here is a fake website button wired
to nothing.

## Permission model

Three separate authorization concepts coexist in this codebase; this phase adds the third and
touches neither of the first two:

1. **Champion's own platform-admin allowlist** (`internal/app/admin_api.go`'s
   `requirePlatformAdmin`) - gates Champion's own founder/staff access to every tenant. Unrelated
   to a Client's own admins; never consulted by anything in this phase.
2. **An organization's flat OWNER/ADMIN/MEMBER membership role**
   (`repository.RoleOwner`/`RoleAdmin`/`RoleMember`) - still governs the onboarding/billing/settings
   routes built in earlier phases. Untouched.
3. **Champion tenant-administration Levels** (`internal/permissions`, this phase): `OWNER >
   ADMINISTRATOR > MODERATOR > GATEKEEPER`, each level a strict superset of every capability the
   levels below it hold. A Client maps their own Discord roles to these Levels
   (`installation_role_permissions`, per-installation); the Level -> capability mapping itself is
   fixed by Champion (`permissions.requiredLevel`), not customer-configurable this phase (the task's
   "Client should be allowed to customize the mapping later" refers to the role-to-Level mapping,
   which is fully customizable today).

**Bootstrap**: an organization's `owner_user_id` is always resolved to Level `OWNER` for that
organization's installations, regardless of Discord role mappings - otherwise a fresh installation
with zero configured mappings would have no one able to create the first one.
`internal/app.App.actorLevel` implements this: check org ownership first, then fetch the actor's
live Discord roles in the installation's guild (`discord.Client.MemberRoles`, cached 15s per
actor+guild to bound REST calls during a burst of admin actions) and take the highest Level any of
those roles is mapped to.

**Escalation safety** (`permissions.CanGrant`): an actor may only create/modify/delete a
permission mapping granting a Level at or below their own resolved Level - enforced server-side in
`handleSetPermission`/`handleDeletePermission`, never trusting the request body's claimed level.

## Capability -> Level table

| Capability | Level | Notes |
|---|---|---|
| `PERMISSIONS_VIEW` | Moderator | list role mappings + read audit log |
| `PERMISSIONS_MANAGE` | Moderator (+ escalation ceiling) | set/delete a mapping, capped by `CanGrant` |
| `ECONOMY_VIEW` | Moderator | alias of the existing OWNER/ADMIN-gated balance search, reachable by capability instead |
| `WARNINGS_VIEW` | Moderator | list + issue |
| `WARNINGS_CLEAR` | Administrator | |
| `FACTION_MODERATE` | Moderator | list/view |
| `FACTION_DISSOLVE` | Administrator | HIGH RISK, typed confirmation `"DISSOLVE"` |
| `BOUNTY_MANAGE` | Moderator | reset (bulk-cancel) a target's active bounties |
| `PLAYER_STATS_RESET` | Administrator | streak only this phase - see Deferred |
| `SERVER_STATS_RESET` | Owner | HIGH RISK, typed confirmation `"RESET EVERYONE"` |
| `SERVER_RESTART` | Moderator | MEDIUM RISK, typed confirmation `"RESTART"`, 1/5min |
| `SERVER_STOP` | Administrator | HIGH RISK, typed confirmation `"STOP"`, 1/min |
| `SERVER_AUTOSTART` | Administrator | DB columns shipped; monitor loop deferred, see below |
| `SERVER_NAME_EDIT` | Administrator | Champion display name only, never a Nitrado rename |
| `WHITELIST_MANAGE` | Gatekeeper | |
| `BANLIST_MANAGE` | Moderator | |
| `PLAYER_LAST_ONLINE_VIEW` | Moderator | |
| `FEED_LOCATION_MANAGE` | Moderator | per-route `show_location` toggle |
| `MAINTENANCE_MODE` | Administrator | Champion-side flag only |
| `PLAYER_DIRECTORY_VIEW` | Moderator | Phase 3, `docs/PLAYER_INTELLIGENCE.md` - the authoritative player directory |
| `PLAYER_LAST_LOCATION_VIEW` | Administrator | Phase 3 - a player's current location; online-players-with-location |
| `PLAYER_LOCATION_VIEW` | Administrator | Phase 3 - full location history |
| `ZONE_VIEW` | Moderator | Phase 4, `docs/ZONES_UAV_RADAR.md` - list/read zones, ignore/authorized/ban lists, intruders/intrusions |
| `ZONE_MANAGE` | Administrator | Phase 4 - zone CRUD (+ `UAV_MANAGE` for UAV/BASE_RADAR), authorized-entry CRUD, ban CRUD |
| `ZONE_IGNORE_MANAGE` | Administrator | Phase 4 - ignore-entry CRUD, its own capability separate from `ZONE_MANAGE` |
| `UAV_MANAGE` | Owner | Phase 4 - required in addition to `ZONE_MANAGE` for any UAV/BASE_RADAR zone |
| `INTRUSION_ACK` | Moderator | Phase 4 - acknowledge an active intrusion |

## Current Actor Client Admin Permissions (Phase 1 Part 2)

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/me` reports the
acting user's own resolved Level and exact capability set for the selected installation - the
website's Client Server Admin UI uses this single call to decide which controls to show, rather
than inferring visibility from the organization OWNER/ADMIN/MEMBER role, a website session flag,
or Champion's own platform-owner status. **These are Champion BOT permission levels
(`internal/permissions`) - not Champion Platform Owner roles and not organization billing roles.**
The three stay entirely separate; `/admin/me` reports only the first.

Unlike every other route in this document, `/me` requires no specific capability - it is the one
route whose entire job is to report whatever Level (possibly none) the actor resolves to, reusing
exactly the same `actorLevel` resolution `requireCapability` already uses for every other route
(organization-owner bootstrap, then the highest Level among the actor's live, cached Discord role
mappings). An actor with no mapped Level gets the same `403 ADMIN_FORBIDDEN` any other route would
give them, not a fabricated "safe" 200.

```
GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/me

200 (has a Level):
{
  "level": "MODERATOR",
  "capabilities": ["BANLIST_MANAGE","BOUNTY_MANAGE","ECONOMY_VIEW","FEED_LOCATION_MANAGE",
                    "PERMISSIONS_MANAGE","PERMISSIONS_VIEW","PLAYER_LAST_ONLINE_VIEW",
                    "SERVER_RESTART","WARNINGS_VIEW","WHITELIST_MANAGE"],
  "discordRoleIds": ["123456789012345678"]
}

403 ADMIN_FORBIDDEN (no mapped Level):
{"error":{"code":"ADMIN_FORBIDDEN","message":"no Champion bot permission level is mapped for this user on this installation"}}

503 ADMIN_DISCORD_UNAVAILABLE (couldn't verify live Discord roles - never falls back to elevated
or silently downgraded access):
{"error":{"code":"ADMIN_DISCORD_UNAVAILABLE","message":"could not verify Discord roles"}}
```

`capabilities` is never a second hand-maintained list - it is `permissions.CapabilitiesForLevel`
applied to the resolved Level, i.e. exactly the same `requiredLevel` table every other route's
`permissions.Allows` check reads, sorted alphabetically for a stable, diff-friendly response (two
calls with the same Level always return byte-identical `capabilities`). `discordRoleIds` is the
live role list Discord returned during resolution - omitted (not fabricated as empty) when the
organization-owner bootstrap resolved the Level without a Discord lookup at all.

## Routes

All under `/api/saas/organizations/{organizationID}/installations/{installationID}/admin`, same
auth chain as every other SaaS route (`Authorization: Bearer <secret>` +
`X-Champion-Acting-User`), then `requireCapability` (`/me` above is the one exception).

```
GET    /me                                    (no capability - reports the actor's own Level/capabilities)
GET    /permissions                          PERMISSIONS_VIEW
PUT    /permissions/{discordRoleID}           PERMISSIONS_MANAGE   body: {"level":"MODERATOR"}
DELETE /permissions/{mappingID}                PERMISSIONS_MANAGE
GET    /audit-log                              PERMISSIONS_VIEW

GET    /warnings/{playerID}                    WARNINGS_VIEW
POST   /warnings/{playerID}                    WARNINGS_VIEW        body: {"reason":"..."}
POST   /warnings/{playerID}/{warningID}/clear  WARNINGS_CLEAR

POST   /bounties/{playerID}/reset              BOUNTY_MANAGE

GET    /factions                               FACTION_MODERATE
POST   /factions/{factionID}/dissolve          FACTION_DISSOLVE     body: {"confirm":"DISSOLVE"}

GET    /players/{playerID}/last-online         PLAYER_LAST_ONLINE_VIEW
GET    /economy/accounts?q=                    ECONOMY_VIEW

PUT    /server/name                            SERVER_NAME_EDIT     body: {"name":"..."}
PUT    /feeds/{routeKey}/location              FEED_LOCATION_MANAGE body: {"showLocation":bool}
PUT    /maintenance-mode                       MAINTENANCE_MODE     body: {"enabled":bool}

POST   /server/restart                         SERVER_RESTART       body: {"reason":"...","confirm":"RESTART"}
POST   /server/stop                            SERVER_STOP          body: {"reason":"...","confirm":"STOP"}

GET    /whitelist                              WHITELIST_MANAGE
POST   /whitelist                              WHITELIST_MANAGE     body: {"identifier":"...","reason":"...","notes":"...","expiresAt":null}
DELETE /whitelist/{identifier}                 WHITELIST_MANAGE

GET    /banlist                                BANLIST_MANAGE
POST   /banlist                                BANLIST_MANAGE       body: (same shape as whitelist)
DELETE /banlist/{identifier}                   BANLIST_MANAGE

POST   /stats/player/{playerID}/reset-streak   PLAYER_STATS_RESET
POST   /stats/reset-season                     SERVER_STATS_RESET   body: {"name":"Season 2","confirm":"RESET EVERYONE"}

GET    /players                                PLAYER_DIRECTORY_VIEW     ?q=&online=&linked=&cursor=&limit=  (Phase 3, docs/PLAYER_INTELLIGENCE.md)
GET    /players/online                         PLAYER_LAST_LOCATION_VIEW  currently-connected players + latest known location each
GET    /players/{playerID}/locations/latest    PLAYER_LAST_LOCATION_VIEW  404 if never observed
GET    /players/{playerID}/locations           PLAYER_LOCATION_VIEW      ?from=&to=&eventType=&cursor=&limit=, newest first

GET    /zones                                  ZONE_VIEW                 (Phase 4, docs/ZONES_UAV_RADAR.md)
POST   /zones                                  ZONE_MANAGE (+UAV_MANAGE for UAV/BASE_RADAR)
GET    /zones/{zoneID}                          ZONE_VIEW
PUT    /zones/{zoneID}                          ZONE_MANAGE (+UAV_MANAGE if the zone's type is UAV/BASE_RADAR)
DELETE /zones/{zoneID}                          ZONE_MANAGE (+UAV_MANAGE if the zone's type is UAV/BASE_RADAR)
GET    /zones/{zoneID}/ignore                   ZONE_VIEW
POST   /zones/{zoneID}/ignore                   ZONE_IGNORE_MANAGE       body: {"entryType":"PLAYER|FACTION|DISCORD_ROLE","entryValue":"..."}
DELETE /zones/ignore/{entryID}                  ZONE_IGNORE_MANAGE
GET    /zones/{zoneID}/authorized               ZONE_VIEW
POST   /zones/{zoneID}/authorized               ZONE_MANAGE              body: {"entryType":"PLAYER|FACTION","entryValue":"..."}
DELETE /zones/authorized/{entryID}              ZONE_MANAGE
GET    /zones/{zoneID}/bans                     ZONE_VIEW
POST   /zones/{zoneID}/bans                     ZONE_MANAGE              body: {"playerId":123,"reason":"..."}
DELETE /zones/bans/{banID}                      ZONE_MANAGE              (lifts the ban)
GET    /zones/{zoneID}/active-intruders         ZONE_VIEW
GET    /intrusions/active                       ZONE_VIEW                ?zoneId=&zoneType=&playerId=&acknowledged=
GET    /intrusions/history                      ZONE_VIEW                ?zoneId=&playerId=&from=&to=&status=&cursor=&limit=, newest first
POST   /intrusions/{intrusionID}/acknowledge    INTRUSION_ACK
```

Error codes added: `ADMIN_FORBIDDEN` (403, missing capability), `ADMIN_ESCALATION_DENIED` (403,
tried to grant/remove above own ceiling), `ADMIN_CONFIRMATION_REQUIRED` (409, missing/wrong typed
confirmation), `ADMIN_DISCORD_UNAVAILABLE` (503, couldn't resolve live Discord roles).

## DayZ/Nitrado capability audit

Verified against Nitrado's own official PHP SDK (`github.com/nitrado/NitrAPI-PHP`,
`lib/Nitrapi/Services/Gameservers/{Gameserver,Whitelist,Banlist}.php`) rather than assumed - the
same standard `docs/NITRADO_DELTA_READS.md` applied to the seek/offset-count endpoints.

| Capability | Status | Evidence |
|---|---|---|
| restart | **NITRADO API** | `POST /services/{id}/gameservers/restart` (optional `message`) |
| stop | **NITRADO API** | `POST /services/{id}/gameservers/stop` (optional `message`) |
| whitelist add/remove | **NITRADO API** | `POST`/`DELETE /services/{id}/gameservers/games/whitelist` (`identifier`) |
| banlist add/remove | **NITRADO API** | `POST`/`DELETE /services/{id}/gameservers/games/banlist` (`identifier`) |
| start (already-installed, currently-stopped server) | **UNVERIFIED** | No distinct endpoint found in the SDK separate from `restart`; `autoStart`'s design assumes `restart` also starts a stopped server (common in game panels) but this was not verified live - see Deferred |
| priority queue | **FILE-BASED, DEFERRED** | Community docs describe a `priority.txt` file on the server install, not a dedicated API endpoint; this codebase has zero Nitrado file-**write** capability (only reads - `internal/nitrado` is read-only), and the task's own "never replace entire config blindly" instruction rules out guessing at an unverified file format |
| ban list *duration* | **DEFERRED** | Nitrado's banlist API takes only `identifier`, no duration/expiry - Champion's own `installation_access_entries.expires_at` records the intent, but nothing currently enforces an automatic un-ban when it passes (see Deferred) |
| base damage / container damage / third-person / raid toggles | **UNSUPPORTED/DEFERRED** | No endpoint for any of these appears anywhere in Nitrado's official SDK; these are almost certainly DayZ `serverDZ.cfg`-style file settings, which would need the same unverified file-write capability as priority |
| generic `setConfig` (arbitrary allowlisted config writes) | **PARTIALLY DEFERRED** | Implemented for Champion-side settings that already exist in `server_configs`/`installation_channel_routes` (feed toggles, maintenance mode, location visibility) via their own dedicated endpoints above; a general DayZ-server-config-file writer is deferred with config writes generally |

## What changed (Phase 4: zones + UAV / Base Radar)

Full design record in `docs/ZONES_UAV_RADAR.md`. Summary: new migration
`0042_zones_uav_base_radar` (`installation_zones`, `zone_ignore_entries`,
`zone_authorized_entries`, `zone_bans`, `zone_presence`, `zone_intrusions`); new
`internal/repository/zone_repository.go`; new `internal/killfeed/zone_cache.go` (bounded,
invalidated-on-CRUD, TTL-fallback per-server zone cache) and
`internal/killfeed/intrusion_engine.go` (the OUTSIDE/INSIDE state machine, sharing one engine for
every zone type including UAV/Base Radar - no duplicated intrusion logic), wired into
`LocationQueue.persist` as one additive, panic-recovered call - `Engine.processLine` is completely
untouched by this phase, exactly like Phase 3. New capabilities `ZONE_VIEW`/`ZONE_MANAGE`/
`ZONE_IGNORE_MANAGE`/`UAV_MANAGE`/`INTRUSION_ACK`; 18 new routes (zone CRUD, ignore/authorized/ban
list CRUD, active-intruders, installation-wide active-intrusions, intrusion history, acknowledge).
Zone bans never touch the Nitrado banlist. Discord alerting only ever targets a zone's own
`alert_channel_id` - never a fallback channel. Heatmaps, Auto Payments, the priority queue, and
Auto Start remain deferred.

## What changed (Phase 3: player intelligence / location history foundation)

Full design record in `docs/PLAYER_INTELLIGENCE.md`. Summary: new migration
`0041_player_location_events`; new `internal/killfeed/location_queue.go` (a fully separate,
non-blocking, batched persistence pipeline for ADM `pos=<...>` data, wired into
`Engine.processLine` as an additive call - `PersistenceQueue` untouched); new
`internal/repository/location_repository.go` (player directory + location history/latest/
retention queries); new capabilities `PLAYER_DIRECTORY_VIEW`/`PLAYER_LAST_LOCATION_VIEW`/
`PLAYER_LOCATION_VIEW`; new routes `GET .../admin/players`, `.../players/online`,
`.../players/{playerID}/locations`, `.../players/{playerID}/locations/latest`; a new hourly
retention job (`CHAMPION_LOCATION_RETENTION_DAYS`, default 30). Zones/UAV/heatmap remain deferred
(task's own explicit instruction) - this phase is the data foundation only.

## What changed (Phase 1 Part 2: permission introspection)

- **New route** `GET .../admin/me` (`handleClientAdminMe`) and **new helper**
  `permissions.CapabilitiesForLevel(level)` - derives the capability list from the single
  `requiredLevel` table rather than a second hand-maintained list, sorted for stable output.
- **Refactor, no behavior change**: `actorLevel` now also returns the resolved Discord role IDs;
  the shared preamble (service auth, acting user, path ids, scope, Level resolution) was extracted
  into `resolveAdminActor`, and `requireCapability` is now that preamble plus its capability gate -
  every existing route's behavior is identical, just built on the shared helper instead of
  duplicating it.

## What changed (Phase 1)

- **New migration** `0040_client_admin_control_plane`: `installation_role_permissions`,
  `admin_audit_log`, `player_warnings`, `installation_access_entries`,
  `installation_channel_routes.show_location`, and `server_configs` columns for maintenance mode
  and an autostart monitor's future state (`autostart_enabled`, `autostart_offline_minutes`,
  `autostart_cooldown_minutes`, `autostart_last_attempt_at`, `autostart_attempt_count`).
- **New package** `internal/permissions`: Level/Capability types, the fixed capability->Level
  table, `Allows`, `CanGrant`.
- **New repositories**: `PermissionsRepository`, `AuditRepository`, `ClientAdminRepository`
  (scope resolution, warnings, server name, feed location, maintenance mode, access-list metadata,
  faction admin listing), plus small additions to `BountyRepository`
  (`ListActiveForTarget`) and `discord.Client` (`MemberRoles`).
- **New Nitrado client methods** (`internal/nitrado/gameserver_actions.go`): `Restart`, `Stop`,
  `WhitelistAdd/Remove`, `BanlistAdd/Remove` - all additive, `ReadLog`/every existing method
  untouched.
- **New API surface**: `internal/app/saas_api_permissions.go` (the `requireCapability` middleware
  + permissions management + audit log) and `internal/app/saas_api_client_admin.go` (every
  capability route above).
- **Tests**: `internal/permissions` unit tests (hierarchy inheritance, escalation safety, fail-closed
  on unknown levels/capabilities); `internal/nitrado` httptest coverage for every new client method
  against the documented endpoint shapes; `internal/app` integration tests (real PostgreSQL, fake
  Discord, fake Nitrado) covering organization-owner bootstrap, Discord-role-mapping resolution,
  escalation denial, audit log writes, typed-confirmation enforcement, the whitelist
  add/list/remove round trip, and the season-reset preserving history.

## Deferred (explicit scope decisions, not silent gaps)

Given the size of the original request (roughly fifteen distinct subsystems), this phase built the
full permission/audit foundation plus every capability with a real, verifiable backing mechanism,
and deferred the rest rather than ship unverified or fabricated plumbing:

- **Priority list enforcement, base/container damage, third-person, raid, generic DayZ config
  writes** - no documented Nitrado API endpoint exists for any of these (see the audit table
  above); building blind file-patch logic against an unverified format was judged a correctness
  risk this phase's own standard rules out, matching `docs/NITRADO_DELTA_READS.md`'s precedent.
- **`resetPlayerStat` for kills/deaths/headshots** (implemented for streak only via the existing
  `StreakRepository.Reset`) - a genuine per-player, per-stat reset for an aggregate currently
  computed from immutable `kills`/`deaths` rows would need a new baseline-override column consulted
  by every existing stat read path (leaderboards, embeds, the player API) to stay consistent; that
  retrofit was scoped out to keep this phase's blast radius bounded to new, additive code paths.
- **`resetServer`/`resetEveryoneStat`** - both map onto the *existing* season mechanism
  (`SeasonRepository.Start`/`End`): ending the active season and starting a new one resets every
  season-scoped stat guild-wide while preserving full history (`seasons`/`season_results`, never a
  deleted `kills`/`deaths` row). This is one primitive serving both task items, not two separate
  single-stat-only implementations, because that is what the real existing mechanism supports.
- **Auto payments** (scheduled weekly/role-based Champion Points payouts) - a genuine new
  subsystem (a table, a CRUD API, and a ticking scheduler with idempotent-per-run execution against
  the existing `EconomyService.AdminCredit`). Not built this phase to keep scope bounded; the
  schema this document's "Routes" section implies (`installation_auto_payments`,
  `discord_role_id`, `amount_points`, `frequency_days`, `next_run_at`, idempotent via a
  `run_key` unique per scheduled run) is specified precisely enough to implement directly as a
  fast follow.
- **~~Location-history data pipeline~~ - built in Phase 3** (`docs/PLAYER_INTELLIGENCE.md`):
  `player_location_events` now persists real ADM positions, off the hot path via a dedicated
  `LocationQueue`, with a full player directory (`PLAYER_DIRECTORY_VIEW`) and location APIs
  (`PLAYER_LAST_LOCATION_VIEW`/`PLAYER_LOCATION_VIEW`).
- **~~Zones, zone-ignore, zone-ban, Base Radar/UAV~~ - built in Phase 4** (`docs/ZONES_UAV_RADAR.md`):
  a stateful intrusion engine consuming `player_location_events`, sharing one engine for every zone
  type (no duplicated UAV/Base Radar logic). **Heatmap aggregation is still deferred** - it was not
  part of Phase 4's scope either.
- **Discord kick/ban/timeout/bulk message clear** - discordgo has direct, well-documented support
  for all four; deferred purely to keep this phase's already-large surface bounded, not because of
  any capability gap.
- **Alert center, live operations dashboard, maintenance-mode UI wiring beyond the flag itself,
  action preview** - these are aggregation/UI-adjacent concerns best built once the underlying
  primitives above exist to aggregate; `maintenance_mode` itself shipped (a real DB column + a real
  toggle endpoint) since it was effectively free.
- **`SERVER_AUTOSTART`** - the `server_configs` monitor-state columns shipped (offline-since,
  cooldown, attempt count) so a follow-up phase's scheduler has real columns to read/write, but
  the actual "server offline > N minutes -> call Restart" monitor loop was not built - it depends
  on the unverified assumption that `Restart` also starts an already-stopped server, and a wrong
  assumption here risks a restart loop (the task's own explicit warning).
