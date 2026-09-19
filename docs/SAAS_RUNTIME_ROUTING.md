# Runtime channel routing (installation routes -> Discord publishers)

The SaaS API stores each installation's feature -> Discord channel choices in
`installation_channel_routes` (`docs/SAAS_SCHEMA.md`). Champion's live
publishers were built earlier around legacy per-guild `GuildSetup` channel
fields. This document records the audit of that runtime, the shared resolver
the publishers migrate onto, and how far the migration has got.

**Migrated to runtime routes: `KILLFEED`, `LINK_GAMERTAG`, `STATS_LEADERBOARDS`,
`AUTO_LEADERBOARD`, `ADMIN_LOGS`. Built and runtime routed: `HITFEED`.** Every other route either has no publisher at
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

## All sixteen routes

| Route key | Existing publisher | Legacy field / source | Event source | Status |
|---|---|---|---|---|
| `KILLFEED` | `KillfeedPublisher` + `RotatingFeed` (`discord/killfeed.go`) | `GuildSetup.KillfeedChannelID`, env `KILLFEED_CHANNEL_ID` | ADM `PLAYER_KILL`, after durable insert | **Migrated** (route -> legacy fallback) |
| `PVE_FEED` | none | - | no infected/environment event type parsed | Not implemented |
| `LINK_GAMERTAG` | Link panel posted by `RouteSyncer` (`discord/route_syncer.go`); interactions in `PublicPanelHandler` (`discord/public_panels.go`, channel-agnostic) | legacy `GuildSetup.LinkPanelChannelID` (posted by `discord/setup.go`) | Discord button/modal interaction | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `STATS_LEADERBOARDS` | Player-stats panel posted by `RouteSyncer`; interactions in `PublicPanelHandler` | legacy `GuildSetup.PlayerStatsChannelID` | Discord button interaction (on-demand, ephemeral) | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `AUTO_LEADERBOARD` | `LeaderboardScheduler` (`discord/leaderboard_scheduler.go`) | legacy `GuildSetup.LeaderboardsChannelID` | 3-hour timer, `/admin` refresh, route change | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |
| `HITFEED` | `HitfeedPublisher` (`discord/hitfeed.go`) | none - no legacy channel, no KILLFEED fallback | ADM `PLAYER_HIT`, after dedupe (hits are not persisted) | **IMPLEMENTED / RUNTIME ROUTED** |
| `BOUNTY` | `BountyCommandHandler` (`discord/competitive_commands.go`) | none (ephemeral replies) | `/bounty` slash command | No channel feed |
| `BOUNTY_TRACKING` | "MOST WANTED" section of the live-panels message (`discord/panels`) | shares the leaderboards panel path | panel flush | No dedicated channel |
| `HEATMAPS` | none | - | - | Not implemented |
| `ECONOMY` | none | - | - | Not implemented |
| `CASINO` | none | - | - | Not implemented |
| `SHOP` | none | - | - | Not implemented |
| `CONNECTIONS` | `VoiceChannelCounter` (`discord/counter.go`) | `GuildSetup.OnlinePlayersChannelID` (a **voice** channel name counter, not a text feed) | `PLAYER_CONNECT`/`DISCONNECT` | Legacy only; no text feed |
| `BUILD_FEED` | none | - | no building event type | Not implemented |
| `ADMIN_ALERTS` | none | - | - | Not implemented |
| `ADMIN_LOGS` | `ADMMonitorPublisher` (`discord/adm_monitor.go`) | legacy `GuildSetup.ADMMonitorChannelID` | ADM snapshot/download callbacks | **MIGRATED TO RUNTIME ROUTES** (route -> legacy) |

Also outside the route vocabulary: `DeathfeedPublisher` (`GuildSetup.DeathChannelID`,
non-PvP deaths/suicides) and the server-status panel
(`GuildSetup.ServerStatusChannelID`) have no route key today.

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

Routes for `PVE_FEED`, `BOUNTY`, `BOUNTY_TRACKING`, `HEATMAPS`,
`ECONOMY`, `CASINO`, `SHOP`, `CONNECTIONS`, `BUILD_FEED` and `ADMIN_ALERTS` are
**not built**: they have no publisher (or no text feed), and this migration
deliberately adds none. The runtime is still a single configured Discord guild
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
