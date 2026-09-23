# Runtime channel routing (installation routes -> Discord publishers)

The SaaS API stores each installation's feature -> Discord channel choices in
`installation_channel_routes` (`docs/SAAS_SCHEMA.md`). Champion's live
publishers were built earlier around legacy per-guild `GuildSetup` channel
fields. This document records the audit of that runtime, the shared resolver
the publishers migrate onto, and how far the migration has got.

**Migrated to runtime routes: `KILLFEED`, `LINK_GAMERTAG`, `STATS_LEADERBOARDS`,
`AUTO_LEADERBOARD`, `ADMIN_LOGS`. Built and runtime routed: `HITFEED`, `CONNECTIONS`, `PVE_FEED` (explicit suicides only - see its section), `BOUNTY` and `BOUNTY_TRACKING` (see `docs/BOUNTY_SYSTEM.md`), `ECONOMY` (see `docs/ECONOMY_SYSTEM.md`).** Every other route either has no publisher at
all (marked *Not implemented* below - nothing was built for them) or has no
text-channel feed to route. See the table below and "Migrated features".

## KILLFEED event path (audited)

```
Nitrado ADM log poll                       internal/killfeed/engine.go (per-server Engine)
  -> parsed PLAYER_KILL event              internal/killfeed/parser.go
  -> Engine.processLine                    engine.go processLine
  -> PersistenceQueue.EnqueueAndWait       durable, non-duplicate insert
  -> killPersistedHook (only AFTER a       engine.go SetPersistence
     successful non-duplicate insert)
  -> KillfeedPublisher.PublishKill         internal/discord/killfeed.go
  -> RotatingFeed.Enqueue                  internal/discord/rotating_feed.go
  -> RotatingFeed.flush (every 10 min)     posts to the channel resolved AT FLUSH
```

What the code actually knows at each point:

- **Server identity**: `runServerWorker` (internal/app/app.go) receives the
  `repository.GameServer` row - `row.ID` (`game_servers.id`) and `row.GuildID`
  (internal `guilds.id`). One worker, one Engine, one `KillfeedPublisher`, one
  `RotatingFeed` per server. The publisher previously was never told which
  server it belonged to.
- **Installation identity**: not known to the runtime. It is derived by joining
  `installations` (`game_server_id`) through `discord_guild_connections`.
- **Guild resolution**: the legacy path uses the single configured Discord
  guild snowflake (`a.Config.DiscordGuildID`) to read `GuildSetup`. The
  resolver instead keys on the server's own internal guild id.
- **Where the channel came from**: `GuildSetup.KillfeedChannelID`
  (`guilds.killfeed_channel_id`), falling back to env `KILLFEED_CHANNEL_ID`.
  Two independent resolution points existed: the publisher's
  `channelID()` (a gate) and the rotating feed's own `GuildSetup` lookup at
  flush. **Both** are now route-aware; migrating only the publisher would have
  left the feed posting to the legacy channel.
- Legacy resolution was per guild, so all servers of a guild shared one
  killfeed channel; routes make it per server.

## The shared resolver

`internal/routing` (`Resolver`, `RouteStore`, the 16 `Route*` constants):

```go
Resolve(ctx, guildRowID, serverID int64, routeKey string) (channelID string, found bool, err error)
```

- Keyed by **(guild, server)**, never guild alone: one Discord guild can host
  several DayZ servers, each with its own installation and routes.
- One joined SQL query (`ChannelRouteRepository.ResolveChannel`): installation
  -> guild connection -> game server -> route. Tenant isolation is structural
  (the runtime has no acting user): the server must belong to the connection's
  guild, and a server claimed by an organization must be claimed by the same
  organization that owns the installation and the connection.
- Read-through cache, TTL **30s** (`routing.DefaultTTL`), misses cached too,
  errors never cached, bounded size, nil-safe.
- **Route-change propagation**: routes written through the in-process SaaS API
  (`completeChannelsStep`) and server (re)selection invalidate the cache, so the
  next kill uses the new channel with **no delay and no restart**. A change made
  outside this process (direct DB edit, another instance) is picked up within
  the TTL - at most **30 seconds**. The rotating feed re-resolves at each flush
  and, if the channel changed, deletes the previous batch from the old channel
  before posting to the new one.

## KILLFEED resolution order

1. installation `KILLFEED` route for this server (new)
2. legacy `GuildSetup.KillfeedChannelID`
3. env `KILLFEED_CHANNEL_ID`

Exactly **one** channel is chosen, so a guild with both a route and a legacy
channel publishes only to the route - never twice.

Failure behaviour: a lookup error is logged (at most once a minute) and treated
as "no route" -> legacy fallback. It never fails kill processing; the kill is
already persisted before publish is attempted, and a publish failure stays
non-fatal. No route and no legacy channel -> the publish is skipped with the
pre-existing "channel not configured" result.

Diagnostic: `event=channel_route_fallback route_key=KILLFEED guild_id=<internal
guilds.id> server_id=<game_servers.id> reason=no_route|lookup_error`. Logged on
state change (not once per kill). Internal ids only - no tokens, no channel
contents.

