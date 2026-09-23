# Zones + UAV / Base Radar (Phase 4)

This document is the design record for installation-scoped geographic zones and the stateful
intrusion engine that watches them, built directly on top of Phase 3's persisted location history
(`docs/PLAYER_INTELLIGENCE.md`). Zones, zone-ignore/authorization/ban lists, and UAV/Base Radar
detection were explicitly deferred in Phase 3 ("Do NOT build zones, UAV, or heatmaps yet") pending
a real, persisted location data source - that source now exists (`player_location_events`), and
this phase is the first consumer of it. **Heatmaps, Auto Payments, the priority queue, and Auto
Start remain deliberately unbuilt** (reserved for later phases).

## Why this was safe to build now

Phase 3's own "Deferred" section named the exact prerequisite this phase needed: a durable,
queryable table of observed player positions. It exists now, with a proven dedupe guarantee
(`UNIQUE(player_id, server_id, event_type, observed_at)`) and a documented freshness model
(`LIVE_RECENT`/`RECENT`/`STALE`). Building the intrusion engine as a **downstream consumer** of that
table - rather than a second hook into the ADM parser - means this phase adds **zero** new code to
`internal/killfeed.Engine.processLine`. The parser's hot path is exactly as untouched as it was
after Phase 3.

## Architecture

```
Engine.processLine → LocationQueue.EnqueueEvent (unchanged, Phase 3)
  → LocationQueue.Run (one goroutine per server, already off the hot path)
    → persist(batch): batched INSERT into player_location_events (unchanged, Phase 3)
    → IntrusionEngine.Evaluate(serverID, batch)   [NEW - same goroutine, strictly after persist]
       → for each location record x each of the server's cached active zones:
          → distance = sqrt((playerX-zoneX)² + (playerZ-zoneZ)²)
          → OUTSIDE/INSIDE transition against the persisted zone_presence row
          → on a genuine entry: ignore-list check → ban check → authorization check (UAV/Base Radar
            only) → cooldown check → zone_intrusions row → IntrusionPublisher event
          → on a genuine exit: close the open zone_intrusions row → IntrusionPublisher event
```

The critical design choice: **intrusion evaluation happens on `LocationQueue`'s existing consumer
goroutine, after that batch is already confirmed durable** - not a new goroutine, not a new queue,
and never inside `processLine`. `internal/killfeed/location_queue.go`'s only change is one
additional, panic-recovered call at the end of `persist()`; the parser and the existing
persist/insert logic are byte-for-byte unchanged.

Because zones are installation-scoped and an installation maps to exactly one `game_server_id`, a
zone belongs to exactly one server. Combined with the fact that only one goroutine ever runs
`LocationQueue.persist` for a given server, **a server's own `zone_presence`/`zone_intrusions` rows
are only ever written by one goroutine at a time** - no additional in-process locking was needed
beyond what Postgres's own row-level guarantees already provide.

## Schema (migration `0042_zones_uav_base_radar`)

Six new tables: `installation_zones`, `zone_ignore_entries`, `zone_authorized_entries`,
`zone_bans`, `zone_presence`, `zone_intrusions`. Full DDL and index rationale is in the migration
itself; the design decisions worth stating explicitly:

- **`installation_zones` carries both `installation_id` (tenant-CRUD identity) and denormalized
  `guild_id`/`server_id`**, resolved once at creation time from `ClientAdminRepository.Scope`. The
  intrusion engine lives in `internal/killfeed`, which only ever knows `guildID`/`serverID` -
  exactly the same reasoning Phase 3's `player_location_events` already applied.
- **`zone_presence` is a deliberate exception to Phase 3's "no second truth" principle.** Phase 3
  never stored a second "current location" - it was always derived at query time. `zone_presence`
  is different: it is the intrusion engine's own **persisted state-machine result** (not duplicated
  location data), and persisting it is what makes restart/recovery safety automatic rather than a
  separate mechanism to build (see below). `zone_presence.last_alert_at` is the cooldown anchor -
  it survives an exit/re-entry cycle for the same zone+player pair, so a boundary-jitter flap can
  never bypass the cooldown by technically closing and reopening an intrusion.
- **`zone_intrusions` is append-mostly and never deleted on exit** - a row is created on every
  genuine entry and only ever updated (`status`, `exited_at`, `acknowledged_at`). `status` is a
  3-stage lifecycle: `ACTIVE` (ongoing, unacknowledged) → `ACKNOWLEDGED` (ongoing, an admin has seen
  it) → `EXITED` (set whenever the player leaves, from either prior state). The partial unique index
  `uq_zone_intrusions_open ON (zone_id, player_id) WHERE status <> 'EXITED'` enforces "at most one
  open intrusion per zone+player" and doubles as the engine's own hot lookup.
