# Live map

The live map is the website's picture of one DayZ server right now: where the fighting is, who
of your faction is where, and - for staff - every connected player. It is read-only, it adds no
new data collection (every position comes from the ADM lines the bot already persists in
`player_location_events`, every kill from `kills`), and it is split into three routes so each
audience sees exactly what it is allowed to see.

| Route | Who | Shows | Positions? |
|---|---|---|---|
| `GET /api/saas/network/servers/{installationID}/map` | anyone (service bearer only, no acting user) | recent kills, a pressure grid, the open hot zone, the server clock | **never** - only where victims died, behind the delay |
| `GET /api/saas/player/servers/{installationID}/map/faction` | a verified linked player of the server | the player's own hub faction members, their bases and raids | only the faction's own members |
| `GET …/admin/map/live` | staff with `PLAYER_LAST_LOCATION_VIEW` | every connected player's latest position, zones, active intrusions | yes, audited |
| `GET …/admin/map/history?from=&to=` | staff with `PLAYER_LOCATION_VIEW` | kills and every player's track in a window | yes, audited |

`…` is `/api/saas/organizations/{organizationID}/installations/{installationID}`.

## Settings

Three columns on `installation_feature_settings` (migration `0119_live_map_settings`), exposed in
`GET …/admin/features` as `liveMap` and saved with `PUT …/admin/features/live-map`
(`FEATURE_SETTINGS_MANAGE`, audited as `LIVE_MAP_SETTINGS_UPDATED`):

```json
{ "visibility": "LISTED", "public": false, "delaySeconds": 120, "factionLayer": true }
```

| Field | Default | Meaning |
|---|---|---|
| `visibility` | `LISTED` | Who can open the public map: `LISTED` only while the server is listed on the network (`network_listed`), `PUBLIC` always, `OFF` never. When it is not open the public route is a plain `404 NOT_FOUND`, indistinguishable from an installation that does not exist. |
| `public` | (read only) | Whether the public map is open right now, from `visibility` and the listing. Ignored when saving. |
| `delaySeconds` | `120` (0..3600) | How far behind real time the public map runs. A kill is only shown once it is at least this old; the pressure grid is computed at the same delayed instant. |
| `factionLayer` | `true` | Verified players get the faction layer. Off: the faction route answers `enabled:false` with empty members/bases/alerts. |

The map follows the network listing by default, so a server that never chose to be public does
not get a public page, and it runs two minutes behind so it cannot be used to find a fight while
it is happening. It is not part of any plan gate.

## 1. Public map state

`GET /api/saas/network/servers/{installationID}/map?sinceKillId=<int64>&window=<5..180, default 60>`

Service bearer only, like the other `/api/saas/network` routes. The full response is cached
in-process for 5 seconds per (installation, window); `sinceKillId` is applied after the cache, so
polling clients cost nothing extra. Saving the settings drops the cache.

```json
{
  "installationId": 12, "name": "DayZ Server", "platform": "PLAYSTATION",
  "map": { "key": "chernarusplus", "name": "Chernarus", "size": 15360, "guessed": false },
  "public": true, "listed": true, "delaySeconds": 120, "generatedAt": "2026-10-04T01:25:36Z",
  "playersOnline": 2, "lastActivityAt": "2026-10-04T01:05:36Z",
  "clock": {
    "serverLocalTime": "03:25:36", "utcOffsetMinutes": 120, "bootedAt": "2026-10-04T00:25:36Z",
    "nextRestartAt": "2026-10-04T03:25:36Z", "inGame": { "time": "18:00", "estimated": true }
  },
  "pressure": { "resolution": 500, "windowMinutes": 30,
    "cells": [ { "centerX": 4750, "centerZ": 10250, "count": 4, "intensity": 1 } ] },
  "hotZone": null,
  "kills": [ { "id": 48, "at": "2026-10-04T01:25:06Z", "killerName": "Ghost", "victimName": "Stranger", "weapon": "KA-M",
               "distanceMeters": null, "headshot": false, "longshot": false,
               "killerX": null, "killerZ": null, "victimX": 4720, "victimZ": 10340 } ],
  "lastKillId": 48
}
```

- `map` comes from the shop delivery map (`shop_delivery_settings.map_key`, `internal/dayzmap`).
  When none is configured (or it is no longer supported) the map is Chernarus with `guessed:true`.
- `playersOnline` is `player_server_activity.currently_connected`; `lastActivityAt` the newest
  `last_seen_at`.
- `clock.serverLocalTime` is the server's own wall clock: the newest `source_local_time` stamped
  on a kill or location line of the current ADM session, advanced by the real seconds since the
  bot ingested that line. `null` when the newest stamp is older than 30 minutes (the server may
  be down) or no line was stamped. `utcOffsetMinutes` is `live_sync_server_clock` (null until a
  restart.log line taught it). `bootedAt` is `server_adm_sessions.selected_at` of the open session.
