# P0 release candidate: staging verification report

**Candidate:** PR #108 at `3cc3748`, plus this verification commit (tests and docs only; no runtime code changed).

## Staging verdict: **NOT RUN (BLOCKED)**

**A staging deployment was not performed.** Staging isolation could not be demonstrated from the verification environment:

* No Railway CLI or API access, and no staging service definition in the repository.
* No staging `DATABASE_URL`, `DISCORD_TOKEN`/`DISCORD_GUILD_ID` or `NITRADO_TOKEN` is available, so nothing proves they are separate from production.

Per the verification brief, work stopped before deployment. **No production or staging service, database, Discord channel, role or player account was touched.**

Everything below was verified on **isolated fixtures**:

* local PostgreSQL 16 databases created for this run;
* the real application code;
* the real discordgo client pointed at a local fake Discord API;
* scripted Nitrado responses.

These results are strong evidence. They are **not** a substitute for the staging gates listed at the end, and they are **not** evidence of production recovery.

## 1. Staging readiness: what a real staging service needs

The runtime serves exactly **one Discord guild per process** (`DISCORD_GUILD_ID`), runs **all migrations at startup**, and talks to Nitrado, Discord and Stripe. A staging service is isolated only if every row below holds.

| Requirement | How to demonstrate |
|---|---|
| Release-candidate commit | the Railway staging deployment's commit SHA equals the PR #108 head |
| Own database | `DATABASE_URL` host/database differs from production's; `SELECT current_database(), inet_server_addr()` differs |
| Own Discord bot + test guild | `DISCORD_TOKEN` is a separate staging application (a different `DISCORD_APPLICATION_ID`); `DISCORD_GUILD_ID` is a guild containing no production players; the bot is a member of no production guild |
| No production channels or roles | follows from the above: every channel/role ID in the staging guild setup belongs to the staging guild. The counter now also refuses channels of another guild (`CHANNEL_NOT_OWNED`) |
| Nitrado read-only | the app only issues GETs. A Nitrado token is not scope-limited to reads, so use a **test service** or a token owned by a test account. `NITRADO_SERVICE_ID` must be a test server |
| Billing | `STRIPE_SECRET_KEY` is a `sk_test_` key; `STRIPE_WEBHOOK_SECRET` is the test endpoint's |
| Required variables present | `APP_ENV`, `DATABASE_URL`, `DISCORD_TOKEN`, `DISCORD_APPLICATION_ID`, `DISCORD_GUILD_ID`, `NITRADO_TOKEN`, `NITRADO_SERVICE_ID`, `CREDENTIAL_ENCRYPTION_KEY`, `WEBSITE_API_SECRET` (set/unset only, never printed); `KILLFEED_DELIVERY_MODE` **unset** for the default-behavior pass |

To give this environment staging access, add **read-only** credentials as environment variables in the cloud environment settings (for example a Railway token scoped to the staging project). Never paste them into chat.

## 2. Migration 0056: VERIFIED (isolated database)

Procedure:

1. Build a database with the **deployed `605f1ec` code** (schema `0055`).
2. Seed links as that code writes them: 3 VERIFIED, 2 PENDING, 1 UNLINKED, and their challenges.
3. Apply the RC migrations.
4. Run the deployed code's suite against the result.

| Check | Result |
|---|---|
| Existing `player_links` (all original columns) | **byte-identical** (md5 `0baff967…` before and after) |
| `link_verifications` | byte-identical (md5 `8cb4439e…`) |
| VERIFIED status preserved | 3/3 VERIFIED; `role_sync_status` NULL, `role_sync_attempts` 0 |
| New columns | `role_sync_status`, `role_sync_error`, `role_sync_last_attempt_at`, `role_synced_at` are nullable; `role_sync_attempts` is `NOT NULL DEFAULT 0` (inserts that omit it get 0, so old code is compatible) |
| Index | `idx_player_links_role_pending` (partial: VERIFIED and PENDING/FAILED) |
| PENDING / FAILED behavior | the RC role-sync suite passes on this migrated database. The seeded legacy links stayed NULL (never reconciled) |
| **Previous version on the new schema** | `605f1ec`'s **full integration suite passes against the 0056 schema: 41/41 packages, 1,712 tests, 0 failures**. The old code ignores the extra `schema_migrations` row and never reads the new columns |

## 3. Player synchronization: PASS (fixtures)

`internal/app/player_sync_matrix_test.go` drives real parsed ADM files, a scripted Nitrado and the real counter loop. Count, confidence (`known`), source and freshness are asserted independently.

