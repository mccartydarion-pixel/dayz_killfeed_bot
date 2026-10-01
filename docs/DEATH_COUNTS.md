# Death counts

Every deaths figure in Champion - the Discord profile and K/D, the leaderboards, faction stats, the
player API, the death heatmap - is a count of rows in the `deaths` table. This is what puts a row there.

## One death, one log line

DayZ writes exactly one admin-log line when a player dies (`PluginAdminLog.PlayerKilled`, in the
game's own scripts):

| What killed the player | ADM line | Parsed as |
| --- | --- | --- |
| Another player's weapon, melee weapon or fists | `... killed by Player "<killer>" ... with <weapon> ...` | `PLAYER_KILL` |
| Nothing else (bleeding out, starvation, drowning) | `... died. Stats> ...` | `PLAYER_DEATH` |
| An infected, an animal, an explosive, anything else | `... killed by <type>` | not parsed |

The branches are exclusive: the victim of a PvP kill gets **no** `died` line. Until migration
`0081_pvp_death_rows` only `PLAYER_DEATH` (and the suicide emote) wrote a `deaths` row, so every
deaths figure left PvP deaths out and every K/D was too high.

## What writes a deaths row now

| Source | `death_type` | Written by |
| --- | --- | --- |
| a persisted kill, for its victim | `PVP` | `KillRepository.InsertKillReturning`, in the same statement as the kill |
| a `died` line | `UNKNOWN` | the persistence worker (`InsertDeath`) |
| a `performed EmoteSuicide` line | `SUICIDE` | the persistence worker (`InsertDeath`) |

The `PVP` row carries the kill's time, season, server and ADM source, and its fingerprint is
`pvp:` plus the kill's fingerprint, so one kill can only ever produce one death row. Because it is
written with the kill, a replayed kill line (rejected by the kill's own unique key) writes nothing.
A self-kill writes no `PVP` row. The row is not published anywhere: the death feed still only
reacts to `died` and suicide lines.

Migration `0081` gives every kill that predates it the same row. It skips a kill whose victim
already has a death row within five seconds of it, so it is correct even for data where a `died`
line did accompany a kill.

## Effect on what players see

Deaths go up and K/D goes down for everyone who has been killed by another player, back to their
first recorded kill. Kills are unchanged. The death heatmap now includes PvP deaths, placed where
the victim stood (the position logged on the kill line), not where the killer stood.

## Known gaps

- Deaths ADM logs as `killed by <non-player>` are still not parsed, so they are counted nowhere.
- A suicide emote writes a `SUICIDE` row when the emote is logged. Whether the death that follows
  also writes a `died` row - which would count that suicide twice - has not been checked against
  real logs.
- A self-kill line is stored as a kill and counts toward the player's kills.
