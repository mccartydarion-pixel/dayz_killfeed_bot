# P0 2026-09-26 — Production reliability incident & recovery

Production: `dayz_killfeed_bot` (Railway `genuine-education`), deployment
`8bbde7db-2a47-4d42-9f13-9f740aad5538`, commit `605f1ec`.

## Evidence scope — read first

* The patch branch is based on exactly the deployed commit `605f1ec`.
* This investigation had **no Railway, production database or Discord access**.
  The three production log signatures were traced to their emitting code and
  each root cause was **reproduced deterministically** (test fails on
  `605f1ec`, passes on the patch). Confirming which specific IDs are affected
  in production requires running
  [`2026-09-26-evidence-queries.sql`](2026-09-26-evidence-queries.sql)
  (read-only, validated against the migrated schema) and preserving its output.
* No latency was measured in production. The latency table below is derived
  from the timing constants in code, and is labelled that way.

## Root causes

### A. Online counter — `HTTP 404 Unknown Channel (10003)`

**Confirmed in code, reproduced.** Two channel sources fought, and the stale one won:

1. The legacy `/setup` stored `guilds.online_players_channel_id`. The Channel
   System V2 layout routes `ONLINE_COUNTER` to a new channel and records the
   legacy one as retired.
2. `syncOnlineCounterRoute` binds the counter to the route, but
   `bindOnlineCounter` re-bound it to the **legacy** ID at every worker start
   *and on every presence change* (`app.go` `OnPlayersChanged`). Once the
   legacy channel was deleted by hand (or reported `GONE` by cleanup, which
   forgot the retired row but left the legacy field set), every presence
   change renamed a dead channel.
3. `counter.go` `handleRenameError` treated 404/10003 as a generic failure. It
   never stopped, so there were 2 Discord calls per presence change indefinitely
   (repro: 40 calls for 20 changes). Its 5xx "one bounded retry" was actually
   unbounded, and a 403 block was only cleared by a process restart.

Files/functions: `internal/app/app.go` (`bindOnlineCounter`, worker
`OnPlayersChanged`), `internal/app/saas_api_channel_routes.go`
(`syncOnlineCounterRoute`), `internal/app/saas_api_channel_layout.go`
(`cleanupRetired`), `internal/discord/counter.go`.

Fix: the route is authoritative (`App.onlineCounterRouted`), and the legacy
binding is only a fallback when a route is *confirmed* absent (never on a lookup
error). 404/10003 and 403/50001/50013 become a `CONFIG_FAULT` that stops all
Discord calls for that channel until a *different* channel is bound (repair
clears it). 5xx/network errors get 3 bounded exponential retries. A `GONE`
retired legacy channel's field is cleared. No channel is created and no
customer route is modified.

### B. PSN verification — `NO_CONNECTED_SERVER`

**Confirmed in code, reproduced on real Postgres.** `ServerRepository.ConnectedServerID`
accepted only `status IN ('connected','ready','active')`. The SaaS dashboard
connect flow (`saas_api_nitrado.go` → `UpsertForInstallation`) writes Nitrado
**power state** into that column (`ONLINE`/`OFFLINE`, `nitradoServiceStatus`).
The runtime starts workers on `active` alone, so activity was recorded for the
server (the "server ID 1" evidence) while `/link` resolved zero servers.
Repro on `605f1ec`: a dashboard-connected server with 6 min of recorded
activity → `link check unavailable`.

Fix: binding = `active AND status <> 'DISCONNECTED'` (the same predicate the
workers use, plus explicit teardown). Guild scoping is unchanged, and
`MULTIPLE_CONNECTED_SERVERS` is still an error rather than a guess. Server ID 1
is not hardcoded anywhere.

### C. ADM source freshness — `WRONG_OR_INACTIVE_ADM_SOURCE`

**Confirmed in code, reproduced.** Three compounding defects:

1. **Sticky label.** `RuntimeDiagnosticSnapshot.ProbeClassification` was only
   ever *set* by the stale probe, never cleared. `Classification()` checks it
   before `HEALTHY`, so one quiet window labelled the pipeline wrong/inactive
   for the rest of the process, even while the same source grew and was read
   normally.
