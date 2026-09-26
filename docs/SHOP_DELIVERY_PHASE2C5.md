# CHAMPIONS® SHOP PHASE 2C.5 — INTEGRATION REPORT

**Scope.** Final integration and pre-deployment QA of the Shop canary stack:

| PR | Content |
|---|---|
| #95 | Canary preparation |
| #97 | Durable attempt ledger, migration 0054 |
| #98 | Controlled operator service, migration 0055 |

**Authorization.** The owner authorized preparation and testing only:

* **Nothing was merged or deployed.**
* No production migration was run.
* No execution variable was set, and nothing was uploaded.
* No purchase was created, and Champions was not restarted.
* All tests ran on CI's disposable PostgreSQL 16, with a local Nitrado fixture.

## 1. PR dependency status

| PR | Base | State | Latest CI |
|---|---|---|---|
| #97 ledger (0054) + Shop refund/fulfil integration + migration advisory lock | `main` (up to date) | open, ready | ✅ run 36101052365 (`9a5b807`) |
| #98 operator service (0055) + 2C.5 QA suites | `feature/shop-2c3-attempt-ledger` (stacked on #97, includes it) | open, ready | ✅ run 36101911507 (`94725a7`) |
| #95 canary preparation | `main` (1 commit behind; merges cleanly) | open, ready | ✅ (`a0575fa`) |
| #99 **[DO NOT MERGE]** combined check: `main` + #97 + #98 + #95 | temporary | closed after CI | ✅ run 36101963599 (`44afbd4`) |

**Integration checks**

