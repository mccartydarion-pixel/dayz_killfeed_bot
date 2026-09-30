# Reliability QA staging: infrastructure plan and pre-deployment report

**Status: NOT DEPLOYED.** Nothing was created, deployed, migrated or changed in Railway, Discord, Nitrado, Stripe or any database. This is the proposal that needs approval, plus the code changes needed before a staging deploy can pass the release gates.

**Release candidate:** PR #108. The brief named `41fd618`. This document recommends pinning a newer commit, the one that adds this file (section 5 explains why).

## 1. Phase 1: infrastructure preflight — BLOCKED

Checked from this session:

| Check | Result |
|---|---|
| Railway API (`backboard.railway.app`) | **Denied** by the environment's network policy (the proxy refuses the connection) |
| Discord API (`discord.com`) | **Denied** by the environment's network policy |
| Railway CLI or token | none present |
| Staging credentials (database, Discord, Nitrado, Stripe) | none present |

So none of the following could be inspected: the `champions-case-staging` services, environments, databases, variables or deploy permissions. The billing QA service was not touched.

To unblock, the owner can:

1. Allow `backboard.railway.app` (and `discord.com`, to observe the staging guild) in this environment's network settings.
2. Add a **read-only** Railway project token for `champions-case-staging` as an environment variable. Never paste it in chat.

Once access exists, the preflight is read-only. It prints variable **names** only, never values:

```sh
railway status                                   # project, environment, linked service
railway service list                             # expect the billing QA service; note its name
railway variables --service <billing-qa> --kv | cut -d= -f1 | sort    # names only
railway variables --kv | cut -d= -f1 | sort      # project-level shared variables: names only
```

Decisions that depend on the preflight:

* **Shared variables.** If the project defines shared variables (for example a `DATABASE_URL` or `DISCORD_TOKEN`), the new services must not reference them. Railway applies a shared variable only where it is referenced, so new services are safe by default. Section 3 verifies this for every variable.
* **Environments.** Add the new services to the **existing** environment. Do **not** create a new Railway environment: that forks every existing service, including the billing QA service, into the new environment.
* **Fallback.** If the project cannot host the new services without touching the billing QA service, create a separate project named `champions-reliability-staging`. The plan below is otherwise unchanged.

## 2. Proposed resources

Nothing below exists yet. Everything is usage-billed and small; tear it down after validation (section 9).

