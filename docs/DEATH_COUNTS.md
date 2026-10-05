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

## PvP deaths and PvE deaths

Every deaths row is one or the other, by its `death_type` alone:

| `death_type` | Counts as | Why |
| --- | --- | --- |
| `PVP` | **PvP death** | killed by another player (a teammate included) |
| `UNKNOWN` | PvE death | a `died` line: bleeding out, starvation, a fall, drowning, and anything else the log gives no cause for |
| `SUICIDE` | PvE death | the suicide emote |
| anything else | PvE death | no other value is written today; a new one is PvE unless it is added to the rule |

The rule lives in one place, `internal/deathstats` (`IsPvP`, and the SQL fragments `PvPPredicate` /
`PvPCount` every query uses), with the K/D arithmetic beside it:

* **Deaths** is every death, as before. **PvP deaths + PvE deaths = deaths**, always: both are read
  from the same rows in one statement.
* **K/D (overall)** = kills / deaths. This is the figure that has always been called K/D, and every
  existing field, variable and board that says K/D still means it.
* **K/D (PvP)** = kills / PvP deaths.
* With no deaths in the divisor, either ratio is the kill count itself (the existing rule).
* There is no "PvE kills" figure and no PvE K/D: DayZ's console logs do not record infected or
  animal kills.

Where the split is shown: `/stats` and the stats panel (`K/D (PvP)`, `K/D (overall)`, and
`PvP X • PvE Y` under the deaths count); `/leaderboard type:pvpkd` (the `kd` board is unchanged);
the kill and death cards (one extra line, `PvP **262 D** • **4.90 K/D**`, only for a player who has
at least one PvE death - otherwise the two K/Ds are the same number); the Champion Card (a caption
under the deaths and K/D tiles); the Embed Designer variables `killer_pvp_deaths`,
`killer_pve_deaths`, `killer_pvp_kd` and the same three for `victim_`; and the JSON fields
`pvpDeaths`, `pveDeaths`, `pvpKd` (player stats, card), `pvpDeaths`, `pveDeaths` (staff player
directory) and `pvpDeaths`, `pveDeaths`, `pvpKdRatio` (faction stats and leaderboard rows).

Not split, on purpose: the classic `/faction` profile and war figures count deaths from the `kills`
table, so they were always PvP-only; the lives summary already has `deathsByPvp` / `deathsBySuicide`
/ `deathsByOther`; the all-time deaths board and the faction `DEATHS` and `KD` boards keep ranking by
every death.

Old data needs no migration: every row has a `death_type` (the column is `NOT NULL`) and migration
`0081` wrote the `PVP` row for every kill that predates it. One case is classified as PvE although a
player caused it: a kill from before `0081` whose victim already had a `died` row within five
seconds. `0081` treated that row as the same death and did not add a `PVP` row, so the death is
counted once, as `UNKNOWN`. DayZ does not log both lines for one death, so this should not occur;
this counts them:

```sql
SELECT COUNT(*) FROM kills k
WHERE k.victim_player_id IS NOT NULL AND k.killer_player_id IS DISTINCT FROM k.victim_player_id
  AND NOT EXISTS (SELECT 1 FROM deaths d WHERE d.guild_id = k.guild_id AND d.event_fingerprint = 'pvp:' || k.event_fingerprint);
```

The split reads `death_type` for each of a player's death rows, where the total alone could be
answered from an index. If the profile lookups on the kill path or the player directory get slow on
a large server, these let the split be answered from the index too. Create them by hand at a quiet
time (never in a migration: an in-transaction build blocks every kill and death write while it runs,
see `internal/database/interactive_reads_schema.go`); the second replaces `idx_deaths_server_player`
suggested there:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deaths_player_type ON deaths (guild_id, player_id, death_type);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deaths_server_player_type ON deaths (guild_id, server_id, player_id, death_type);
```

## Effect on what players see

Deaths go up and K/D goes down for everyone who has been killed by another player, back to their
first recorded kill. Kills are unchanged. The death heatmap now includes PvP deaths, placed where
the victim stood (the position logged on the kill line), not where the killer stood.

## Known gaps

- Deaths ADM logs as `killed by <non-player>` are still not parsed, so they are counted nowhere.
- A suicide emote writes a `SUICIDE` row when the emote is logged. Whether the death that follows
  also writes a `died` row - which would count that suicide twice - has not been checked against
  real logs.
- A self-kill line is stored as a kill and counts toward the player's kills. It writes no deaths
  row, so it is neither a PvP nor a PvE death.
- A death to an explosive, a trap or a vehicle is logged as `killed by <non-player>` even when a
  player set it, so it is not parsed and is counted nowhere - not as a PvP death either.
- A team kill is a kill by a player: the victim's death is a PvP death.
