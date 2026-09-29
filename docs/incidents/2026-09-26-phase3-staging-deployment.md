# Phase 3: reliability staging deployment — approval report

**Release candidate:** PR #108 at `05e02c913eb2d29383121c238398a4e3f197c7b8` (CI green).

**Status: AWAITING ACCESS AND APPROVAL.** Nothing was created, deployed or migrated. No secret was read or printed. The billing QA service and production were not touched. There are no live results yet; every live item below is **NOT TESTED**.

## 1. Isolation: inspection result

| Step | Result |
|---|---|
| Inspect `champions-case-staging` | **BLOCKED**: the network policy denies `backboard.railway.app` (proxy 403); no Railway CLI, token or connector in this session |
| Inspect the Discord application and test guild | **BLOCKED**: the network policy denies `discord.com`; no staging bot token |
| Staging database, Nitrado, Stripe | no credentials present (and none needed for Nitrado or Stripe, see section 2) |

Unblocking: see `2026-09-26-staging-preflight.md` sections 1 and 7. In short:

1. Allow `backboard.railway.app` and `discord.com` in the environment's network settings.
2. Add a `champions-case-staging` **project** token as `RAILWAY_TOKEN` in the environment settings.
3. Enter the QA bot token directly in Railway, never in chat.

The read-only inspection commands and stop conditions are in the preflight report, section 2.

## 2. Resources (proposed; none exist)

| Resource | Name | Isolation property |
|---|---|---|
| Bot service | `reliability-qa-bot` | this repo, `Dockerfile`, pinned to `05e02c9` |
| PostgreSQL | `reliability-qa-postgres` | new, empty volume; private network only; the only `DATABASE_URL` the bot receives |
| Synthetic Nitrado | `reliability-qa-nitrado-fixture` | `Dockerfile.nitrado-fixture`; read-only (all writes 403); no Nitrado credential exists |
| Discord application | "Champion Reliability QA" | new application; never the production bot |
| Test guild | "Champion Reliability QA" | owner-created; the QA bot is never invited anywhere else |
| Channels | `#qa-killfeed`, `#qa-deathfeed` (text), online counter (voice), link channel | created by `/setup` in the test guild |
| Kill/death routing | legacy `KillfeedChannelID` and `DeathChannelID` set by Discord `/setup`, **without** a `KILLFEED` route: do not apply the SaaS dashboard channel layout | the dashboard layout maps both kill and death feeds to the `KILLFEED` route (`saas_api_channel_layout.go`), so they share one channel by design. Separate channels need no `KILLFEED` row in `guild_channel_routes` (checked with a read-only query before L12) |

The billing QA service is not used, referenced or modified. No new Railway environment is forked.

## 3. Configuration for `05e02c9`

### `reliability-qa-bot`

| Variable | Value | Source |
|---|---|---|
| `APP_ENV` | `staging` | plain |
| `KILLFEED_DELIVERY_MODE` | `immediate` | plain (**staging only**) |
| `NITRADO_API_BASE_URL` | `http://reliability-qa-nitrado-fixture.railway.internal:8080` | plain (accepted only with `APP_ENV=staging`) |
| `DATABASE_URL` | `${{reliability-qa-postgres.DATABASE_URL}}` | Railway reference (private URL) |
| `DISCORD_TOKEN` | QA bot token | secret, entered by the owner |
| `DISCORD_APPLICATION_ID` | QA application ID | plain |
| `DISCORD_GUILD_ID` | test guild ID | plain |
| `DISCORD_GUILD_MEMBERS_INTENT_ENABLED` | `true` | plain (Server Members intent enabled in the portal) |
| `CREDENTIAL_ENCRYPTION_KEY` | new random 32-byte key | secret, generated for staging |
| `WEBSITE_API_SECRET` | new random value | secret, generated for staging |
| `CHAMPION_ADMIN_DISCORD_IDS` | owner's Discord user ID | plain |

**Must be unset:**

