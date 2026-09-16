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
`playersOnline` | `Engine.PresenceSnapshot().OnlineCount` | live tracker's current online count
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
