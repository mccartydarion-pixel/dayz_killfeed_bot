# Immediate killfeed delivery: staging validation report

**Release candidate:** PR #108. The brief named `3cc3748`. The code validated here is that commit plus the immediate-delivery fixes in this change (runtime changes are listed in section 4).

**No live staging deployment was performed, and nothing in production or `champions-case-staging` was touched.** The classifications are:

* **BLOCKED:** needs live staging.
* **PASS/FAIL:** verified on the staging-equivalent harness described in section 2. Those results are synthetic; they are **not** live latency and **not** live verification.

## Summary

| Area | Result |
|---|---|
| 1. Staging isolation (Railway `champions-case-staging`, staging guild, database) | **BLOCKED** |
| 2. Feature configuration (staging `immediate`, production unset) | code **PASS**; live **BLOCKED** |
| 3. Latency, normal load (immediate queue p95 < 2 s; detect→publish p95 < 5 s) | **PASS** (synthetic) |
| 3. Latency, stress load (beyond Discord's per-channel rate) | **FAIL** vs targets (saturation, see below) |
| 3. Exactly-once, immediate mode | **PASS** (normal 60/60, stress 120/120) |
| 3. Exactly-once, rotating mode (production default) | **FAIL (by design)**: posts only the newest 10 per cycle |
| 4. Event ordering and independent routing | **PASS** |
| 5. Rolling 10-card display (checked against channel state) | **PASS** |
| 6. Failure recovery (429/403/404/5xx/timeout/route change/cleanup/restart) | **PASS**, with documented limitations |
| 7. Rollback to rotating mode | harness **PASS**; live **BLOCKED** |
| 7. Compatibility with `605f1ec` | **PASS** (full suite on the RC schema, from the staging report) |
| Production readiness | **NOT READY**: live staging blocked |

## 1. Staging isolation: BLOCKED

From this session:

* The network policy denies `backboard.railway.app` (Railway API) and `discord.com` (the proxy returns 403).
* No Railway CLI or token is present.
* No staging `DATABASE_URL`, `DISCORD_TOKEN`, `DISCORD_GUILD_ID` or `NITRADO_TOKEN` is present.

So the `champions-case-staging` project could not be inspected, and no dedicated service could be created. The billing QA service in that project was not touched. Per the brief, nothing was deployed without verified isolation.

To unblock:

* Allow `backboard.railway.app` (plus `discord.com` if staging is to be observed from here) in the environment's network settings.
* Add read-only staging credentials as environment variables. Never paste them in chat.

The dedicated service must meet the isolation checklist in `2026-09-26-staging-verification.md` section 1:

* its own database;
* its own bot application and test guild;
* a test Nitrado service;
* a Stripe test key;
* `KILLFEED_DELIVERY_MODE=immediate` on **that service only**.

## 2. Test environment identifiers (staging-equivalent harness)

| Component | What ran |
|---|---|
| ADM ingestion | production `killfeed.Engine` (parser, dedupe) over an in-memory ADM file |
| Persistence | production `PersistenceQueue` (durable ack before publish), in-memory store |
| Feeds | production `RotatingFeed` + `FeedSession`, both modes |
| Discord client | real `discordgo` v0.29 session (HTTP, rate limiter, error decoding) |
| Discord API | local emulator: channel state, message create/delete/bulk-delete, `enforce_nonce` idempotency, injected 429/403/404/5xx/timeouts; 40–200 ms per call |
| Isolation | `TestMain` points discordgo at a local dispatcher for the whole test run: no request can reach real Discord |

Files:

* `internal/discord/killfeed_staging_harness_test.go` (latency; run with `KILLFEED_STAGING_HARNESS=1`);
* `internal/discord/killfeed_immediate_recovery_test.go` (window, recovery, rollback);
* `internal/app/app_test.go` (`TestFeedDeliveryModeFromEnvironment`).

## 3. Latency (SYNTHETIC, not live production latency)

The same seeded fixture was run through both modes. It contains:

* kills and deaths;
* rapid consecutive kills in one ADM write;
* duplicate lines;
* replayed records.

Immediate mode ran with the **production 10-minute interval**. Rotating mode's cycle was scaled to 3 s (production waits are about 200× longer).

**Normal load** (60 events at about 1 event/s: above typical DayZ rates, below Discord's ~1 message/s per channel):

| Stage | Mode | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| queue (ack → Discord call) | immediate | 1 ms | **741 ms** | 1.207 s | 1.207 s |
| Discord publication | immediate | 133 ms | 193 ms | 197 ms | 197 ms |
| **detect → published** | immediate | 196 ms | **878 ms** | 1.26 s | 1.26 s |
| queue | rotating (3 s cycle) | 1.764 s | 3.001 s | 3.067 s | 3.067 s |
| detect → published | rotating (3 s cycle) | 1.952 s | 3.116 s | 3.167 s | 3.167 s |

Detection (ADM line available → parsed) and persistence (parsed → durable ack) were ≤ 2 ms in both modes. Delivery mode does not affect them.

**Acceptance (normal load):**

| Criterion | Result |
|---|---|
| Immediate queue p95 < 2 s | **PASS** (741 ms) |
| Detect → publish p95 < 5 s | **PASS** (878 ms) |
| No intentional 10-minute delay | **PASS** |
| Exactly once | **PASS**: 60/60 created once each |

At production scale, rotating mode's queue wait is **p50 ≈ 5.9 min and p95 ≈ 10 min** (the scaled figures × 200).

**Stress load** (120 events at about 30 events/s): immediate mode delivered **120/120 exactly once, in order**, but the queue saturated: p95 13.6 s, max 14.6 s. One feed posts about 4 cards/s against the emulator (a create plus an eviction delete per card), and real Discord allows about 1 message/s per channel. Any design queues above that rate. Rotating mode posted **40/120**: it shows only the newest 10 per cycle by design.

**Not measured here:** the wait between Nitrado making an ADM line available and the engine reading it. The production poll interval is 10 s (`NITRADO_POLL_INTERVAL`), so the analytic wait is up to 10 s (about 9.5 s at p95) *before* detection. The brief's 5 s detect → publish target starts at detection, and it passes. Measured from Nitrado availability, the end-to-end p95 would be about 10.4 s. That is set by the poll interval, not by delivery mode.

## 4. Defects found and fixed in this pass (runtime changes)

Each was found by the harness or by reviewing the code against the brief, fixed, and covered by a test.

| # | Defect | Fix |
|---|---|---|
| 1 | **Immediate mode silently dropped events under backlog**: it trimmed the pending queue to the newest 10, the same as the rotating cycle (42 of 82 kills lost in the first stress run) | every card is posted in order; the oldest shown card is evicted right after each confirmed post; the backlog is bounded at 500, with overflow counted in the ledger (`dropped`) and logged, never silent |
| 2 | **Duplicate card on retry**: discordgo v0.29 has no message nonce, so a create that timed out but succeeded was posted again | `FeedSession.ChannelMessageSendNonce` sends `nonce` + `enforce_nonce`; each card keeps one nonce across all attempts |
| 3 | **Out-of-order delivery after a transient failure**: the next card was posted while the failed one waited | delivery stops at the failed card; it and all later cards stay queued in order |
| 4 | **Stall after a transient failure**: a card that exhausted retries waited for the next event or the 10-minute tick | retry timer that doubles from 2 s up to 60 s |
| 5 | **Request per event during a config fault** (403/404) | the channel is paused for 5 min (no requests); cards are held; recovery is automatic after the pause or on a route change |
| 6 | **Failed cleanup lost forever** (a card stayed in the channel; both modes) | failed deletes become orphans, retried (bulk falls back to single deletes, "already gone" counts as done), up to 5 attempts; counted in the ledger (`cleanup_failures`, `orphaned_cards`, `abandoned_cleanup`) |
| 7 | **Test harness could reach real Discord**: a leftover test goroutine used the restored global endpoint; the network policy refused it | `TestMain` pins discordgo to a local dispatcher for the whole run |

## 4b. Event ordering: PASS

* The kill and death channels contain exactly their own card types.
* The final windows equal the newest 10 of each in fixture order.
* Duplicate and replayed ADM records are dropped once by the engine, so persistence counts equal the number of distinct events.
* A 5xx before two queued cards still delivers them in order.
* A timeout retry (nonce) creates exactly one card.

## 5. Rolling 10-card display: PASS (checked against channel state)

15 distinct events (`TestImmediateRollingWindowChannelState`):

* Each card was published in under 1 s.
* The channel never held more than 10 managed cards once each post settled.
* The final channel is `kill-06 … kill-15` in order, with the oldest removed first.
* The stored embed is identical to the one built (title, description, colour, fields, footer).
* Exactly 15 creates.
* After one quiet interval the channel is empty.

Cleanup failure (`TestImmediateCleanupFailureIsRecordedAndRecovered`): two injected delete failures are recorded (`cleanup_failures`), the channel temporarily holds 11 cards, the retry timer converges it back to the newest 10, and the orphan gauge returns to 0.

Note: a card is created, then the oldest is deleted. Discord has no atomic swap, so the channel shows 11 cards for about one request.

## 6. Failure recovery: PASS

| Failure | Result |
|---|---|
| 429 | honoured (discordgo waits for Retry-After on the feed goroutine, never the ADM goroutine); delivered once |
| 5xx | bounded: 3 attempts per pass, then the doubling retry timer; delivered in order; the ledger counts only confirmed posts |
| Network timeout | the retry reuses the nonce; **exactly one card** |
| 403 (50013) / 404 (10003) | one failed request, then **no requests** for 19 further events during the pause; ledger `delivered=0, failed=1` (no false success); after the pause all 20 cards delivered once, window = newest 10 |
| Route change | old channel cleared; the next card goes to the new route immediately |
| Cleanup failure | recorded and recovered (section 5) |
| Restart during queued delivery | **graceful** (deploy/SIGTERM): Run's final pass delivers the queue. **Crash:** queued cards are lost from Discord (the events are already durable in the database); no false success is recorded |

**Delivery limitations:**

* **Crash loses in-memory cards.** Delivery is at-most-once across a crash and exactly-once otherwise. Persisting the feed queue would be needed for crash-proof delivery.
* **Nonce deduplication** relies on Discord honouring `enforce_nonce` for a few minutes. A retry after a longer outage cannot be deduplicated by Discord (retries here happen within seconds).
* **Restart leaves cards:** neither mode removes cards posted by a previous process. Up to 10 cards remain after each restart; this was already true of rotating mode.
* **Engine metric wording:** the engine metric `discord_kills_published` counts cards handed to the feed. The authoritative delivery count is the ledger (`/api/runtime/status` → `health.deliveries`).
* **Poll interval:** the 10 s ADM poll dominates end-to-end latency from Nitrado availability (section 3).

## 7. Rollback: harness PASS, live BLOCKED

* **`TestFeedDeliveryModeFromEnvironment`** (tests the real `feedDeliveryMode()`): unset, `rotating` or any other value selects rotating; only `immediate` enables immediate. `runServerWorker` applies the one value to both killfeed and deathfeed.
* **`TestRollbackImmediateToRotating`:** after immediate delivery, a restart with the variable unset runs rotating mode on the same channel. Nothing is posted before the cycle; the card is posted at the cycle; no duplicates; the next cycle replaces only its own batch. Feeds never create channels. The previous process's cards remain (the limitation above).
* **Compatibility with `605f1ec`:** feed cards are ordinary embed messages, and the schema compatibility is proven (`605f1ec`'s full suite passes on the RC schema; staging report, section 2).
* Unsetting the variable and restarting **only** the isolated staging service could not be done live (BLOCKED).

## 8. Regression

* `go build`, `go vet`, `go vet -tags integration`: clean.
* Unit: 40/40 packages.
* Integration on a fresh isolated Postgres: 41/41 packages, **1,851 tests, 0 failures**. The skips are the pre-existing locked canary test and the gated harness.
* `go test -race ./internal/discord`: **10/10 consecutive runs pass**. Race also passes for killfeed, app, linking, repository and nitrado.

## Production readiness verdict: NOT READY

The code is ready for a staging trial of `KILLFEED_DELIVERY_MODE=immediate`. Every harness criterion passes, including the latency targets under normal load and exactly-once delivery.

Enabling it anywhere near production still requires:

1. a live isolated staging run (section 1 unblocked);
2. live latency measured there (the figures here are synthetic);
3. owner approval.

Production keeps `KILLFEED_DELIVERY_MODE` unset.
