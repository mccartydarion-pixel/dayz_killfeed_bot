# PvP / Activity / Intrusion Heatmaps (Phase 5)

This document is the design record for Champion's heatmap datasets: PvP kill, PvP death, player
activity, and zone intrusion heatmaps, aggregated entirely from data Phase 3 (`docs/
PLAYER_INTELLIGENCE.md`) and Phase 4 (`docs/ZONES_UAV_RADAR.md`) already persist. **No new Nitrado
polling was added, no coordinate is ever fabricated, and no frontend map rendering was built this
phase** - this is backend aggregation only.

## Heatmap types

| Type | Source | Coordinate |
|---|---|---|
| `PVP_KILLS` | `kills` | the killer's position at the moment of the kill |
| `PVP_DEATHS` | `deaths` | the victim's position at the moment of death |
| `PLAYER_ACTIVITY` | `player_location_events` | every observed position, sampled (see below) |
| `ZONE_INTRUSIONS` | `zone_intrusions` | the entry coordinate (OUTSIDE->INSIDE transition) |

`HITS` was deliberately **not** implemented this phase. Hit events do flow through the same
`player_location_events` pipeline as kills/deaths (so "reliable persisted coordinates" genuinely
exist for them), but the task marked it optional and the four required types already prove out the
entire aggregation/cache/API surface - adding a fifth type was judged unnecessary scope. See
"Deferred" below.

## Coordinate association: no fabrication, ever

