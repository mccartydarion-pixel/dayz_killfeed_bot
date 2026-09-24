# Player Intelligence & Location History Foundation (Phase 3)

This document is the design record for the authoritative player-directory and location-history
foundation required by Last Locations, Online Player Intelligence, Zones, UAV/Base Radar,
Heatmaps, and intrusion history - all of which are explicitly **deferred** (`docs/CLIENT_ADMIN.md`
"Deferred") because none of them had a real, persisted location-history data source to build on.
This phase builds that source. **Zones, UAV, and heatmaps are still not built** - this is
backend-first, foundation-only, matching the task's own explicit instruction.

## Why this was safe to build now (and wasn't before)

`docs/CLIENT_ADMIN.md`'s own "Deferred" section explained the blocker precisely: ADM parsing
already extracts a `Position{X,Y,Z}` on any event whose metadata block includes `pos=<...>`
(`internal/killfeed/parser.go`'s `posRe`), but it was read only transiently (kill-distance
calculation, an optional embed coordinate) and discarded immediately after use - and wiring
persistence into `internal/killfeed.Engine.processLine`, "the single most safety-critical,
extensively regression-tested hot path in this codebase," was judged too large a risk to add as a
side effect of an already-large phase.

This phase takes that risk on **deliberately and narrowly**, following the task's own prescribed
architecture exactly:

```
ADM parser → location event candidate → bounded in-memory queue → background persistence worker → batched insert
```

The critical design choice that makes this safe: **`LocationQueue.EnqueueEvent` is always
fire-and-forget**, never blocking `processLine`. This is a deliberate departure from the existing
`PersistenceQueue` (kills/deaths/connect/disconnect), whose `EnqueueAndWait` **does** block
`processLine` until the database write completes (or times out) - correct there, because a kill
must be durable before Discord publish. A location event has no such ordering requirement, and the
task's own instruction is explicit: *"Do NOT add synchronous DB writes directly into the parser
loop if it can stall ADM processing."* `internal/killfeed/location_queue.go` is a fully separate,
additive component; `internal/killfeed/persistence.go`'s `PersistenceQueue` is **byte-for-byte
untouched**.

## Architecture

```
Engine.processLine (after dedupe, same guard point as hit-publish)
  → LocationQueue.EnqueueEvent(ev)          [never blocks; per-server bounded channel, cap 2000]
    → one location_candidate per PlayerRef with a non-nil Position (a kill can enqueue 2: killer+victim)
  → LocationQueue.Run (one goroutine per server)
    → batches candidates (100 events OR 500ms, whichever first) → batched multi-row INSERT
    → full queue: drop the newest candidate, count it (identical overflow policy to PersistenceQueue)
    → Close(): drains and flushes whatever remains before returning (shutdown flush)
```

One `LocationQueue` per server (`internal/app/app.go`'s `runServerWorker`, mirroring
`PersistenceQueue`'s exact per-server scoping and `addPersistQueue`/`allPersistQueues` pattern via
`addLocationQueue`/`allLocationQueues`), using the **same `persistenceStoreAdapter`** already
passed into `NewPersistenceQueueWithServerID` - `InsertLocationEvents` was added to that adapter
rather than building a second adapter, since it already implements the identical `UpsertPlayer`
`LocationStore` needs.

## Schema (migration `0041_player_location_events`)

```sql
CREATE TABLE player_location_events (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    gamertag TEXT NOT NULL,
    x DOUBLE PRECISION NOT NULL,
    z DOUBLE PRECISION NOT NULL,
    y DOUBLE PRECISION,
    event_type TEXT NOT NULL CHECK (event_type IN ('CONNECT','DISCONNECT','HIT','KILL','DEATH','RESPAWN','UNCONSCIOUS','OTHER_ADM')),
    observed_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL DEFAULT 'ADM',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (player_id, server_id, event_type, observed_at)
);
```

Two design decisions worth stating explicitly:

- **`guild_id`/`server_id`, not `organization_id`/`installation_id`.** Every other bot-native,
  guild-scoped table in this schema (`kills`, `deaths`, `player_warnings`,
  `player_server_activity`) is guild/server-scoped, not organization-scoped - `internal/killfeed`
  only ever knows `guildID`/`serverID`, never an `organizationID`. The SaaS API resolves
  organization/installation → guild/server the same way every other Client Admin route already
  does (`ClientAdminRepository.Scope`), so no second identity needs to be carried on every row.
