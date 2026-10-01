# C.A.S.E. Suspicious Logins — shadow review runbook

Goal: validate the Suspicious Logins detector on real traffic before it may alert anyone. The shadow run observes only. It cannot send Discord messages or take any enforcement action, and nothing in this runbook changes that.

## 1. Prerequisites and current state

The shadow run (`loginShadow` in the protected `/anti-cheat/sessions` read, backend #179) needs three things for a game server:

| Prerequisite | How it is met | Production state (checked 2026-09-30 from Railway logs) |
| --- | --- | --- |
| Evidence collection on | `CASE_EVIDENCE_ENABLED=true` and the internal `game_servers.id` listed in `CASE_EVIDENCE_SERVER_IDS` (comma-separated). Startup logs `component=case event=evidence_collector_enabled server_id=<id>` | **On for server `1`** |
| Learned UTC offset | Learned automatically from a `restart.log` line that states its offset. Logs `component=livesync event=server_clock_learned server_id=<id> utc_offset_minutes=<n>` | **Server `1`: `-240` (UTC-4), `restart.log` FRESH** |
| Current ADM feed | The running ADM worker's selected source is healthy or quiet and polled within 2 minutes | **Often `STALE`** for server `1` (`known_stale_no_new_evidence`). While stale, the shadow run reports `SUSPENDED` |

So server `1` needs no configuration change: the review can start there now. Expect `SUSPENDED` during quiet periods. That is the intended fail-closed behaviour, not a bug.

## 2. Adding another server (optional)

1. Find the server's internal ID: `game_servers.id`, not the Nitrado service ID.
2. In Railway (service `65e896ed…`, production), append it to `CASE_EVIDENCE_SERVER_IDS`, e.g. `1,7`. Leave `CASE_EVIDENCE_ENABLED=true`. **Do not enable `CASE_BUILD_EVIDENCE_ENABLED`** for this review; it is separate.
3. Railway redeploys the bot, which briefly restarts the killfeed service. Game servers are not touched.
4. Confirm `evidence_collector_enabled server_id=<id>` in the startup logs. Then wait for `server_clock_learned` for that server. It appears after the next `restart.log` line that states an offset, typically at the next restart.

## 3. Killfeed safety check (every day of the review)

The collector shares the ADM poller with the killfeed. **If an evidence database write fails, the checkpoint does not advance, which delays the killfeed** (docs/CASE_PHASE2B.md).

- Search the Railway logs for `C.A.S.E.` errors: `persist C.A.S.E. evidence`, `source offset collision`, `replay evidence mismatch`. None were found for 2026-09-29 to 30.
- Watch for killfeed delay reports from the server.
- **Rollback:** remove the server's ID from `CASE_EVIDENCE_SERVER_IDS` (or set `CASE_EVIDENCE_ENABLED=false`) and let Railway redeploy. Retained evidence stays; no other feature depends on the collector.

## 4. Running the review

1. Open **Dashboard → Anti-cheat → Sessions** for the server and pick a player. The **Suspicious Logins · shadow run** card (website PR for `loginShadow`) shows:
   - status: paused, not enough data, no observation, or observations;
   - trusted and untrusted event counts, with reasons;
   - restart windows found;
   - exclusions such as restarts and ordinary reconnects;
   - each observation, with links to its evidence.
2. For every observation, open the linked evidence and record in the review sheet:
   - the date, server and player ID;
   - the reconnect count and time span shown;
   - the likely explanation: network drop, console crash, server restart missed by the boot boundary, queue or priority slot, or unexplained;
   - the verdict: **legitimate** or **needs follow-up**.
3. Also sample players with **no** observation who staff know reconnect a lot. Missing them isn't a false positive, but it shows what the detector misses.
4. Note every untrusted-time reason that appears (for example `ROLLOVER_AMBIGUOUS` or `CLOCK_OUT_OF_ORDER`). Frequent ones mean the time basis needs work before release.

Suggested length: at least **14 days** and **30 observations** on server `1`, including at least 3 server restarts.

## 5. Exit criteria (to propose release)

All of these must hold before Suspicious Logins can move from `BLOCKED` to `VALIDATED_SHADOW`:

- **False positives:** below an owner-agreed rate, e.g. no more than 1 in 10 observations judged legitimate and noisy, measured with the default parameters (4 reconnects in 10 minutes, ≤ 90 s gaps, 10-minute restart grace).
- **Restarts:** no observation during a verified restart.
- **Time basis:** untrusted-time reasons are rare and understood.
- **Killfeed:** no delay attributable to the collector.
- **Thresholds:** numeric thresholds approved (Relaxed ≥ Balanced ≥ Strict ≥ minimum), to be entered as validated thresholds.
- **Approval:** separate owner approval for the release change. Staff alerts then go through the existing `casealert` → `caseoutbox` path, private to staff, still with no enforcement.

If the criteria are not met, adjust the parameters (`LoginReconnectGap`, `LoginBurstWindow`, `LoginMinReconnects`, `RestartGrace` in `caseintel.DefaultCore8Params`) and repeat the review.