* `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `CHAMPION_BILLING_PLANS_JSON`, `CHAMPION_SHOP_CANARY_*`;
* `NITRADO_TOKEN`, `NITRADO_SERVICE_ID`;
* `DATABASE_PUBLIC_URL`, `KILLFEED_CHANNEL_ID`, `LIVE_SYNC_WATCHERS`, `CASE_EVIDENCE_*`.

**Staging-only start command** (needed for the live SIGKILL test, section 5): `/bin/sh -c '/app/dayz-killfeed & echo $! > /tmp/bot.pid; wait $!'`.

The image's entrypoint makes the bot PID 1, and the kernel ignores SIGKILL sent to a namespace's PID 1 from inside it. Running under `sh` makes the bot killable. When it dies, `sh` exits non-zero and Railway's on-failure restart starts a new container. Production's start command is unchanged.

### `reliability-qa-nitrado-fixture`

* `PORT=8080`
* `FIXTURE_SERVICE_ID=90000001`
* `FIXTURE_CONTROL_TOKEN=<new random>`
* no public domain

### `reliability-qa-postgres`

Railway PostgreSQL template with defaults. No public TCP proxy.

### Production

`KILLFEED_DELIVERY_MODE` and `NITRADO_API_BASE_URL` stay **unset**, and production is not modified. The only production action proposed is a names-only variable listing, and only with owner approval.

### Pinning `05e02c9`

Choose one; either needs approval:

* **(a) Branch:** create `staging/reliability-rc` at `05e02c9` and have the service track it (never push to it). Railway sets `RAILWAY_GIT_COMMIT_SHA`, and `/api/runtime/status` reports `build.commit = 05e02c9…`.
* **(b) `railway up`:** deploy from a clean checkout of `05e02c9`. No branch is needed, but `build.commit` reports `unknown`, so the pin can only be shown from the deploy log.

## 4. Actions requiring explicit approval

Nothing below has been done.

1. Network allowlist and a `RAILWAY_TOKEN` project token (environment settings).
2. Read-only inspection of `champions-case-staging` (names only). Stop if the preflight stop conditions are hit.
3. Owner creates the QA Discord application and test guild, and invites the bot to that guild only.
4. Create `reliability-qa-postgres` (billable).
5. Create `reliability-qa-nitrado-fixture` (billable).
6. Create `reliability-qa-bot` with section 3's variables and start command (billable).
7. Pin `05e02c9` by (a) or (b).
8. First deploy, which runs migrations `0001`–`0057` on `reliability-qa-postgres` only.
9. Live verification (section 5). This includes temporary permission changes on test-guild channels, creating and deleting one throwaway test channel, restarts and one SIGKILL of `reliability-qa-bot`, and toggling `KILLFEED_DELIVERY_MODE` on that service only.
10. Teardown after the report.

## 5. Live verification plan (runs only after approval)

**Evidence sources:**

* **Channel state:** the real Discord API, read with the QA bot token (`GET /channels/{id}/messages?limit=50`), run through `railway run` so the token is never printed.
* **Visible-count observer:** polls that endpoint every 500 ms during each test (within Discord's read limits), recording the maximum count and every message ID with its first/last seen time.
* **Journal:** `discord_feed_cards` on the staging database (`posted_at`, `removed_at`, `message_id`).
* **Delivery ledger and build block:** `/api/runtime/status`.
* **Per-card latency:** `feed card delivered` log lines.
* **Fixture state:** `/_fixture/state`.

**Preconditions (checked before any test):**

* `build.commit` / deploy log = `05e02c9`, `appEnv=staging`, `killfeedDeliveryMode=immediate`, `nitradoSource=fixture`;
* `GET /users/@me/guilds` with the QA token lists only the test guild;
* the staging database host is `reliability-qa-postgres`, and `schema_migrations` max = `0057_discord_feed_cards`.

| # | Test | Method | Pass criterion (from actual channel state) |
|---|---|---|---|
| L1 | 15 kills → exactly 10 visible | inject 15 kills | the channel lists the newest 10 victims in order; the ledger shows `delivered=15` |
| L2 | visible count never exceeds 10 | observer running during L1 and during a 40-kill burst | observer maximum ≤ 10 |
| L3 | deletion before publication | journal | for each post after the window is full, the evicted card's `removed_at` < the new card's `posted_at`; the observer never sees the new ID while the evicted ID is still present |
| L4 | failed deletion blocks the next post | with 10 cards shown, deny the bot **View Channel** on `#qa-killfeed` just before expiry, or with a full window (deletes and posts both fail), inject 1 kill; restore after 2 minutes | during the denial no new message appears; the ledger shows `cleanup_failures ≥ 1`; after restoring, the oldest card is deleted first and then the new card appears; maximum ≤ 10 |
| L5 | pending cards stay journaled | during L4 | the new card's journal row is open (`message_id` NULL) until it posts |
| L6 | retries preserve order | deny Send Messages, inject 5 kills, restore | the 5 cards appear in injection order |
| L7 | SIGKILL recovery | deny Send Messages, inject 3 kills (journaled), `railway ssh` → `kill -9 $(cat /tmp/bot.pid)`, then restore | after the automatic restart the 3 cards appear exactly once each, in order; `replayed=3`; maximum ≤ 10 |
| L8 | 403 | L6 (Missing Permissions, 50013) | one failed request, then a pause; delivered after restore |
| L9 | 404 | route the killfeed to a throwaway test channel, delete it, inject 1 kill, route back | `CONFIG_FAULT` in the ledger; no request loop; the card is delivered after re-routing |
| L10 | 429 | a burst of 40 kills (above Discord's per-channel rate) | all 40 delivered once, in order. This is **PASS only if a 429 or `RATE_LIMITED` is actually observed**; discordgo usually waits in advance, and if no 429 occurs the result is **NOT TESTED**, not PASS |
| L11 | 5xx and timeout | cannot be induced against real Discord without a fault-injecting proxy | **NOT TESTABLE LIVE**: harness evidence only (section 6) |
| L12 | independent killfeed and deathfeed | inject 12 kills and 3 deaths | `#qa-killfeed` shows the newest 10 kills only; `#qa-deathfeed` shows the 3 deaths only; separate ledger routes and journal keys |
| L13 | immediate latency | 60 kills at about 1/s | p50/p95/p99/max of `queue_ms` and `detect_to_publish_ms` from the logs, and end-to-end (Discord message timestamp − fixture injection time, which includes the 10 s ADM poll). Targets: queue p95 < 2 s, detect→publish p95 < 5 s, exactly once |
| L14 | rollback to rotating | unset `KILLFEED_DELIVERY_MODE` on `reliability-qa-bot` only, restart | `build.killfeedDeliveryMode=rotating`; the immediate cards are removed at startup; new kills post only at the 10-minute cycle; no open journal rows. Then code rollback per `2026-09-26-staging-infrastructure.md` section 10 |

Results will be recorded as PASS, FAIL, BLOCKED or NOT TESTED. A staging PASS is not evidence of production recovery.

## 6. Evidence available now (harness, synthetic, not live)

Production code, the real discordgo client, emulated Discord, CI green at `05e02c9`.

| Item | Test | Result |
|---|---|---|
| never more than 10 visible (delete before post) | `TestImmediateWindowNeverExceedsTenCards` (peak = 10). It fails against the earlier create-then-delete code (`6aab65d`), where the peak was 11. | PASS |
| failed deletion blocks the next post; pending retained; recovery in order | `TestImmediateCleanupFailureIsRecordedAndRecovered`, `TestJournalStrictCapacityBlocksUntilOldestRemoved` | PASS |
| SIGKILL recovery (real process kill, real Postgres) | `TestJournalSurvivesSIGKILL` | PASS |
| 403 / 404 / 429 / 5xx / timeout | `TestImmediateFailureRecovery` | PASS |
| independent feeds | `TestDeathFeedIndependentInSharedChannel` and the harness's separate-channel runs | PASS |
| latency, normal load (`ddec207`, same delivery code as `05e02c9`) | `TestImmediateKillfeedStagingEquivalent` | queue p95 876 ms, detect→publish p95 963 ms, 60/60 exactly once |
| rollback | `TestRollbackImmediateToRotating`, `TestJournalRollbackToRotatingCleansImmediateCards` | PASS |

## 7. Remaining defects and gaps (known now)

* **No live evidence.** Everything above is synthetic until section 4 is approved and access exists.
* **5xx and timeout cannot be verified live** without a fault-injecting Discord proxy. Adding a staging-only Discord API base override would be a separate code change; it is not proposed here.
* **Strict capacity is also backpressure.** In immediate mode, a card that cannot be deleted pauses posting on that channel until the deletion succeeds; it is never abandoned. The backlog is bounded at 500 cards, and overflow is counted.
* **Log repetition (cosmetic).** In `retryOrphans`, after 5 attempts, `o.attempts` is set to `maxCleanupAttempts` and then compared with it, so that check is always true. The "cleanup still failing" error is logged on every retry pass, at most once per 60 s.
* **Crash residual.** A card enqueued but not yet journaled at the instant of a kill (about 1 ms) is still lost. A replay after longer than Discord's nonce window could duplicate an unconfirmed card.
* **Channel dips during eviction.** Delete-before-post means a full channel briefly shows 9 cards per new card. If the post then fails transiently, 9 cards are shown until the retry succeeds.

## 8. Production readiness

**NOT READY.** Live staging (section 5), the read-only production evidence SQL and owner approval are all outstanding. Production keeps `KILLFEED_DELIVERY_MODE` unset.