- **No separate "current location" table.** Task section 6 was explicit: *"Do not store a second
  conflicting truth unless necessary."* `LocationRepository.LatestLocation` derives it at query
  time (`ORDER BY observed_at DESC, id DESC LIMIT 1`, served by the table's own leading index) -
  there is only ever one truth for a player's location.
- **`UNIQUE(player_id, server_id, event_type, observed_at)`** is the durable dedupe backstop
  against a duplicate ADM replay (task section 14), inserted with `ON CONFLICT DO NOTHING` -
  exactly the same pattern `kills`/`deaths` already use (`UNIQUE(guild_id, event_fingerprint)`),
  at ADM's own timestamp resolution (whole seconds).

### Coordinate axes (ADM `pos=<...>` order; migration `0045_player_location_events_adm_axis_fix`)

`x`/`z` are the two **horizontal** map coordinates (east/west, north/south) and `y` is
**altitude** - the same axes DayZ's engine uses, and the ones heatmaps, zone distance checks and
the location APIs all key on. ADM does **not** print them in that order: DayZ's
`PluginAdminLog.GetPlayerPrefix` (`scripts/4_world/plugins/pluginbase/pluginadminlog.c`) builds
`pos=<...>` from engine components `[0], [2], [1]`, i.e. `pos=<x, z, altitude>` - e.g.
`pos=<7504.7, 1334.4, 0.9>` is x=7504.7, z=1334.4, 0.9m up. `killfeed.Position` keeps the raw ADM
order and exposes `MapX()`/`MapZ()`/`Altitude()`; the location writer (`LocationQueue`) uses only
those accessors.

The original Phase 3 writer stored the second ADM value as `y` and the third as `z`, so every
pre-fix `source='ADM'` row had altitude in `z` and the real north coordinate in `y`. Migration
`0045_player_location_events_adm_axis_fix` swaps `y`/`z` on those rows once (rows with a `NULL`
`y` can't be repaired and were never written). Zone intrusions evaluated **before** the fix were
computed against altitude instead of north and are not re-derived - intrusion history from that
window should be treated as unreliable.

## Location freshness (task section 5)

ADM does not provide continuous GPS - a location is only ever as fresh as its last observed event.
Every exposed location carries `observedAt`, `ageSeconds`, and `freshness`:

| Freshness | Age |
|---|---|
| `LIVE_RECENT` | ≤ 60s |
| `RECENT` | ≤ 5 minutes |
| `STALE` | > 5 minutes |

60 seconds tolerates a couple of missed/slow ADM polls (Champion polls roughly every 10s,
`docs/PERFORMANCE.md`) before downgrading from `LIVE_RECENT`; the word "live" is never used unless
that threshold genuinely qualifies (task's explicit instruction).

## Player directory (task sections 1-2)

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/players`
(`PLAYER_DIRECTORY_VIEW`) is a dedicated query (`LocationRepository.ListPlayerDirectory`) against
`players`/`player_server_activity`/`kills`/`deaths`/`player_links`/`app_users`/`faction_members`/
`factions`/`player_warnings`/`player_location_events` - **never** the economy account search
(task section 1's explicit instruction), which remains its own, separate, OWNER/ADMIN-role-gated
endpoint for economy purposes only.

Supported filters: `q` (display name, case-insensitive substring), `online` (true/false),
`linked` (true/false - has a `VERIFIED` `player_links` row), `cursor`/`limit` (keyset pagination on
`players.id`, matching this codebase's existing admin-cursor convention,
`admin_api.go`'s `encodeAdminCursor`/`decodeAdminCursor`).

Every field returned is real, observed data - never fabricated when unavailable (nullable Go
pointer fields marshal as `null`/omitted, never a guessed default):

```json
{
  "playerId": 4821,
  "gamertag": "SurvivorBob",
  "discordUserId": "123456789012345678",
  "discordDisplayName": "bob#0",
  "linked": true,
  "online": true,
  "lastSeenAt": "2026-09-23T04:12:00Z",
  "lastConnectedAt": "2026-09-23T03:40:00Z",
  "currentSessionStartedAt": "2026-09-23T03:40:00Z",
  "kills": 14,
  "deaths": 6,
  "factionId": 9,
  "factionName": "The Wolves",
  "warningCount": 0,
  "currentLocation": { "x": 4102.5, "z": 8811.2, "y": 12.1, "eventType": "HIT", "observedAt": "2026-09-23T04:11:55Z", "ageSeconds": 5, "freshness": "LIVE_RECENT", "sessionScope": "CURRENT_SESSION", "sourceLocalTime": "2026-09-23T00:11:55" },
  "locationFreshness": "LIVE_RECENT",
  "currentLocationStatus": "CURRENT",
  "lastKnownLocation": { "...": "same shape; may be HISTORICAL" }
}
```

**Current-session semantics (Champion Live Sync phase 1, docs/CHAMPION_LIVE_SYNC.md section 4).**
`currentLocation` is present only for a connected player observed in the server's current ADM file at
or after their latest connect in it; otherwise it is absent and `currentLocationStatus` is `UNKNOWN`. A
previous-session position is never current - it is exposed as `lastKnownLocation`. ADM player-list
entries are stored as `PLAYER_LIST` location events every five minutes.

## Location and online APIs (task sections 6-8)

```
GET .../admin/players/{playerID}/locations/latest   PLAYER_LAST_LOCATION_VIEW  -> a single locationDTO (last known, may be HISTORICAL), 404 if never observed
GET .../admin/players/{playerID}/locations/current  PLAYER_LAST_LOCATION_VIEW  -> { status: CURRENT | UNKNOWN, location: locationDTO | null }
GET .../admin/players/{playerID}/locations           PLAYER_LOCATION_VIEW       -> newest-first, filters: from/to (RFC3339), eventType, cursor, limit
GET .../admin/players/online                         PLAYER_LAST_LOCATION_VIEW  -> currently-connected players + latest known location each
```

`admin/players/online` is gated at `PLAYER_LAST_LOCATION_VIEW`, not the plain directory floor,
because its response inherently includes location data. It never marks a disconnected player
online (task section 8) - sourced directly from `player_server_activity.currently_connected`, the
same column the existing Phase 1 `lastOnline` endpoint already treats as authoritative.

## Capabilities (task section 11)

| Capability | Level | Notes |
|---|---|---|
| `PLAYER_DIRECTORY_VIEW` | Moderator | the player directory list |
| `PLAYER_LAST_LOCATION_VIEW` | Administrator | one player's current location; online-players-with-location |
| `PLAYER_LOCATION_VIEW` | Administrator | full location history |

Location access is privileged (task section 10) - both location capabilities sit one level above
the plain directory listing. Escalation model unchanged: these are ordinary entries in
`permissions.requiredLevel`, checked the same way every other Client Admin capability is.

## Hot-path safety (task section 4) - what was actually verified

- `go test -race ./internal/killfeed/...` passes, including new `LocationQueue`-specific tests for
  concurrent enqueue-while-draining.
- `LocationQueue.EnqueueEvent` is provably non-blocking: it does a single buffered-channel send
  with a `default` fallback (drop + count), never a wait.
- Ordering: a single goroutine both reads the queue and builds each batch in `Run`, so "no event
  reordering" is structural, not a property requiring separate enforcement - proven by
  `TestLocationQueueClosePreservesEnqueueOrderAndFlushesRemainder` (250 candidates across multiple
  batches, asserted byte-for-byte in enqueue order after `Close()`).
- Shutdown flush: `Close()` drains the channel and flushes any partial batch before returning -
  proven by the same test.
- Per-installation isolation: one `LocationQueue` instance per server, exactly mirroring
  `PersistenceQueue`.

## Audit and privacy (task section 10)

Viewing a player's location history (`PLAYER_LOCATION_HISTORY_VIEWED`) and viewing online-player
locations (`ONLINE_PLAYER_LOCATIONS_VIEWED`) both write an `admin_audit_log` row via the existing
`a.recordAudit` helper (`docs/CLIENT_ADMIN.md`) - the first *read* actions in this codebase to be
audited, since location access is explicitly privileged. Never exposed through the public Player
Hub - these routes require a Champion bot permission Level (Administrator+), resolved the same way
every other Client Admin route resolves one; a Player Hub session has no such Level.

## Retention (task section 9)

`CHAMPION_LOCATION_RETENTION_DAYS` (default 30, fails closed to the default on anything unparsable
or non-positive - a misconfigured value must never accidentally disable retention or delete
everything every sweep). A background job (`App.runLocationRetention`, mirroring
`refreshHealth`'s ticker shape) sweeps hourly, deleting `player_location_events` older than the
retention window in bounded batches (2000 rows/call) so one sweep never holds a long-running lock.
Retention's `DELETE` statement only ever targets `player_location_events` - it has no join or
subquery touching `kills`/`deaths`, so kill/death history is structurally unreachable by this job
(task: "Do not delete kill/death history").

## Observability (task section 12)

`LocationQueue.Health()` returns `{ServerID, Depth, Capacity, HighWater, Seen, Persisted, Dropped,
LastWriteMs}` per server, mirroring `PersistenceQueue.QueueHealth()`'s shape. Wiring these into
`GET /api/admin/health`'s existing `performance` object (`docs/PERFORMANCE.md`) is a small, natural
follow-up (the aggregation point, `a.allLocationQueues()`, already exists) - not done in this PR to
keep the change surface focused on the foundation itself; the counters exist and are ready to be
surfaced.

## Website contract (task section 13 - no UI built this phase)

The DTOs above (`playerDirectoryEntryDTO`, `locationDTO`) are the exact wire shapes a future
frontend phase would consume. See `docs/CLIENT_ADMIN.md` and `docs/saas-openapi.yaml` for the full
route/response reference, matching the convention already established for every other Client Admin
route.

## Deferred (still, deliberately)

Zones, zone-ignore, zone-ban, Base Radar/UAV intrusion detection, and heatmap aggregation are
**not** built in this phase - the task's own instruction ("Do NOT build zones, UAV, or heatmaps
yet") is explicit, and this phase's job was only to build the data foundation those features need.
With `player_location_events` now real and persisted, a follow-up phase can build a zone-membership
check against this same table (on write, or on read) without needing to solve the "where does
location data come from" problem again.