`kills` and `deaths` carry **no coordinate columns of their own** - they never did, before or after
this phase (a genuine finding from auditing the schema at the start of this work, task section 2's
"do not infer a coordinate from an unrelated recent location unless the system already has a
documented event-location association"). The association this phase uses is not invented: Phase 3's
`LocationQueue.EnqueueEvent` already persists a `player_location_events` row for **every**
position-carrying `PlayerRef` on **every** event, including kills and deaths, classified by
`locationEventTypeFor` into the exact same coarse vocabulary (`KILL`, `DEATH`, ...) already used
throughout Phase 3/4. Critically, `kills.event_time`/`deaths.event_time` and
`player_location_events.observed_at` are set from the **exact same value**
(`internal/killfeed/persistence.go`'s `eventTimePtr` and `internal/killfeed/location_queue.go`'s
`EnqueueEvent` both call the same `eventTime(ev)` helper on the same `*Event`, during the same
`processLine` call) - so the join

```sql
JOIN player_location_events ple
  ON ple.player_id = k.killer_player_id AND ple.server_id = k.server_id
  AND ple.event_type = 'KILL' AND ple.observed_at = k.event_time
```

is an **exact identity match**, never an approximation, and Phase 3's own
`UNIQUE(player_id, server_id, event_type, observed_at)` guarantees it matches at most one row. A
kill or death whose event carried no position (the ADM line had no `pos=<...>`, or the location
queue dropped the candidate on a full queue) simply has no matching row and is **silently excluded**
- never fabricated, exactly as the task requires.

`zone_intrusions` (Phase 4) also carries no coordinate of its own - deliberately: adding one would
have meant touching zone intrusion behavior, which this phase's own "DO NOT TOUCH" list rules out.
Its entry coordinate is recovered the same way, joined on `(player_id, server_id,
observed_at = entered_at)` via a `LATERAL` join (no `event_type` filter is possible here, since the
transition could have been produced by any position-carrying event type - a deterministic
lowest-`id` tiebreak handles the rare case of two event types sharing the exact same timestamp).

## Grid aggregation and intensity (task sections 5, 7)

```
cellX = floor(x / resolution)
cellZ = floor(z / resolution)
centerX = cellX * resolution + resolution/2
centerZ = cellZ * resolution + resolution/2
intensity = cellCount / maxCellCountInThisResponse   (0.0-1.0; 0 cells -> empty array)
```

Every aggregation is a SQL `GROUP BY` (task section 18: "database GROUP BY grid cell" - never
"load events into Go, then aggregate in memory"). `internal/repository/heatmap_repository.go`'s
four `Aggregate*` methods are the only place raw rows are ever touched, and they never return more
than `MaxCells+1` rows (see "Result size protection" below) - `internal/heatmap`'s service layer
does the cheap, already-bounded center/intensity math in Go.

## Player activity sampling policy (task section 11)

ADM can emit many closely-spaced position pings for one player. Counting each one would show ADM's
own logging density, not real player activity - so `AggregateActivity` applies exactly one
documented sampling rule: **at most one contribution per (player, minute, grid cell)**, implemented
as a `SELECT DISTINCT` on `(player_id, date_trunc('minute', observed_at), cellX, cellZ)` before the
`COUNT(*)`. A player pinging the same cell 50 times in one minute contributes exactly once to that
cell for that minute; the same player in the same cell a minute later contributes a second time.
`TestAggregateActivityDedupesPerPlayerPerMinutePerCell` proves this at the SQL layer with 20 noisy
pings from one player collapsing to a single contribution.

## Zone intrusion heatmap: one contribution per entry (task section 14)

This needs no special aggregation-side logic at all: `zone_intrusions` (Phase 4) already has
exactly one row per OUTSIDE->INSIDE transition, by construction of the intrusion engine's own
invariant (a still-inside player only ever updates `zone_presence`, never creates a second
intrusion row). `AggregateIntrusions` counts `zone_intrusions` rows directly, so repeated
inside-zone location updates can never inflate the heatmap - proven by
`TestAggregateIntrusionsOneContributionPerIntrusionEntry`, which seeds 10 additional location pings
after entry and asserts the contribution count stays at 1.

## Time windows and validation (task sections 8-9, 42-43)

`from`/`to` are RFC3339 timestamps, always handled in UTC internally. Defaults to the last 24 hours
when omitted. Validation (`internal/heatmap.Request.Validate`):

- `type` must be one of the four supported types.
- `resolution` must be one of `50, 100, 250, 500, 1000` (task section 6 - an arbitrary resolution
  could produce a pathologically expensive `GROUP BY`).
- `to` must be after `from`.
- the range must not exceed **30 days**.
- `zoneId` is only accepted for `type=ZONE_INTRUSIONS` (see "Zone filter" below) - supplying it for
  any other type is rejected with an actionable message, never silently ignored.

A fully-future range is not rejected (it simply returns an empty result) - heatmaps are historical
data with no notion of "now" that could be reliably validated across timezones/clock skew.

## Zone filter (task sections 10, 27)

Only `ZONE_INTRUSIONS` supports `zoneId` this phase - PvP/activity zone filtering was explicitly
optional in the task ("do not delay core heatmap implementation for this") and was deferred. When
supplied, `internal/app/saas_api_heatmap.go`'s handler verifies the zone belongs to the caller's own
installation via `a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID)` - the exact same tenant
check every zone-scoped Client Admin route already performs - before it ever reaches the query
layer, so a zone ID from another installation always 404s.

## Result size protection (task section 20)

`internal/heatmap.MaxCells = 5000`. Every `Aggregate*` query is issued with `LIMIT MaxCells+1`; if
the store returns more than `MaxCells` rows, `Service.Query` rejects the request with
`ErrTooManyCells` (a `ValidationError`, mapped to HTTP 400) and an actionable message ("narrow the
time range or increase the resolution") - **the resolution is never silently mutated**, per the
task's explicit instruction.

## Privacy (task section 15)

The response is always an aggregate grid: `cellX`, `cellZ`, `centerX`, `centerZ`, `count`,
`intensity`. No player ID, gamertag, Discord ID, or faction identity ever appears anywhere in a
heatmap response - `internal/heatmap.Cell`/`Result` structurally cannot carry one, and no handler
code path attaches one.

## Permissions

`HEATMAP_VIEW` (Moderator) - an ordinary entry in `internal/permissions.requiredLevel`, gated by
`requireCapability` exactly like every other Client Admin route. No second authorization model.

## API

```
GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/heatmap
    ?type=PVP_KILLS|PVP_DEATHS|PLAYER_ACTIVITY|ZONE_INTRUSIONS
    &from=<RFC3339>&to=<RFC3339>          (default: last 24h)
    &resolution=50|100|250|500|1000       (default: 250)
    &zoneId=<int>                          (ZONE_INTRUSIONS only)
```

```json
{
  "type": "PVP_KILLS",
  "from": "2026-09-22T12:00:00Z",
  "to": "2026-09-23T12:00:00Z",
  "resolution": 250,
  "totalEvents": 452,
  "cells": [
    {"cellX": 10, "cellZ": 18, "centerX": 2625, "centerZ": 4625, "count": 27, "intensity": 0.82}
  ]
}
```

No data is a valid `200`, never a `404` (task section 25): `{"totalEvents": 0, "cells": []}`.
Validation failures are `400`; a missing capability is `403`; an internal/DB failure is a generic
`500` that never leaks SQL or internal error text (task section 49, the same `writeSaaSError`
contract every SaaS route already uses).

## Caching (task sections 21-23)

`internal/heatmap.Cache` is a thread-safe, bounded (2000 entries, opportunistic eviction of expired
entries once full - mirroring `internal/killfeed.ZoneCache`'s exact pattern), 45-second-TTL cache
keyed by `(guildID, serverID, installationID, type, from, to, resolution, zoneId)`. 45s sits inside
the task's suggested 30-60s window. No invalidation logic beyond the TTL was built - the task was
explicit that a short TTL is sufficient for this phase, and heatmap data changes continuously as new
kills/deaths/activity/intrusions accumulate, so there is no discrete "this changed, invalidate now"
event to hook the way zone CRUD has for `ZoneCache`.

## Metrics (task sections 23-24)

`internal/heatmap.Metrics` tracks, both overall and broken down per type (`Metrics.ByType`):
`heatmap_requests`, `heatmap_request_latency_ms` (cumulative - divide by requests for an average),
`heatmap_cells_returned`, `heatmap_events_aggregated`, `heatmap_query_errors`,
`heatmap_cache_hit`, `heatmap_cache_miss`. `heatmap_cache_entries` is read directly from
`Cache.Len()` rather than tracked as a counter (it's a gauge, not a cumulative count). Not yet
wired into `GET /api/admin/health`'s performance snapshot - the same deliberate deferral Phase 3/4
made for their own queue/engine metrics; the counters exist and are ready to be surfaced.

## Database indexes (task section 17 - audited first, added only what was missing)

- `kills` already had `idx_kills_server(guild_id,server_id,created_at DESC)`, but heatmap queries
  filter by `event_time` (the column that exactly matches `player_location_events.observed_at`),
  not `created_at` - added `idx_kills_server_event_time(guild_id,server_id,event_time DESC)`.
- `deaths` already had `idx_deaths_server(guild_id,server_id,event_time DESC)` - covers heatmap
  death queries as-is, nothing added.
- `player_location_events` had `(player_id,observed_at)` and `(server_id,observed_at)`, but every
  heatmap join/scan also filters by `event_type` - added a composite
  `idx_player_location_events_server_type_time(server_id,event_type,observed_at)`.
- `zone_intrusions` had `(installation_id,status)` and `(entered_at DESC)` separately, but heatmap
  intrusion queries filter by `(installation_id, entered_at range)` together - added
  `idx_zone_intrusions_installation_entered(installation_id,entered_at DESC)`.

All in migration `0043_heatmap_indexes`.

## Performance (task sections 18-19, 45)

`TestHeatmapPerformanceAtScale` (`internal/repository`, `-tags=integration`) seeds 20,000 kills,
20,000 deaths, and 150,000 location events for 200 synthetic players spread across a 30-day window,
then measures each of `AggregateKills`/`AggregateDeaths`/`AggregateActivity` over 24h/7d/30d
windows. Measured on this session's local PostgreSQL 16 instance:

| Query | 24h | 7d | 30d |
|---|---|---|---|
| `AggregateKills` | ~26ms (0 cells) | <1ms (0 cells) | ~11.5ms (150 cells) |
| `AggregateDeaths` | ~3.6ms (0 cells) | <1ms (0 cells) | ~12.1ms (277 cells) |
| `AggregateActivity` | <1ms (0 cells) | <1ms (0 cells) | ~80.5ms (962 cells) |

(The 24h/7d windows return 0 cells because the synthetic data is spread evenly across the full
30-day span - the important number is the 30d column, which exercises every row.) All nine queries
completed in well under the 10-second admin-request budget, with the largest (`AggregateActivity`
over 30 days, the only query that scans `player_location_events` without a `kills`/`deaths` join to
narrow it first) at ~80ms.

**One real, worth-documenting finding from building this benchmark**: immediately after the bulk
`COPY` seed, before an explicit `ANALYZE`, the planner had stale statistics and chose a
catastrophically bad plan for the 30-day kill/death queries (27-28 **seconds**, vs ~12ms after
`ANALYZE`). This is standard PostgreSQL behavior after any bulk load - `autovacuum`'s own `ANALYZE`
would normally catch up within seconds to minutes on a live server, and production traffic never
bulk-loads 190,000 rows in one transaction (Phase 3's `LocationQueue` inserts in batches of ~100
every 500ms) - but it is a real, reproducible planner-statistics sensitivity worth knowing about,
not a defect in the query or index design themselves (the same query plan, once statistics are
current, is fast and uses the new indexes correctly).

## Website contract (task section 47 - no frontend built this phase)

A future frontend consumes `cells[].centerX/centerZ/count/intensity` directly - `centerX`/`centerZ`
are already in the DayZ map's own coordinate space, so a map-overlay renderer only needs to draw a
point or heat-blob per cell, scaled by `intensity`. No map image is embedded in the backend (task
section 48) - this endpoint returns coordinate data only.

## Discord heatmap summary (Channel System V2)

The website stays the full interactive heatmap. Discord gets a compact **PvP heatmap summary**
on the `HEATMAPS` route (`🗺️・heatmaps`), published by `HeatmapBoard`
(`internal/discord/heatmap_board.go`):

- **Source:** `Service.Query` with `PVP_KILLS`, the last 24 hours at 250 m - the same persisted
  Phase 5 aggregates the API serves (and its cache). It never polls Nitrado and never reads raw
  events. Zero activity is shown as zero; hot zones are never invented.
- **Content:** Window, Type, Resolution, total Activity, and the top three hot zones (cell centre
  X/Z and kill count), footer `CHAMPION • LIVE SERVER INTELLIGENCE`. Several servers routed to one
  channel each get their own Activity/Hot Zones fields.
- **No spam:** one persistent message per routed channel, recorded through `RoutePanels`, edited in
  place. A restart edits the same message; a route change moves it without leaving a copy.
- **Schedule:** refreshed every `HEATMAP_DISCORD_INTERVAL_MINUTES` (default 30, clamped to
  5-1440), immediately when routes change (auto-setup or a route save), and on startup. Route
  changes made outside the process are noticed within 30 s; the aggregate query itself only runs
  on the interval.
- **Customization:** `HEATMAPS` stays a valid Embed Designer route, but custom templates are not
  rendered for it (`runtimeRendering` stays `NOT_ENABLED`): the summary is a multi-row aggregate
  panel, not a single-event card. The Champion default presentation is always used.
- **Not included yet:** `PVP_DEATHS`, `PLAYER_ACTIVITY` and `ZONE_INTRUSIONS` summaries. Intrusion
  aggregates are keyed by installation, which the runtime publishers (keyed by server) do not
  resolve today.

## What was NOT touched

OAuth, billing, Stripe, shop, the existing Economy ledger, the Nitrado delta reader, zone intrusion
behavior, UAV, Base Radar, player linking, and Owner Hub are all untouched - this phase's only
schema change is three new, purely additive indexes (migration `0043`), and its only new code lives
in `internal/heatmap`, `internal/repository/heatmap_repository.go`, and
`internal/app/saas_api_heatmap.go`.

## Deferred (still, deliberately)

- **`HITS` heatmap type** - technically buildable (reliable coordinates exist), but not built this
  phase; see "Heatmap types" above.
- **PvP/activity zone filtering** (`zoneId` for non-`ZONE_INTRUSIONS` types) - explicitly optional
  per the task, deferred to avoid delaying the core four-type implementation.
- **Website heatmap UI / map overlay rendering** - explicitly out of scope for this backend phase.
- **Discord summaries for deaths, activity and intrusions** - see "Discord heatmap summary".
- **`heatmap_*` metrics in `GET /api/admin/health`** - the counters exist (`Service.Metrics`) but
  are not yet wired into the admin health snapshot, matching Phase 3/4's own precedent.
