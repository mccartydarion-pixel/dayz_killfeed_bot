# Staging access and deployment preflight (PR #108)

**Nothing was created, deployed, merged or modified.** No secret value was read or printed. The billing QA service, production and Cloudflare were not touched.

Requested commit: `096f90e`. The newer commit `4fb68c2` on the same PR changes the recommendation; see section 6.

## 1. Access: available vs missing

| Access path | Status | Evidence (this session) |
|---|---|---|
| Railway API (`backboard.railway.app`, `railway.app`) | **MISSING**: network policy denies it | the egress proxy answers 403 to CONNECT (`connect_rejected`) |
| Railway CLI | **MISSING** | not installed; no `~/.railway` |
| Railway token (`RAILWAY_TOKEN` / `RAILWAY_API_TOKEN`) | **MISSING** | no such environment variable |
| Railway connector / MCP tool | **MISSING** | connectors installed for the org: Gmail, Google Calendar, Google Drive (all not connected); no Railway connector |
| Discord API (`discord.com`) | **MISSING**: network policy denies it | proxy 403 (`connect_rejected`) |
| Discord bot token for staging | **MISSING** | no `DISCORD_*` environment variable |
| Staging database URL | **MISSING** | no `DATABASE_URL` / `TEST_DATABASE_URL` for staging |
| Nitrado test credentials | **MISSING** (not needed from `4fb68c2`: fixture) | no `NITRADO_*` variable |
| Stripe | not needed (billing stays unconfigured) | — |
| GitHub (repo, PR, CI) | **AVAILABLE** | the GitHub tools work; CI on `096f90e` and `4fb68c2` is green |
| Local Postgres 16, Go toolchain | **AVAILABLE** | used for the regression tests below |

**To unblock:**

1. In the environment's network settings, allow `backboard.railway.app` (and `discord.com` to observe the staging guild).
2. In the environment's settings, add a Railway **project token** scoped to `champions-case-staging` as `RAILWAY_TOKEN`. Never paste it in chat.

   Railway tokens should be treated as write-capable: I know of no read-only token type. A project token is limited to one project and environment, so it cannot reach the production project; an account or team token could.
3. Staging Discord and database credentials are needed only for the live run, entered directly in Railway. They are never needed in this session.

## 2. Inspection of `champions-case-staging`: NOT POSSIBLE

The project could not be listed, so its services, environments, variables and permissions are **unknown**. Nothing here is inferred as fact.

Once access exists, the inspection is read-only and prints names only:

```sh
railway status
railway service list
railway variables --service <billing-qa> --kv | cut -d= -f1 | sort
railway variables --kv | cut -d= -f1 | sort    # project-level shared variables
```

**Stop conditions.** Use a separate project `champions-reliability-staging` instead if either of these is found:

* the billing QA service has a public domain or variables that a new service would inherit;
* shared variables contain Discord, database, Nitrado or Stripe credentials.

Never create a new Railway *environment* in that project: it forks every existing service, including billing QA.

## 3. Required dedicated resources

| Resource | Name | Notes |
|---|---|---|
| Railway service | `reliability-qa-bot` | this repo, `Dockerfile`, pinned commit |
| Railway Postgres | `reliability-qa-postgres` | new, empty volume; private network only |
| Railway service (from `4fb68c2`) | `reliability-qa-nitrado-fixture` | `Dockerfile.nitrado-fixture`; private only |
| Discord application + bot | "Champion Reliability QA" | new application, never the production bot |
| Discord server (test guild) | "Champion Reliability QA" | owner-created; only the QA bot and testers |
| Channels | killfeed (text), online counter (voice), link-gamertag (text), optional deathfeed (text) | created by `/setup` inside the test guild |
| Role | "Verified" (created by `/setup`) | the QA bot's role must sit above it |
| Nitrado | **`096f90e`:** a test DayZ server on a *separate* Nitrado account and its token. **`4fb68c2`+:** none (read-only fixture) | a Nitrado token can restart or stop its server; `096f90e` cannot avoid a write-capable Nitrado credential |
| Credentials to generate | `CREDENTIAL_ENCRYPTION_KEY`, `WEBSITE_API_SECRET` (and `FIXTURE_CONTROL_TOKEN` from `4fb68c2`) | new random values; never production's |

## 4. Minimum permissions

**Railway:** a project token for `champions-case-staging` only (section 1). The owner creates the services; if Claude runs the preflight and deploy, it needs the same token and nothing account-wide.

**Discord, QA application:**

