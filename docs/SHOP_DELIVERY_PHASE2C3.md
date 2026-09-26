# Champion Shop Phase 2C.3 — Durable Delivery Attempt Ledger (migration 0054)

This PR adds the durable ledger for automatic console delivery attempts and integrates it into the existing Shop refund and fulfilment paths.

**What it does not do:**

* It creates no attempts in production, and it has no worker.
* Automatic delivery stays disabled; `nitradodelivery.PrototypeAdapter` stays `Enabled=false`.
* It does not touch the live database, mission files, or the Champions server.

**Owner decisions this phase implements (Phase 2C.2 review):**

* Lost City stays disabled.
* The canary uses a legitimate 1-point Shop purchase.
* The ledger comes before any item staging.
* The first restart is owner-controlled.

**Separation from PR #95.** The Phase 2C.2 canary preparation stays there. This PR carries only the ledger, plus the `UNSTAGE_REQUIRED` state in `internal/shop/nitradodelivery/attempt.go`, which is identical to #95's change. Once this merges, #95 must drop its unregistered proposal (`internal/shop/canary/migration.go` and `ledger_integration_test.go`); its `TestMigrationIsProposalOnly` would otherwise fail.

## 1. Schema changes (additive)

| Object | Purpose |
|---|---|
| `uq_shop_deliveries_id_tenant` | A unique index on `shop_deliveries(id, organization_id, installation_id)`. `id` is already unique, so building it cannot fail on existing rows. It exists only so the attempt foreign key can be tenant-bound. |
| `shop_delivery_attempts` | One row per delivery attempt: the plan (immutable), file and boot evidence, physical evidence and review resolution (all write-once). |
| `shop_delivery_attempt_events` | Append-only history, one row per creation, transition and review resolution, each with its actor. |
| `trg_shop_delivery_attempt_guard` | Enforces creation rules, immutable identity, write-once evidence, the state machine and the event log. |
| `trg_shop_delivery_attempt_created` | Writes the creation event. |
| `trg_shop_delivery_attempt_events_immutable` | Makes history rows un-updatable. |
| `trg_shop_delivery_exposure_guard` (on `shop_deliveries`) | Blocks refund and manual fulfilment while an attempt may have put the item on the server. |
| `trg_shop_delivery_attempt_fulfilled` (deferred constraint trigger) | Lets a `FULFILLED` attempt commit only together with its delivery's `FULFILLED`. |

No existing row, column or constraint is modified. While no attempt exists, every existing Shop path behaves exactly as before (VERIFIED IN TEST).

### Database-enforced guarantees

| Guarantee | Mechanism |
|---|---|
| Tenant isolation | FK `(delivery_id, organization_id, installation_id)` → `uq_shop_deliveries_id_tenant`; the creation guard re-checks the tenant; every repository call filters by organization and installation. |
| Unique attempt identity | `attempt_id UNIQUE`; `attempt_id = 'champion:d' \|\| delivery_id \|\| ':a' \|\| attempt` (the preview ID `champion:d0:a1` can never be stored); `UNIQUE (delivery_id, attempt)`; the attempt number must follow history (`SA412`). |
| Duplicate prevention | A partial unique index allows one open attempt per delivery. No new attempt is allowed after a fulfilled attempt or a `FAILED_REVIEW` (resolved or not), so an uncertain attempt is **never re-staged automatically**. Creation locks the delivery row. |
| Compare-and-set transitions | `UPDATE … WHERE attempt_id AND state = expected AND tenant`. A lost race changes nothing (`ErrShopAttemptStale`). |
| State machine | Only the transitions of `repository.ShopAttemptTransitions`, identical to `nitradodelivery` (enforced by a test), including `RESTART_OBSERVED → UNSTAGE_REQUIRED → VERIFICATION_REQUIRED`. Terminal rows are frozen. `FULFILLED` is reachable only through `FulfillAttempt`. |
| Immutable history | Plan facts are immutable. Evidence fields are write-once. Events are append-only, with a mandatory actor (`SET LOCAL champion.actor`). |
| Verified evidence | See the evidence table below. |
| Delivery must be open | Creation, and `PLAN_CREATED → FILE_PREPARED`, require `MANUAL_READY`, `MANUAL_COORDINATE` and a `PENDING_FULFILLMENT` purchase (`SA411`). |

Evidence each state requires:

| State | Required evidence |
|---|---|
| `FILE_STAGED` and later | staged SHA-256, staged time, staged boot |
| `RESTART_OBSERVED` and later | a restart boot that differs from the staged boot, plus its time |
| `VERIFICATION_REQUIRED`, `UNSTAGED`, `FULFILLED` | unstaged SHA-256 (different from the staged one) and the unstage time |
| `FULFILLED` | a named observer; sighting after staging; pickup (who) at or after the sighting; a second boot that differs from both earlier boots and started after the verified unstage; a no-respawn check at or after the second boot; the verifier |
| `FAILED_REVIEW` | a reason |