* **No duplicate ledger.** #95 contains no ledger proposal (its `internal/shop/canary/migration.go` and the rolled-back test were removed in `a0575fa`). It only names `LedgerMigrationName = "0054_shop_delivery_attempts"`.
* **One state machine.** `repository.ShopAttemptTransitions` (#97) is the only durable state machine. `canaryops` (#98) drives it and adds none of its own. `TestAttemptStatesMatchDurableLedger` keeps the in-memory prototype identical.
* **`UNSTAGE_REQUIRED` overlap.** `internal/shop/nitradodelivery/attempt.go` is byte-identical in #95 and #97. The combined merge auto-resolved `nitradodelivery_test.go` with no conflict.
* **Migrations registered once, in order.** The combined tree registers `0054_shop_delivery_attempts` then `0055_shop_delivery_attempt_evidence`, each exactly once.

**Blocker (outside this stack).** Draft PR **#94** (C.A.S.E. Phase 6) registers `0054_case_addon_subscriptions` … `0060_case_watch_requester`.

* A trial merge conflicts in `internal/database/migrations.go`: both workstreams append after 0053.
* The schemas are disjoint (`case_*` tables only), and the runner keys migrations by full name. So nothing would be applied twice or overwritten.
* But the numbering must be made unique **before either set is deployed**. Renaming an undeployed migration is safe; renaming a deployed one is not.
* Whichever PR merges second renumbers its migrations after the other's. If the Shop renumbers, update `LedgerMigrationName` (#95), the doc references, and `indexOfMigration("0054_shop_delivery_attempts")` in `migrations_shop_attempts_integration_test.go`.

**Resolved on 2026-09-25.**

* Production history was verified read-only: applied up to `0053_installation_embed_activation`, nothing numbered 0054 or above, and no Shop or C.A.S.E. Phase 6 table exists.
* #94's migrations were renumbered to `0056_case_addon_subscriptions` … `0062_case_watch_requester` (commit `43b719b`). Their SQL is byte-identical and their relative order unchanged.
* The Shop keeps 0054 and 0055.
* The combined tree (main + #97 + #98 + #95 + #94) passed CI in run 36110561884.
* `TestMigrationRegistryNumbersAreUniqueAndOrdered` (#94) and `TestLedgerMigrationObjectsAreDisjoint` (#97) guard against a recurrence.
* Whichever of #97/#98 and #94 merges second must resolve one textual conflict in `internal/database/migrations.go`: keep both blocks, Shop entries first.

## 2. Exact merge order

1. **#97** (into `main`).
2. **#98**. GitHub retargets it to `main` when #97's branch merges. It already contains #97.
3. **#95**, independent code. It can also go before #98, but after #97 so that its Gate C reference is real.

**Resolve the #94 numbering before step 1** if #94 is to land first, or require #94 to renumber after step 2.

## 3. Combined test results

| Suite | Result |
|---|---|
| Build, `go vet`, `go vet -tags integration` (combined tree) | ✅ |
| Unit tests (combined tree, locally and in CI) | ✅ 40 packages |
| Race (CI set + `internal/shop/...`, repository, app, config, database) | ✅ |
| PostgreSQL integration (combined CI) | ✅. `internal/app` 28.7 s, `internal/repository` 17.8 s, `internal/database` 1.2 s, `internal/shop/canaryops` 0.9 s, plus all other packages |

### End-to-end canary simulation

`TestShopCanaryEndToEndSimulation` runs over the real routes, with the real Nitrado client against a local file-server fixture:

1. A legitimate 1-point purchase through the Shop: balance −1, one `SHOP_PURCHASE` debit.
2. The delivery is `MANUAL_READY`, with the purchased X/Z.
3. The drop point is read from the current boot's ADM through the Nitrado client: X/Z equal to the delivery, altitude from the line, byte offset, and boot authority naming that file.
4. A durable attempt is created (lock open for this installation only).
5. The empty file is read back, the owner's staging is simulated in the fixture, then `STAGED_FILE_HASH` (verified to be the exact artifact) and `STAGING_BOOT` are recorded.
6. First restart: a new ADM file appears and boot authority moves; `SPAWNER_LOG` (informational) and `NEW_BOOT` are recorded.
7. `ITEM_OBSERVED` and `PICKUP_CONFIRMED` are recorded by named observers.
8. Verified unstaging: `UNSTAGED_FILE_HASH` must be the empty file.
9. `SECOND_BOOT` and `NO_ADDITIONAL_SPAWN` are recorded.
10. Atomic fulfilment.

**Consistency afterwards**

* Purchase, delivery and attempt are all `FULFILLED`.
* Exactly one debit of 1 point, no refund, and the balance is the start minus 1.
* `ReconcileShop` finds no mismatch, and ledger integrity holds.
* 9 evidence records and 8 history events.
* **The Nitrado fixture received zero non-GET requests.**
* A new attempt on the delivered order is refused.

## 4. Migration compatibility

`internal/database/migrations_shop_attempts_integration_test.go` uses fresh, isolated schemas on the disposable database.

**Sequential application on existing data.** Migrations 0001–0053 are built exactly as in production, then open, fulfilled and refunded Shop orders are seeded. `Migrate` then:

* applies only 0054, then 0055, in order;
* leaves the seeded rows byte-for-byte unchanged;
* records every migration exactly once, and a second startup applies nothing.

Manual fulfilment, refund cancellation and cascade deletes keep working with no attempts present.

**Concurrent startup.** Three instances start at once on an empty schema, and all succeed with each migration recorded once.

This needed a fix, added to #97. `Migrate` had no lock: two overlapping instances could both apply 0054, and the loser would fail its startup on a duplicate object or `schema_migrations` key. Now:

* each migration runs under `pg_advisory_xact_lock` and re-checks inside the lock;
* the `schema_migrations` bootstrap takes the same lock.

**Existing Shop behaviour without attempts.** The purchase, fulfil and refund suites (`saas_api_shop*_integration_test.go`), and the "plain" cases in `TestShopRefundAndFulfilRespectDeliveryAttempts`, pass unchanged.

### Deployment-time locks and compatibility risks

* **0054** builds `uq_shop_deliveries_id_tenant` (not `CONCURRENTLY`, because migrations are transactional) and creates triggers on `shop_deliveries`. That takes a SHARE lock on `shop_deliveries` for the build: Shop purchase, fulfil and refund writes wait rather than fail. The table has one row per purchase, so this is milliseconds (0054 and 0055 together took well under a second on seeded data in CI).
* **0055** builds a unique index on the new `shop_delivery_attempts` table, which is empty, and adds triggers.
* **Rolling deploy.** Old instances keep working: the new triggers allow everything while no attempt exists, and none will exist until the lock is opened.
* **Rollback.** There are no down migrations. The manual removal SQL is in `SHOP_DELIVERY_PHASE2C3.md` and `SHOP_DELIVERY_PHASE2C4.md`.
* **Migration timeout.** `Migrate` has a 30 s context timeout overall. The advisory lock adds no wait unless two instances overlap.

## 5. Security findings

| Check | Result | Evidence |
|---|---|---|
| Tenant and installation ownership at every entry point | ✅ | `TestShopCanaryRoutesRequireAuthorization`: all 7 routes refuse a missing service secret (401), a missing actor (401), a MEMBER, a player, another organization's owner (403), and another organization's installation ID under the path (404). The service re-checks OWNER/ADMIN and ownership (`TestAuthorizationAndTenantIsolation`, `TestCanaryAuthorizationIsolationAndLock`). |
| Execution locked by default | ✅ | Zero-value, disabled, other-installation and empty-list gates block all five mutations with zero ledger writes. The API returns 423 and writes nothing. |
| Physical observations need an authorized, named actor | ✅ | OWNER/ADMIN plus an open lock. `observedBy` is required (service and table CHECK). `recorded_by` is the acting Discord ID. |
| A log entry cannot substitute for physical observation | ✅ | Physical kinds accept only `IN_GAME_OBSERVATION` (service, API 400, and the table's source CHECK even through direct SQL). Fulfilment with only `SPAWNER_LOG`, second boot and no-respawn evidence is refused. |
| Evidence cannot be edited or duplicated | ✅ | `UPDATE` is refused (append-only trigger). Proof kinds are write-once (16 concurrent writes: exactly 1 recorded). Ledger evidence columns are write-once. |
| Invalid state transitions rejected | ✅ | Exhaustive 90-pair test (#97), service and API skips refused (`ATTEMPT_TRANSITION_REFUSED`), stale compare-and-set returns `ATTEMPT_STATE_CHANGED`. |
| Refunds cannot bypass an exposed attempt | ✅ | `FILE_PREPARED` … `VERIFICATION_REQUIRED` and unresolved or `SPAWNED` reviews return 409 `DELIVERY_ATTEMPT_ACTIVE` (repository, service and API). The trigger backstop covers any writer. |
| `FAILED_REVIEW` needs evidence and authorization to resolve | ✅ **(new in 2C.5)** | `NOT_SPAWNED` / `SPAWNED` need a recorded in-game `REVIEW_OBSERVATION` (or, for `SPAWNED`, an earlier sighting or pickup), enforced by the service **and** a 0055 trigger (`SA424`). `UNCERTAIN` records but keeps the attempt blocked. OWNER/ADMIN plus lock are required. |
| Artifact integrity | ✅ **(new in 2C.5)** | Staged and unstaged read-backs must hash to the exact artifact rebuilt from the ledger row. A mismatch records nothing (`ARTIFACT_HASH_MISMATCH`). |
| No operator endpoint can upload or restart | ✅ | `canaryops`, `nitradodelivery` and the handler file import only an allowlist (no Nitrado client, no HTTP client, no `os/exec`). The handlers call nothing named upload, restart, stop or file-server. The service exposes only its 7 recording operations (reflection test). The E2E fixture saw zero writes. The repository links Nitrado *types* through Live Sync, but nothing on the canary path constructs or receives a client. |

**Residual observations**

* **ADM line re-verification.** `CreateAttempt` trusts the operator's ADM file, offset and altitude, bound to the current session and freshness. It does not re-read the ADM line itself, because the service has no Nitrado access by design. The Phase 2C.2 read-only tool (#95) and the E2E test show how the operator obtains them. A future improvement would be to check the offset against the persisted `player_location_events` row.
* **`ResolveReview` requires the open lock.** If the lock is closed with an unresolved `FAILED_REVIEW`, that purchase stays blocked (409) until the lock is reopened. This is conservative by design.

## 6. Execution-lock validation

The lock opens **only** when `CHAMPION_SHOP_CANARY_EXECUTION` is exactly `enabled` **and** `CHAMPION_SHOP_CANARY_INSTALLATION_IDS` lists the installation (`TestExecutionLockFromEnvironment`, `TestShopCanaryExecutionIsLockedByDefault`).

| Case | Result |
|---|---|
| Unset; allowlist only; switch only | locked |
| `true`, `ENABLED`, `enable` | locked |
| Malformed IDs (`11abc; x`, `11;12`), negative or zero IDs | locked |
| Unrelated Shop, embed, Nitrado or delivery settings | locked |
| `enabled` + `11` | only 11 opens; 12, 13, 1, 0 and −11 stay locked |
| `enabled` + ` 11 , 13 ` | 11 and 13 open |
| `enabled` + `x,11,,-2` | only 11 opens |

**These variables were not set anywhere in production.**

## 7. Failure and recovery simulation

| Scenario | Behaviour | Test |
|---|---|---|
| Worker crash | Open attempts are durable and listed by `ListAttempts(open)`. Refund and a second attempt are refused. | `TestShopAttemptRecovery`, `TestCanaryReviewResolutionAndRecovery` |
| Lost upload response (`FILE_PREPARED`) | 409 on refund and manual fulfil. The read-back decides between `FILE_STAGED` (verified hash) and `ABANDONED` (with a reason). There is no way back to `FILE_PREPARED`. | same |
| Invalid artifact hash | Nothing recorded, the state is unchanged, the refund stays blocked. | `TestCanaryFailureAndRecoverySimulation`, API test |
| Restart before staging | `AMBIGUOUS_BOOT`, then `FAILED_REVIEW`. Blocked; no retry. | `TestCanaryFailureAndRecoverySimulation` |
| Second restart before unstaging | `FAILED_REVIEW`. `UNCERTAIN` stays blocked; resolution needs an in-game observation. | #97 recovery tests, 2C.4 review tests |
| Missing physical confirmation | Fulfilment refused (`EVIDENCE_REQUIRED`); only review can close the attempt; the refund stays blocked. | `TestCanaryFailureAndRecoverySimulation` |
| Concurrent operator actions | 16 concurrent advances: 1 wins. 16 concurrent evidence writes: 1 wins. 24/24 at ledger level. | `TestCanaryConcurrency`, #97 |
| Refund vs fulfilment | The fulfilment always completes. The refund is either 409 or a refund of the already-delivered order. Never a cancelled delivery with a fulfilled attempt. | #97 races, `TestCanaryFailureAndRecoverySimulation` |
| Operator disconnection | A cancelled request changes nothing. The retry applies exactly once; a replay gets `ATTEMPT_STATE_CHANGED`. | `TestCanaryFailureAndRecoverySimulation` |

**No uncertain outcome retries or creates another item.** No delivery ever has more than one attempt (asserted). The ledger refuses a new attempt after any `FAILED_REVIEW`. The service has no staging or upload operation.

## 8. Remaining blockers

1. ~~Migration numbering against draft PR #94~~: **resolved** (section 1). Only the textual `migrations.go` conflict remains; its resolution is deterministic.
2. **Owner approval** to merge #97, then #98, then #95, and to deploy. Deploying runs 0054 and 0055 on production at startup.
3. **An owner-created canary product and purchase** (Gate D), and the Phase 2C.2 gates A–I, each approved separately.
4. **Setting the lock variables** for installation 11 is a separate approval, and should only happen right before Gate D.

## 9. Production deployment checklist (for the owner; nothing here was executed)

1. Resolve the #94 migration numbering; re-run CI on the affected branch.
2. Take a database backup (Railway snapshot) before the deploy.
3. Merge #97, confirm CI on `main`, then deploy. Watch the startup logs for `migration applied name=0054_shop_delivery_attempts`, and confirm that Shop purchase, refund and fulfil still work (no attempts exist yet).
4. Merge #98 (retargeted to `main`), confirm CI, deploy. Watch for `migration applied name=0055_shop_delivery_attempt_evidence` and `component=shop_canary execution_enabled=false installations=0`.
5. Merge #95.
6. **Leave `CHAMPION_SHOP_CANARY_EXECUTION` unset.** Verify `GET …/shop/canary/attempts` answers `executionLocked: true`, and that a `POST` returns 423.
7. Only for the approved canary window: set both variables for installation 11, then run the Phase 2C.2 gates A–I one at a time with separate approvals. Unset the variables afterwards.

## 10. Automatic delivery

**Automatic delivery remains disabled.**

* `nitradodelivery.PrototypeAdapter.Enabled` is `false`.
* No worker exists, and nothing creates attempts without an OWNER/ADMIN request through the open canary lock.
* No code path uploads a file or restarts a server.

## Verdict

**READY FOR OWNER DEPLOYMENT REVIEW.** #97, #98 and #95 are individually green and green combined (CI run 36101963599). Before any merge, the owner must resolve one external item: the migration-number conflict with draft PR #94.
