# Server status for server owners

"Is my killfeed working?" - answered for one installation, in plain facts, without asking
support.

Code: `internal/app/saas_api_server_status.go` (route and response),
`internal/app/feed_watch.go` (what is timed in memory), `internal/ownerops/feedwatch.go` (the
decisions: state and reason, position logging, the silent feed).

**The JSON shape on this page is a contract.** The website is built against it. Fields may be
added; an existing field is never renamed, retyped or removed, and an existing enum value never
changes meaning. `TestServerStatusContractShape` pins the field names.

## Route

```
GET /api/saas/organizations/{organizationId}/installations/{installationId}/admin/server-status
```

Read-only. It never calls Nitrado or Discord: every value comes from the running bot's memory or
from rows Champion already keeps. It is safe to poll; do not poll faster than every 15 seconds
(the stored facts are cached for 20 seconds and the worker state is sampled every 30).

### Who may call it

The same chain as every other installation admin route ([CLIENT_ADMIN.md](CLIENT_ADMIN.md)):

1. `Authorization: Bearer <WEBSITE_API_SECRET>` - the website's service secret.
2. `X-Champion-Acting-User: <discord user id>` - the signed-in user, already synced.
3. The installation must belong to the organization in the path.
4. The user must hold the capability **`SERVER_STATUS_VIEW`**, which needs level
   **Administrator** or **Owner** on that installation. The organization's owner is always Owner;
   anybody else needs a Discord role mapped to Administrator or Owner. A Moderator or Gatekeeper
   is refused.

`SERVER_STATUS_VIEW` is listed by `GET .../admin/me` for users who hold it, so the website can
decide whether to show the page.