Not gated on installation `status`: a configured route is honoured even while an
installation is still `CONFIGURING`.

## All routes

| Route key | Existing publisher | Legacy field / source | Event source | Status |
|---|---|---|---|---|
| `KILLFEED` | `KillfeedPublisher` + `RotatingFeed` (`discord/killfeed.go`) | `GuildSetup.KillfeedChannelID`, env `KILLFEED_CHANNEL_ID` | ADM `PLAYER_KILL`, after durable insert | **Migrated** (route -> legacy fallback) |
| `PVE_FEED` | `PveFeedPublisher` (`discord/pvefeed.go`) | none - no legacy PvE channel, no KILLFEED fallback (an *unclaimed* death continues to the separate legacy death feed, `GuildSetup.DeathChannelID`) | ADM `SUICIDE_ACTION` after durable persistence (explicit infected/animal/environment causes are supported by the classifier but the parser cannot produce them yet) | **IMPLEMENTED / RUNTIME ROUTED** (suicides) |
| `LINK_GAMERTAG` | Link panel posted by `RouteSyncer` (`discord/route_syncer.go`); interactions in `PublicPanelHandler` (`discord/public_panels.go`, channel-agnostic) | legacy `GuildSetup.LinkPanelChannelID` (posted by `discord/setup.go`) | Discord button/modal interaction | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `STATS_LEADERBOARDS` | Player-stats panel posted by `RouteSyncer`; interactions in `PublicPanelHandler` | legacy `GuildSetup.PlayerStatsChannelID` | Discord button interaction (on-demand, ephemeral) | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `AUTO_LEADERBOARD` | `LeaderboardScheduler` (`discord/leaderboard_scheduler.go`) | legacy `GuildSetup.LeaderboardsChannelID` | 3-hour timer, `/admin` refresh, route change | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `HITFEED` | `HitfeedPublisher` (`discord/hitfeed.go`) | none - no legacy channel, no KILLFEED fallback | ADM `PLAYER_HIT`, after dedupe (hits are not persisted) | **IMPLEMENTED / RUNTIME ROUTED** |
| `BOUNTY` | `BountyBoard` (`discord/bounty_feeds.go`) - one persistent board message per routed channel | none - no fallback (the `/bounty` command is unrelated and still replies ephemerally) | durable `bounties` table; reconciled on lifecycle events, route changes, restart and a 30s tick | **IMPLEMENTED / RUNTIME ROUTED** |
| `BOUNTY_TRACKING` | `BountyTracker` (`discord/bounty_feeds.go`) - placed / increased / claimed / expired / cancelled cards | none - no fallback (the "MOST WANTED" section of the live panels is unrelated) | `bounties.Service` events, published only after the change committed | **IMPLEMENTED / RUNTIME ROUTED** |
| `HEATMAPS` | `HeatmapBoard` (`discord/heatmap_board.go`) - one persistent PvP summary per routed channel, edited in place | none - no fallback | Phase 5 `heatmap.Service` aggregates (`PVP_KILLS`, 24 h, 250 m), every `HEATMAP_DISCORD_INTERVAL_MINUTES` and on route change | **IMPLEMENTED / RUNTIME ROUTED** |
| `ECONOMY` | `EconomyFeed` (`discord/economy_feed.go`) - bounty reward / admin credit / admin debit cards | none - no fallback (never `KILLFEED`; the economy works fully without the route) | `economy.Service` and the bounty claim, published only after the transaction committed | **IMPLEMENTED / RUNTIME ROUTED** |
| `SHOP` | `EconomyFeed` - shop purchase/refund cards are published on the `ECONOMY` route; the `SHOP` key maps to the same economy channel | - | shop transaction, after commit | **IMPLEMENTED** (via `ECONOMY`) |
| `CONNECTIONS` | `ConnectionsPublisher` (`discord/connections.go`) | none - no legacy text channel, no KILLFEED/voice-counter fallback (the `OnlinePlayersChannelID` voice counter is a separate, untouched feature) | ADM `is connected` / `has been disconnected`, after dedupe + durable persistence + a real presence state change | **IMPLEMENTED / RUNTIME ROUTED** |
| `BUILD_FEED` | `BuildFeedPublisher` (`discord/build_feed.go`) - single cards, or one summary for a burst | none - no fallback | ADM `BUILD_ACTION` (placed / built / dismantled), after dedupe; only present when the server enables `adminLogPlacement` / `adminLogBuildActions` | **IMPLEMENTED / RUNTIME ROUTED** (source depends on server config) |
| `ADMIN_ALERTS` | `AdminAlertPublisher` (`discord/admin_alerts.go`) - alert on entering a condition, resolution on leaving it | none - no fallback | per-server ADM snapshots and download reports; zone intrusion engine | **IMPLEMENTED / RUNTIME ROUTED** |
| `ADMIN_LOGS` | `ADMMonitorPublisher` (`discord/adm_monitor.go`) | legacy `GuildSetup.ADMMonitorChannelID` | ADM snapshot/download callbacks | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `SERVER_STATUS` | `ServerStatusBoard` (`discord/server_status_board.go`) - one persistent message per routed channel; `LiveCompletionPublisher` season/war/event results | legacy `GuildSetup.ServerStatusChannelID` (completion announcements only) | per-server ADM snapshots | **IMPLEMENTED / RUNTIME ROUTED** |
| `ONLINE_COUNTER` | `VoiceChannelCounter` (`discord/counter.go`) - binds to the routed voice channel | legacy `GuildSetup.OnlinePlayersChannelID` | the public counter server's presence tracker | **IMPLEMENTED / RUNTIME ROUTED** |

