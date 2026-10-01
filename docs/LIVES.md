# Lives

A **life** is the stretch between two deaths of one player on one server. Lives turn the kill and
death logs into something players compare: how long they survived, what they did with the life, how
it ended.

Migrations `0054_player_lives_and_daily_activity` (table `player_lives`) and
`0055_player_recap_prefs`. Code: `internal/repository/life_repository.go`,
`internal/app/lives.go`, `internal/discord/lives.go`.

## When a life ends

DayZ's admin log writes exactly one line per death:

| ADM line | Persisted as | Ends a life |
| --- | --- | --- |
| `killed by Player ...` | a `kills` row | yes, for the victim (cause `PVP`) |
| `died. Stats> ...` | a `deaths` row | yes (cause `SUICIDE` if a suicide emote was logged in the previous 60 seconds of the same life, else `OTHER`) |
| `performed EmoteSuicide` | a `deaths` row (type `SUICIDE`) | no - the `died` line that follows is the death |
| `killed by <infected, animal, ...>` | not parsed | no |

The last row is a real gap: a player killed by an infected keeps their life here until their next
recorded death. It closes when the parser learns those lines.

A second death for the same player within 10 seconds of the previous one is ignored (a replayed
line, or a `died` line trailing a `killed by` line).

The life is closed inside the persistence worker, right after the kill or death row is durable
(`persistenceStoreAdapter.recordLifeEnd` / `recordLifeEndFromKill`). It is best-effort and bounded
to four seconds: a failure is logged and never fails or delays the kill pipeline.

## What a life records

| Column | Meaning |
| --- | --- |
| `started_at` | the previous life's end; for a player's first life, when they were first seen |
| `ended_at` | the death's ADM time |
| `playtime_seconds` | observed playtime inside the life. `NULL` when the life began before `player_lives` existed |
| `playtime_mark` | `player_server_activity.total_observed_seconds` at the death - the next life's baseline |
| `kills`, `headshots`, `longest_kill_m` | the player's kills inside the life (self-kills excluded) |
| `tracked_distance_m`, `location_samples` | straight-line distance between the life's consecutive location samples |
| `cause`, `killer_player_id`, `kill_id`, `weapon`, `distance_m` | how it ended |

Three things to know when presenting these:

- **Playtime is observed playtime.** It is the difference between two marks of the running total,
  so it inherits that total's behaviour: a session left open across a bot restart stops accruing
  until the player reconnects.
- **Tracked distance is a lower bound.** Positions are logged every five minutes and on events;
  the straight lines between them are shorter than the path walked. Location history older than
  the retention window (30 days by default) is gone, so a very long life is measured over what remains.
- **Nothing is backfilled.** A player's first life after this ships has an unknown playtime unless
  they had never died on the server before.

A life in progress is derived, never stored: it began at the last recorded life's end and its
playtime is the running total minus the mark taken there.

## Discord

`/life me` - your life in progress, your record and your last five lives (ephemeral).
`/life top [board]` - `alive` (longest lives in progress, players seen in the last 14 days),
`longest`, `kills`, `distance` (ephemeral).
`/life recap on|off` - the death recap DM.

The recap is **opt-in per player** (`player_recap_prefs`): the bot DMs nobody who has not turned it
on, and only the holder of the player's VERIFIED link. Recaps go through a bounded queue on their
own goroutine; a full queue or closed DMs drop the recap, never the life.

`/life` and `/card` report on the guild's selected public server, or its only connected server.

## API

Both routes use the player API's authorization (docs/PLAYER_API.md): service secret, acting user,
a VERIFIED link in the installation's guild, observed activity on the installation's server.

`GET /api/saas/player/servers/{installationId}/lives?limit=1..50`

```json
{
  "installationId": 12,
  "current": {"playerName": "Ace", "startedAt": "...", "playtimeSeconds": 600, "kills": 0, "online": false},
  "summary": {"lives": 1, "longestPlaytimeSeconds": 1200, "averagePlaytimeSeconds": 1200, "mostKills": 1,
              "trackedDistanceMeters": 0, "deathsByPvp": 1, "deathsBySuicide": 0, "deathsByOther": 0},
  "recent": [{"id": 9, "playerName": "Ace", "startedAt": "...", "endedAt": "...", "playtimeSeconds": 1200,
              "kills": 1, "headshots": 0, "longestKillMeters": 210, "trackedDistanceMeters": null,
              "cause": "PVP", "killerName": "Rival", "weapon": "Mosin", "distanceMeters": 35}]
}
```

`current` is null for a player never observed on the server. `playtimeSeconds` and
`trackedDistanceMeters` are null when not measured - show a dash, not a zero.

`GET /api/saas/player/servers/{installationId}/lives/leaderboard?board=ALIVE|LONGEST|KILLS|DISTANCE&days=1..365`

`ALIVE` fills `alive` (lives in progress); the others fill `lives` (ended lives). `days` limits the
ended-life boards to lives that ended in the window.
