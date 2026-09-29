# P0 reliability consolidation: PRs #106, #107, #108 → release candidate

**Status: release candidate on PR #108. Not merged, not deployed. No production reads or writes, no Discord changes.**

## Ancestry at consolidation

| PR | Branch | Head | Base | Commits | CI `test` | Cloudflare check |
|---|---|---|---|---|---|---|
| #106 | `claude/bold-pasteur-cqqmyy` | `327741b` | `605f1ec` (= `main`) | 1 | green | historical failure (0 s, integration since deleted) |
| #107 | `claude/clever-meitner-lt9922` | `560e7ce` | `605f1ec` | 1 | green | historical failure |
| #108 | `claude/festive-feynman-xi6nmw` | `9f2aec5` → this commit | `605f1ec` | 2 | green at `9f2aec5` | historical failure |

The three PRs are siblings: none contains another. `main` has not moved (`605f1ec` is also the deployed commit). No commit was cherry-picked whole. Each change below was taken, merged or excluded individually.

## 1. Comparison matrix

### Overlapping or conflicting files

| File | #106 | #107 | #108 (before) | Consolidated |
|---|---|---|---|---|
| `repository/server_repository.go` | `ActiveServerIDs`; `ConnectedServerID` = the one active server | `ConnectedServerID` = selected public server, else the only active one | `active AND status<>'DISCONNECTED'` | #106 `ActiveServerIDs` for linking. #107's selection rule for `ConnectedServerID` (diagnostics). **Conflict resolved:** status ignored everywhere (#106/#107) so linking uses exactly the workers' predicate. #108's DISCONNECTED guard dropped. |
| `linking/service.go` | all active servers, best single server, never summed; `ErrNoConnectedServer` not an outage | refined outcomes (invalid username, not observed, shortfall "3m 20s of 5m", activity unavailable); `ErrServerSelectionRequired` | untouched | #106 server evaluation + #107 outcomes. `ErrInstallationNotConfigured` now wraps `ErrNoConnectedServer` (not `ErrLinkCheckUnavailable`). `ErrServerSelectionRequired` excluded (superseded by best-single-server). **New:** role-sync tracking and `ReconcileRoles`. |
| `discord/link_commands.go`, `public_panels.go` | adds SERVER NOT CONNECTED to both copies of the table | one shared `linkErrorMessage` table | untouched | #107's shared table + #106's SERVER NOT CONNECTED wording |
| `repository/link_server_resolution_integration_test.go` | 3 tests (dashboard status, multi-server 3+3, no server) | 2 tests (SaaS end to end, selection rule, restart reset) | n/a | **Same filename in both PRs.** Merged into one file with all 5 scenarios. |
| `discord/public_panels_link_error_test.go` (#106), `link_messages_test.go` (#107) | message test | message test, **same test name** | n/a | merged into `link_messages_test.go` |
| `discord/counter.go` | n/a | `CounterReading`, "?" state, 5-min rename spacing, non-blocking 429 (`RateLimitError`) | permanent faults (404/10003, 403/50001/50013), bounded 5xx retries, `Health()`, fault cleared only by a different channel | **Both.** #107's reading/scheduling model + #108's fault handling. **New:** channel ownership validation (guild + voice type). #107's unbounded 5 s 5xx retry and #108's int-based `Publish` excluded. |
| `app/app.go` counter wiring | n/a | counter loop goroutine; ADM callback only pokes; `OnNewBoot` closes activity sessions | ADM callback published/reconciled the counter itself, gated on presence evidence | #107's loop architecture (Discord never on the ADM goroutine) + #107's `OnNewBoot` activity reset. #108's in-callback publishing **excluded**. |
| `app/saas_api_channel_routes.go` | n/a | `onlineCounterRouteChannel`; falls back to legacy on **any** lookup failure | route authoritative, legacy only on **confirmed** absence | single `bindCounterChannel` with #108's semantics (three outcomes: found / absent / lookup error) |
| `killfeed/engine.go`, `engine_observations.go` | n/a | `resetPresenceForNewBoot` in `selectLog`; `OnNewBoot`; `LastCompleteSnapshotAt` | `resetPresenceForNewBoot` in `acceptBoot`; presence evidence; ADM freshness fixes | **Duplicate `resetPresenceForNewBoot` resolved:** one implementation at #108's `acceptBoot` site, plus #107's `OnNewBoot` hook and `LastCompleteSnapshotAt`. #108 ADM fixes kept. |
| `killfeed/presence_evidence.go` | n/a | n/a | `UNKNOWN / SNAPSHOT_CONFIRMED / BOOT_RESET / EVENT_DERIVED` (6-min fallback) | `EVENT_DERIVED` **excluded**: it was a second, weaker count authority, and connect lines alone are never a live count. Nitrado covers servers without `adminLogPlayerList`. |
| `app/saas_channel_layout*.go`, `discord/setup.go` | n/a | `IsOnlineCounterName` (no duplicate channel for new/legacy/unknown names) | GONE-branch legacy field clear | both |

### Unique to one PR, retained

| PR | Change |
|---|---|
| #106 | per-server best-single playtime; `ErrNoConnectedServer` message; "3+3 is not 5" integration test |
| #107 | Nitrado `GameserverLive` (whitelist-decoded; **extended** with `service_id` ownership check); `SessionAPI.ChannelRename` (`WithRetryOnRatelimit(false)`); counter loop + tests; restart activity-session close; username plausibility; counter diagnostics fields; `ONLINE_COUNTER_AND_LINK_CHECK.md` (rewritten for the consolidated policy) |
| #108 | ADM freshness (sticky label, quiet ≠ wrong, throttled give-up latency); Discord delivery helper + per-route ledger; killfeed failures no longer counted as published; rotating feed re-queue; health block; runtime API guild isolation; GONE legacy-field clear; incident report + evidence SQL |

### Excluded or superseded

| Change | From | Why |
|---|---|---|
| `ErrServerSelectionRequired` / "SERVER SELECTION REQUIRED" | #107 | superseded by #106's best-single-server evaluation, which is no weaker and does not block multi-server guilds |
| `status <> 'DISCONNECTED'` guard | #108 | linking must match the worker predicate exactly; `Deactivate` already clears `active` |
| `EVENT_DERIVED` presence state and window | #108 | would be a second count authority |
| In-callback counter publish, `knownPresenceCount` gating in the ADM hook | #108 | Discord I/O on the ADM goroutine; replaced by #107's loop |
| `App.bindOnlineCounter` | #108 | dead after `bindCounterChannel`; its test was folded into `TestCounterChannelPrecedence` |
| Unbounded 5 s 5xx retry in the counter | #107 | replaced by #108's bounded retry |
| Legacy fallback on any route lookup failure | #107 | replaced by fallback only on confirmed absence |

## 2. New in the consolidation

* **Nitrado ownership check:** a live count is used only if the response's `service_id` equals the requested service.
* **Presence disagreement:** Nitrado versus a proven ADM list is recorded with its start time. After 11 minutes it marks the installation DEGRADED.
* **Channel ownership validation** on the counter.
* **Verified role reconciliation** (migration `0056`, additive, nullable). Failed role assignments are now persisted and retried across restarts. Pre-migration links are never touched automatically.
* **Killfeed immediate-delivery mode**, off by default (`KILLFEED_DELIVERY_MODE=immediate`). Each card is posted as soon as it is persisted, while keeping the rolling presentation: at most 10 cards, the oldest removed first, cards expire after one interval so a quiet period still empties the channel. Also added: per-feed latency in the ledger, and `Event.DetectedAt`.
* The death feed now reports under its own ledger route (`DEATH_FEED`).

## 3. Killfeed latency

| Stage | Measurable? | Source |
|---|---|---|
| DayZ event → line in ADM → visible via Nitrado | Only approximately. ADM lines carry server-local `HH:MM:SS` with no zone; listing lag is observable via `NITRADO_METADATA_STALE` | Nitrado |
| ADM detection | yes: ≤ 10 s poll; ≤ 10 s after a long quiet period (was ≤ 60 s) | engine |
| Processing + persistence | yes: synchronous, acked before any publish | engine / persistence queue |
| **Discord queue** | yes: ledger `avg/max_queue_wait_ms`. **Rotating: up to 10 min.** Immediate: about 0 | `RotatingFeed` |
| Discord delivery | yes: `deliverMessage` bounded retry; `last_detect_to_deliver_ms` | delivery ledger |

Measured with the production `Run` loop and the 10-minute cycle scaled to 400 ms (`TestQueueWaitRotatingVersusImmediate`):

* rotating: average queue wait 264 ms (66% of the cycle; about **6.6 min** at production scale), maximum 358 ms (about 9 min);
* immediate: 0 ms.

**Production behavior is unchanged until `KILLFEED_DELIVERY_MODE=immediate` is approved and set.**

## 4. Regression results (release-candidate tree)

Isolated PostgreSQL 16 database, created fresh (`champ_rc`), all migrations `0001`–`0056` applied from scratch:

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...`, `go vet -tags integration ./...` | clean |
| `go test ./...` (unit) | 40/40 packages pass |
| `go test -race` (discord, killfeed, app, linking, repository, nitrado) | pass |
| `go test -tags integration -p 1 ./...` | **41/41 packages, 1,611 top-level tests (1,815 including subtests) pass, 0 fail, 1 skip** (`TestShopCanaryExecutionIsLockedByDefault`, pre-existing, also skipped on #106) |

Passing top-level tests by package: killfeed 217 · livesync 21 · discord 317 · app 406 · repository 138 · linking 22 · billing 61 · shop 16 · shop/canaryops 15 · caseintel 6 · routing 7 · nitrado 60 · database 13.

The combined behavior is covered, not just each PR's tests:

* multiple guilds and multiple servers in the same database;
* two worker sources where the counter reads only the public server;
* a restart followed by reconnects;
* Nitrado versus ADM disagreement;
* the channel-precedence sequence;
* verified-role failure → new process instance → reconciled;
* the migration applied on a fresh database.

## 5. Staging verification plan (needs a staging environment + owner approval)

1. **Deploy** the RC to a staging service pointed at a copy (or a fresh database) with `KILLFEED_DELIVERY_MODE` unset.
2. **Migration:** `0056` applies; `SELECT COUNT(*) FROM player_links WHERE role_sync_status IS NOT NULL` is 0.
3. **Counter:** within about 60 s the log shows `voice_counter event=reading source=NITRADO_QUERY`. The channel reads `🟢・Online: N/M` matching the Nitrado panel, and `health.onlineCounter.state=OK`.
4. **Counter fault:** bind the counter to a deleted channel in staging. Expect `CONFIG_FAULT UNKNOWN_CHANNEL` and no further calls. Then `/setup repair` clears it.
5. **Link:** a player with ≥ 5 min gets PENDING VERIFICATION. A player with 3+3 min on two servers gets the shortfall. Disconnect and reconnect ⇒ VERIFIED, role assigned, `verifiedRoles.ASSIGNED` increments.
6. **Role failure:** remove the bot's Manage Roles permission, then verify ⇒ `FAILED`, health DEGRADED. Restore the permission and restart the service ⇒ reconciled to `ASSIGNED` within 5 min.
7. **Restart:** restart the DayZ server ⇒ `server_restart_reset`, open activity sessions closed, counter follows Nitrado.
8. **Killfeed immediate mode:** set `KILLFEED_DELIVERY_MODE=immediate` on staging only. A kill appears in seconds, the channel holds ≤ 10 cards and empties after a quiet 10 minutes; ledger `max_queue_wait_ms` stays low.
9. **Regression:** check C.A.S.E. reports, the shop (manual pickup), billing pages, and the server status board.

## 6. Production rollback plan

* **Code:** redeploy `605f1ec` (Railway → Deployments → `8bbde7db…` → Redeploy).
* **Schema:** migration `0056` only adds nullable columns and a partial index. `605f1ec` never reads them, so rollback is state-compatible with **no down-migration needed**. If a clean schema is ever required later: `DROP INDEX IF EXISTS idx_player_links_role_pending; ALTER TABLE player_links DROP COLUMN IF EXISTS role_sync_status, …` (not needed for rollback).
* **Feature flag:** `KILLFEED_DELIVERY_MODE` is unset by default. Unsetting it reverts to the rotating cycle without a redeploy of different code (it needs a restart to take effect).
* **Discord:** rollback creates no channels. The RC renames the counter from `🟢・Online Players: N` to `🟢・Online: N/M`. `605f1ec` recognises only the `Online Players:` format, so after a rollback its first reconcile renames the channel back once. That is expected and harmless (one rename, within Discord's limit).

## 7. Remaining known gaps

* The bot status line, the server status board and the ADM monitor still read the raw ADM tracker (a lower bound after a bot restart).
* A bot restart closes open activity sessions at worker start, so a player online across a deploy accrues no link playtime until they reconnect (pre-existing).
* Verified links from before `0056` are not role-reconciled automatically. A backfill changes live Discord roles and needs explicit approval.
* "DayZ event → ADM visible" cannot be measured exactly without the server's time zone.
* Affected production IDs are still unconfirmed: run `2026-09-26-evidence-queries.sql` (read-only) first.
* The Nitrado `service_id` field is required for a live count. If Nitrado ever omits it, the counter falls back to ADM evidence or unknown (safe, but less timely). Staging step 3 confirms it is present.

## 8. Release readiness

| Gate | Status |
|---|---|
| One coherent implementation; duplicate `resetPresenceForNewBoot` / tests / message tables resolved | PASS |
| One documented count authority (Nitrado → complete ADM evidence → unknown) | PASS |
| Link: status mismatch fixed, 5-min rule per single server, challenge + admin fallback + role unchanged | PASS (unit + real Postgres) |
| Counter: route precedence, ownership, permanent faults, bounded retry, Retry-After, 5-min spacing, never blocks ADM | PASS |
| ADM freshness protections and regressions retained | PASS (all prior tests + new) |
| Role delivery survives restart (additive migration) | PASS (real Postgres) |
| Killfeed latency: stages separated; immediate mode built, default unchanged | PASS |
| Full unit / integration / race suites, fresh isolated DB, no regressions | PASS (40/40, 41/41, 1,815 tests, 0 fail) |
| C.A.S.E., Shop, billing, routing, server status unaffected | PASS (suites) |
| CI on the consolidated commit | see PR #108 checks (GitHub Actions `test`) |
| Staging verification | **PENDING** (no staging access) |
| Production evidence SQL | **PENDING** |
| Owner approval: deploy, `KILLFEED_DELIVERY_MODE`, role backfill | **PENDING** |

**Decision: ready for staging verification. Not ready for production until the three PENDING gates pass.**
