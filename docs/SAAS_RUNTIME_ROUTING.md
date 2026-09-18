# Runtime channel routing (installation routes -> Discord publishers)

The SaaS API stores each installation's feature -> Discord channel choices in
`installation_channel_routes` (`docs/SAAS_SCHEMA.md`). Champion's live
publishers were built earlier around legacy per-guild `GuildSetup` channel
fields. This document records the audit of that runtime, the shared resolver
the publishers migrate onto, and how far the migration has got.

**Migrated so far: `KILLFEED` only.** Every other route is still served by its
legacy source (or has no publisher at all) - see the table below.

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
| `LINK_GAMERTAG` | `PublicPanelHandler.handleLink`; panel posted by `discord/setup.go` | `GuildSetup.LinkPanelChannelID` | Discord button/modal interaction | Legacy only |
| `STATS_LEADERBOARDS` | `PublicPanelHandler.handleMyStats/handleSearch` | `GuildSetup.PlayerStatsChannelID` | Discord button interaction (on-demand, ephemeral) | Legacy only |
| `AUTO_LEADERBOARD` | `LeaderboardScheduler` (`discord/leaderboard_scheduler.go`) | `GuildSetup.LeaderboardsChannelID` | 3-hour timer / `/admin` refresh | Legacy only |
| `HITFEED` | none | - | `PLAYER_HIT` parsed and counted in metrics only | Not implemented |
| `BOUNTY` | `BountyCommandHandler` (`discord/competitive_commands.go`) | none (ephemeral replies) | `/bounty` slash command | No channel feed |
| `BOUNTY_TRACKING` | "MOST WANTED" section of the live-panels message (`discord/panels`) | shares the leaderboards panel path | panel flush | No dedicated channel |
| `HEATMAPS` | none | - | - | Not implemented |
| `ECONOMY` | none | - | - | Not implemented |
| `CASINO` | none | - | - | Not implemented |
| `SHOP` | none | - | - | Not implemented |
| `CONNECTIONS` | `VoiceChannelCounter` (`discord/counter.go`) | `GuildSetup.OnlinePlayersChannelID` (a **voice** channel name counter, not a text feed) | `PLAYER_CONNECT`/`DISCONNECT` | Legacy only; no text feed |
| `BUILD_FEED` | none | - | no building event type | Not implemented |
| `ADMIN_ALERTS` | none | - | - | Not implemented |
| `ADMIN_LOGS` | `ADMMonitorPublisher` (`discord/adm_monitor.go`) | `GuildSetup.ADMMonitorChannelID` | ADM download/health callbacks | Legacy only |

Also outside the route vocabulary: `DeathfeedPublisher` (`GuildSetup.DeathChannelID`,
non-PvP deaths/suicides) and the server-status panel
(`GuildSetup.ServerStatusChannelID`) have no route key today.

Migrating another publisher is: give it the same `(guildRowID, serverID)`,
call `routing.Resolver.Resolve` with its `Route*` key, and fall back to its
legacy field - `KillfeedPublisher.RouteChannelID` is the reference
implementation. Publishers that are interaction-driven panels
(`LINK_GAMERTAG`, `STATS_LEADERBOARDS`) are guild-level today and need a
per-server decision before they can resolve per server.