| Scenario | Count | Known | Source | ADM evidence |
|---|---|---|---|---|
| 0 players | 0 | yes | NITRADO_QUERY | UNKNOWN |
| 1 player | 1 | yes | NITRADO_QUERY | UNKNOWN |
| 2 players | 2 | yes | NITRADO_QUERY | UNKNOWN |
| duplicate connect lines (Nitrado down) | 2 | yes | ADM_PLAYER_LIST | SNAPSHOT_CONFIRMED |
| disconnect | 1 | yes | ADM_PLAYER_LIST | SNAPSHOT_CONFIRMED |
| reconnect | 2 | yes | ADM_PLAYER_LIST | SNAPSHOT_CONFIRMED |
| replayed (identical) reconnect line | 2 | yes | ADM_PLAYER_LIST | SNAPSHOT_CONFIRMED |
| server restart, before reconnects | 0 | yes | ADM_BOOT_RESET | BOOT_RESET |
| restart, one reconnect | 1 | yes | ADM_BOOT_RESET | BOOT_RESET |
| restart, Nitrado back | 1 | yes | NITRADO_QUERY | BOOT_RESET |
| no ADM data, Nitrado fine | 3 | yes | NITRADO_QUERY | UNKNOWN |
| no ADM data, Nitrado down | unknown (last value held) | no | UNKNOWN | UNKNOWN |
| stale Nitrado: previously 5, now failing | unknown (5 never reused) | no | UNKNOWN | UNKNOWN |
| Nitrado answer for another service | unknown | no | UNKNOWN | UNKNOWN |
| Nitrado restarting (no query) | unknown | no | UNKNOWN | UNKNOWN |
| conflict: Nitrado 2 vs ADM list 1 | 2 (Nitrado) + disagreement recorded | yes | NITRADO_QUERY | SNAPSHOT_CONFIRMED |

For the restart, the engine's own boot scan drained the old boot, selected the newer one, cleared the previous 2 players and fired `OnNewBoot`.

## 4. Discord counter: PASS (real discordgo client, fake Discord API)

`internal/discord/counter_http_test.go` uses the production `SessionAPI` over a real `discordgo.Session` pointed at a local fake API.

| Behavior | Result |
|---|---|
| Legacy name `Online Players: 4` migrated to `🟢・Online: 4/18`; unchanged count ⇒ no rename | pass |
| 404 / 10003 ⇒ `CONFIG_FAULT UNKNOWN_CHANNEL`; **0 further requests** over 10 presence changes | pass |
| 403 / 50013 ⇒ `CONFIG_FAULT MISSING_PERMISSIONS`; 0 further requests | pass |
| Permanent-fault recovery: `/setup repair` binds a working channel ⇒ renamed, health OK | pass |
| Ownership: channel of another guild ⇒ `CHANNEL_NOT_OWNED`; text channel ⇒ `WRONG_CHANNEL_TYPE`; no rename | pass |
| 429 with a 30 s Retry-After ⇒ returns in milliseconds, `RATE_LIMITED` (not a fault), retry scheduled | pass |

Also covered by existing tests:

* **No duplicate channel:** setup/repair reuse the counter in every name format.
* **Route precedence:** a route lookup error never falls back to the legacy channel.
* **Non-blocking:** the ADM callback only pokes the counter loop.

## 5. PSN linking: PASS (real PostgreSQL)

* 5-minute requirement: pending at ≥ 5 min; shortfall under 5 min.
* **Best single server:** 3 + 3 minutes on two servers is rejected; 6 minutes on one of two servers is accepted.
* Dashboard `ONLINE`/`OFFLINE` servers resolve.
* No server ⇒ SERVER NOT CONNECTED.
* Guild isolation holds.
* Automatic disconnect/reconnect challenge ⇒ VERIFIED ⇒ role ASSIGNED.
* **Pending challenge survives an application restart:** started in one service instance, completed in another.
* **Admin manual fallback** ⇒ VERIFIED + role; it refuses when there is no pending request, so the 5-minute rule still holds.
* Failed role ⇒ FAILED, then a **restarted instance re-delivers it once**; ASSIGNED is never retried; MEMBER_GONE is terminal; with no role configured the link stays PENDING and then delivers once one is configured.
* **No production player roles were granted:** every role assignment went to a recording fake.

## 6. Killfeed delivery: rotating vs immediate: PASS (fixtures)

`internal/discord/killfeed_mode_e2e_test.go`: real ADM lines → real engine (parse, dedupe) → real persistence queue (durable ack before publish) → `RotatingFeed` in each mode → fake Discord with 20 ms per call. The 10-minute cycle is scaled to 400 ms.