Custom SQLSTATEs are mapped to typed errors in `internal/repository/shop_attempt_repository.go`:

| SQLSTATE | Error |
|---|---|
| `SA409` | `ErrShopDeliveryAttemptActive` |
| `SA410` | `ErrShopAttemptConflict` |
| `SA411` | `ErrShopAttemptDeliveryClosed` |
| `SA412` | `ErrShopAttemptSequence` |
| `SA422` | `ErrShopAttemptRejected` |
| CHECK violations | `ErrShopAttemptEvidence` |

## 2. Refund protection

`ShopRepository.Refund` checks the ledger under the purchase row lock, before it writes anything. The database trigger is a backstop that also covers any other writer.

| Attempt state for the purchase's delivery | Refund (undelivered order) | Manual fulfil |
|---|---|---|
| none / `ABANDONED` / `UNSTAGED` | allowed (unchanged behaviour) | allowed |
| `PLAN_CREATED` (nothing touched the server) | allowed; the attempt becomes `ABANDONED` in the same transaction | allowed; same |
| `FILE_PREPARED` … `VERIFICATION_REQUIRED` | **409 `DELIVERY_ATTEMPT_ACTIVE`** | **409** |
| `FAILED_REVIEW`, unresolved | **409** | **409** |
| `FAILED_REVIEW`, resolved `SPAWNED` | **409**: never refunded automatically | allowed: records the delivery |
| `FAILED_REVIEW`, resolved `NOT_SPAWNED` | allowed | allowed |
| `FULFILLED` | the purchase is `FULFILLED`: the existing "refund a delivered order" rule applies (the delivery keeps its fulfilment) | n/a |

**Why FAILED_REVIEW is conservative.** It is terminal, but it means *uncertain*: the item may have spawned. Reaching it therefore never makes a refund possible. A human must first record `NOT_SPAWNED` with `ResolveReview`, which is one-time, write-once and written to history. Even then a new attempt stays forbidden.

**HTTP contract.** The response is 409 with code `DELIVERY_ATTEMPT_ACTIVE` and an explanatory message (`shopFailed`). No 500 is returned, and a refused refund credits nothing (VERIFIED IN TEST, API level).

## 3. Fulfilment integrity

`ShopAttemptRepository.FulfillAttempt` is Gate I, run as one transaction:

1. Lock the purchase row. This is the Shop's lock order, so it serializes with `Refund` and `Fulfill`.
2. Move the attempt `VERIFICATION_REQUIRED → FULFILLED` with the physical evidence (checked in Go and by the table's CHECK).
3. Move the delivery `MANUAL_READY → FULFILLED`.
4. Move the purchase `PENDING_FULFILLMENT → FULFILLED`.

Any failure rolls all three back. The deferred constraint trigger refuses a `FULFILLED` attempt whose delivery is not fulfilled in the same transaction. A manual `Fulfill` cannot bypass an active attempt (409).

## 4. Recovery

Every case is VERIFIED IN TEST (`internal/repository/shop_attempt_integration_test.go`, PostgreSQL):

| Case | Behaviour |
|---|---|
| Worker crash | Non-terminal attempts are durable and listed by `ListOpen` for reconciliation (`nitradodelivery.Reconcile`). |
| Missing upload response (`FILE_PREPARED`) | Refund and manual fulfil get 409; a second attempt is refused. After reading the file, the worker either records `FILE_STAGED` with the read-back evidence, or `ABANDONED` if the upload never landed (then refund and a retry are safe). There is no path back to `FILE_PREPARED`. |
| Duplicate attempts | 24 concurrent creates: exactly 1 succeeds and 23 get `ErrShopAttemptConflict`. A skipped attempt number is `ErrShopAttemptSequence`. |
| Concurrent state transitions | 24 concurrent compare-and-sets: exactly 1 moves and 23 get `ErrShopAttemptStale`. |
| Second boot before a verified unstage | `→ FAILED_REVIEW` with a reason. It cannot leave that state; refund and fulfil are blocked until it is resolved; no new attempt, ever. |
| Tenant isolation | Another tenant cannot get, transition, resolve, fulfil, list, read history of, or attach an attempt to the delivery. |
| Refund vs worker (`PLAN_CREATED → FILE_PREPARED`), 12 races | Either the refund wins (attempt `ABANDONED`, the worker's compare-and-set fails) or the worker wins (refund 409). Never a cancelled delivery with a prepared attempt. |
| Refund vs `FulfillAttempt`, 8 races | The fulfilment always succeeds. The refund is either 409, or a refund of the delivered order after it committed. |
| Manual fulfil vs worker, 8 races | Same exclusivity. |
| Exhaustive state machine | All 90 (from, to) pairs are accepted exactly when the Go map allows them. Missing evidence is refused on each transition that needs it. |

## 5. Compatibility, rolling deployment and rollback

**Compatibility**

* The migration is additive and makes no data change.
* `Refund` and `Fulfill` gain one indexed read of `shop_delivery_attempts` (by delivery), plus an `UPDATE` only when a `PLAN_CREATED` attempt exists.
* The delivery trigger adds one indexed `EXISTS` per delivery status change.
* The response contract is unchanged except for the new 409 code, which can only occur once attempts exist.

**Migration execution.** The migration runs in one transaction at startup (`db.Migrate`), before the new instance serves traffic.

* `CREATE UNIQUE INDEX` (not `CONCURRENTLY`, because migrations run inside a transaction) and `CREATE TRIGGER` briefly lock `shop_deliveries` against writes.
* The table has one row per purchase, so the lock lasts milliseconds.
* Purchases, refunds and fulfilments wait during it rather than fail.

**Rolling deployment**

* While old and new instances overlap, the old instance keeps writing `shop_deliveries` through its own `Refund` and `Fulfill`.
* The new triggers fire for it too. With no attempts in the table they allow everything, so behaviour is identical.
* If attempts ever existed during an overlap, the old code would receive `SA409` from the trigger and return a 500 instead of a 409. It fails safe: no refund, no double delivery.

**Rollback limitations**

* The migration framework has no down migrations.
* Rolling back the code leaves the tables, index and triggers in place. That is harmless while no attempts exist; with attempts, the old code fails safe as above.
* Removing the schema is a manual, owner-approved step:

```sql
DROP TRIGGER IF EXISTS trg_shop_delivery_exposure_guard ON shop_deliveries;
DROP TABLE IF EXISTS shop_delivery_attempt_events;
DROP TABLE IF EXISTS shop_delivery_attempts;           -- drops its triggers
DROP FUNCTION IF EXISTS shop_delivery_exposure_guard(), shop_delivery_attempt_guard(), shop_delivery_attempt_created_event(),
    shop_delivery_attempt_fulfilled_check(), shop_delivery_attempt_events_immutable();
DROP INDEX IF EXISTS uq_shop_deliveries_id_tenant;
DELETE FROM schema_migrations WHERE name = '0054_shop_delivery_attempts';
```

Dropping the tables deletes the attempt history. Take a copy first if any attempt exists.

**Deletion behaviour.** The attempt foreign key cascades from `shop_deliveries`, which cascades from purchases, installations and players. Tenant or player deletion therefore keeps working and removes the attempts. Events cascade with their attempt. Events are append-only against `UPDATE`; a `DELETE` of history is possible only through that cascade or by direct SQL.

## 6. Tests

| Suite | Contents |
|---|---|
| Unit | `TestMapAttemptErr`, `TestShopAttemptTransitionsShape`, `TestAttemptStatesMatchDurableLedger` (prototype = ledger), `TestShopFailedMapsDeliveryAttemptActive` (409 contract), plus the existing `nitradodelivery` tests including `UNSTAGE_REQUIRED` |
| Integration, repository | `TestShopAttemptLifecycleAndFulfillment`, `TestShopAttemptStateMachineIsExhaustive`, `TestShopAttemptDuplicatesAndConcurrentTransitions`, `TestShopAttemptRecovery`, `TestShopAttemptTenantIsolation`, `TestShopAttemptRefundRaces` |
| Integration, API | `TestShopRefundAndFulfilRespectDeliveryAttempts`: real routes, real purchases; 409 `DELIVERY_ATTEMPT_ACTIVE`, balance untouched, review resolution, plain purchases unchanged, ledger integrity |
| Race | `go test -race` over `internal/repository`, `internal/shop/...` and `internal/app` |

## 7. Remaining deployment blockers

1. **Owner approval to merge and deploy this PR (Gate C).** Deployment runs the migration on production at startup.
2. **After it merges, PR #95 must drop its unregistered ledger proposal** (`internal/shop/canary/migration.go`, `ledger_integration_test.go`) and rebase.
3. **No production code creates attempts yet.** The canary (Gates D–I) needs an operator tool that calls `Create`, `Transition` and `FulfillAttempt` with the evidence. That is a separate, reviewed change, and no worker or automatic path is part of it.
4. **No admin UI or endpoint exists for `ResolveReview`.** Until one is built, a `FAILED_REVIEW` can only be resolved by an operator tool, and the purchase stays blocked (409) meanwhile. That is the intended conservative default.