| Status | `error.code` | When |
| --- | --- | --- |
| 200 | | the status |
| 401 | `UNAUTHORIZED` | missing or wrong service secret, or no acting user |
| 403 | `ADMIN_FORBIDDEN` | the user has no level on this installation, or a level below Administrator |
| 404 | `NOT_FOUND` | no such installation in this organization (also for another organization's installation) |
| 429 | `RATE_LIMITED` | more than 120 admin reads a minute from one caller |
| 503 | `ADMIN_DISCORD_UNAVAILABLE` | the user's Discord roles could not be checked |
| 500 | `INTERNAL_ERROR` | the stored facts could not be read |

Errors use the usual envelope: `{"error": {"code": "...", "message": "..."}}`.

### What it never returns

No Nitrado token or credential, no Nitrado service id, no log file name or path, no Discord
token, no raw error text from Nitrado or Discord (only an error class), no player names or ids,
and nothing about any other installation or organization.

## Response

```json
{
  "installationId": 12,
  "serverId": 4,
  "serverName": "Chernarus PvP",
  "checkedAt": "2026-10-05T12:00:00Z",
  "feed": {
    "state": "RUNNING",
    "reason": "The server log is being read and events are flowing."
  },
  "lastLogCheckAt": "2026-10-05T11:59:40Z",
  "lastLogLineAt": "2026-10-05T11:59:00Z",
  "lastKillAt": "2026-10-05T09:00:00Z",
  "lastKillPostedAt": "2026-10-05T09:00:03Z",
  "players": { "online": 7, "source": "NITRADO_QUERY" },
  "nitrado": {
    "reachable": true,
    "lastSuccessAt": "2026-10-05T11:59:40Z",
    "lastFailureAt": null,
    "errorClass": null
  },
  "discord": {
    "reachable": true,
    "botInGuild": true,
    "problems": []
  },
  "positionLogging": {
    "state": "OK",
    "lastPositionAt": "2026-10-05T11:57:00Z",
    "playersOnlineMinutes": 40,
    "advice": null
  },
  "mapRotation": {
    "currentMap": "Chernarus",
    "nextMap": "Livonia",
    "switchAt": null,
    "voteOpen": false,
    "stopped": false,
    "stoppedReason": null
  }
}
```

Every timestamp is RFC 3339 in UTC. **`null` means "not known", never zero**: a field Champion
cannot vouch for is `null`, not `0`, `false` or an old value. Every key is always present.

### Top level

| Field | Type | Meaning |
| --- | --- | --- |
| `installationId` | number | the installation asked about |
| `serverId` | number or null | Champion's id of the installation's DayZ server; `null` when none is connected |
| `serverName` | string or null | the server's display name |
| `checkedAt` | string | when this answer was put together |
| `feed` | object | the verdict, below |
| `lastLogCheckAt` | string or null | when Champion last finished a check of the server log (it checks every few seconds, whether or not anything is new). `null` since the bot last restarted until the first check |
| `lastLogLineAt` | string or null | when Champion last read a **new line** from the server log. `null` when it has read none since the bot last restarted |
| `lastKillAt` | string or null | when the newest kill on this server was recorded by Champion. Stored, so it survives restarts. `null` when the server has no kill yet |
| `lastKillPostedAt` | string or null | when a killfeed message was last delivered to Discord. Kept in memory: `null` after a bot restart until the next kill is posted. In the default (rotating) delivery mode kills are posted in batches, so this can trail `lastKillAt` by up to the batch interval |
| `players` | object | below |
| `nitrado` | object | below |
| `discord` | object | below |
| `positionLogging` | object | below |
| `mapRotation` | object or null | below; `null` unless map rotation is available to the installation and the owner switched it on |

### `feed`

| `state` | Meaning |
| --- | --- |
| `RUNNING` | The log is being read and events are flowing. |
| `QUIET` | The log is being read; nothing has happened on the server in the last few minutes. Healthy. |
| `DEGRADED` | Working in part, or expected to recover by itself: Nitrado is not answering, Discord refuses a channel, the log is not advancing while players are online, or the bot restarted a moment ago. |
| `DOWN` | The customer is not getting a feed: suspended, setup unfinished, no server, bot removed from Discord, Nitrado refusing the token, or nothing is reading the log. |

`reason` is always one plain-English sentence that can be shown to the owner as it is. The
sentences can be reworded in a later release; show the text, do not match on it. Match on `state`.

Every reason, in the order they are decided (the first that applies wins):

| State | Reason |
| --- | --- |
| DOWN | This installation is suspended, so its feed is switched off. |
| DOWN | No DayZ server is connected to this installation yet. |
| DOWN | Setup is not finished yet, so the feed has not started. |
| DOWN | The DayZ server is disconnected from Champion. |
| DOWN | The Champion bot is not in your Discord server, so nothing can be posted. |
| DEGRADED | Champion has just restarted and is reconnecting to your server; this takes a minute or two. |
| DOWN | Nothing is reading your server's log right now; Champion will try to restart it. |
| DOWN | Nitrado is refusing the saved access token; reconnect Nitrado to bring the feed back. |
| DOWN | The log reader has stopped making progress; Champion will try to restart it. |
| DEGRADED | Nitrado is not answering Champion's requests for the server log; the feed continues as soon as it does. |
| DEGRADED | The server log is being read, but Discord refuses the killfeed channel: it was deleted or the bot lost permission to post there. |
| DEGRADED | Players are online but the server log has produced nothing for a long time. |
| DEGRADED | Players are online but the server log is not advancing, or a newer log file has not been picked up yet. |
| DEGRADED | The server log is being read, but the last killfeed message could not be posted to Discord; it is being retried. |
| QUIET | The server log is being read; nothing has happened on the server in the last few minutes. |
| RUNNING | The server log is being read and events are flowing. |

During the first three minutes after the bot restarts (every deploy), a server whose reader has
not started or not finished its first check is `DEGRADED` "just restarted", not `DOWN`.

### `players`

| Field | Type | Meaning |
| --- | --- | --- |
| `online` | number or null | players on the server now; `null` while Champion does not know. Never a guess |
| `source` | string | where the number comes from: `NITRADO_QUERY` (Nitrado's live player count), `NITRADO_SERVER_STOPPED` (Nitrado says the server is stopped, so 0), `ADM_PLAYER_LIST` (the server log's own player list), `ADM_BOOT_RESET` (the server just restarted, so 0 until somebody connects), `UNKNOWN` |

The rules behind the number are in
[ONLINE_COUNTER_AND_LINK_CHECK.md](ONLINE_COUNTER_AND_LINK_CHECK.md).

### `nitrado`

| Field | Type | Meaning |
| --- | --- | --- |
| `reachable` | boolean or null | `true`: the last read of the server log worked. `false`: reads are failing right now, or the last recorded check of the connection failed. `null`: Champion has not tried yet |
| `lastSuccessAt` | string or null | the last successful read (or, with no log reader running, the last successful check of the Nitrado connection) |
| `lastFailureAt` | string or null | the last failed check of the Nitrado connection that was recorded |
| `errorClass` | string or null | set only while `reachable` is `false`: `authentication` or `permission` (the token is refused - the owner must reconnect Nitrado), or another short class name such as `temporary`, `not_found` or `unknown` (treat any other value as "temporary"). Never the raw error |

### `discord`

| Field | Type | Meaning |
| --- | --- | --- |
| `reachable` | boolean | the bot is connected to Discord right now |
| `botInGuild` | boolean | the bot is a member of the installation's Discord server (as last recorded) |
| `problems` | array | feeds whose messages Discord is refusing or failing right now; empty when there are none |

Each problem:

| Field | Type | Meaning |
| --- | --- | --- |
| `feed` | string | which feed: `KILLFEED`, `DEATH_FEED`, `HITFEED`, `CONNECTIONS`, `PVE_FEED`, `BUILD_FEED`, `BOUNTY_TRACKING`, `ECONOMY_FEED`, ... (treat an unknown name as "another feed") |
| `problem` | string | `CHANNEL_OR_PERMISSION`: the channel was deleted or the bot may not post there - fix it with setup repair or by restoring the bot's permissions. `FAILING`: posts are failing for another reason and are retried |
| `channelId` | string or null | the Discord channel concerned, when known |
| `since` | string or null | when the latest failure happened |

Only problems Champion has already run into are listed: a channel that is broken but has had
nothing posted to it since the bot restarted is not known to be broken.

### `positionLogging`

A server that does not write its player list to the log gives Champion no player positions, and
the live map stays empty although players are online.

| Field | Type | Meaning |
| --- | --- | --- |
| `state` | string | `OK`: a player list arrived in the last 12 minutes. `NOT_ARRIVING`: players have been online for at least 15 minutes and no player list arrived in that time. `UNKNOWN`: nobody has been online long enough to tell, or the log is not being read |
| `lastPositionAt` | string or null | when the last player list was read; `null` when none has been since the bot last restarted |
| `playersOnlineMinutes` | number | how many minutes at least one player has been online without a gap, as far as this bot process has seen (0 when nobody is online or the count is unknown) |
| `advice` | string or null | set only with `NOT_ARRIVING`: what the owner should change, ready to show |

The advice text:

> Players are online but your server is not logging their positions, so the live map stays empty.
> In your Nitrado web interface open Settings > General, switch on "Log player list"
> (adminLogPlayerList = 1 in the server configuration), save and restart the server. Positions
> then arrive every five minutes.

`NOT_ARRIVING` does not change `feed.state`: kills are still read and posted.

### `mapRotation`

Present only when map rotation is on for the installation ([MAP_ROTATION.md](MAP_ROTATION.md)).

| Field | Type | Meaning |
| --- | --- | --- |
| `currentMap` | string or null | the map the server runs now, when known |
| `nextMap` | string or null | the map it will switch to, when decided |
| `switchAt` | string or null | when the switch is expected: set only when the next restart is the one that switches and its time is known |
| `voteOpen` | boolean | a player vote is open right now |
| `stopped` | boolean | the rotation stopped itself after repeated failures and waits for the owner to save the settings again |
| `stoppedReason` | string or null | why, when `stopped` |

## Good to know

- Durations and "since the bot last restarted" values live in the bot's memory. After a deploy
  they start again; `lastKillAt`, `botInGuild` and the recorded Nitrado checks do not.
- If the installation's Discord server is not the one this bot process serves, nothing is known
  about a log reader for it: `feed.state` is then `DOWN` and the memory-backed fields are `null`.
- The same facts drive the platform owner's silent-feed incident
  ([OWNER_OPS.md](OWNER_OPS.md), "Silent feeds"): Champion's owner hears about a silent feed
  before the customer has to ask.
- This is separate from `health` in `GET /api/runtime/status`
  ([runtime-status-api.md](runtime-status-api.md)), which is the website's server-to-server
  diagnostic for the bot's own guild and carries far more detail than a customer should see.
