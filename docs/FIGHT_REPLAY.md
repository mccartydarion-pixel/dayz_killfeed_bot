# Fight replay

Kills grouped into fights, and for one fight the recorded positions of everyone in it, as a
timeline the website can animate on the map.

No new tables and no new collection: a replay is built from `kills` and `player_location_events`,
which already hold a position for both sides of every hit and kill line plus the five-minute
player-list samples. It does **not** need the C.A.S.E. evidence collector. Code: `internal/fights`
(grouping), `internal/repository/fight_repository.go`, `internal/app/fight_replay.go`.

## What a fight is

Kills on one server, in time order. A kill joins an open fight when the fight's last kill was at
most three minutes earlier **and** the kill is connected to it: it shares a killer or victim with
one of the fight's kills, or it happened within 800 m of one. Otherwise it starts a new fight.
A fight's id is its first kill's id.

Grouping is pure and deterministic. Self-kills are not PvP and are never part of a fight.

## What a replay contains

`GET .../fights/{killId}` returns the fight containing that kill:

```json
{
  "id": 501, "startedAt": "…", "endedAt": "…", "kills": 3, "participants": ["Ace", "Bob", "Cat", "Dan"],
  "centerX": 5113.3, "centerZ": 5066.7,
  "replayStart": "…", "durationSeconds": 410,
  "players": [{"playerId": 7, "name": "Ace", "kills": 1, "deaths": 1}],
  "killEvents": [{"killId": 501, "t": 300, "killerId": 7, "victimId": 8, "weapon": "SVAL", "distanceMeters": 42.5,
                  "headshot": true, "killerX": 5000, "killerZ": 5000, "victimX": 5040, "victimZ": 5000}],
  "tracks": [{"playerId": 7, "points": [{"t": 60, "x": 4000, "z": 4000, "type": "OTHER_ADM"}]}],
  "bounds": {"minX": 4000, "minZ": 4000, "maxX": 5300, "maxZ": 5200},
  "truncated": false, "mapKey": "chernarusplus"
}
```

`t` is seconds from `replayStart`. Coordinates are map metres (east, north). `type` is the ADM
event the sample came from (`HIT`, `KILL`, `OTHER_ADM` for a player-list sample, ...).
`mapKey` is the server's configured map (the shop delivery map), or `""` when none is set. Fights
don't record their own map, so this is the current setting, and the website draws the replay on
that map's satellite tiles.

These are **samples, not a path**: a point exists only where ADM logged the player. Draw them as
points, or as straight segments clearly presented as such; there is nothing between two samples.
Only participants appear - a bystander's position is never in a replay.

A replay holds at most 5,000 samples (`truncated` says when later ones were left out). A fight is
looked up within 30 minutes either side of the requested kill, so a single chain of kills longer
than that is cut at the window.

## Who can watch

Positions are sensitive in DayZ: a track can lead back to a base.

| Viewer | Routes | Lead-in before the first kill | Which fights |
| --- | --- | --- | --- |
| Staff with `PLAYER_LOCATION_VIEW` (Administrator) | `GET .../admin/fights?hours=1..168&minKills=1..50`, `GET .../admin/fights/{killId}` | 5 minutes | any; each replay view is audited (`FIGHT_REPLAY_VIEWED`) |
| Verified players of the server | `GET /api/saas/player/servers/{installationId}/fights`, `.../fights/{killId}` | 2 minutes | only if the installation opted in, and only fights that ended at least `delayMinutes` ago |

`PUT .../admin/features/fight-replay` (`FEATURE_SETTINGS_MANAGE`): `{"public": false, "delayMinutes": 60}`
(delay 0-10080). Off by default.

For players the list returns `{"enabled": false, "items": []}` until the installation opts in, and
a replay that is not public yet answers 404 - indistinguishable from a fight that does not exist.
The player list covers the 72 hours before the delay cut-off and only fights of two or more kills.

A fight whose last kill is within three minutes of the cut-off is not listed yet: it may still be
going on.

## Linking from Discord

Nothing posts replay links yet. Every kill card's kill has an id, and `fights/{killId}` resolves
any kill of a fight to the whole fight, so the website can link a kill straight to its replay.

## Plan

Fight replay is Champion-only (entitlement `fight_replay`). With `CHAMPION_PLAN_GATING_ENABLED` off (the default) nothing here applies. With it on, only the
Survivor plan is restricted; Champion, the trial, retired plans and organizations without a
subscription keep the feature (`internal/entitlements`).

On Survivor:

- `GET .../admin/fights` and `GET .../admin/fights/{killId}` answer `403 PLAN_FEATURE_REQUIRED`,
  checked after the capability.
- `PUT .../admin/features/fight-replay` with `public: true` answers the same. Saving with
  `public: false` stays open.
- The player routes behave exactly as if replays were not public: the list is
  `{"enabled": false, "items": []}` and a replay is a 404. Players are never shown a plan error.
  The saved setting is kept and applies again after an upgrade.