* Privileged intent: **Server Members** (the code requests `GUILD_MEMBERS` for welcome and verified-role delivery). Not needed: Message Content, Presence.
* Other intents requested in code: Guilds, Guild Messages.
* Bot permissions in the test guild only:

  | Permission | Why |
  |---|---|
  | View Channel, Send Messages, Embed Links, Read Message History | feeds, panels |
  | Manage Messages | bulk-deleting feed cards (single deletes of its own messages do not need it) |
  | Manage Channels | `/setup` creates channels; online-counter rename |
  | Manage Roles | Verified role (the bot's role must be above it) |
  | Connect | counter voice-channel checks |
  | `applications.commands` scope | slash commands |

  **Not** Administrator.

**Database:** the bot's own Postgres user on `reliability-qa-postgres` (owner of that database only; it runs migrations `0001`–`0057` at startup).

**Nitrado (`096f90e` only):** a token for the separate test account. Nitrado offers no read-only scope that this code is known to work with, so treat it as write-capable on that test server.

## 5. Production cannot be modified: why, and how it is checked

| Production resource | Why the staging deployment cannot touch it | Check before any gate |
|---|---|---|
| Discord (production guild and channels) | The QA bot is a different application and is never invited to the production guild; Discord rejects any request there | `GET /users/@me/guilds` with the QA token lists only the test guild |
| Database | `DATABASE_URL` references only `reliability-qa-postgres`; migrations run only against it | masked host of `DATABASE_URL` = `reliability-qa-postgres.railway.internal`; `schema_migrations` absent before the first deploy |
| Nitrado | `096f90e`: only the separate test account's token. `4fb68c2`+: no token; `NITRADO_API_BASE_URL` → fixture, accepted only with `APP_ENV=staging` | `/api/runtime/status` → `build.nitradoSource = "fixture"` (`4fb68c2`+) |
| Stripe | no Stripe variables; `4fb68c2`+ refuses `sk_live_`/`rk_live_` with `APP_ENV=staging` | names-only listing shows no `STRIPE_*` |
| Railway (production project `genuine-education`) | the token is a project token for `champions-case-staging` | the token's scope, shown by `railway status` |

## 6. Deployment configuration

### 6a. Exactly as requested: commit `096f90e`

Service `reliability-qa-bot`:

| Variable | Value |
|---|---|
| `KILLFEED_DELIVERY_MODE` | `immediate` (**staging only**) |
| `APP_ENV` | `staging` (informational at `096f90e`: no guard reads it) |
| `DATABASE_URL` | `${{reliability-qa-postgres.DATABASE_URL}}` |
| `DISCORD_TOKEN` | QA bot token (secret, entered by the owner) |
| `DISCORD_APPLICATION_ID` | QA application ID |
| `DISCORD_GUILD_ID` | test guild ID |
| `DISCORD_GUILD_MEMBERS_INTENT_ENABLED` | `true` (intent enabled in the portal) |
| `CREDENTIAL_ENCRYPTION_KEY` | new random key |
| `WEBSITE_API_SECRET` | new random value |
| `CHAMPION_ADMIN_DISCORD_IDS` | owner's Discord user ID |
| `NITRADO_TOKEN`, `NITRADO_SERVICE_ID` | unset; the test server is connected with `/server connect` using the separate account's token |

**Must be unset:** `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `CHAMPION_BILLING_PLANS_JSON`, `CHAMPION_SHOP_CANARY_*`, `DATABASE_PUBLIC_URL`, `KILLFEED_CHANNEL_ID`, `LIVE_SYNC_WATCHERS`, `CASE_EVIDENCE_*`.

**Production:** `KILLFEED_DELIVERY_MODE` stays **unset**.

**Known consequences of pinning `096f90e`:**

* Crash loss is present (section 8).
* No synthetic ADM source, so a real test Nitrado server and a write-capable token are required.
* No build block, so the deployed commit cannot be read back from `/api/runtime/status`.
* There is no guard against a live Stripe key.

### 6b. Recommended: `4fb68c2` or the commit adding this report

The runtime code is identical; this commit adds only tests and documentation. The configuration is the same as 6a, plus:

| Variable | Value |
|---|---|
| `APP_ENV` | `staging` (required: gates the fixture override and refuses a live Stripe key) |
| `NITRADO_API_BASE_URL` | `http://reliability-qa-nitrado-fixture.railway.internal:8080` |

Service `reliability-qa-nitrado-fixture`:

* `PORT=8080`
* `FIXTURE_SERVICE_ID=90000001`
* `FIXTURE_CONTROL_TOKEN=<new random>`
* Dockerfile path `Dockerfile.nitrado-fixture`
* no public domain

The test server is then connected with `/server connect` using the non-credential token `fixture`. No Nitrado credential exists anywhere.

Migration `0057_discord_feed_cards` (new table) runs on the staging database only.

## 7. Actions requiring explicit approval

Nothing below has been done.

1. Network allowlist: `backboard.railway.app` (+ `discord.com`).
2. Add a `champions-case-staging` project token as `RAILWAY_TOKEN` in the environment settings.
3. Read-only preflight (section 2). If a stop condition is hit, create a separate project instead (needs its own approval).
4. Owner creates the QA Discord application and test guild; tokens go directly into Railway.
5. Create `reliability-qa-postgres` (billable).
6. `4fb68c2`+ only: create `reliability-qa-nitrado-fixture` (billable).
7. Create `reliability-qa-bot` with the section 6 variables (billable).
8. Choose the pin: `096f90e` (6a) or `4fb68c2`/this commit (6b, recommended). Then either create a branch `staging/reliability-rc` at that SHA for Railway to track, or use `railway up` from a clean checkout.
9. First deploy: runs migrations `0001`–`0056` (`096f90e`) or `0001`–`0057` (6b) on `reliability-qa-postgres` only.
10. Live gate runs (`2026-09-26-staging-infrastructure.md` section 8), including temporary permission changes on test-guild channels and restarts of `reliability-qa-bot`.
11. Teardown after validation.

Production deploys are out of scope and need separate approval.

## 8. Production-readiness assessment: crash loss and the 11-card window

### Crash loss

| Commit | Status | Demonstrated by |
|---|---|---|
| `096f90e` | **NOT FIXED.** Queued immediate-mode cards live only in memory; a crash loses them from Discord (the kill/death events stay durable in the database). | `TestImmediateFailureRecovery` / "crash" subtest (the undelivered card exists only in memory). `TestJournalSurvivesSIGKILL` **fails** when the journal is disabled: nothing is recorded, so nothing survives the kill. |
| `4fb68c2`+ | **FIXED in tests; not live-verified.** Each card is journaled in Postgres before it is posted, and the next start replays it. | **`TestJournalSurvivesSIGKILL`** (integration, real Postgres): a child process running the feed is killed with **SIGKILL** (no shutdown, no flush) with three cards queued, the first already created in Discord but unconfirmed. The next process delivers all three exactly once, in order: kill-01 is not duplicated (nonce), and the ledger reports `replayed = 3`. Passed 5/5. Also the harness tests `TestJournalCrashReplaysQueuedCards` and `TestJournalCrashAfterCreateDoesNotDuplicate`. |

Residual risk at `4fb68c2`+ (not covered by a fix):

* A card enqueued but not yet journaled at the instant of the kill is still lost. This window is the time until the feed goroutine's next journal insert, about 1 ms (p95 1.4 ms per card, measured locally).
* A replay after longer than Discord's `enforce_nonce` window could duplicate a card that was created but never confirmed.
* Cards queued for longer than 1 h are not replayed: they are marked dropped, counted and logged.
* If the journal database is unavailable, delivery continues from memory (counted as `journal_failures`), and crash protection is lost for that period.

**Production default (rotating mode) is not changed by either commit.** Its pre-existing crash exposure is the in-memory batch: up to one 10-minute cycle of cards, which a SIGKILL loses. A graceful restart flushes it.

**Verdict:** crash loss is acceptable for a **staging** trial on `4fb68c2`+. It is **not production-ready** until the live staging G5 run passes. `096f90e` should not be used where crash loss matters.

### Temporary 11-card window

> **Update (`a8e9040`–`05e02c9`, owner commits):** immediate mode now deletes the oldest card **before** posting when the window is full, and a failed deletion blocks the next post. `TestImmediateWindowNeverExceedsTenCards` asserts a peak of 10; it fails on the earlier code, where the peak was 11. `TestImmediateCleanupFailureIsRecordedAndRecovered` and `TestJournalStrictCapacityBlocksUntilOldestRemoved` cover the blocking. This is fixed in tests, not live-verified; see `2026-09-26-phase3-staging-deployment.md`. The assessment below describes `6aab65d` and earlier.

**NOT FIXED up to `6aab65d`.** Immediate mode creates the new card, then deletes the oldest; Discord has no atomic swap.

| Case | Behaviour | Demonstrated by |
|---|---|---|
| Normal eviction | a full channel shows **11** cards for about one Discord request per new card | **`TestImmediateWindowPeaksAtElevenCards`**: 15 kills, peak visible = 11, settles to the newest 10 |
| Delete fails | the 11th card stays until the orphan retry succeeds (backoff 2 s → 60 s, up to 5 attempts). After that it is left in place, logged "manual cleanup needed" and counted in `abandoned_cleanup`. | `TestImmediateCleanupFailureIsRecordedAndRecovered` |
| Kill and death feeds share a route | the channel shows up to 10 kills **plus** up to 10 deaths (separate windows) | `TestDeathFeedIndependentInSharedChannel` |

**Possible fix (not implemented; owner decision):** delete the oldest card *before* posting the new one when the window is full. The channel would never exceed 10, but it would dip to 9, and if the post then fails it shows 9 until the retry succeeds.

**Verdict:** a cosmetic, bounded limitation. It is acceptable for staging and, in my assessment, for production if the brief's "exactly 10" is read as "at rest". It is **not** acceptable if the requirement is "never more than 10 at any instant"; that needs the change above and a new test.

### Overall

**NOT READY for production.** Live isolated staging has not run (access missing, sections 1–2). The read-only production evidence SQL and owner approval are still outstanding.
