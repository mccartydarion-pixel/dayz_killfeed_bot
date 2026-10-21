# Server Ranked season lifecycle

Owner-only endpoints under the installation admin API (released with #175; the owner UI is the website's Server Admin → Server Controls → Server Ranked):

- `GET /ranked/server-season` returns the selected server's active season or `null`.

- `POST /ranked/server-season` opens the selected server's first local season.
- `POST /ranked/server-season/reset` archives the current local season and opens another. The request must include `"confirm": "RESET SERVER RANKED"`.
- `PATCH /ranked/server-season` changes the same-victim wait of the ACTIVE season without a reset (see "Changing the wait mid-season").

Both accept `rpPerKill` (positive integer) and `thresholds` (seven strictly increasing cumulative RP values for Rookie, Bronze, Silver, Gold, Platinum, Diamond, Master). They also accept `sameVictimCooldownMinutes`: a whole number from 0 to 120, the minutes before the same attacker earns RP from the same victim again. `0` means no wait (every eligible kill of the same victim counts). When the field is absent it is `5`, which was the fixed rule before migration `0129_ranked_same_victim_cooldown`; any other value is a 400. The season object returned by every endpoint carries `sameVictimCooldownMinutes` (the current value). The owner supplies actual values; no default point economy has been chosen. `rpPerKill` and `thresholds` are frozen per season: changing one means a reset. The wait is the one rule that can also change on the active season. A scheduled Ranked reset (season planner) copies all of them, the wait's current value included, to the new season. A reset leaves archived awards intact and changes only the installation's selected physical server. Starting a second active season fails. The selected server must be `active`, belong to the installation's guild and be PLAYSTATION or XBOX; `game_servers.status` is a display label (CONNECTED / ONLINE / OFFLINE) and does not gate a season. Rejections return `RANKED_SERVER_INELIGIBLE` (400) or, for a reset with nothing active, `RANKED_NO_ACTIVE_SEASON` (409). Season changes and award writes lock the season/server transactionally.

Example rules for a proposed season, **not a product default**:

```json
{"rpPerKill":100,"sameVictimCooldownMinutes":5,"thresholds":[100,300,600,1000,1500,2100,2800]}
```

The local kill persistence path now awards RP after a new kill row is saved. The award is idempotent and observes the same-victim wait of the kill's own season that was in force at the kill's event time (`ranked_season_cooldown_changes`, read in the award transaction; `ranked_seasons.same_victim_cooldown_minutes` is the current value). A per-server worker reconciles missing decisions at startup and every minute after transient errors without reposting killfeed messages. Starting a season does not retroactively award kills before its start time. The code is released; RP awards stay dormant until an owner explicitly starts a season with their own values. Global awards and Elite Top 250 still depend on verified platform player identity and cross-server event identity.

## Changing the wait mid-season

`PATCH /ranked/server-season` with `{"sameVictimCooldownMinutes": <whole number 0..120>}` changes the wait of the selected server's ACTIVE season and returns the season object. Same gate as start and reset: the `SERVER_STATS_RESET` capability (Owner), the Ranked seasons plan feature and the admin action rate limit. No confirmation phrase: nothing is archived or reset.

- The field is required here (no default): absent, `null`, out of range or not a whole number is a 400 with the same message as start/reset.
- A body that carries `rpPerKill` or `thresholds` is refused with a 400 and changes nothing; those stay frozen. Other unknown fields are ignored, as on every endpoint.
- With no active season the answer is `RANKED_NO_ACTIVE_SEASON` (409).
- Saving the value the season already has is a 200 and records nothing.

**What a change does to kills.** Every value a season's wait has had is kept in `ranked_season_cooldown_changes` (season, minutes, `effective_from`, `changed_by`; migration `0130_ranked_cooldown_changes`, which seeds each existing season with its current value from its start). The award transaction uses the row in force at the kill's own event time, so:

- kills that happen from the moment of the change onward use the new wait; a repeat that was inside the old wait but is outside the new one counts;
- a kill that happened before the change is judged by the old wait, even when it is first processed or reconciled after the change (the per-server reconcile worker only ever decides kills that have no decision yet);
- a decision already written to `ranked_awards` (AWARDED, COOLDOWN, OUT_OF_ORDER) is immutable: a change never removes, duplicates or re-evaluates it. Shortening the wait does not hand out RP for earlier kills that were on cooldown.

The change and award transactions lock the season row, so a kill is never judged half-way through a change. Each real change is also written to the admin audit log as `SERVER_RANKED_WAIT_CHANGE` with the value before and after.

## Stats season vs Ranked season

They are different things and are always labelled separately:

| | Stats season | Ranked (RP) season |
|---|---|---|
| Scope | Guild | One game server |
| Table | `seasons` | `ranked_seasons` / `ranked_awards` |
| Controls | `/season start`, `/season end` (Administrator / Manage Server) | Website Server Admin (Owner only), reset needs `RESET SERVER RANKED`; the wait can be changed without a reset |
| Effect | Labels kill/death history; season results | Awards RP per eligible enemy kill; tiers Unranked → Master |

`/season status` shows both: the stats season first, then each active server's Ranked status (not started / active since, RP per eligible kill, the same-player wait). The Server Ranks panel's inactive notice says the stats season is separate. Neither season resets the all-time Auto Leaderboard boards (kills, killstreaks, deaths, longest kills).
