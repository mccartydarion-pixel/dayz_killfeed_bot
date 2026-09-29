# Server Ranked season lifecycle

This draft adds owner-only endpoints under the installation admin API:

- `POST /ranked/server-season` opens the selected server's first local season.
- `POST /ranked/server-season/reset` archives the current local season and opens another. The request must include `"confirm": "RESET SERVER RANKED"`.

Both accept `rpPerKill` (positive integer) and `thresholds` (seven strictly increasing cumulative RP values for Rookie, Bronze, Silver, Gold, Platinum, Diamond, Master). The owner supplies actual values; no default point economy has been chosen. The rules are frozen per season. A reset leaves archived awards intact and changes only the installation's selected physical server. Starting a second active season fails. Season changes and award writes lock the season/server transactionally.

Example rules for a proposed season, **not a product default**:

```json
{"rpPerKill":100,"thresholds":[100,300,600,1000,1500,2100,2800]}
```

The local kill persistence path now awards RP after a new kill row is saved. The award is idempotent and observes the five-minute same-victim cooldown. A per-server worker reconciles missing decisions at startup and every minute after transient errors without reposting killfeed messages. Starting a season does not retroactively award kills before its start time. This stack remains draft and should not be merged or exposed to owners in production until season rules and rollout are approved. Global awards and Elite Top 250 still depend on verified platform player identity and cross-server event identity.