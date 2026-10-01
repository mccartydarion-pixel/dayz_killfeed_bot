# Retention

Who is playing, who came back and who stopped. Two parts: collection, which runs for every server
from the moment this ships, and a read-only dashboard API.

Migration `0054_player_lives_and_daily_activity`. Code: `internal/repository/activity_rollup.go`,
`internal/repository/retention_repository.go`, `internal/app/retention.go`.

## Collection

`player_server_activity` only ever held running totals, and location events are deleted after the
retention window, so history could not be rebuilt later. Two rollups now record it as it happens.

`player_daily_activity` - one row per player, server and **UTC day** the player was observed on,
with the seconds and sessions observed that day. Written by the same four calls that maintain the
running total (`Connect`, `Disconnect`, `Checkpoint`, `CheckpointConnected`): each runs a rollup
statement first that reads the same pre-update row and adds the same seconds.

`server_hourly_activity` - one row per server and hour, from the 30-second presence checkpoint:
the most players connected at once (`peak_players`), the seconds they accrued and the number of
samples. An hour with no row was never sampled; an hour with `peak_players = 0` was sampled and empty.

The rollups are separate statements and deliberately so: a rollup failure is logged
(`component=retention`) and ignored, and can never fail or roll back the presence write the killfeed
pipeline depends on.

### Presence backfill

On every server-worker start a background job adds presence-only days from data that already
exists: kills, deaths and retained location events. Those rows have `source = 'BACKFILL'`, zero
seconds and zero sessions; they answer "was this player here that day" and nothing else. It never
touches a day already recorded, so it is safe to repeat. A backfilled day the player is later
observed on becomes `OBSERVED`.

So on day one the dashboard already knows first-seen days for everyone who ever killed or died, and
about 30 days of presence for everyone else. Seconds, sessions and concurrency start from deploy.

### Limits

- Days are UTC. A session across midnight is split at the checkpoint that crosses it.
- One observation adds at most 300 seconds to a day (the same bound the running total uses).
- Presence is rebuilt from connect lines. After a bot restart a player who was already online is
  not counted as connected until their next connect line, so seconds and peaks under-read until then.

## Dashboard API

Capability `RETENTION_VIEW` (Administrator): the lapsed list names players.

`GET .../admin/retention?days=7..180&tz=<IANA zone>` (default 30 days, UTC)

```json
{
  "days": 30, "timeZone": "UTC",
  "summary": {"activeToday": 2, "active7d": 2, "active30d": 3, "new7d": 1, "new30d": 3, "trackedPlayers": 3,
              "stickiness": 0.67, "averageSessionSeconds": 1450, "observedHours30d": 4.03,
              "collectingSince": "2026-09-07"},
  "daily": [{"day": "2026-10-01", "active": 2, "new": 1, "returning": 1, "observedSeconds": 1200, "sessions": 2}],
  "cohorts": [{"weekStart": "2026-09-07", "size": 2, "retained": [2, 1, null, null]}],
  "peakHours": [{"weekday": 3, "hour": 20, "averagePeak": 4, "maxPeak": 5, "samples": 2}],
  "shop": [{"productId": 4, "productName": "Base Kit", "purchases": 1, "units": 1, "points": 2000, "buyers": 1}]
}
```

- `daily` has one entry per day in the window, including days nobody played.
- `new` is players whose first day on the server is that day; `returning` is the rest.
- `stickiness` is `activeToday / active30d`; null with nobody active.
- `cohorts` are the last eight Monday-based UTC weeks. `retained[i]` is how many of the cohort were
  seen in week `i+1` after their first week. A week still in progress or in the future is **null,
  not zero** - render it as empty, not as 0%.
- `peakHours` is by weekday (0 = Sunday) and hour **in the requested time zone**; only observed
  hours contribute.
- `shop` is Champion Points per product in the window, best earner first. Refunded, cancelled and
  failed purchases are excluded.

`GET .../admin/retention/lapsed?minDays=1..365&maxDays=1..365&limit=1..200` (defaults 7, 30, 50)

Players last seen between `minDays` and `maxDays` ago, most invested first (active days, then
playtime). `linked` says whether the player holds a VERIFIED Discord link, i.e. whether staff can
reach them. Each call is written to the admin audit log (`RETENTION_LAPSED_VIEWED`).

Not built: an automatic win-back ping. The list tells staff who to reach; messaging them stays a
human decision.
