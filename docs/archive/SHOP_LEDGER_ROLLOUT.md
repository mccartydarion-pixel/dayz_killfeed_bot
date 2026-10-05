# Shop Delivery Ledger — Production Rollout Runbook (PRs #97, #98, #95)

This is an exact, reversible rollout of the Shop delivery-attempt ledger (migration 0054), the canary operator service and evidence (migration 0055), and the canary preparation code.

**It enables nothing.**

* Automatic delivery stays disabled (`nitradodelivery.PrototypeAdapter.Enabled = false`), and there is no worker.
* The canary execution lock stays closed.
* No mission file is written, and Champions is not restarted.

**Every step needs the owner's explicit approval. Nothing here has been executed.**

## 0. Facts verified before the rollout (read-only, 2026-09-25)

| Fact | Value |
|---|---|
| Deployment target | Railway project `genuine-education`, environment `production`, service `dayz_killfeed_bot`. It deploys branch **`main`** (Dockerfile, 1 replica, restart on failure ×10, no healthcheck). **Merging a PR into `main` is the production deploy.** |
| Database | Railway service `Postgres` (volume `postgres-volume`), **PostgreSQL 18.6**. The bot's `DATABASE_URL` resolves to it. |
| Applied migrations | 53, the last being `0053_installation_embed_activation`. Nothing ≥ 0054, and no duplicates. |
| Shop data | 0 purchases, 0 deliveries (`shop_deliveries` is 48 kB). |
| Economy data | 2 `point_transactions` rows, sum 10,000,500; `player_points` balances sum 10,000,500 |
| Existing objects | No triggers on `shop_deliveries`. No ledger tables or `uq_shop_deliveries_id_tenant`. |
| Locks | 0 long-running transactions and 0 advisory locks at the time of the check |
| Canary settings | `CHAMPION_SHOP_CANARY_EXECUTION` and `CHAMPION_SHOP_CANARY_INSTALLATION_IDS` are **not set** in production |
| Game server | `cfggameplay.json` SHA-256 `4d000807963a…` (2951 bytes, `objectSpawnersArr = ["custom/The_Lost_City.json"]`); no Champion artifact; accepted boot `DayZServer_PS4_x64_2026-09-25_04-25-57.ADM` |

## 1. Merge order and migration expectations

| Step | PR | Migration applied at startup | Other effect |
|---|---|---|---|
| 1 | **#97** | `0054_shop_delivery_attempts` | Refund and manual-fulfil guard (409 `DELIVERY_ATTEMPT_ACTIVE`, only when attempts exist); migration advisory lock; `cmd/shop-ledger-verify` |
| 2 | **#98** (retargets to `main` after step 1) | `0055_shop_delivery_attempt_evidence` | Canary operator API under `/shop/canary/attempts`: reads work for OWNER/ADMIN; **mutations return 423 while locked** |
| 3 | **#95** | none | Canary preparation code and tools (read-only) |
| later | #94 (C.A.S.E. Phase 6) | `0056_case_…` … `0062_case_…` | Independent. Whichever merges second keeps both blocks in `internal/database/migrations.go`, Shop entries first. |

**Per deployment**

* Each migration runs once, inside its own transaction, under `pg_advisory_xact_lock`.
* A failed migration rolls back completely and the instance does not start. Railway retries up to 10 times; the previous deployment keeps serving until a new one starts.

## 2. Before each merge

1. **Database backup (required).** Take a manual backup of the `Postgres` service volume in the Railway dashboard (service → Backups), and confirm it appears with the current timestamp.

   The Railway CLI (v5.57) has no backup command, so this must be confirmed in the dashboard. As an independent copy, the owner may also run a local, schema-plus-data dump:

   ```
   pg_dump --format=custom --file=champion-pre-0054.dump "$DATABASE_PUBLIC_URL"
   ```

   Never paste the URL into logs or chat.
2. **Baseline.** Record the current baseline with the verifier. Before 0054 the ledger checks fail, as expected; the baseline lines must pass.

   ```
   railway run -s Postgres go run ./cmd/shop-ledger-verify -phase 0054 -migrations 53 -purchases 0 -deliveries 0 -ledger-rows 2 -ledger-sum 10000500 -balance-sum 10000500
   ```

   Update the numbers if Shop or economy activity happened since 2026-09-25.
3. **Game-server baseline.**
   * `railway run go run ./cmd/shop-capability-probe -service 19806451 -org 1 -installation 11 -game-server 1`: note the `cfggameplay` SHA-256 and the objectSpawnersArr entries.
   * `GET /api/admin/live-sync`: note `bootAuthority.acceptedBoot`.
4. Confirm there are no open long transactions: `SELECT COUNT(*) FROM pg_stat_activity WHERE state <> 'idle' AND xact_start < NOW() - INTERVAL '60 seconds'` must be 0.
5. Confirm the PR's CI is green on its latest commit.

## 3. Deployment behaviour

* **Locks.** 0054 creates `uq_shop_deliveries_id_tenant` (not `CONCURRENTLY`, because migrations are transactional) and a trigger on `shop_deliveries`. Both take a lock that blocks writes to `shop_deliveries` for the build. With 0 rows that is milliseconds; purchases, refunds and fulfilments wait rather than fail. 0055 only touches the new, empty `shop_delivery_attempts` table.
* **Rolling deployment.** The new instance migrates before it serves; the old instance keeps serving meanwhile.
  * The old code never touches the new tables.
  * The new triggers allow every existing Shop write while no attempt exists.
  * If Railway ever overlaps two new instances, the advisory lock makes the second wait and skip, instead of failing.
