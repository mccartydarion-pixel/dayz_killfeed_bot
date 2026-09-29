# Online counter, player presence and PSN link check

This is the consolidated behavior from PRs #106, #107 and #108. PR #108 is the release candidate. The investigation used code and tests only; no production reads or writes were made.

## 1. PSN account linking

### Server resolution

Linking evaluates **every active server of the guild**. That is the exact set of servers the ADM workers ingest activity for (`ServerRepository.ActiveServerIDs`, `WHERE guild_id=$1 AND active`).

`game_servers.status` is not a filter. It is a display label written in different vocabularies:

* `/server connect` writes `CONNECTED`.
* The SaaS dashboard writes Nitrado's power state, `ONLINE` or `OFFLINE`.

The old filter, `status IN ('connected','ready','active')`, rejected every dashboard-connected server. That is the production `NO_CONNECTED_SERVER` failure. Teardown (`Deactivate`) always clears `active`.

Each server's observed playtime is judged **on its own**, and the best single server counts. Playtime is **never summed across servers**: 3 minutes on one server plus 3 on another is rejected. A multi-server guild therefore links without choosing a public server, and no one can reach 5 minutes by combining servers. Tenant isolation is unchanged: every lookup is scoped by `guild_id`.

`ServerRepository.ConnectedServerID` is kept for single-server views such as admin diagnostics. It returns the selected public server while that server is active, otherwise the only active server. It wraps `ErrNoConnectedServer` or `ErrMultipleConnectedServers`.

### Outcomes

`/link` and the Link Username panel share one message table, `linkErrorMessage`.

| Outcome | Error | Player sees |
|---|---|---|
| Malformed name (mention, URL, wrong length) | `ErrInvalidUsername` | INVALID PLAYSTATION USERNAME |
| Name never seen | `ErrPlayerNotFound` | PLAYER NOT FOUND |
| Name known, no time on any active server | `ErrPlayerNotObserved` | NOT OBSERVED ON SERVER |
| Less than 5 minutes on the best single server | `*PlaytimeShortfallError` | MORE PLAYTIME REQUIRED ("3m 20s of the required 5m") |
| No active server | `ErrInstallationNotConfigured` (wraps `ErrNoConnectedServer`) | SERVER NOT CONNECTED |
| Database or activity read failed | `ErrActivityUnavailable` / `ErrLinkCheckUnavailable` | LINK CHECK UNAVAILABLE |
| Discord account already verified | `ErrAlreadyLinked` | ACCOUNT ALREADY LINKED |
| PSN account claimed by someone else | `ErrPlayerClaimed` | ALREADY LINKED |
| 5 minutes or more | none | PENDING VERIFICATION |

**LINK CHECK UNAVAILABLE now means only a real backend failure.** A missing server is a configuration state.

### Verification is unchanged

A pending link still completes only in one of two ways:

* the **disconnect-then-reconnect challenge**, observed in ADM within the expiry window;
* an admin's **`/admin verify-link`** manual approval.

The ownership rules are also unchanged: `UNIQUE(guild_id, player_id)` and `UNIQUE(guild_id, discord_user_id)`.

### Verified role delivery (migration 0056)

A VERIFIED link and an assigned Discord role are separate facts. Before this change, a failed role assignment was only logged. It was never retried, before or after a restart, and the link simply stayed VERIFIED without a role.

The migration adds nullable columns only (`role_sync_status`, `role_sync_attempts`, `role_sync_last_attempt_at`, `role_synced_at`, `role_sync_error`). No row is rewritten.

* The statement that makes a link VERIFIED (challenge or admin approval) also sets `role_sync_status = PENDING`.
* The link becomes `ASSIGNED` only after Discord confirms the role add.
* A failure sets `FAILED` and counts the attempt. Unknown Member (10007) sets `MEMBER_GONE`, which is terminal.
* With no Verified role configured, the link stays `PENDING` and no attempt is used up.
* `runRoleReconciler` runs 30 s after start and then every 2 minutes. It handles at most 10 links per run, at most 10 attempts per link, and at most one attempt per link every 5 minutes. It only reads `PENDING`/`FAILED` VERIFIED links, so it cannot affect the challenge. Discord's role add is idempotent, and `ASSIGNED` links are never selected again.
* Links verified **before** the migration keep `NULL` and are **not** reconciled automatically. Their role state was never tracked, and the migration must not change live Discord roles on deploy.
* `/api/runtime/status` → `health.verifiedRoles` counts links by state. Any `FAILED` link marks the installation DEGRADED.

## 2. Current player count: one authority

The voice counter, `/api/runtime/status` (`playersOnline`, `health.playersOnline`) and the runtime state all use the **online counter loop's reading** (`internal/app/online_counter.go`). Sources, in order:

1. **Nitrado live query**: `GET /services/:id/gameservers`, `query.player_current` and `player_max`, whitelist-decoded (credentials in the same body are never decoded). It is used only when:
   * the read **in this evaluation** succeeded (10 s timeout), so there is no cached value; and
   * the response's `service_id` **equals the requested service** (`GameserverLive.BelongsTo`). A response for another service, or one without `service_id`, is ignored.

   `started` with a `player_current` is the count. `stopped` or `suspended` is a known 0. `restarting`, or an empty query, is not a count.