- **Zone bans are explicitly not server bans.** `zone_bans` never touches
  `installation_access_entries` or calls the Nitrado banlist API - a zone ban only flags a future
  entry (`zone_intrusions.banned=true`, a `ZONE_BAN_VIOLATION` event) for admin awareness.

## The intrusion engine (`internal/killfeed/intrusion_engine.go`)

### State machine

For each (zone, location record) pair, per-event:

1. **Not inside, wasn't inside** → no-op.
2. **Not inside, was inside** → genuine exit: close the open intrusion, emit `ZONE_EXIT` (always
   `Suppressed=true` - exits are tracked but never posted to Discord), never re-alerts.
3. **Inside, was already inside** → ongoing presence: refresh `last_seen_at` only. **No new
   intrusion, no new alert** - this is what makes "must NOT alert on every location update" true
   structurally, not by a rate limit.
4. **Inside, wasn't inside (or first-ever observation)** → genuine entry, evaluated in this order:
   - **Ignore list** (`PLAYER`/`FACTION`/`DISCORD_ROLE`, every zone type): if matched, the player is
     **never tracked at all** - no presence row, no intrusion, no alert. `DISCORD_ROLE` entries are
     the only check requiring a live lookup (`internal/killfeed.RoleChecker`, an `internal/app`
     adapter over the same 15s-TTL cached `MemberRoles` Client Admin actor-resolution already uses)
     - a role-check failure fails **open** (not ignored), since silently suppressing a genuine
       intrusion is worse than a spurious alert.
   - **Ban check** (`zone_bans`, every zone type): sets `banned=true` on the created intrusion and
     emits an additional `ZONE_BAN_VIOLATION` event. A ban is never bypassed by authorization.
   - **Authorization** (`PLAYER`/`FACTION`, **UAV/Base Radar zone types only** - task's explicit
     scope): suppresses the *alert*, not the tracking. An authorized UAV/Base Radar entry still
     creates a `zone_intrusions` row (so "who flew over my base" stays visible in history), just
     with `Suppressed=true`.
   - **Cooldown** (`zone.cooldown_seconds`, default 300): if the zone+player pair's persisted
     `last_alert_at` is within the cooldown window, the intrusion is still created (so a rapid
     exit/re-entry flap doesn't lose history) but the alert is suppressed
     (`AlertsSuppressedCooldown`). A **new** entry after the cooldown has elapsed alerts again
     immediately - re-entry after fully leaving is always a new intrusion.

### Presence freshness / uncertainty (never a fabricated exit)

The engine only ever closes an intrusion in response to an actual OUTSIDE location event. If a
player simply stops sending location updates (disconnect without a final low-frequency event,
Nitrado log gap, etc.), their intrusion stays `ACTIVE`/`ACKNOWLEDGED` indefinitely - it is never
auto-exited by a timeout. Instead, every read of active intruders/intrusions computes a
`presenceStatus` at read time from the persisted presence row's `last_seen_at`, reusing Phase 3's
own `ClassifyFreshness`:

| `presenceStatus` | Condition |
|---|---|
| `PRESENT_RECENT` | last confirmed ≤ 5 minutes ago (`LIVE_RECENT` or `RECENT`) |
| `PRESENCE_UNCERTAIN` | last confirmed > 5 minutes ago (`STALE`) |

This is the same "no second truth" principle as Phase 3's location freshness - `presenceStatus` is
never stored, only computed at read time.

### Restart / recovery safety

`zone_presence`/`zone_intrusions` live in Postgres, never in memory. A process restart therefore
needs **no separate recovery step**: the first post-restart location event for an already-inside
player finds `presence.status='INSIDE'` already persisted, so the engine evaluates it as case 3
above (an ongoing-presence refresh), never as case 4 (a fresh entry) - no fake entry alert is
possible by construction. `TestIntrusionEngineRestartRecoveryDoesNotFabricateEntry` proves this by
seeding a fresh `IntrusionEngine` (simulating a new process) with presence/intrusion rows a
"previous process" already wrote, then asserting the first evaluation creates nothing new.

### Duplicate-event safety

The engine never re-implements dedupe: it relies entirely on Phase 3's
`UNIQUE(player_id,server_id,event_type,observed_at)` backstop upstream in `LocationQueue.persist`.
A replayed ADM line that resolves to the same (zone, player, distance) outcome either way is
idempotent to evaluate again (an ongoing-presence refresh, never a spurious re-entry).

### Multi-zone / overlapping zones

A location record is evaluated against **every** enabled zone on its server independently - a
player inside two overlapping zones gets two independent `zone_presence`/`zone_intrusions` rows,
tracked to completely separate lifecycles (entering/exiting one never affects the other).
`TestIntrusionEngineMultiZoneIndependentTracking` proves this.

### UAV / Base Radar

UAV and `BASE_RADAR` are ordinary `zone_type` values sharing the **exact same** state machine above
- there is no second intrusion-detection code path (task's explicit "do NOT duplicate intrusion
logic"). The only differences: `zone_authorized_entries` only ever suppresses alerting for these
two types, the operational event kind is `UAV_INTRUSION`/`BASE_RADAR_INTRUSION` instead of
`ZONE_INTRUSION`, and managing (create/update/delete) a zone of either type requires `UAV_MANAGE`
(Owner) **in addition to** `ZONE_MANAGE` (Administrator) - enforced both at zone-CRUD time
(`internal/app/saas_api_zones.go`'s `requireZoneManageFor`) and permanently thereafter, since
`zone_type` is immutable after creation.

## Zone cache (`internal/killfeed/zone_cache.go`)

A thread-safe, bounded (500 zones/server, opportunistic eviction past 4000 cached servers),
per-server cache of enabled zones with a 30s TTL fallback. This is what makes "must NOT query zones
from the database per location event" true: `IntrusionEngine.Evaluate` calls `ZoneCache.Zones`
once per batch per server, never a per-event query. Every zone/ignore/authorized/ban mutation in
`saas_api_zones.go` calls `ZoneCache.Invalidate(serverID)` immediately, so a configuration change
takes effect on the very next location batch; the TTL fallback exists only to self-heal a missed
invalidation, never as the primary invalidation mechanism.

## Capabilities

| Capability | Level | Notes |
|---|---|---|
| `ZONE_VIEW` | Moderator | list/get zones, ignore/authorized/ban lists, active intruders, active intrusions, history |
| `ZONE_MANAGE` | Administrator | create/update/delete zones (non-UAV/Base Radar), authorized-entry CRUD, ban CRUD |
| `ZONE_IGNORE_MANAGE` | Administrator | ignore-entry CRUD (its own capability, separate from `ZONE_MANAGE`) |
| `UAV_MANAGE` | Owner | required **in addition to** `ZONE_MANAGE` for any UAV/Base Radar zone |
| `INTRUSION_ACK` | Moderator | acknowledge an active intrusion |

All five are ordinary entries in `permissions.requiredLevel` - no second permission model.

## API routes

```
GET    .../admin/zones                                ZONE_VIEW
POST   .../admin/zones                                ZONE_MANAGE (+ UAV_MANAGE for UAV/BASE_RADAR)
GET    .../admin/zones/{zoneID}                        ZONE_VIEW
PUT    .../admin/zones/{zoneID}                        ZONE_MANAGE (+ UAV_MANAGE if the zone's type is UAV/BASE_RADAR)
DELETE .../admin/zones/{zoneID}                        ZONE_MANAGE (+ UAV_MANAGE if the zone's type is UAV/BASE_RADAR)

GET    .../admin/zones/{zoneID}/ignore                 ZONE_VIEW
POST   .../admin/zones/{zoneID}/ignore                 ZONE_IGNORE_MANAGE
DELETE .../admin/zones/ignore/{entryID}                ZONE_IGNORE_MANAGE

GET    .../admin/zones/{zoneID}/authorized             ZONE_VIEW
POST   .../admin/zones/{zoneID}/authorized             ZONE_MANAGE
DELETE .../admin/zones/authorized/{entryID}            ZONE_MANAGE

GET    .../admin/zones/{zoneID}/bans                   ZONE_VIEW
POST   .../admin/zones/{zoneID}/bans                   ZONE_MANAGE
DELETE .../admin/zones/bans/{banID}                    ZONE_MANAGE   (lifts the ban)

GET    .../admin/zones/{zoneID}/active-intruders       ZONE_VIEW
GET    .../admin/intrusions/active                     ZONE_VIEW     filters: zoneId, zoneType, playerId, acknowledged
GET    .../admin/intrusions/history                    ZONE_VIEW     filters: zoneId, playerId, from, to, status, cursor, limit (newest-first)
POST   .../admin/intrusions/{intrusionID}/acknowledge  INTRUSION_ACK
```

Zone create/update validation: `name` required, `zoneType` one of `SAFEZONE`/`PVP`/`RESTRICTED`/
`EVENT`/`UAV`/`BASE_RADAR`/`CUSTOM`, `radius > 0`, `cooldownSeconds >= 0` (default 300). `zoneType`
is immutable after creation (never accepted on update).

## Discord alerting (never a fallback channel)

Each zone carries its own `alert_channel_id` (nullable). If unset, the zone is fully functional
(tracked, audited, queryable) but **nothing is ever posted to Discord for it** - there is no
fallback channel and none is ever invented. When set, `internal/app/intrusion_publisher.go`'s
`intrusionPublisher` sends an embed via the exact same `ChannelMessageSendEmbed` interface every
other Champion publisher already uses (`internal/discord/panels.go`'s `MessageEditor`) - no new
Discord-sending mechanism was built.

**Staff copy (Channel System V2):** independently of the zone's own channel, every
non-suppressed `ZONE_INTRUSION`, `UAV_INTRUSION`, `BASE_RADAR_INTRUSION` and
`ZONE_BAN_VIOLATION` is also sent as an admin alert on the installation's `ADMIN_ALERTS` route
(`docs/ADMIN_LOGS.md`), when one is configured. That route is a channel the customer (or
one-click setup) chose for staff alerts, not an invented fallback; if it resolves to the zone's
own alert channel the copy is skipped, so nothing is posted twice. `ZONE_EXIT` is not an alert.

## Operational event output

Every transition the engine decides is noteworthy - `ZONE_INTRUSION`, `ZONE_EXIT`,
`UAV_INTRUSION`, `BASE_RADAR_INTRUSION`, `ZONE_BAN_VIOLATION` - is delivered to
`killfeed.IntrusionPublisher.PublishIntrusionEvent`, whether or not a Discord alert was actually
sent (`Suppressed` distinguishes the two). `internal/app`'s adapter first writes a stable,
structured `slog` line (`component=zone_intrusion`) for every event unconditionally - this codebase
has no existing internal event bus, so a consistently-shaped log line, keyed on
`event`/`zone_id`/`player_id`/`intrusion_id`, is the concrete implementation of "for future Live Ops
integration" this phase ships; a future consumer can tail/parse it, or this adapter can grow a
second sink later with no caller-visible change.

## Metrics

`IntrusionEngine.Metrics()` returns a point-in-time snapshot: `LocationEventsEvaluated`, `Entries`,
`Exits`, `IntrusionsCreated`, `IntrusionsSuppressed` (ignore-list skips - never tracked at all,
distinct from an alert being suppressed), `AlertsSent`, `AlertsSuppressedCooldown`,
`EvaluationLatencyMs` (cumulative). `zone_active_intrusions` is deliberately **not** one of these
counters - it is a live gauge, always computed at read time from `zone_intrusions`/`zone_presence`
by the API layer, never an incrementally-maintained counter that could drift from the persisted
truth. Not yet wired into `GET /api/admin/health`'s performance snapshot (same deliberate deferral
Phase 3 made for `LocationQueue.Health()` - the counters exist and are ready to be surfaced).

## Audit

Every zone/ignore/authorized/ban mutation and every acknowledgement writes an `admin_audit_log` row
via the existing `a.recordAudit` helper: `ZONE_CREATE`, `ZONE_UPDATE`, `ZONE_DELETE`,
`ZONE_IGNORE_ADD`, `ZONE_IGNORE_REMOVE`, `ZONE_AUTHORIZED_ADD`, `ZONE_AUTHORIZED_REMOVE`,
`ZONE_BAN_ADD`, `ZONE_BAN_LIFT`, `INTRUSION_ACKNOWLEDGE` - no new audit mechanism, the same one
Client Admin Phase 1 built.

## What was NOT touched

Billing, shop, the existing Economy ledger, OAuth, Owner Hub, Stripe, the Nitrado delta reader, and
the existing permission Level/capability hierarchy (only additive `requiredLevel` entries) are all
untouched by this phase - none of this phase's code lives in any of those files or packages.

## Deferred (still, deliberately)

Heatmap aggregation, Auto Payments, the priority queue, generic file writes, and Auto Start remain
unbuilt - reserved for later phases, exactly as instructed.
