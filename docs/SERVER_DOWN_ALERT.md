# Server-down alert

One staff alert when a game server has stopped running, and one when it is back.

## Why log growth and not Nitrado's status

On 2026-10-10 the Champions server crashed while shutting down for a scheduled restart and was
not started again for more than nine hours. Nitrado's status answered `started` the whole time,
so the status cannot be trusted for this.

What did change is that the log files stopped. A running DayZ server writes to its engine report
(RPT) every minute or two even with nobody on it: production shows the RPT growing every 27 to
97 seconds on an empty server. Files that have stopped growing mean the game is not running.

## The rule

`ownerops.ServerDown` (internal/ownerops/serverdown.go). The server looks down when:

- Champion listed the server's files within the last 5 minutes and that listing worked, and
- none of its log files (RPT, script, crash, restart, ADM) has grown for 20 minutes, and
- the files have been still for less than 24 hours.

When the first condition fails Champion cannot see the server, so the answer is "unknown" and an
alert stays as it is. The 24-hour limit keeps a server that was switched off for good from
alerting after every bot restart.

20 minutes outlasts a restart (a few minutes) and Nitrado's five-minute steps in showing new
bytes (docs/NITRADO_POLLING.md).

## What is posted

To the server's staff alerts channel (the `ADMIN_ALERTS` route), kind `SERVER_DOWN`:

- **Game server looks down** (critical) once, when the rule first holds.
- **Game server is back** (resolved) once, when a file grows again, with how long it was quiet.

The organization owner gets the same two messages by DM, so an outage at night is seen without
watching a channel. The DM goes out whether or not a staff alerts channel is set; if the owner's
DMs are closed it is logged (`owner_dm_failed`) and skipped.

With no staff alerts channel only the DM is sent. The state is kept in memory, so a server that is
still down when the bot restarts is reported once more.

## Setting

`SERVER_DOWN_ALERT_MINUTES`: minutes of stillness before the alert. Default 20, never under 10.
`0` or `off` switches the alert off for every server.

The watch runs beside each server's live sync supervisor (internal/app/server_down_watch.go) and
checks once a minute. It needs live sync watchers to be on (`LIVE_SYNC_WATCHERS`).