`DeathfeedPublisher` (non-PvP deaths) has no route key of its own: Channel
System V2 sends it to the `KILLFEED` route (the combat feed), with
`GuildSetup.DeathChannelID` only as the fallback for guilds without routes. A
suicide is still claimed by `PVE_FEED` when that route exists.

Migrating another publisher is: give it the same `(guildRowID, serverID)`,
call `routing.Resolver.Resolve` with its `Route*` key (or wrap that in
`discord.RouteBinding`, which adds the never-fatal fallback and throttled
diagnostics), and fall back to its legacy field - `KillfeedPublisher.RouteChannelID`
is the reference implementation.

## Migrated features

All four reuse the one `routing.Resolver` and its single cache - no publisher
has its own cache. Resolution is always `(guild row, server)`, never guild alone.

Common rule for every migrated feature:

| Situation | Result |
|---|---|
| route configured | route only |
| route absent | legacy channel |
| route lookup error | legacy channel (logged; never fatal, never cached) |
| route and legacy both absent | safe no-op / the pre-existing behaviour |

Exactly one destination is chosen, so nothing is ever published to both.

### Per-server vs guild-level

* **`ADMIN_LOGS`** is per server. Each server's `ADMMonitorPublisher` already
  knows its `game_servers.id`, so it resolves its own route (`SetRouting` in
  `runServerWorker`); a sibling server without a route does **not** inherit
  another server's, it falls back to legacy.