- `clock.nextRestartAt` (best effort) is the earliest future `next_run` of a Nitrado scheduled task
  whose `action_method` is a restart (`GET /services/{id}/tasks`, `nitrado.ListScheduledTasks`).
- `clock.inGame` (best effort, always `estimated:true`) is the in-game time of day estimated from
  the Nitrado DayZ settings `serverTime`, `serverTimeAcceleration` (the night rate is ignored) and
  the boot: start of day at boot (`serverTime` as `YYYY/M/D/H/MM`, or the boot's local wall time
  for `SystemTime`) plus real seconds since `bootedAt` times the acceleration. `null` when any
  input is missing.
- Nitrado is read at most once per 5 minutes per installation (facts and tasks together, cached in
  process) and a failure only makes both fields `null`; it never fails the request.
- `pressure` is the KILL and HIT location rows of the last 30 minutes (ending at `now - delay`)
  in 500 m cells, busiest first, at most 400 cells, `intensity = count / max`. A cell needs at
  least two events to be shown, so one wounded player cannot be placed by their own hits. It is
  the same aggregate the heatmap shows, only fresher - no identity, no position of a living player.
- `hotZone` is the open hot zone exactly as `GET /api/saas/player/servers/{id}/hot-zone` returns it.
- `kills` are newest first, at most 100, within `window` minutes, only kills with
  `at <= now - delaySeconds`, with where the victim died (nullable). `killerX`/`killerZ` are
  always `null` on the public route: the killer is alive. With `sinceKillId` the kills with a
  greater id are returned, plus the five most recent again - ids follow ingest order, not event
  time, so behind a delay a late kill can have a lower id; clients dedupe by id. `lastKillId` is
  the highest visible id (0 when there is none) so a client can poll with it.
- `listed` is whether the server is listed on the network (the website lets search engines index
  only listed servers' maps).

`404 NOT_FOUND` when the installation does not exist, has no game server, or `public` is off.
`400 INVALID_REQUEST` for a bad `window` or `sinceKillId`.

## 2. Faction layer

`GET /api/saas/player/servers/{installationID}/map/faction`

Service bearer plus `X-Champion-Acting-User`, resolved with the player API's chain
(`docs/PLAYER_API.md`): the acting user must hold a verified DayZ link for the installation's
guild and have been observed on its server, else the usual `404` / `409 PLAYER_IDENTITY_REQUIRED`.
Rate-limited per acting user (60/minute).

```json
{
  "enabled": true,
  "faction": { "id": 12, "name": "Red Dawn", "tag": "RD", "primaryColor": "#ff1726", "secondaryColor": "#f0c65e" },
  "self": { "playerId": 45, "gamertag": "Deelo" },
  "members": [
    { "playerId": 45, "gamertag": "Deelo", "isSelf": true, "online": true,
      "lastKnown": { "x": 4650, "z": 10300, "observedAt": "2026-10-04T01:23:36Z", "ageSeconds": 120, "eventType": "PLAYER_LIST" },
      "trail": [ { "x": 4612, "z": 10390, "observedAt": "2026-10-04T00:35:36Z" } ] },
    { "playerId": 46, "gamertag": "Mate", "isSelf": false, "online": false, "lastKnown": null, "trail": [] }
  ],
  "bases": [ { "id": 12, "name": "Kabanino ridge", "centerX": 5300, "centerZ": 9700, "radius": 80, "ownerPlayerId": 46 } ],
  "alerts": [ { "baseId": 12, "baseName": "Kabanino ridge", "kind": "RAID", "part": "Wall", "target": "Gate", "raiderName": "Stranger", "at": "2026-10-04T01:21:36Z" } ]
}
```

- `faction` is the acting user's Faction Hub faction on this installation (`hub_faction_members`,
  the same membership `PlayerServerRepository.CurrentFaction` reads); `null` when they are in none.
- `members` are the faction's members with a `player_id` on this installation. A player with no
  faction gets `faction:null` and `members:[self]`. The acting player is always listed.
- `lastKnown` is each member's newest `player_location_events` row (any event type, player-list
  samples included) within the current ADM session, or within the last 24 hours when no boot was
  recorded - **one** `DISTINCT ON (player_id)` statement for every member, never a query per
  player. `trail` is up to the 6 positions before it from the last 2 hours, oldest first.
- `bases` are the live (`state <> 'REVOKED'`) `case_registered_bases` of the server that a member
  owns, or that a member - or a classic faction a member belongs to - holds a current
  `case_base_authorizations` row for. `alerts` are the `base_raid_alerts` on those bases from the
  last 60 minutes, newest first, at most 20.