2. **Quiet ≠ wrong.** A probe that found no new bytes after 2 minutes reported
   `WRONG_OR_INACTIVE_ADM_SOURCE`. The code itself documents that a healthy
   noftp ADM can go 3–6 minutes between writes. That verdict now needs silence
   past `staleGiveUpAfter` (8 min, the point where the engine demotes and
   rediscovers). Before that the label is `SOURCE_QUIET`.
3. **Detection latency after a quiet period.** Past `staleGiveUpAfter`, the
   give-up branch returned early whenever the probe and rediscovery were
   throttled (60 s). The normal 10 s metadata poll never ran, so growth that the
   Nitrado listing already showed waited up to 60 s. It now falls through to
   the metadata poll, which reads that growth on the next poll.

Preserved: boot authority (an older boot is never selected), noftp/ftproot
alias collapsing, boot header verification, checkpoint resume, rediscovery
throttling, and "a stale source reselected alone is never falsely healed". All
existing source-integrity tests pass unchanged. One test's expectation moved
from `WRONG_OR_INACTIVE` to `SOURCE_QUIET` at 3 minutes, and it still asserts
the source is not `HEALTHY`.

Files: `internal/killfeed/engine.go` (`pollSelected`, `probeStaleSource`,
`noteSourceGrowth`), `internal/killfeed/runtime_diagnostics.go`.

## Additional defects found during the audit

| Area | Defect | Fix |
|---|---|---|
| Presence | A verified newer boot (server restart) **retained** previous-session players until the next PlayerList | `acceptBoot` → `resetPresenceForNewBoot` (only for a strictly newer verified boot, after the old file is drained) |
| Presence | After every deploy the tracker starts empty and the counter was reconciled to **0** for a populated server | Presence evidence model: `UNKNOWN` → `SNAPSHOT_CONFIRMED` / `BOOT_RESET` / `EVENT_DERIVED` (6 min cap for servers without `adminLogPlayerList`); nothing is published while `UNKNOWN` |
| Killfeed | Immediate-send failure returned `nil`, so a dropped kill counted as **published** | Returns the error |
| All feeds | One send attempt; failures only logged | `deliverMessage`/`deliver`: bounded retry + backoff, Retry-After (capped 10 s), no retry for config faults; per-route ledger |
| Rotating feed | A failed post was dropped from the cycle | Transient failures and config faults are re-queued for the next cycle; a payload Discord rejects (400) is dropped but recorded in the ledger |
| Role / DM | No retry, no visibility | Through `deliver` (`VERIFIED_ROLE`, `VERIFICATION_DM` in the ledger) |
| Runtime API | `?server_id=` returned another guild's server | `404 unknown_server` |

Known limitation: the bot's own Discord status line (`PresenceCounts`) still
sums raw trackers, so it can show a lower bound during the ≤ 6 min `UNKNOWN`
window after a deploy. The channel counter and runtime API do not.

## Latency budget (derived from code constants, not measured in production)

| Stage | 605f1ec | Patch |
|---|---|---|
| Nitrado write → visible in listing | outside our control (source visibility delay) | same |
| Listing change → read (active source) | ≤ 10 s poll | ≤ 10 s |
| Growth after a quiet period > 8 min | ≤ 60 s (probe window) | ≤ 10 s (next poll) |
| Growth hidden by lagging metadata (2–8 min quiet) | ≤ 60 s direct probe | same |
| Parse → persist | synchronous ack | same |
| Presence → online counter | 3 s debounce; **0 shown for a populated server after every deploy** | 3 s; nothing shown until evidence (≤ 6 min cap) |
| Counter on a deleted channel | never (endless 404) | `CONFIG_FAULT` until repair |
| **Kill → Discord killfeed** | **0–10 min (`rotatingFeedInterval`)** | **unchanged — owner decision** |
| Hit/build feed batching | 5 s window | same |

**The dominant killfeed delay is by design.** Every kill and death, routed or
legacy, is posted by `RotatingFeed` on a **10-minute** cycle
(`app.go rotatingFeedInterval`, introduced in `c93e7f9`, 2026-09-19). A kill
waits 5 minutes on average before it appears. This is a presentation decision (a
rolling display of 10 cards replaced wholesale), so it is **not** changed in
this patch. Options for the owner: (a) post immediately on enqueue and keep the
10-minute cycle only for deleting old cards, or (b) shorten the interval.

## Tests

New deterministic tests (each root-cause test was shown to fail on `605f1ec`):