| # | Resource | Where | Purpose | Public access |
|---|---|---|---|---|
| R1 | Service `reliability-qa-bot` | `champions-case-staging`, existing environment | this repo, `Dockerfile`, pinned commit | public domain only for `/api/runtime/status` (bearer-protected); optional |
| R2 | Postgres service `reliability-qa-postgres` | same | the staging bot's **own** database (new volume) | **none** (private network only) |
| R3 | Service `reliability-qa-nitrado-fixture` | same | read-only synthetic Nitrado API (`Dockerfile.nitrado-fixture`) | **none** (private network only) |
| D1 | Discord application "Champion Reliability QA" (bot) | Discord developer portal (owner's account) | a bot identity separate from production | n/a |
| D2 | Discord server "Champion Reliability QA" | owner-created | the staging guild; only the QA bot and testers | n/a |
| D3 | Channels and role in D2 | created by `/setup` in D2 | killfeed text channel, online-counter voice channel, link channel, a "Verified" role | n/a |

The following is **not** proposed:

* no Nitrado account or token (the fixture replaces it);
* no Stripe key (billing stays unconfigured and fails closed);
* no change to the billing QA service;
* no change to production, the production guild or the production database.

### Git pin (needs approval)

A branch `staging/reliability-rc`, created at the pinned SHA and never pushed to again. R1 tracks that branch, so Railway sets `RAILWAY_GIT_COMMIT_SHA` and `/api/runtime/status` reports the commit.

The alternative is `railway up` from a clean checkout at the SHA. It creates no branch, but the commit then shows as `unknown`.

## 3. Isolation design

| Requirement | How it is met | How it is verified before any gate runs |
|---|---|---|
| Own database | R2, referenced as `DATABASE_URL=${{reliability-qa-postgres.DATABASE_URL}}` | Host of `DATABASE_URL` (password masked) is `reliability-qa-postgres.railway.internal`; before the first deploy `SELECT to_regclass('schema_migrations')` returns NULL (empty database) |
| Own Discord bot | D1 token in `DISCORD_TOKEN`; `DISCORD_APPLICATION_ID` = D1 | `GET /users/@me` with that token returns the QA bot, not the production bot's ID |
| Own staging guild | `DISCORD_GUILD_ID` = D2 | `GET /users/@me/guilds` with the QA token lists **only** D2 |
| No way to post to live Champions channels | The QA bot is never invited to the production guild, so Discord rejects any post there (403/404) whatever the configuration | The same `/users/@me/guilds` check; the production guild ID is absent |
| Staging-owned channels and roles | Created by `/setup` inside D2 | Every route or channel ID in R2's `guild_setups` / `guild_channel_routes` belongs to D2 (a read-only query on R2) |
| No production Nitrado write credentials | No Nitrado token at all. `NITRADO_API_BASE_URL` points every Nitrado client at R3, which refuses every write (403, counted) | `/api/runtime/status` → `build.nitradoSource = "fixture"`; R3's `/_fixture/state` → `refused_writes` stays 0 unless a write was attempted |
| No production Stripe credentials | `STRIPE_SECRET_KEY` and `STRIPE_WEBHOOK_SECRET` unset. The code refuses `sk_live_`/`rk_live_` keys when `APP_ENV=staging` | Variable-name listing shows no `STRIPE_*`; startup would fail with a live key |
| Synthetic ADM only | R3 generates ADM lines on request (`/_fixture/kills`, `/_fixture/deaths`, `/_fixture/restart`) | R3 state; the bot's selected ADM path is `/games/fixture/noftp/dayzps/config/...` |
| Fixture cannot be used outside staging | `NITRADO_API_BASE_URL` is accepted only with `APP_ENV=staging` (allowlist). Any other value, including unset, refuses to start | Unit tests (`TestNitradoAPIBaseURLOnlyInStaging`, `TestLoadRefusesNitradoOverrideOutsideStaging`) |

Production credentials are never reused to complete staging. If a D1/D2 identity is not available, staging stays blocked.

## 4. Configuration

### R1 `reliability-qa-bot`

| Variable | Value | Notes |
|---|---|---|
| `APP_ENV` | `staging` | required for the fixture override; enables the live-Stripe-key refusal |
| `KILLFEED_DELIVERY_MODE` | `immediate` | **staging only** |
| `NITRADO_API_BASE_URL` | `http://reliability-qa-nitrado-fixture.railway.internal:8080` | fixture |
| `DATABASE_URL` | `${{reliability-qa-postgres.DATABASE_URL}}` | the private URL; never `DATABASE_PUBLIC_URL` |
| `DISCORD_TOKEN` | D1 bot token | secret, entered by the owner in Railway |
| `DISCORD_APPLICATION_ID` | D1 application ID | |
| `DISCORD_GUILD_ID` | D2 guild ID | |
| `CREDENTIAL_ENCRYPTION_KEY` | a **new** random key | never production's |
| `WEBSITE_API_SECRET` | a **new** random value | bearer for `/api/runtime/status` |
| `CHAMPION_ADMIN_DISCORD_IDS` | the owner's Discord user ID | `/admin` access in D2 |
| `DISCORD_GUILD_MEMBERS_INTENT_ENABLED` | `true` only if D1 has the intent enabled | role and link tests need it |
| `NITRADO_TOKEN`, `NITRADO_SERVICE_ID` | unset | the server is connected through `/server connect` with a non-credential string; the fixture accepts any token |

**Must be unset on R1:**

* `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `CHAMPION_BILLING_PLANS_JSON`;
* `CHAMPION_SHOP_CANARY_EXECUTION`, `CHAMPION_SHOP_CANARY_INSTALLATION_IDS`;
* `DATABASE_PUBLIC_URL`, `KILLFEED_CHANNEL_ID`, `LIVE_SYNC_WATCHERS`, `CASE_EVIDENCE_*`;
* any reference to the billing QA service's variables.

### R3 `reliability-qa-nitrado-fixture`

| Variable | Value |
|---|---|
| `PORT` | `8080` |
| `FIXTURE_SERVICE_ID` | `90000001` |
| `FIXTURE_CONTROL_TOKEN` | a new random value (required on `/_fixture/*`) |
| `FIXTURE_KILLS_PER_MINUTE` | unset (events only on request) |

Build it with the `Dockerfile.nitrado-fixture` Dockerfile path.

### Production

Production must keep `KILLFEED_DELIVERY_MODE` **unset** (rotating mode, unchanged) and `NITRADO_API_BASE_URL` **unset**.

* If it were ever set in production, the service refuses to start unless `APP_ENV=staging`.
* After an approved production deploy, `/api/runtime/status` shows `build.killfeedDeliveryMode = "rotating"` and `build.nitradoSource = "nitrado"`.
* This session did not read production configuration. A names-only listing of the production service's variables needs owner approval.

## 5. Commit to pin: the commit that adds this document, not `41fd618`

`41fd618` is missing fixes that the Phase 4 gates depend on:

| Missing from `41fd618` | Effect on the gates |
|---|---|
| Immediate-mode fixes from `096f90e` | Immediate mode silently dropped cards under backlog, could post a duplicate on a timeout retry, and delivered out of order after a transient failure. Gates 1, 3 and 6 fail. |
| Feed journal (this change) | A crash loses queued cards. Gate 5 fails. |
| Nitrado fixture and `NITRADO_API_BASE_URL` (this change) | No synthetic ADM source; staging would need a real Nitrado service and credentials. Phase 2 cannot be met. |
| Same-second ADM growth fix (this change) | New kill lines can wait for the next ADM write (minutes on a quiet server). Gate 6 is at risk. |
| Build identity in `/api/runtime/status` (this change) | The pinned commit and mode cannot be verified without shell access. |

## 6. Defects found and fixed in this pass

Each was reproduced by a test that fails on the previous code and passes now.

| # | Defect | Fix | Tests |
|---|---|---|---|
| 1 | **Gate 5: a restart (crash) loses queued immediate-mode cards**, and cards still shown by the previous process stay in the channel, so the window can exceed 10 | Migration `0057_discord_feed_cards` (new table only) and the feed journal. Immediate mode records each card before posting, marks it posted and removed. The next start replays unconfirmed cards in order with their original nonce, and takes back or removes the previous process's shown cards. Cards older than 1 h are not posted; they are marked dropped, counted and logged. A rotating process (rollback) removes the immediate cards and drains the journal. | `TestJournalCrashReplaysQueuedCards`, `TestJournalCrashAfterCreateDoesNotDuplicate`, `TestJournalRestartKeepsNewestTenVisible`, `TestJournalStaleQueuedCardIsDroppedAndCounted`, `TestJournalRollbackToRotatingCleansImmediateCards`, `TestJournalFailureDoesNotBlockDelivery`, `TestFeedCardJournalLifecycle` (Postgres) |
| 2 | A card enqueued while a posting pass was already running could be posted **before** it was journaled, so a restart would replay it as a duplicate (found by the race-detector runs) | The posting loop journals new cards before each post | the journal tests above, run 30 times under `-race` |
| 3 | **Gate 6: ADM growth within the same second as the last read is not read** until the next write in a later second. Nitrado's `modified_at` has one-second resolution. This also affects the production default path. | Re-read when the listed size exceeds the size seen at the last read, at most once per distinct listed size, so a listing that overstates the file cannot cause a download every poll | `TestEngineReadsGrowthWithinSameSecond` (including the overstated-listing case), `TestEngineReadsNitradoFixture` |
| 4 | Staging could not run the ADM pipeline without real Nitrado credentials | Read-only Nitrado fixture (`internal/nitrado/nitradofixture`, `cmd/nitrado-fixture`); `NITRADO_API_BASE_URL` accepted only with `APP_ENV=staging` | `TestEngineReadsNitradoFixture`, `TestFixtureControlRequiresTokenAndWritesAreRefused`, `TestAPIBaseURLOverrideAppliesOnlyToDefaultClients`, config tests |
| 5 | A staging service could start with a live Stripe key | `APP_ENV=staging` refuses `sk_live_` / `rk_live_` | `TestStagingRefusesLiveStripeKey` |
| 6 | The deployed commit and mode were not observable | `build` block in `/api/runtime/status`; one `feed card delivered` log line per card in immediate mode, with `queue_ms`, `discord_ms` and `detect_to_publish_ms` (for live p95) | `TestRuntimeBuildReportsDeploymentIdentity` |

Journal cost per card (one insert plus one update on the feed goroutine, local Postgres 16, 300 cards): **p50 0.65 ms, p95 1.38 ms, p99 2.39 ms.**

## 7. Release gates

"Harness" means the production code with the real discordgo client against the emulated Discord API. Harness results are synthetic; they are **not** live results. Every gate stays **NOT TESTED live** until the staging run.

| # | Gate | Harness evidence | Harness | Live |
|---|---|---|---|---|
| 1 | 15 distinct kills → exactly the newest 10 visible | `TestImmediateRollingWindowChannelState` (channel = kill-06…kill-15, exactly 15 creates); `TestJournalRestartKeepsNewestTenVisible` (same across a restart) | PASS | NOT TESTED |
| 2 | Deletion failures observable and recoverable | `TestImmediateCleanupFailureIsRecordedAndRecovered` (`cleanup_failures`, orphan retried, window back to 10) | PASS | NOT TESTED |
| 3 | Ordering survives retries | 5xx and timeout subtests of `TestImmediateFailureRecovery`; the staging-equivalent run (60/60 and 120/120 in order) | PASS | NOT TESTED |
| 4 | Deathfeed independent | `TestDeathFeedIndependentInSharedChannel` (shared channel: 10 kills + 3 deaths, evictions never cross feeds, separate journals and ledger routes); the harness uses separate channels | PASS | NOT TESTED |
| 5 | Restart does not silently lose queued events | `TestJournalCrashReplaysQueuedCards`, `TestJournalCrashAfterCreateDoesNotDuplicate`, stale cards counted and logged | PASS | NOT TESTED |
| 6 | Latency targets | Synthetic, normal load: queue p95 **737 ms** (< 2 s); detect→publish p95 **875 ms** (< 5 s); 60/60 exactly once. Stress (~30 events/s): 120/120 exactly once but p95 13.6 s (saturation above Discord's per-channel rate) | PASS normal / FAIL stress | NOT TESTED |
| 7 | Unsetting the flag restores rotating mode after a restart | `TestFeedDeliveryModeFromEnvironment`, `TestRollbackImmediateToRotating`, `TestJournalRollbackToRotatingCleansImmediateCards` | PASS | NOT TESTED |
| 8 | Full rollback procedure | Code rollback: `605f1ec`'s full integration suite on the RC schema (`0001`–`0057`): 41 packages, **1,712 tests, 0 failures**; flag rollback: gate 7 tests; see section 10 | PASS | NOT TESTED |

Note on gate 1: with a `KILLFEED` route, the death feed posts into the killfeed channel with its own window. Such a channel shows the newest 10 kills **plus** up to 10 deaths. Gate 1 is measured with kills only.

## 8. Validation procedure (after approval)

Every step is read-only toward production. `$QA` means commands run with R1's environment, for example `railway run --service reliability-qa-bot -- sh -c '…'`, so secrets stay in the environment and are never printed.

1. **Preflight** (section 1) and the isolation checks (section 3), with results recorded. Stop if any fails.
2. **Create** R2, R3 and R1, in that order, with the section 4 variables. Before R1's first deploy:
   * confirm R2 is empty (`schema_migrations` absent);
   * confirm R1's `DATABASE_URL` host is R2's.
3. **Deploy** R1 at the pinned SHA. The bot migrates R2 (`0001`–`0057`) on startup; confirm `SELECT max(name) FROM schema_migrations` on R2 = `0057_discord_feed_cards`.
4. **Identity:** `GET /api/runtime/status` (bearer `WEBSITE_API_SECRET`) must return:
   * `build.commit` = pinned SHA;
   * `build.appEnv` = `staging`;
   * `build.killfeedDeliveryMode` = `immediate`;
   * `build.nitradoSource` = `fixture`.
5. **Set up D2:** `/setup`, then `/server connect` with the non-credential token `fixture` for service `90000001`, then a killfeed route. Confirm the worker selected `/games/fixture/noftp/dayzps/config/…ADM`.
6. **Gates.** The fixture is controlled from inside the project, for example `railway ssh --service reliability-qa-nitrado-fixture`, then `wget -qO- --post-data= --header "X-Fixture-Token: $FIXTURE_CONTROL_TOKEN" "http://localhost:8080/_fixture/kills?n=15"`.
   * **G1:** inject 15 kills. Read the channel (`GET /channels/{id}/messages?limit=50` with the QA token): exactly 10 cards, the newest 10 victims in order, and `KILLFEED.delivered` = 15 in the ledger.
   * **G2:** a bot may always delete its own messages, so removing Manage Messages does not cause a failure. Live checks:
     * **(a)** delete one card by hand: the feed treats Discord's "unknown message" as already removed, with no error loop, and the next post keeps 10 cards.
     * **(b)** let the window go quiet until its cards are due to expire (one interval, 10 minutes), deny the bot View Channel on that channel just before expiry, and confirm `cleanup_failures` and `orphaned_cards` rise. Restore the permission: the orphans are removed on the retry backoff and `orphaned_cards` returns to 0.

     Injected 5xx delete failures are covered only by the harness.
   * **G3:** revoke Send Messages, inject 5 kills (config fault: one failed request, then a pause), restore the permission, and confirm all 5 arrive in order after the pause (at most 5 minutes). Discord 5xx errors and timeouts cannot be induced live; they are covered by the harness.
   * **G4:** inject 3 deaths and 12 kills. The channel shows 10 kills plus 3 deaths (with a route) and the ledger counts both routes separately.
   * **G5:** revoke Send Messages, inject 3 kills (the cards are journaled; the channel is paused), **restart R1** (a Railway restart is a graceful SIGTERM), then restore the permission. The 3 cards are posted once each by the new process (`replayed` = 3), and the previous window is kept at 10. A hard crash cannot be induced safely on Railway; the harness covers it.
   * **G6:** inject 60 kills at about 1 per second, then compute p50/p95/p99/max of `queue_ms` and `detect_to_publish_ms` from the `feed card delivered` log lines. Also measure end-to-end: Discord message timestamp minus injection time (includes the 10 s ADM poll). Targets: queue p95 < 2 s; detect→publish p95 < 5 s; exactly once.
   * **G7:** unset `KILLFEED_DELIVERY_MODE` on **R1 only** and restart. Expect:
     * `build.killfeedDeliveryMode` = `rotating`;
     * the immediate cards are removed at startup;
     * new kills post only at the 10-minute cycle;
     * the journal has no open rows.

     Set the variable back to `immediate` afterwards if more testing follows.
   * **G8:** run section 10 against R1 and R2 only.
7. Record every result as PASS, FAIL, BLOCKED or NOT TESTED. A staging PASS is not evidence of production recovery.

## 9. Expected impact

* **Production:** none. No production service, variable, database, guild or channel is touched. Production keeps `KILLFEED_DELIVERY_MODE` unset.
* **Billing QA service:** none. It is not modified and nothing references its variables. The new services have distinct names; no new environment is forked.
* **Cost:** three small usage-billed services (a bot, an idle fixture, a Postgres with a small volume) for the duration of validation. Tear them down afterwards: delete R1, R3 and R2 (with its volume), then D1 and D2 when no longer needed.
* **Discord:** only D1 and D2, which are new and staging-owned.

## 10. Rollback procedure

| Level | Steps | Data | Verified by |
|---|---|---|---|
| Flag | Unset `KILLFEED_DELIVERY_MODE`, then restart | The rotating process removes the immediate cards and drains the journal (queued cards go to its cycle) | `TestJournalRollbackToRotatingCleansImmediateCards`, `TestRollbackImmediateToRotating` |
| Code | Redeploy `605f1ec` | Migrations `0056` and `0057` stay (additive; `605f1ec` never reads them; no down-migration is needed). Up to 10 immediate cards per feed from the last RC process stay in the channel, because `605f1ec` does not know the journal. Delete them manually, or do a flag rollback first, which removes them. | `605f1ec`'s full integration suite on the RC schema (`0001`–`0057`) |
| Schema (optional, not recommended) | `DROP TABLE discord_feed_cards; DELETE FROM schema_migrations WHERE name = '0057_discord_feed_cards';` only after the code rollback is confirmed and the owner approves | The journal is lost (no production data) | — |

Order for a staging rollback test: flag rollback first, then code rollback.

## 11. Approval needed

The following need an explicit yes from the owner:

1. Network access for `backboard.railway.app` (and `discord.com`), plus a read-only Railway token for the preflight.
2. Creating R1–R3 in `champions-case-staging` (billable), or a separate project if the preflight finds shared-variable risk.
3. The D1 bot application and D2 guild (owner-created; tokens entered directly in Railway).
4. The pinned SHA and the `staging/reliability-rc` branch (or `railway up` instead).
5. Running migrations `0001`–`0057` on R2 only.

Production deployment is out of scope. It needs the live staging PASS, the read-only production evidence SQL, and a separate approval.