- `enabled:false` (layer switched off): `faction:null`, empty `members`, `bases`, `alerts`; `self`
  is still filled.

**Only members' positions ever leave this route.** The member list is resolved first and every
position query is bound to those ids, so a player outside the faction - or in none - can never
read anyone's position but their own.

## 3. Staff live and history

`GET …/admin/map/live` (`PLAYER_LAST_LOCATION_VIEW`)

```json
{
  "generatedAt": "2026-10-04T01:25:36Z",
  "players": [ { "playerId": 45, "gamertag": "Deelo", "online": true, "factionTag": "RD",
                 "lastKnown": { "x": 4650, "z": 10300, "observedAt": "…", "ageSeconds": 120, "eventType": "PLAYER_LIST" } } ],
  "zones": [ zoneDTO… ],
  "intrusions": [ { …zoneIntrusionDTO, "centerX": 9000, "centerZ": 9000 } ]
}
```

Every currently connected player (`player_server_activity.currently_connected`, at most 500)
with their hub faction tag and latest position (one bulk query, the same session rule as the
faction layer), the enabled zones (`zoneDTO` of `docs/ZONES_UAV_RADAR.md`) and the active
intrusions (`zoneIntrusionDTO` with presence, plus the zone's centre). The view polls, so it is
audited as `LIVE_MAP_VIEWED` once per actor per installation per 10 minutes, not per request.

`GET …/admin/map/history?from=&to=` (`PLAYER_LOCATION_VIEW`)

```json
{
  "from": "2026-10-04T01:10:36Z", "to": "2026-10-04T01:25:36Z",
  "map": { "key": "chernarusplus", "name": "Chernarus", "size": 15360, "guessed": false },
  "kills": [ { …public kill shape, "killerId": 48, "victimId": 47 } ],
  "tracks": [ { "playerId": 45, "gamertag": "Deelo", "points": [ { "t": "…", "x": 4600, "z": 10250, "type": "PLAYER_LIST" } ] } ],
  "intrusions": [ { …zoneIntrusionDTO, "centerX": 9000, "centerZ": 9000 } ],
  "truncated": false
}
```

`from`/`to` are RFC 3339; default the last hour; the window must be 6 hours or shorter
(`400 INVALID_REQUEST` otherwise). Kills are newest first, at most 500. Tracks are every
player's location rows in the window (all event types including `PLAYER_LIST`), grouped per
player and oldest first within a track, at most 20,000 points in total: when the window holds
more, the **oldest** are dropped and `truncated` is `true`. Intrusions are the installation's
intrusion history entered in the window (at most 500). Every request is audited as
`LIVE_MAP_HISTORY_VIEWED` with the window.

## Privacy rules

- **Public = deaths and pressure only.** The public route carries where victims died (behind the
  delay), an aggregate grid of cells with two or more events and a clock. It never carries the
  killer's position, a player's current or last known position, a player id, or anything about a
  player who has not killed or died.
- **Faction layer = verified members.** A hub faction membership keeps the player id from when the
  member joined; a member only shows while their DayZ link to that player is still VERIFIED.
- **Faction layer = own members only.** Positions are read for the resolved member ids and
  nothing else; a non-member's position cannot be returned by construction (and the integration
  test asserts it).
- **Staff = audited.** Both staff routes sit behind the location capabilities and write
  `admin_audit_log` rows (`LIVE_MAP_VIEWED` throttled to one per 10 minutes, `LIVE_MAP_HISTORY_VIEWED`
  per request), like every other location read (`docs/PLAYER_INTELLIGENCE.md`).
- The delay is the owner's: it applies to the public route only. Staff and faction members see
  real time.

## Code

| Piece | Where |
|---|---|
| Handlers, DTOs, caches, audit throttle | `internal/app/live_map.go` |
| Settings DTO and `PUT …/features/live-map` | `internal/app/hot_zones.go` |
| Queries (installation, status/clock, pressure points, bulk positions, trails, members, bases, alerts, tracks) | `internal/repository/live_map_repository.go` |
| Kills with positions and `longshot` | `internal/repository/fight_repository.go` (`RecentKills`) |
| Pure rules: pressure grid, wall clock, in-game estimate, next-restart pick | `internal/livemap` |
| Nitrado: clock settings in `GameserverFacts`, `ListScheduledTasks` | `internal/nitrado/service_facts.go` |
| Fixture: `/services/{id}/tasks`, `settings.config.serverTime*` | `internal/nitrado/nitradofixture` |
| Migration | `0119_live_map_settings` |
| Tests | `internal/livemap/livemap_test.go`, `internal/app/live_map_test.go`, `internal/app/live_map_integration_test.go`, `internal/nitrado/scheduled_tasks_test.go` |