* **`LINK_GAMERTAG`, `STATS_LEADERBOARDS`, `AUTO_LEADERBOARD`** are *guild-level*
  artifacts: the link/stats panels are buttons that act on the whole guild, and
  the leaderboard is computed from guild-wide stats (`TopByKills(guildRowID)` ...).
  There is no per-server leaderboard data to route, and building one is out of
  scope. They therefore follow the **union of the routes of every active server
  in the guild**: each server is resolved on its own `(guild, server)` identity
  (so cross-organization isolation is the resolver's SQL), the distinct channels
  are collected in server order, and one message is kept per channel. Two servers
  routing to different channels get a panel in each; two routing to the same
  channel share one. The legacy channel is used **only when no server of the
  guild has a route**.

### How persistent messages avoid duplicates

Legacy state was one message id per guild in `GuildSetup`, which cannot describe
several routed channels. Migration `0028_guild_route_panels` adds
`guild_route_panels(guild_id, route_key, channel_id, message_id)`, keyed by
**channel** - so a route change, a restart or two servers sharing a channel can
never produce a second live copy:

* the reconciler (`discord.RoutePanels.Sync`) makes the panel exist in every
  desired channel, records it, and retires panels recorded for channels no longer
  desired;
* a message is re-created **only** when Discord reports it gone (unknown
  message/channel) - never on a transient error;
* if a freshly posted message cannot be recorded it is deleted again, so the next
  sync cannot post a second copy;
* `ADMIN_LOGS` keeps its existing per-server id (`server_configs.adm_monitor_message_id`)
  and additionally remembers, in memory, which channel the message lives in.

### Feature notes

* **`LINK_GAMERTAG` / `STATS_LEADERBOARDS`** - `public_panels.go` never chose a
  channel (its handlers are channel-agnostic, keyed by the interacting guild), so
  the "publisher" is the place the panel message is posted: previously
  `SetupManager.EnsureConfigured`. `RouteSyncer` now posts the same panel
  (`LinkUsernameInfoEmbed`/`PlayerStatsInfoEmbed` + the same component custom IDs)
  to routed channels, so interactions behave exactly as before. When route mode
  is live the legacy panel message is deleted and its id cleared (no second
  copy); `EnsureConfigured` no longer creates a legacy panel for a routed key
  (`SetRouteGate`). If every route is later removed, the syncer calls
  `EnsureConfigured` once to bring the legacy panel back.
* **`AUTO_LEADERBOARD`** - `LeaderboardScheduler.RefreshOnce` publishes to the
  routed channels when any server has a route, otherwise edits the legacy panel
  exactly as before. The first time route mode takes over, the legacy message is
  retired; on return to legacy a fresh message is posted and persisted. The
  scheduler now exists whenever routing is available (not only when a legacy
  leaderboard channel was configured), so a route-only guild gets a leaderboard.
  Refreshes are serialised. Manual `/admin` refresh uses the same path.
* **`ADMIN_LOGS`** - `activeChannel()` is re-evaluated on every snapshot and
  download (it previously cached the legacy channel forever). If the destination
  changes while a message exists elsewhere, the old message is deleted and a new
  one posted immediately. If a route is set while the process is down, the stored
  id belongs to the legacy channel: the first routed edit gets "unknown message",
  the stray legacy copy is deleted and one message is posted in the routed channel
  (only on route-derived destinations, so legacy behaviour is unchanged).

### Propagation

`RouteSyncer` (`discord/route_syncer.go`) runs at startup, on `Trigger()`, and
every `RouteSyncInterval` (30s = `routing.DefaultTTL`):

* **In-process route writes** (`completeChannelsStep`, server (re)selection) call
  `ChannelRoutes.InvalidateAll()` and `RouteSyncer.Trigger()`, so panels move and
  the leaderboard refreshes immediately, with no restart.
* **Out-of-process changes** are seen once the resolver's cache expires: within
  the 30s TTL plus at most one 30s sync tick.
* `ADMIN_LOGS` needs no trigger: it re-resolves per snapshot/download.
* Static panels are re-verified only when the resolved channels change or every
  10 minutes; the leaderboard is refreshed when its resolved channels change.
* A failed lookup never tears anything down: nothing is retired while a lookup is
  failing (it says nothing about whether the route exists).

Diagnostic (all four): `event=channel_route_fallback route_key=<KEY> guild_id=<guilds.id>
server_id=<game_servers.id> reason=no_route|lookup_error` - internal ids only.

### Not covered

Every route key now has a publisher. `CASINO` was removed entirely
(migration 0044). The runtime is still a single configured Discord guild
(`DISCORD_GUILD_ID`) with its own set of servers; multi-guild workers are out of
scope. Migration `0027` back-filled `STATS_LEADERBOARDS` and `ADMIN_LOGS` routes
from installation settings, so those existing routes now take effect at runtime.

## HITFEED (implemented, runtime routed)

`HITFEED` publishes a compact card per attacker/victim exchange, from the parsed
ADM `PLAYER_HIT` lines, to the server's `HITFEED` route.

### Event source (audited)

```
Nitrado ADM log poll                        internal/killfeed/engine.go (per-server Engine)
  -> ADMParser.parseHit                     internal/killfeed/parser.go   ("... hit by Player ...")
  -> Event{Type: PLAYER_HIT}                internal/killfeed/event.go
  -> Engine.processLine                     metrics.HitsParsed++, dedupe.Contains
  -> Engine.publishHit                      NEW - after dedupe, recover()-guarded, non-blocking
  -> HitfeedPublisher.PublishHit            internal/discord/hitfeed.go (in-memory aggregation only)
  -> HitfeedPublisher.Run goroutine         route lookup + Discord send, off the parse loop
```

Fields the parser actually produces for a hit (nothing else is assumed):

| Field | Source in the line | Used by the card |
|---|---|---|
| `Attacker`, `Victim` (`Name`, `ID`, optional `Position`) | `Player "<n>" (id=... pos=<...>)` | names (ids only key the encounter; never shown) |
| `Weapon` | `with <weapon>` | yes |
| `Ammo` | `(Bullet_...)` | yes, minus the `Bullet_` prefix |
| `Distance` (optional) | `from <n> meters` | yes, latest value |
| `HitZone`, `HitZoneID` | `into <Zone>(<id>)` | zone name only, latest value |
| `Damage` (optional) | `for <n> damage` | summed, only if every hit in the card had it |
| `HP` (optional) | `[HP: <n>]` | **not shown** |
| `Dead` | `(DEAD)` marker on the victim | not shown (see below) |
| `TimeOfDay`, `Raw` | clock / raw line | not shown |

Hits are **not persisted**: only connect/disconnect/death/suicide/kill go through
the durable persistence queue. The hit feed is therefore best-effort and
ephemeral - a process restart can lose the encounters in the current window, and
hits replayed after a *restart* (in-memory dedupe is lost) may be re-sent once.
Replays inside a running process (retry after a later persistence failure,
rotation overlap) are dropped by the ADM dedupe before reaching the feed.

`HP` is parsed but deliberately not rendered: the ADM line does not say whether
it is the value before or after the hit, and the feed only shows what it can
state with certainty. Coordinates are never shown.

The hit fingerprint used by the ADM dedupe now also includes the hit zone and
damage. A hit line has no unique id, and auto-fire at a stationary target repeats
time/weapon/distance within a second, so without this distinct hits collapsed
into one and the feed under-counted. A genuine replay still matches on all
fields. Kill/death fingerprints are unchanged.

### Route and fallback

* Route key `HITFEED`, resolved through the shared `routing.Resolver` on the
  server's own `(guild row, server id)` - the same identity as `KILLFEED`, via
  `discord.RouteBinding`. One `HitfeedPublisher` per server worker
  (`runServerWorker`), so two servers of one guild cannot leak into each other.
* **No legacy fallback.** There was never a legacy HITFEED channel, and it does
  **not** fall back to `KILLFEED`.
  * route configured -> publish
  * route absent -> no-op (hits are not even buffered)
  * route lookup error -> no-op; `event=channel_route_fallback route_key=HITFEED
    reason=lookup_error` warning (at most once a minute); recovers on its own
* The route is re-resolved every tick (2s) and at close of each window through
  the shared cache, so an in-process route change (which invalidates the cache)
  is used within one tick, and an out-of-process change within the resolver TTL
  (30s). No restart. Encounters already open when the route changes are sent to
  the **new** channel; if the route is removed they are dropped.

### Aggregation and flood policy

A firefight can log many hits per second, so hits are never sent one per message:

1. **Per-encounter aggregation** - hits are grouped by `(attacker, victim, weapon)`
   (ids, or names if the id is missing) over a fixed **5s** window starting at the
   encounter's first hit. The card shows the hit count, latest distance/zone/ammo
   and, when every hit carried it, the summed damage. A different weapon is a
   separate card.
2. **Batching** - closed encounters are sent as embeds, up to **10 per message**.
3. **Rate cap** - at most **1 message per 2s tick** per server worker, i.e. at most
   10 cards / 2s (5 cards/s sustained, well inside Discord's per-channel limit).
4. **Bounds** - at most **200 open encounters** (new ones beyond that are dropped
   and counted) and a **100-card send backlog** (oldest dropped and counted). A
   drop is reported as one `hitfeed_flood_drop` warning per minute, not per hit.
5. **Isolation** - `PublishHit` only touches an in-memory map; route lookups and
   Discord I/O run on the feed's own goroutine, panic-guarded. A Discord failure
   drops that message (no retry backlog) and is logged; it cannot stop ADM
   parsing, kill/death processing, persistence or checkpointing, and a panicking
   consumer is recovered by the engine.

Worst case: one message every 2s carrying 10 cards, after a latency of up to 5s
(window) + 2s (tick). This changes no existing event semantics: hits stay
unpersisted and still count in `HitsParsed`; aggregation only affects what the
Discord card shows.

### Card

```
🎯 HIT  PlayerA  ➜  PlayerB
M4-A1 · 556x45 · 42m
Torso · 3 hits · 84 dmg
```

Only parsed values appear; a missing weapon/ammo/distance/zone/damage is omitted
rather than shown as "unknown". Names are sanitised (`@`/`#` stripped) and the
message carries an empty `AllowedMentions`.

### No duplicate kills

Feed roles stay separate. The engine hands **only** `PLAYER_HIT` events to the hit
feed, and `HitfeedPublisher` ignores every other type. The lethal hit line (victim
already `(DEAD)`) renders as an ordinary hit card with no kill wording; the kill
itself is `PLAYER_KILL` and only ever goes to `KILLFEED`.

## CONNECTIONS (implemented, runtime routed)

`CONNECTIONS` publishes player connect/disconnect notices, from the parsed ADM
presence lines, to the server's `CONNECTIONS` route.

### Event source (audited)

```
Nitrado ADM log poll                        internal/killfeed/engine.go (per-server Engine)
  -> ADMParser.parseConnecting              "... is connecting"           -> PLAYER_CONNECTING
  -> ADMParser.parseConnected               "... is connected"            -> PLAYER_CONNECT
  -> ADMParser.parseDisconnected            "... has been disconnected"   -> PLAYER_DISCONNECT
  -> Engine.processLine                     metrics, dedupe.Contains
  -> PersistenceQueue.EnqueueAndWait        durable: upsert player + activity session
       (persistOne)                         (ActivityRepository.Connect/Disconnect) + link challenge
  -> dedupe.Remember
  -> PlayerTracker.PlayerConnected /        in-memory online set; true only on a real state change
     DisconnectSession
  -> Engine.publishConnection               NEW - recover()-guarded, non-blocking
  -> ConnectionsPublisher.PublishConnection bounded queue only
  -> ConnectionsPublisher.Run goroutine     route lookup + Discord send, off the parse loop
```

Fields the parser produces for these events: `Player` (`Name`, `ID`, optional
`Position` from `pos=`), `TimeOfDay`, `Raw`. `PLAYER_CONNECTING` is parsed but is
not a state change (nothing acts on it), so it is never announced.

**Ordering.** A notice is published only after the event passed the ADM dedupe
**and** the durable persistence acknowledged it, and only when the in-memory
tracker says the state really changed. If persistence fails, nothing is
published and the tracker is not updated; the event is retried on the next poll
(the failed attempt is not remembered by the dedupe) and announced exactly once
when it succeeds. Nothing bypasses persistence ordering.

### What is (and is not) shown

```
🟢 CONNECTED                       🔴 DISCONNECTED
PlayerName joined the server.      PlayerName left the server.
                                   Session: 42m
```

Privacy/exposure choices: only the **display name**, the kind and - for a
disconnect - the session length. The ADM player id, the position/coordinates and
any Discord/gamertag link state are deliberately **not** passed to the publisher
at all (`killfeed.ConnectionNotice` has no such fields), so they cannot leak.
Names are sanitised (`@`/`#`/control characters stripped, length bounded) and
every message carries an empty `AllowedMentions`.

`Session` is the time between Champion processing the player's connect and
their disconnect (the same "observed" notion the link/playtime code uses). It is
shown only when the tracker saw the connect and the session is at least a minute;
otherwise it is omitted rather than guessed. A duplicate "is connected" does not
restart the session clock.

### Reconnect

The ADM log has **no explicit reconnect event**, and Champion does not model one:
a repeated "is connected" for an already-online player is a *refresh* (no new
notice), and a disconnect followed by a connect is exactly `DISCONNECTED` then
`CONNECTED`. A `RECONNECTED` state is therefore **not** rendered and is never
inferred from timing.

### Route and fallback

* Route key `CONNECTIONS`, resolved through the shared `routing.Resolver` on the
  server's own `(guild row, server id)` via `discord.RouteBinding` - the same
  identity as `KILLFEED`/`HITFEED`. One `ConnectionsPublisher` per server worker
  (`runServerWorker`), so two servers of one guild cannot leak into each other.
* **No fallback.** No legacy CONNECTIONS text channel exists, and it does **not**
  fall back to `KILLFEED`, `HITFEED` or the player-count voice channel
  (`GuildSetup.OnlinePlayersChannelID`, which is untouched).
  * route configured -> publish
  * route absent -> no-op (events are not even queued)
  * route lookup error -> no-op; `event=channel_route_fallback route_key=CONNECTIONS
    reason=lookup_error` warning (at most once a minute); recovers on its own
* The route is re-resolved every tick (2s) through the shared cache, so an
  in-process route change (which invalidates the cache) is used within one tick and
  an out-of-process change within the resolver TTL (30s). No restart.

### Dedupe

The existing ADM dedupe is reused - there is no second dedupe system. Two
things matter:

1. **Replays** (checkpoint retry after a later persistence failure, rotation
   overlap) are dropped by `dedupe.Contains` before persistence, tracker or feed.
2. **State-change gating**: only `PlayerConnected == true` (player was not online)
   and `DisconnectSession` removed (player was online) publish. A duplicate connect
   for an online player and a disconnect for a player Champion never saw connect
   are not announced.

**Fix included:** the ADM dedupe fingerprint previously ignored `ev.Player`, the
only identity a connect/disconnect carries. Two *different* players connecting in
the same second - exactly what a server-restart burst looks like - shared a
fingerprint, so the second was dropped as a "duplicate": never persisted, never
added to the online set, never announced. The fingerprint now includes the
player for every event that has one (kill/hit fingerprints are unaffected). A
genuine replay is the same player and still matches. This also corrects the
online count and death/suicide handling for same-second events.

### Burst policy

Restarts make many players reconnect within seconds, so delivery is bounded:

* one bounded queue and **one goroutine per server worker** - never a goroutine per
  event;
* at most **1 message per 2s tick**, batching up to **20 events** into one embed
  (a lone event gets the plain single card):
  ```
  🔌 SERVER CONNECTIONS
  🟢 PlayerA connected
  🟢 PlayerB connected
  🔴 PlayerC disconnected · 42m
  ```
  Every event keeps its own line; different players are never merged;
* the queue holds at most **300 events**; on overflow the **oldest** are dropped
  and the next message says `… N earlier connection events were not shown`;
  drops are also logged once a minute (`connections_flood_drop`);
* `PublishConnection` only appends to memory; a Discord failure drops that
  message (no retry backlog), is logged, and cannot stop ADM parsing, presence
  tracking, persistence, the killfeed or the hitfeed; a panicking consumer is
  recovered by the engine, a panicking sender by the feed's tick.

Worst case: one message every 2s carrying 20 events (10 events/s sustained).

### Limitations

Connection notices are ephemeral (not stored). After a **process restart** the
in-memory tracker is empty, so connects replayed from the durable checkpoint may
be announced again once, and players who joined before Champion started are not
announced leaving. Session lengths are only known for players Champion saw
connect. Latency is up to one 2s tick.

## PVE_FEED (implemented, runtime routed - narrowly)

`PVE_FEED` publishes deaths that are **provably not PvP**, to the server's
`PVE_FEED` route. Read "what the parser can prove" first: with today's ADM
parser that is **explicit suicides only**. The classifier and the feed already
support explicit infected/animal/environment causes, but nothing can produce
them yet, so no such label is ever shown.

### Death event path (audited)

```
Nitrado ADM log poll                        internal/killfeed/engine.go (per-server Engine)
  parseExplicitKill  "... killed by Player ..."       -> PLAYER_KILL         (Killer, Victim, Weapon, Distance)
  parseHit           "... hit by Player ..."          -> PLAYER_HIT
  parseDeath         "... died ..." (no "killed by")  -> PLAYER_DEATH        (Player, Dead) - NO cause field
  parseSuicideAction "... performed EmoteSuicide ..." -> SUICIDE_ACTION      (Player, Weapon = item held)
  -> Engine.processLine                     metrics, dedupe.Contains
  -> PersistenceQueue.EnqueueAndWait        kill: KillRecord; death/suicide: DeathRecord
       (persistOne)                           (death_type = SUICIDE or UNKNOWN), durable fingerprint
  -> killPersistedHook -> KillfeedPublisher       ONLY after a non-duplicate durable insert
  -> deathPersistedHook                           ONLY after a non-duplicate durable insert
       -> NEW: Engine.claimByPveFeed -> PveFeedPublisher.PublishPveDeath  (claimed: stop here)
       -> otherwise the pre-existing DeathfeedPublisher (GuildSetup.DeathChannelID)
```

What ADM actually distinguishes, from the sample lines this project has (the
parser tests) - nothing else is assumed:

| Cause | Distinguishable today? |
|---|---|
| PvP kill | **yes** - `killed by Player ...` (`PLAYER_KILL`) |
| Suicide | **yes** - `performed EmoteSuicide` (`SUICIDE_ACTION`) |
| Generic death | a `died.` line with no `killed by`: states **no cause** (`PLAYER_DEATH`) |
| Infected, animal, environment | **no** - no such line exists in the samples; the parser does not even read a `killed by <non-player>` line (`parseExplicitKill` needs `killed by Player`, `parseDeath` rejects any `killed by`), so such a line yields **no event at all** |
| Fall, drowning, bleeding, starvation, dehydration, cold, explosion, fire, gas | **no** - nothing in the model tells them apart; none is implemented and none is inferred from weapon strings or names |

If real ADM logs show `killed by <entity>` or other cause lines for infected,
animals or the environment, that is a parser extension (it needs real samples,
not memory of DayZ's formats) - once the parser sets `Event.Cause`, the
classifier, the claim logic and the feed need no change.

### Classification rules (`killfeed.PveCause`, ordered)

1. **Explicit player attacker** (`PLAYER_KILL`, or any `Killer`/`Attacker`) -> PvP:
   stays on `KILLFEED`, never PvE. Valid PvP kills are never moved.
2. **Explicit suicide** (`SUICIDE_ACTION`) -> `PVE_FEED`.
3. **Explicit non-player source** (`PLAYER_DEATH` whose parser-proven `Cause` is
   infected / animal / environment) -> `PVE_FEED`. *(Never set by the parser yet.)*
4. **Anything else is ambiguous**, notably a generic `died.` line: **no guess is
   made and current behaviour is preserved** - it goes to the legacy death feed
   as before, exactly like a suicide does when there is no `PVE_FEED` route.

A generic death is deliberately **not** sent to `PVE_FEED` with a neutral "died"
text: its cause is unknown, and the ordered rules say unknown means "preserve
current behaviour". That is a one-line policy in `PveCause` if you want it changed.

### Mutual exclusion with KILLFEED and the legacy death feed

Each event has exactly one home; ownership is decided **once**, at the death
hook, after the durable insert:

* PvP kills go through `KillfeedPublisher` only - the PVE feed is never offered them.
* A death/suicide the PVE feed **claims** is not posted to the legacy death feed.
* A death it does **not** claim (no `PVE_FEED` route, lookup error, ambiguous
  cause, panicking consumer) continues to the legacy death feed unchanged.

A suicide is never reclassified as a kill. Note the ADM logs a `died.` line after
the emote; that is a separate `PLAYER_DEATH` event which stays on the legacy death
feed (correlating the two would be inference, so it is not done).

### Route and fallback

* Route key `PVE_FEED`, resolved through the shared `routing.Resolver` on the
  server's own `(guild row, server id)` via `discord.RouteBinding`. One
  `PveFeedPublisher` per server worker, so servers of one guild cannot leak.
* **`PVE_FEED` has no fallback**: no legacy PvE channel, and it never falls back
  to `KILLFEED`.
  * route configured -> the feed claims and publishes
  * route absent -> no-op, claims nothing, buffers nothing
  * lookup error -> no-op with a throttled `channel_route_fallback` warning; it
    recovers on its own
  What happens to an *unclaimed* death is the pre-existing legacy death feed's
  behaviour, not a `PVE_FEED` fallback.
* Re-resolved every 2s tick through the shared cache: an in-process route change
  is used within one tick, an out-of-process one within the resolver TTL (30s). No
  restart. Removing the route makes the feed stop claiming, so the legacy death
  feed takes over again.

### Ordering, dedupe, failure isolation

* **Persist before publish.** The notice is offered only from the death-persisted
  hook, i.e. after the durable insert succeeded; if it fails nothing is published
  and the retry (next poll) publishes exactly once. Nothing bypasses persistence.
* **Replays.** The ADM dedupe drops in-process replays before persistence; the
  durable `(guild, fingerprint)` uniqueness returns `ErrDuplicate` for anything
  already stored (including after a restart), and the hook does not fire for it.
* **Failure isolation.** `PublishPveDeath` only appends to a bounded queue (100,
  oldest dropped and reported); route lookups and Discord I/O run on the feed's
  own goroutine, one message per 2s tick carrying up to 10 cards. A Discord
  failure drops that message and is logged; it cannot reach persistence, ADM
  parsing, killfeed, hitfeed or connections. A panicking consumer is recovered by
  the engine and the death simply continues to the legacy feed.

### Card

```
💀 SUICIDE                 ☣️ PVE DEATH               🐺 PVE DEATH               ⚠️ PVE DEATH
Alice died by suicide.     Bob was killed by an       Bob was killed by an       Bob died to the
                           infected.                  animal.                    environment.
```

Only the sanitised display name and the proven cause. No id, no coordinates, and
no weapon (a suicide's "weapon" is just the item held). A cause label is never
shown unless it is proven; there is no generic "unknown cause" card. Names are
sanitised (`@`/`#`/control characters stripped, length bounded) and every message
has an empty `AllowedMentions`.

## BOUNTY and BOUNTY_TRACKING (implemented, runtime routed)

Phase 1 of the bounty system. Full architecture, state model, claim ordering,
stacking policy and failure behaviour: **`docs/BOUNTY_SYSTEM.md`**. Routing
summary:

* **Route keys:** `BOUNTY` = the public board (one persistent message per routed
  channel, top 10 targets by combined active amount, names only);
  `BOUNTY_TRACKING` = the lifecycle feed (placed, increased, claimed, expired,
  cancelled). They are independent - either can be configured without the other.
* **Resolution:** the shared `routing.Resolver` on the `(guild row, server)`
  identity. The board resolves `BOUNTY` for every active server and keeps one board
  per distinct channel (a shared channel shows the union of its servers' bounties
  plus guild-wide ones). The tracker resolves `BOUNTY_TRACKING` per event: a
  server-scoped bounty -> that server's route; a claim or streak placement -> the
  server the kill happened on; a guild-wide event -> every server's route,
  deduplicated by channel.
* **Fallback:** none. Route absent -> no board / no cards; lookup error -> no-op with
  a throttled warning (an existing board is left alone). Bounty correctness never
  depends on either route or on Discord: the database is written first and Discord is
  only told afterwards.
* **Board persistence:** the message id is recorded in `guild_route_panels`
  (`route_key = 'BOUNTY'`), so restarts and route changes edit or move the same
  board; an edit that would change nothing is skipped.
* **Propagation:** in-process route writes invalidate the resolver cache and call
  `BountyBoard.Trigger()`; out-of-process changes are picked up within the resolver
  TTL plus the board's 30s tick. The tracker re-resolves per event, so it needs no
  trigger.
* **Ordering:** persist kill -> atomic claim -> commit -> notify. A claim is never
  made from a raw ADM line, and a Discord failure never rolls back or repeats one.

## ECONOMY (implemented, runtime routed)

Phase 1 of the Champion Points economy. Full ledger/balance model, atomic credit and
debit, idempotency and commands: **`docs/ECONOMY_SYSTEM.md`**. Routing summary:

* **Route key:** `ECONOMY` = the lifecycle feed (bounty rewards, admin credits and
  debits). Balances and history are never posted here - they are ephemeral replies.
* **Resolution:** the shared `routing.Resolver` on the `(guild row, server)` identity,
  resolved per event: a server-attributed event (a bounty reward) -> the server the
  kill happened on; a guild-wide event (an admin adjustment) -> every server's route,
  deduplicated by channel.
* **Fallback:** none. Route absent -> no cards; lookup error -> no-op with a throttled
  warning. Never `KILLFEED`. The economy is correct without any route.
* **Ordering and failure:** the database transaction commits first; the feed is told
  afterwards and can only describe committed state. A failed send is dropped, never
  retried, and never rolls back or repeats a transaction.
* **Propagation:** in-process route writes invalidate the resolver cache, so the next
  event uses the new route; out-of-process changes are picked up within the resolver TTL.
* **Privacy:** cards show the display name, type, amount and (rewards only) the new
  balance - no internal ids, no admin, no reason.

## Custom embed templates (Embed Designer Phase 4)

Routes whose publishers can render an installation's saved custom embed template: `KILLFEED`,
`HITFEED`, `PVE_FEED`, `BOUNTY_TRACKING`, `ECONOMY` and `CONNECTIONS` (single-event cards only).
Behind `CHAMPION_CUSTOM_EMBEDS_ENABLED` (default off); presentation only - route resolution, channel
selection, aggregation, rate limits and fallbacks are unchanged, and any template problem publishes
the existing default card. The template is resolved for the same `(guild, server)` installation the
route channel is, never by guild alone. `BOUNTY` (persistent board), `ADMIN_LOGS` (live monitor) and
every route without a publisher are not rendered. Full architecture, variables, limits and the
compatibility matrix: **`docs/EMBED_RUNTIME.md`**.
