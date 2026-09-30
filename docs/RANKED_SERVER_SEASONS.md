# Server Ranked season lifecycle

Owner-only endpoints under the installation admin API (released with #175; the owner UI is the website's Server Admin → Server Controls → Server Ranked):

- `GET /ranked/server-season` returns the selected server's active season or `null`.

- `POST /ranked/server-season` opens the selected server's first local season.
- `POST /ranked/server-season/reset` archives the current local season and opens another. The request must include `"confirm": "RESET SERVER RANKED"`.

Both accept `rpPerKill` (positive integer) and `thresholds` (seven strictly increasing cumulative RP values for Rookie, Bronze, Silver, Gold, Platinum, Diamond, Master). The owner supplies actual values; no default point economy has been chosen. The rules are frozen per season. A reset leaves archived awards intact and changes only the installation's selected physical server. Starting a second active season fails. The selected server must be `active`, belong to the installation's guild and be PLAYSTATION or XBOX; `game_servers.status` is a display label (CONNECTED / ONLINE / OFFLINE) and does not gate a season. Rejections return `RANKED_SERVER_INELIGIBLE` (400) or, for a reset with nothing active, `RANKED_NO_ACTIVE_SEASON` (409). Season changes and award writes lock the season/server transactionally.

Example rules for a proposed season, **not a product default**:

```json
{"rpPerKill":100,"thresholds":[100,300,600,1000,1500,2100,2800]}
```

The local kill persistence path now awards RP after a new kill row is saved. The award is idempotent and observes the five-minute same-victim cooldown. A per-server worker reconciles missing decisions at startup and every minute after transient errors without reposting killfeed messages. Starting a season does not retroactively award kills before its start time. The code is released; RP awards stay dormant until an owner explicitly starts a season with their own values. Global awards and Elite Top 250 still depend on verified platform player identity and cross-server event identity.

## Stats season vs Ranked season

They are different things and are always labelled separately:

| | Stats season | Ranked (RP) season |
|---|---|---|
| Scope | Guild | One game server |
| Table | `seasons` | `ranked_seasons` / `ranked_awards` |
| Controls | `/season start`, `/season end` (Administrator / Manage Server) | Website Server Admin (Owner only), reset needs `RESET SERVER RANKED` |
| Effect | Labels kill/death history; season results | Awards RP per eligible enemy kill; tiers Unranked → Master |

`/season status` shows both: the stats season first, then each active server's Ranked status (not started / active since, RP per eligible kill). The Server Ranks panel's inactive notice says the stats season is separate. Neither season resets the all-time Auto Leaderboard boards (kills, killstreaks, deaths, longest kills).