* Real Postgres (`-tags integration`): dashboard-connected server resolves
  (`ONLINE`/`OFFLINE`/`CONNECTED`); teardown and `DISCONNECTED` stay unbound;
  cross-guild isolation; `/link` pending link at ≥ 5 min; `ErrPlaytimeRequired`
  under 5 min; disconnect→reconnect challenge → `VERIFIED` → role assigned;
  another guild's player is never found.
* Counter: Unknown Channel stops every Discord call (rename and GET); same-ID
  re-bind keeps the fault; new binding clears it and writes the count; 50001
  permanent; 5xx bounded to 1+3 attempts; 429 is not a fault and is honoured.
* App: the legacy binding never overrides a route; a `GONE` legacy channel's field is cleared.
* ADM: sticky label cleared by proven growth; quiet ≠ wrong before 8 min;
  listing-visible growth read while throttled; all existing rotation, alias,
  boot, delayed-metadata and throttle tests pass.
* Presence: unknown until snapshot; confirmation publishes once; restart
  clears the previous session; same-boot reselection retains; unknown window
  bounded; two simultaneous players, duplicate connect/disconnect, reconnect;
  existing per-engine isolation test.
* Delivery: classification; config fault not retried; bounded exponential
  backoff; recovery; Retry-After honoured and capped; rotating feed keeps
  undelivered items.
* Health: HEALTHY requires evidence; no worker ⇒ UNKNOWN; UNAVAILABLE and
  DEGRADED reasons; unknown presence ⇒ `null` count.

Results on the patch: `go vet ./...` clean. Unit: 40/40 packages pass.
Integration against Postgres 16: 41/41 packages pass, which is the same as the
`605f1ec` baseline (C.A.S.E., Shop + canary ops, billing/subscriptions,
installations, migrations). `-race` passes on the changed packages. **No
migration, no schema change, no data rewrite.** The only new write is clearing
a legacy channel field for a channel Discord confirms is deleted, during an
operator-initiated cleanup.

## Rollout plan (requires owner authorization)

1. Preserve evidence: run the evidence SQL and export the last 24 h of Railway
   logs for `online counter rename failed`, `component=link`, `stale_probe`.
2. Deploy the patch commit to production as a normal Railway deploy (single
   service; there is no staging service in this project, and the isolated
   fixture tests above stand in for staging).
3. Watch for 30 minutes:
   * `GET /api/runtime/status` → `health.state`, `health.reasons`,
     `health.onlineCounter.state`, `health.presenceState`.
   * Logs: `component=voice_counter event=bound_to_route` at startup, and no
     further `online counter rename failed`. If `CONFIG_FAULT` shows, run
     `/setup repair`.
   * `presence_known` within ≤ 6 minutes, then the counter equals the in-game count.
   * `/link` from a player with ≥ 5 min on the server returns the pending-link
     message, not `LINK CHECK UNAVAILABLE`.
   * `stale_probe classification=SOURCE_QUIET` during quiet periods,
     `WRONG_OR_INACTIVE` only after 8 minutes of silence, and the label clears
     on the next growth.
4. Success = all four signals observed live. Tests passing or Railway showing
   `SUCCESS` do **not** count as success.

## Rollback

Redeploy `605f1ec` (Railway → Deployments → `8bbde7db…` → Redeploy). The
patch has no migrations or persisted-state format changes, so rollback is
state-compatible. The only persisted difference, a cleared legacy channel field
for a deleted channel, is harmless under `605f1ec` (it stops the 404 loop there too).

## Readiness gates

| Gate | Status |
|---|---|
| Root cause A identified and reproduced | PASS |
| Root cause B identified and reproduced (real Postgres) | PASS |
| Root cause C identified and reproduced | PASS |
| Existing source-integrity protections preserved | PASS (all prior tests) |
| Full unit + integration suites, no regressions vs baseline | PASS (40/40, 41/41) |
| C.A.S.E., Shop, billing, installations unaffected | PASS (integration suites) |
| No destructive DB change / migration | PASS |
| Affected production IDs confirmed with evidence SQL | **PENDING** (no prod access) |
| Live verification after deploy (counter, `/link`, presence, ADM) | **PENDING** (needs authorization) |
| Killfeed 10-min rotating delay | **OPEN — owner decision** |

**Decision: ready for a controlled production release pending owner
authorization. It is not yet verified in production.**
