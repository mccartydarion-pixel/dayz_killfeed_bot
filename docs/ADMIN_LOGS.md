# Admin Logs: consolidated staff operations channel

Channel System V2 routes three features into one private staff channel, `🛡️・admin-logs` under
`🔒 CHAMPION • STAFF` (hidden from `@everyone`): `ADMIN_LOGS`, `ADMIN_ALERTS` and `BUILD_FEED`.
A feature route is not a Discord channel. Each feature keeps its own publisher and its own,
visually distinct cards.

| Card | Route | Color | Source |
|---|---|---|---|
| `📥 ADM DOWNLOADED` / `🚨 ADM DOWNLOAD FAILED` / `🔄 ADM ROTATION` + the live ADM panel | `ADMIN_LOGS` | green / red / steel | `ADMMonitorPublisher` (`internal/discord/adm_monitor.go`) |
| `🚨 ADMIN ALERT` | `ADMIN_ALERTS` | amber (warning), red (critical) | `AdminAlertPublisher` (`internal/discord/admin_alerts.go`) |
| `✅ ALERT RESOLVED` | `ADMIN_ALERTS` | green | same, when the condition clears |
| `🏗️ BUILD ACTIVITY` | `BUILD_FEED` | gold | `BuildFeedPublisher` (`internal/discord/build_feed.go`) |

All staff cards carry the footer `CHAMPION • STAFF INTELLIGENCE` (the ADM monitor keeps
`CHAMPION • ADM MONITOR`). With no route configured, a publisher sends nothing: there is no
fallback channel.

## Admin alerts

`AdminAlertPublisher` raises an alert when a server **enters** a condition and a resolution when
it **leaves** it, never once per poll. Sends run on one goroutine behind a bounded queue (64); a
full queue drops the alert and logs it (first drop, then every 50th), so no source can stall.

| Alert | Severity | Trigger | Source |
|---|---|---|---|
| `ADM STALE` | WARNING | the ADM log has not changed for 5 minutes while players are online (same threshold as `operations.ADMMonitor`) | per-server `AdmSnapshot` |
| `NITRADO API FAILURE` | CRITICAL | 3 consecutive failed ADM downloads (each single failure is already an ADM monitor card) | per-server `DownloadReport` |
| `ZONE INTRUSION`, `UAV INTRUSION`, `BASE RADAR INTRUSION` | WARNING | the zone engine's non-suppressed event (`docs/ZONES_UAV_RADAR.md`) | intrusion engine |
| `ZONE BAN VIOLATION` | CRITICAL | same | intrusion engine |

An intrusion alert is skipped when `ADMIN_ALERTS` resolves to the zone's own alert channel, so it
is never posted twice.

**Not emitted, because no such system exists yet:** server offline, route permission failure,
autostart failure (only the `server_configs.autostart_*` columns exist - no monitor), file sync
failure. Billing payment failures are organization-level billing events, not server
operations, and are not sent to a guild's staff channel.

## Build feed

The DayZ server writes build lines to the ADM only when its config enables
`adminLogPlacement = 1` (placements) and/or `adminLogBuildActions = 1` (building/dismantling).
The parser (`internal/killfeed/parser.go` `parseBuildAction`) recognizes, directly after a
player's metadata block:

```
Player "<name>" (id=... pos=<x, y, z>) placed <item>[<Class>]
Player "<name>" (id=... pos=<x, y, z>) built <part> [on <structure>] [with <tool>]
Player "<name>" (id=... pos=<x, y, z>) dismantled <part> [from <structure>] [with <tool>]
```

The verb is case-insensitive. Anything else, including chat text containing "placed", is ignored.
**No real ADM log with these lines is in the repository**, so the patterns follow DayZ's ADM
conventions and are covered by unit tests only. Verify them against a real log from a server with
those flags enabled before relying on them.

Build actions are not persisted. They go through the normal dedupe (the fingerprint includes the
action, part, structure and tool), then to the per-server `BuildFeedPublisher`. It flushes every
5 s: up to 5 actions as individual cards in one message, a larger burst as one summary card (10
lines, `+N more actions not shown`). The queue is capped at 200 per flush window. A card shows
only the fields the line actually carried. `Location` comes from the first two `pos` values (the
map plane; the third is altitude).

**Health.** One-click setup reports `BUILD_FEED` as `ACTIVE` once any server in the process has
parsed a build line. Until then it is `BLOCKED` / `SOURCE_BLOCKED` with the config hint above. The
route is still mapped to admin-logs so it works as soon as the server starts logging. It never
justifies creating the channel on its own: admin-logs exists because `ADMIN_LOGS` and
`ADMIN_ALERTS` have real producers.