2. **ADM tracker**, only with evidence that it is complete (`killfeed.PresenceEvidence`):
   * `ADM_PLAYER_LIST`: a **complete** ADM PlayerList snapshot reconciled the tracker within the last 11 minutes (DayZ writes one every 5 minutes when `adminLogPlayerList` is on).
   * `ADM_BOOT_RESET`: a verified newer boot (server restart) was read from its start and the worker checked its source within the last 2 minutes. Everyone since the restart has been seen connecting.
   * Connect and disconnect lines alone are **never** a live count. A worker that resumed from a checkpoint or the log tail only has a lower bound (`UNKNOWN`).
3. **Unknown**: the channel keeps its last value for 10 minutes (from the last known reading, or from process start), then shows `⚪・Online: ?/18`. An unknown count is **never** shown or reported as 0. The runtime API returns `null`.

**Disagreement.** When Nitrado and a proven ADM tracker both have a count and they differ:

* Nitrado is shown, because it is what the Nitrado panel and players see.
* Both values and the time the disagreement started are recorded (diagnostics `presence_disagreement_since`, `health.presenceDisagreementSince`), and a `presence_disagreement` warning is logged.
* A disagreement lasting longer than 11 minutes (two snapshot intervals) marks the installation DEGRADED.

**No double counting.** Each evaluation uses exactly one source for the public server. Servers are never summed, and the tracker is keyed by DayZ ID. A duplicate connect refreshes an entry; it never adds one.

**Restart and reconnect.** A verified newer boot:

1. clears the tracker;
2. clears the complete-snapshot proof;
3. sets `BOOT_RESET`;
4. fires `OnNewBoot`, which closes open `player_server_activity` sessions so phantom sessions stop earning link playtime (accrued time is kept).

The old file's tail is drained first. A same-boot reselection (another mount, a listing gap) or an unstamped file is not a restart. Reconnecting players are counted from their new connect line.

## 3. Discord voice counter

* **Channel precedence:**
  * The `ONLINE_COUNTER` route always wins.
  * The legacy `GuildSetup.OnlinePlayersChannelID` is used only when the route lookup **confirms no route exists**. A lookup error keeps the current binding.
  * `bindCounterChannel` is the only place that applies this rule.
* **Ownership validation:** on the first reconcile of a channel, the channel must belong to the configured guild and be a voice channel. Otherwise the counter records `CHANNEL_NOT_OWNED` or `WRONG_CHANNEL_TYPE` and never renames it.
* **Permanent errors:** 404 / Unknown Channel (10003) and 403 / Missing Access (50001) / Missing Permissions (50013) become a `CONFIG_FAULT`. All Discord calls for that channel stop; there are no retries. Only binding a **different** channel clears the fault (`/setup repair`, a route change). Re-binding the same ID does not.
* **Transient errors:** 5xx and network errors get at most 3 retries with exponential backoff (5 s, 10 s, 20 s). The next evaluation starts a new budget.
* **Rate limits:**
  * Discord allows 2 channel renames per channel per 10 minutes. The counter spaces renames at least **5 minutes** apart (`DefaultCounterMinRenameInterval`), coalescing to the latest value, so it never reaches that limit.
  * Renames use `WithRetryOnRatelimit(false)`, so a 429 returns at once and the retry is scheduled from Retry-After instead of sleeping.
* **ADM ingestion is never blocked:** the ADM callback only pokes the loop, which is non-blocking. Renames run on the loop's goroutine and the counter's timers.
* **Startup and restart recovery:** the loop evaluates once at start. The first publish to a channel reads its actual name (`Reconcile`), so an identical name is never renamed and a stale one is corrected. At startup, before workers register, the last value is held and the channel is never flashed to `?` or 0.
* **No duplicate voice channels:** setup and repair recognise the counter in every format (`🟢・Online: 2/18`, `⚪・Online: ?/18`, legacy `🟢・Online Players: N`) through `IsOnlineCounterName`.

## 4. Before deploy (read-only)

```sql
SELECT id, guild_id, provider_service_id, status, active, organization_id FROM game_servers WHERE provider_service_id = '19806451';
SELECT id, discord_guild_id, selected_public_server_id FROM guilds WHERE id = (SELECT guild_id FROM game_servers WHERE provider_service_id = '19806451');
```

Expected:

* exactly one active row, with status `ONLINE` or `OFFLINE` (the value the old filter rejected);
* `selected_public_server_id` = that row's id.

More queries are in `docs/incidents/2026-09-26-evidence-queries.sql`.

## Known gaps

* The bot's Discord status line ("N Players Online"), the server status board and the ADM monitor still read the ADM tracker directly. The restart reset removes their phantoms, but after a bot restart they show the lower bound until evidence arrives.
* A bot restart closes open activity sessions at worker start (`ResetConnectedForRestart`). A player who stays online across a bot deploy accrues no link playtime until they reconnect.
* Verified links from before migration 0056 are not reconciled automatically. An operator backfill would set `role_sync_status='PENDING'` for them, and that changes live Discord roles, so it needs explicit approval.