* **Timeouts.** `Migrate` has a 30 s overall timeout. The Shop migrations need well under 1 s: CI measured under a second for 0054 and 0055 together on seeded data, on PostgreSQL 16 and 18.
* **Existing Shop transactions.** Purchase, refund and manual fulfilment behave exactly as before when no attempt exists (integration-tested). The only new responses (`DELIVERY_ATTEMPT_ACTIVE` 409, `CANARY_EXECUTION_LOCKED` 423) require attempts, or the canary API.

## 4. Post-deployment acceptance (after each step)

**Automated (read-only).**

After step 1:

```
railway run -s Postgres go run ./cmd/shop-ledger-verify -phase 0054 -migrations 54 -purchases 0 -deliveries 0 -ledger-rows 2 -ledger-sum 10000500 -balance-sum 10000500
```

After step 2:

```
railway run -s Postgres go run ./cmd/shop-ledger-verify -phase 0055 -migrations 55 -purchases 0 -deliveries 0 -ledger-rows 2 -ledger-sum 10000500 -balance-sum 10000500 -api https://dayzkillfeedbot-production.up.railway.app
```

For `-api`, the environment must provide `WEBSITE_API_SECRET`, plus an OWNER/ADMIN Discord ID of organization 1 in `CANARY_CHECK_ACTING_USER` (falls back to the first `CHAMPION_ADMIN_DISCORD_IDS`). It must report every line `PASS`:

* 0054 (and 0055) applied exactly once, after 0053; no duplicate migration names or numbers; expected count;
* ledger tables, index and triggers present;
* **0 delivery attempts, 0 history, 0 evidence**;
* Shop purchases and deliveries, point-transaction rows and sum, and balances unchanged;
* every purchase has its delivery;
* canary API: read answers 200 with `executionLocked: true` and no attempts; **the mutation probe answers 423**. It probes delivery 0, which cannot create anything even if the lock were open.

**Logs.** The startup logs show:

* `migration applied name=0054_shop_delivery_attempts` (step 1);
* `migration applied name=0055_shop_delivery_attempt_evidence` and `component=shop_canary execution_enabled=false installations=0` (step 2);
* no migration error and no restart loop.

**Functional (owner, optional, costs 1 point that is refunded).** Buy any active 1-point product with a test player, refund it: 200, and the balance is restored. Buy again and fulfil manually: 200. Then re-run the verifier with the new baseline. Skip this if you prefer to keep production data untouched; the integration suite covers these paths.

**Game server unchanged.**

* Re-run the capability probe: the `cfggameplay` SHA-256 is identical to the baseline, and `champion_artifact_exists = false`.
* `GET /api/admin/live-sync`: any boot after the deploy must match the server's own schedule. The deploy restarts only the bot, never Champions.
* Nothing in #97, #98 or #95 can upload a file or restart a server.

## 5. Rollback

| Situation | Action |
|---|---|
| Migration fails at startup | Nothing was committed (per-migration transaction). The previous deployment keeps serving. Revert the merge commit on `main` (or redeploy the previous deployment in Railway), then investigate. |
| Code problem after a successful deploy | Revert the merge commit on `main` → Railway redeploys the previous code. **The schema stays.** This is safe: old code never touches the new tables, and the triggers allow everything while no attempt exists (0 attempts are guaranteed while the lock is closed). |
| Schema removal (only if required; owner-approved; take a backup first) | Run the SQL below in one transaction, in reverse order (0055 first). It deletes only the new, empty tables. |
| Catastrophic data problem | Restore the pre-deploy backup (section 2, step 1). |

**Schema removal SQL**

```sql
BEGIN;
-- 0055
DROP TRIGGER IF EXISTS trg_shop_attempt_resolution_evidence ON shop_delivery_attempts;
DROP TABLE IF EXISTS shop_delivery_attempt_evidence;
DROP FUNCTION IF EXISTS shop_attempt_evidence_guard(), shop_attempt_resolution_evidence();
DROP INDEX IF EXISTS uq_shop_delivery_attempts_id_tenant;
DELETE FROM schema_migrations WHERE name = '0055_shop_delivery_attempt_evidence';
-- 0054
DROP TRIGGER IF EXISTS trg_shop_delivery_exposure_guard ON shop_deliveries;
DROP TABLE IF EXISTS shop_delivery_attempt_events;
DROP TABLE IF EXISTS shop_delivery_attempts;
DROP FUNCTION IF EXISTS shop_delivery_exposure_guard(), shop_delivery_attempt_guard(), shop_delivery_attempt_created_event(),
    shop_delivery_attempt_fulfilled_check(), shop_delivery_attempt_events_immutable();
DROP INDEX IF EXISTS uq_shop_deliveries_id_tenant;
DELETE FROM schema_migrations WHERE name = '0054_shop_delivery_attempts';
COMMIT;
```

**Limitations**

* There are no down migrations.
* Removing the schema while newer code is deployed would make that code re-apply the migrations at its next start. Remove the schema only together with a code revert.

## 6. Execution lock

The normal deployment sets **no** canary variable, so the lock stays closed:

* mutation endpoints return **423** `CANARY_EXECUTION_LOCKED`;
* read endpoints return 200 for OWNER/ADMIN only.

The lock opens only with `CHAMPION_SHOP_CANARY_EXECUTION=enabled` **and** `CHAMPION_SHOP_CANARY_INSTALLATION_IDS=<id>`, which is a separate, later approval for the canary window.
