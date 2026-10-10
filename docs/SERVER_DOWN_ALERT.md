# Server-down alert and restart status

One alert when a game server has stopped running, edited in place when it is back, and the
server's restart times on the status board.

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

## A restart that does not come back

Both outages of 2026-10-10 began at a scheduled restart: the server shut down and was never
started again. So the wait is shorter when the logs stopped at a restart that was due.

- The usual time from one start to the next is learned from the start times in the engine report
  file names (the median gap of the last dozen; three starts are needed).
- A restart is "due" when the logs stopped within 5 minutes of the end of the current run.
- Then the alert goes out after 15 minutes of stillness instead of 20.

It cannot be much faster. Nitrado shows the files of a new start about 10 minutes after the
shutdown was seen (production, 2026-10-09: 8.6 to 10.2 minutes), so a healthy restart looks the
same as a failed one for that long.

## What is posted

One message per outage in each place, never one per check:

- **Game server looks down** (critical) is posted once, to the server's staff alerts channel (the
  `ADMIN_ALERTS` route) and to the organization owner by DM.
- When a file grows again, **those same messages are edited** to **Game server is back**, with
  when it stopped, when it came back and how long it was out. Nothing new is posted.

The message ids are kept in memory. If the bot restarted during the outage, or a message was
deleted, the "back" notice is posted as a new message instead. With no staff alerts channel only
the DM is sent; with the owner's DMs closed only the channel message (`owner_dm_failed`).

The alert state is kept in memory too, so a server that is still down when the bot restarts is
reported once more.

## On the status board

The server status message (the `SERVER_STATUS` route, one message edited in place) shows the game
server for each server:

- **Game server:** Online, Restarting (the logs have been still for 3 minutes at a due restart)
  or Not running since a time (while an alert is open).
- **Last restart:** when the current run started.
- **Next restart:** about when the next start is due, while online and the schedule is known.

The times are Discord timestamps, so each reader sees their own time zone and the message is
edited only when something changes: about three edits per restart, no new messages.

## Setting

`SERVER_DOWN_ALERT_MINUTES`: minutes of stillness before the alert. Default 20, never under 10.
`0` or `off` switches the alert off for every server.

The watch runs beside each server's live sync supervisor (internal/app/server_down_watch.go) and
checks once a minute. It needs live sync watchers to be on (`LIVE_SYNC_WATCHERS`).