| Preserved in immediate mode | Result |
|---|---|
| Kill ordering | `Victim1, Victim2, Victim3` in both modes |
| Deduplication | a replayed kill line is posted once in both modes |
| Deathfeed | the death goes to the death channel (`DEATH_FEED` ledger route) in both modes |
| Existing embeds | the posted embed is exactly the card the publisher built |
| C.A.S.E. input | identical persistence in both modes (3 kills, 1 death). Delivery mode acts only after the durable write |
| Rolling retention | ≤ 10 cards, oldest removed first, expire after one interval (existing tests) |

Stage split (milliseconds, maximum over 4 cards):

| Mode | Detection | Persistence | Queue | Discord delivery |
|---|---|---|---|---|
| rotating (production default) | 0 | 0 | **419** (= one cycle; ≤ 10 min in production) | 21 |
| immediate | 0 | 0 | **19** (behind the previous card's call) | 21 |

Detection here is the poll being run immediately; in production it is bounded by the 10 s poll. Persistence is in-memory here; in production it is database latency. **The production default stays rotating:** `KILLFEED_DELIVERY_MODE` is unset.

## 7. Production evidence (read-only)

`docs/incidents/2026-09-26-evidence-queries.sql` is validated with `ON_ERROR_STOP` against:

* **the deployed schema (0055)**: all 8 queries pass;
* the RC schema (0056) with fixture data: all 8 pass.

A write inside its read-only transaction is rejected by PostgreSQL.

The file was fixed in this pass. Its "new resolver" column still used a pre-consolidation predicate; it now reports `deployed_resolver_matches` against `rc_link_eligible` (active). A public-server selection query was also added. On the fixture it reproduces the root-cause signature: an `ONLINE` server with `deployed_resolver_matches = false` and `rc_link_eligible = true`.

**Procedure** (for someone with production database read access):

1. Connect with a read-only role if one exists; otherwise the file's `BEGIN TRANSACTION READ ONLY` and `ROLLBACK` enforce it (15 s statement timeout).
2. Run `psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v guild="'<DISCORD_GUILD_ID>'" -f docs/incidents/2026-09-26-evidence-queries.sql > evidence-$(date -u +%Y%m%dT%H%MZ).txt`.
3. Also export 24 h of Railway logs for `online counter rename failed`, `component=link`, `stale_probe`.
4. Store both with the incident record before any deploy.
5. **Expected root-cause evidence:**
   * **A:** the legacy `online_players_channel_id` differs from the `ONLINE_COUNTER` route channel.
   * **B:** active server rows have `deployed_resolver_matches = false`.
6. **Abort** the procedure on any `ERROR`. Never remove `READ ONLY`.

## 8. Remaining defects and gaps

* **Staging not run** (this report's blocker).
* The bot status line, server status board and ADM monitor still read the raw ADM tracker (a lower bound after a bot restart).
* A player online across a bot deploy accrues no link playtime until they reconnect (pre-existing).
* VERIFIED links from before 0056 are not role-reconciled automatically; a backfill needs approval.
* "DayZ event → ADM visible" is not exactly measurable (server-local timestamps).
* The live count requires Nitrado to include `service_id`. If it is omitted, the counter falls back to ADM evidence or unknown. Confirm this in staging.

## 9. Rollback readiness: READY

* Redeploy `605f1ec`.
* Proven here: the deployed code passes its full suite on the 0056 schema, so no down-migration is needed.
* The counter channel is renamed back to the legacy format once by the old build.
* `KILLFEED_DELIVERY_MODE` is unset by default.

## 10. Gates

| Gate | Result |
|---|---|
| RC commit identified | PASS (`3cc3748` + test/docs-only verification commit) |
| Staging isolation demonstrated | **FAIL: no staging access** |
| Staging deployment | **NOT RUN** |
| Migration 0056 (isolated DB, deployed-version data) | PASS |
| Old version tolerates new schema | PASS (1,712 tests) |
| Player synchronization matrix | PASS (fixtures) |
| Discord counter (real client, fake API) | PASS |
| PSN linking lifecycle incl. restart and admin fallback | PASS (real Postgres) |
| Killfeed modes + stage split, default unchanged | PASS (fixtures) |
| Evidence SQL validated on deployed and RC schemas | PASS |
| Full suites on fresh isolated DB | PASS (unit 40/40; integration 41/41, 1,839 tests; race) |
| Production recovery | **NOT CLAIMED** |

**Final staging verdict: NOT RUN / BLOCKED on staging access. Fixture verification: PASS.** Production release stays blocked until:

1. staging runs against the isolated services above;
2. the production evidence SQL has been run and preserved;
3. the owner approves.
