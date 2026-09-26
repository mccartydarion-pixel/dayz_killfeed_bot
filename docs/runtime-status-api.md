# Runtime status API

`GET /api/runtime/status` exposes live, in-memory runtime state for the
website's server-side (Next.js) code. It is additive to the website's
existing direct PostgreSQL reads - it does not replace them, and it never
polls Nitrado from the request path.

## Auth

```
Authorization: Bearer <WEBSITE_API_SECRET>
```

Missing or wrong token returns `401`. Set `WEBSITE_API_SECRET` in both the
bot's Railway service and the website's environment
(`CHAMPION_RUNTIME_API_URL`, `CHAMPION_RUNTIME_API_SECRET`). Leaving
`WEBSITE_API_SECRET` unset on the bot disables the endpoint entirely (every
request gets `401`).

## Request

```
GET /api/runtime/status?discord_guild_id=<snowflake>&server_id=<optional>
```

- `discord_guild_id` - optional; defaults to the Discord guild this process
  is configured for (`DISCORD_GUILD_ID`). Each Champion process/Railway
  service owns exactly one guild, so any other guild ID returns `404`.
- `server_id` - optional; overrides the guild's currently selected public
  server (`SelectedPublicServerID`). Omit it to get that selected server.

## Response

```json
{
  "ok": true,
  "guildId": "111222333444",
  "serverId": 456,
  "serverName": "Champions",
  "playersOnline": 3,
  "trackerCount": 3,
  "workerRunning": true,
  "logSource": "DayZServer_PS4_x64_2026-09-16_08-00-00.ADM",
  "sourceStatus": "ACTIVE",
  "pipelineClassification": "HEALTHY",
  "lastLogReadAt": "2026-09-16T08:00:00Z",
  "lastParsedEventAt": "2026-09-16T08:00:01Z",
  "lastPersistedEventAt": "2026-09-16T08:00:01Z"
}
```

Field | Source | Notes
--- | --- | ---
`playersOnline` | online counter loop reading (`online_counter.go`) | the same authoritative count the voice counter shows (Nitrado live query, else complete ADM evidence); `null` while unknown (never a false `0`)
`trackerCount` | `Engine.PresenceSnapshot().TrackedEntries` | total entries the tracker currently holds (may exceed `playersOnline`)
`workerRunning` | `RuntimeDiagnosticSnapshot.WorkerRunning` | `false` when no worker/engine is registered for the server
`logSource` | `RuntimeDiagnosticSnapshot.SelectedADM` | `null` if no ADM log has been selected yet
`sourceStatus` | derived | `"NO_SOURCE_SELECTED"`, `"ACTIVE"` (selected and worker running), or `"STOPPED"`
`pipelineClassification` | `RuntimeDiagnosticSnapshot.Classification()` | same classification the `/admin` diagnostics command reports (e.g. `HEALTHY`, `LIVE_SOURCE_STALE`, `PARSER_FAILURE`)
`lastLogReadAt` | `RuntimeDiagnosticSnapshot.LastDownloadSuccess` | last successful ADM download
`lastParsedEventAt` | `RuntimeDiagnosticSnapshot.LastParsedEventAt` |
`lastPersistedEventAt` | `RuntimeDiagnosticSnapshot.LastPersistenceAt` | only set when the last persistence attempt succeeded

Any field above is `null` when the underlying value has never been observed -
never a fabricated timestamp or zero value standing in for "unknown".

### `health` (per-installation diagnostics)

Every per-server response carries a `health` object. Its `state` is derived
only from evidence the running worker holds - a successful deployment alone
never makes it `HEALTHY`.

State | Meaning
--- | ---
`HEALTHY` | server bound, worker reading a selected ADM, presence known, no delivery fault
`DEGRADED` | working, but with a listed fault (presence unknown, counter/route config fault, failing deliveries, stuck persistence queue, stale/wrong ADM source)
`UNAVAILABLE` | server not bound/active, worker stopped, or no ADM source selected
`UNKNOWN` | no live evidence yet (no worker registered, or no source check completed)

Field | Notes
--- | ---
`state`, `reasons` | the verdict and every reason behind it
`installationBinding` | `BOUND`, `INACTIVE`, `NOT_FOUND`
`selectedAdm`, `pipelineStatus` | selected ADM and its classification (`SOURCE_QUIET` = no new bytes yet, `WRONG_OR_INACTIVE_ADM_SOURCE` only after `staleGiveUpAfter`)
`lastSourceReadAt` / `lastSourceGrowthAt` | last successful ADM read / last read that consumed new bytes
`lastPersistedEventAt`, `lastDiscordDeliveryAt` | last durable event write / last successful Discord delivery on any route
`playersOnline`, `playersSource` | the counter's authoritative reading and its source (`NITRADO_QUERY`, `NITRADO_SERVER_STOPPED`, `ADM_PLAYER_LIST`, `ADM_BOOT_RESET`, `UNKNOWN`); `null` while unknown. Policy: docs/ONLINE_COUNTER_AND_LINK_CHECK.md
`presenceState`, `presenceEvidenceAt` | ADM evidence state: `UNKNOWN`, `SNAPSHOT_CONFIRMED` or `BOOT_RESET`
`presenceDisagreementSince` | set while Nitrado and a proven ADM player list disagree (Nitrado is shown); DEGRADED after 11 minutes
`verifiedRoles` | VERIFIED links by Verified-role delivery state (`PENDING`, `ASSIGNED`, `FAILED`, `MEMBER_GONE`; `""` = verified before tracking); any `FAILED` is DEGRADED
`pendingEvents`, `oldestPendingAgeSeconds`, `droppedEvents` | persistence queue backlog
`failedDeliveries`, `deliveries[]` | per-route Discord delivery ledger (feeds that queue cards also report `avg_queue_wait_ms`, `max_queue_wait_ms`, `last_detect_to_deliver_ms`) (`KILLFEED`, `DEATH_FEED`, `HITFEED`, `CONNECTIONS`, `PVE_FEED`, `BUILD_FEED`, `BOUNTY_TRACKING`, `ECONOMY_FEED`, `VERIFIED_ROLE`, `VERIFICATION_DM`) with last success/failure and error class
`onlineCounter` | channel, `state` (`OK`, `PENDING`, `RETRYING`, `RATE_LIMITED`, `CONFIG_FAULT`, `UNBOUND`), fault class/channel, last success

A `server_id` belonging to a different guild returns `404 unknown_server`.

### No server selected

If the guild has no single selected public server and no `server_id` was
given, the response omits the per-server fields and instead returns:

```json
{ "ok": true, "guildId": "111222333444", "servers": [
  { "serverId": 456, "serverName": "Champions", "active": true }
] }
```

## Status codes

Code | Meaning
--- | ---
`200` | success (including the "no server selected" shape above)
`401` | missing/invalid bearer token, or `WEBSITE_API_SECRET` is unset
`404` | unknown/unowned `discord_guild_id`, or no guild row for it yet
`500` | database unavailable, or an unexpected internal error

## CORS

None enabled. This endpoint is meant to be called server-side (Next.js API
route / server component), not directly from the browser.
