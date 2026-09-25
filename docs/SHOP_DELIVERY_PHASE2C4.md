# Champion Shop Phase 2C.4 — Controlled Canary Operations

This phase adds operator tooling for a **single, controlled, live Shop delivery experiment** (the BandageDressing ×1 canary from Phase 2C.2). The tooling is a **recorder**:

* It has no Nitrado client and no HTTP client.
* It never uploads a file or restarts a server, and it has no background worker.
* The owner or operator performs each approved gate by hand; this service records what was verified, and the durable ledger enforces the rules.
* The production adapter (`nitradodelivery.PrototypeAdapter`) stays `Enabled=false`, and no generic automatic-delivery worker exists.

**Nothing was merged or deployed.** No production file, database row or server was touched.

## 1. Dependencies (PR #95 → PR #97 → this PR)

| PR | Content | Relation |
|---|---|---|
| #97 | Migration **0054** (durable attempt ledger), Shop refund/fulfil integration, `UNSTAGE_REQUIRED` | **Base of this PR** (stacked; GitHub retargets it to `main` when #97 merges). |
| #95 | Phase 2C.2 canary preparation: patch generator, drop-point rules, gates, read-only tool | Independent of this PR's code. It **consumed 0054** as of commit `a0575fa`: its unregistered ledger proposal (`internal/shop/canary/migration.go`), the rolled-back integration test and `TestMigrationIsProposalOnly` were removed, and Gate C now names PR #97. |

**Overlap review**

* **State machine.** `internal/shop/nitradodelivery/attempt.go` (`UNSTAGE_REQUIRED`) is byte-identical in #95 and #97, so there is no conflict in either merge order. #97 also adds `TestAttemptStatesMatchDurableLedger`, which keeps the prototype and the ledger identical.
* **Migrations.** Exactly one ledger migration exists (0054 in #97). This PR adds **0055**, a new additive table for structured evidence. It is not a duplicate, and it depends only on 0054.
* **Duplicate implementations.** This PR adds no second state machine. `canaryops` calls `repository.ShopAttemptRepository` (#97) for every state change, and the database guard enforces it.

**Merge order:** #97, then this PR (after retargeting), with #95 independent. Deploying this PR runs 0054 (if not yet applied) and 0055.

## 2. Canary operator service (`internal/shop/canaryops`)

Every operation is tenant-scoped: organization ID plus installation ID. The acting user must be an **OWNER or ADMIN** of that organization (`VerifyMembership`), and the installation must belong to it (the economy scope resolver). The HTTP layer checks the same thing first; the service re-checks it, so it is safe for any future caller.

| Operation | Effect | Mutating (needs the lock) |
|---|---|---|
| `CreateAttempt` | Validates the drop point (see below), builds the plan with the production validator `nitradodelivery.NewPlan` (BandageDressing, quantity 1, tenant, service binding, map, altitude), and records `PLAN_CREATED` with the plan's attempt ID and fingerprint. | yes |
| `GetAttempt` | The attempt plus its evidence, history and **next steps**: each allowed transition, with the evidence still missing for it. | no |
| `ListAttempts` | All attempts, or open ones only for reconciliation after a crash. | no |
| `AdvanceAttempt` | One ledger compare-and-set. The ledger's write-once columns are filled **only from recorded evidence**. `FAILED_REVIEW` and `ABANDONED` need a reason. `FULFILLED` is refused (see `FulfillAttempt`). | yes |
| `RecordEvidence` | Appends one structured observation (section 4). | yes |
| `ResolveReview` | Records the human conclusion on a `FAILED_REVIEW` (section 5). | yes |
| `FulfillAttempt` | Gate I: attempt, delivery and purchase become `FULFILLED` in one transaction (from #97), built from physical evidence only. | yes |

**Drop-point validation in `CreateAttempt`:**

* the source must be the **current** boot's ADM file (`server_adm_sessions`), and that session must not have ended;
* an ADM byte offset is required;
* the observation must be at most 20 minutes old and not in the future;
* the altitude must come from the ADM line; the API refuses a request that omits it.

The plan uses the canary delivery's own X and Z, from the purchase made in the normal Shop flow.

**API** (`/api/saas/organizations/{org}/installations/{inst}/shop/canary/attempts`):

| Method and path | Operation |
|---|---|
| `GET` (`?open=true`) | list |
| `POST` | create |
| `GET /{attemptID}` | get |
| `POST /{attemptID}/advance` | `{from, to, reason}` |
| `POST /{attemptID}/evidence` | `{kind, source, sha256, previousSha256, bootFile, bootStartedAt, observedBy, observedAt, detail}` |
| `POST /{attemptID}/review` | `{outcome, note}` |
| `POST /{attemptID}/fulfill` | `{note}` |

**Error codes:**

| Code | Status |
|---|---|
| `SHOP_FORBIDDEN` | 403 |
| `NOT_FOUND` | 404 (installation) |
| `CANARY_EXECUTION_LOCKED` | 423 |
| `ATTEMPT_NOT_FOUND` | 404 |
| `ATTEMPT_STATE_CHANGED` | 409 (lost compare-and-set) |
| `ATTEMPT_CONFLICT` | 409 |
| `ATTEMPT_TRANSITION_REFUSED` | 409 |
| `EVIDENCE_REQUIRED` | 409 |
| `EVIDENCE_NOT_ACCEPTED` | 409 |
| `EVIDENCE_ALREADY_RECORDED` | 409 |
| `EVIDENCE_INVALID` | 422 |
| `AMBIGUOUS_BOOT` | 409 |
| `CANARY_NOT_READY` | 409 |
| `INVALID_REQUEST` | 400 |

Every mutation is audit-logged: `shop_canary_*` events with organization, installation and acting user.

## 3. Production execution lock

The lock is a **canary-specific gate**, separate from every other setting:

* `CHAMPION_SHOP_CANARY_EXECUTION` must be exactly `enabled`. The values `true`, `1`, `yes` and `ENABLED` keep it locked, so a generic boolean can never open it by accident.
* `CHAMPION_SHOP_CANARY_INSTALLATION_IDS` must list the installation, for example `11`. Without a valid ID it stays locked.
* No Shop, economy, delivery, embed or Nitrado setting opens it (`TestShopCanaryExecutionIsLockedByDefault`).
* **Default: locked.** While locked, `GET` works for OWNER/ADMIN, and every mutation returns `CANARY_EXECUTION_LOCKED` (423) **before** reaching the ledger. The unit and API tests confirm nothing is written.
* A suspended installation is locked too.
* Startup logs `component=shop_canary execution_enabled=… installations=…`.

## 4. Manual evidence workflow (migration 0055)

`shop_delivery_attempt_evidence` is an append-only table (UPDATE is refused). Each row has its **kind**, **source** and **recording actor** (`recorded_by`, the Discord ID), plus its observation time. The foreign key is tenant-bound (`(attempt_row_id, organization_id, installation_id)`). Proof kinds are write-once: one record per kind.

| Evidence | Kind | Required source | Accepted while | Feeds |
|---|---|---|---|---|
| Verified staged-file hash | `STAGED_FILE_HASH` (with the previous/empty hash) | `NITRADO_READBACK` | `FILE_PREPARED` | `staged_sha256`, `before_sha256`, `staged_at` |
| Staging boot | `STAGING_BOOT` | `BOOT_AUTHORITY` | `FILE_PREPARED` | `staged_boot_file` |
| New boot identity | `NEW_BOOT` (file and start time) | `BOOT_AUTHORITY` | `AWAITING_RESTART` | `restart_boot_file`, `restart_observed_at`; the service refuses (`AMBIGUOUS_BOOT`) a boot that did not start after the verified staging |
| Physical item observation | `ITEM_OBSERVED` (who) | **`IN_GAME_OBSERVATION` only** | `AWAITING_RESTART` … `VERIFICATION_REQUIRED` | `item_observed_by/at` |
| Pickup confirmation | `PICKUP_CONFIRMED` (who picked it up) | **`IN_GAME_OBSERVATION` only** | same | `picked_up_by`, `pickup_observed_at` |
| Verified unstaged-file hash | `UNSTAGED_FILE_HASH` | `NITRADO_READBACK` | `FILE_STAGED`, `AWAITING_RESTART`, `UNSTAGE_REQUIRED` | `unstaged_sha256`, `unstage_verified_at` |
| Second boot | `SECOND_BOOT` (file and start time) | `BOOT_AUTHORITY` | `VERIFICATION_REQUIRED` | `second_boot_file/started_at` |
| No additional item spawned | `NO_ADDITIONAL_SPAWN` (who checked) | **`IN_GAME_OBSERVATION` only** | `VERIFICATION_REQUIRED` | `no_respawn_checked_at` |
| Server log note | `SPAWNER_LOG` | `RPT_LOG` | after staging | nothing: informational only |

**An absence of RPT errors is never physical proof:**

* The table's source CHECK refuses an RPT source for any physical kind, even from direct SQL (tested).
* The service refuses it with `ErrNotPhysicalProof`.
* `FulfillAttempt` never reads `SPAWNER_LOG`.

The original bandage does not have to remain at the drop point. The pickup is recorded instead, and after the second boot the check is that **no new** item appeared. The ledger's CHECKs from #97 still enforce the ordering: sighting after staging, pickup after sighting, second boot after the verified unstage, and the check after the second boot.

## 5. Review resolution (`FAILED_REVIEW`)

| Outcome | Meaning | Effect |
|---|---|---|
| `NOT_SPAWNED` | The item definitely did not spawn | One-time ledger resolution (actor, note, history). The refund becomes possible. |
| `SPAWNED` | The item was observed spawning | One-time ledger resolution. The refund stays blocked (409); a manual fulfilment is allowed. |
| `UNCERTAIN` | A human looked and cannot decide | Recorded as a `REVIEW_UNCERTAIN` assessment (repeatable, append-only). **The attempt stays unresolved: refund and manual fulfilment remain blocked.** |

* Every outcome needs a note.
* After a resolution, further assessments are refused.
* **No outcome creates an attempt**, and the ledger refuses any new attempt after a `FAILED_REVIEW` whatever the resolution. An uncertain attempt is never re-staged automatically.

## 6. Tests

| Suite | Coverage |
|---|---|
| Unit (`canaryops`) | Authorization (no actor, MEMBER, non-member, other organization, other installation). The execution lock: zero-value, disabled, other installation, empty list, suspended; each blocks all five mutations with zero ledger writes while reads keep working. Drop-point validation. Evidence source rules (RPT refused for physical kinds). Evidence carried into the ledger. Ambiguous boot. `FULFILLED` only through `FulfillAttempt`. Reasons required. Review outcomes, including `UNCERTAIN` not resolving and never creating an attempt. Fulfilment built only from physical evidence. Import guard (no `net`, `net/http`, `os/exec`, `internal/nitrado`, capability or canary packages). |
| Unit (`config`) | The lock parses only `enabled` plus IDs; other settings never open it. |
| Integration (`canaryops`, PostgreSQL) | Full run A→I with every evidence kind. Refund and manual fulfil blocked while staged. Evidence refused in the wrong state. Write-once evidence. The database refuses RPT as physical proof. Evidence is immutable. Authorization and tenant isolation (member, other tenant's owner, other installation, other tenant's delivery, cross-tenant read and list). A locked installation writes nothing. Invalid transitions. Duplicate attempts. 16 concurrent advances (1 wins). 16 concurrent evidence writes (1 wins). Review: `UNCERTAIN` stays blocked, then `NOT_SPAWNED` refunds, `SPAWNED` manual fulfilment, a second resolution refused, no retry. Recovery: crash at `FILE_PREPARED` listed, refund blocked, abandoned with a reason, then the refund works. |
| Integration (API) | 423 while locked, including when the lock is opened for another installation (nothing written). 403 for MEMBER, player and another organization. 400 without altitude. Create, duplicate 409, stale 409, missing evidence 409. Refund 409 `DELIVERY_ATTEMPT_ACTIVE`. RPT-as-proof 400. Wrong-state 409. Write-once 409. Stage. Review `UNCERTAIN`, then manual fulfil 409, then `NOT_SPAWNED`, then refund 200. History and evidence actors. 404 for an unknown attempt. Ledger integrity. |
| Race | `go test -race` over shop, canaryops, repository, app, config, database, discord and killfeed. |

## 7. Deployment notes

* **Migration 0055** is additive:
  * a new table;
  * a unique index on `shop_delivery_attempts(id, organization_id, installation_id)`, where `id` is already unique;
  * a trigger.

  It changes no existing row. It has no down migration; the manual removal is `DROP TABLE shop_delivery_attempt_evidence; DROP FUNCTION shop_attempt_evidence_guard(); DROP INDEX uq_shop_delivery_attempts_id_tenant; DELETE FROM schema_migrations WHERE name='0055_shop_delivery_attempt_evidence';`.
* **Rolling deploy.** Old instances neither know nor touch the new table. The new routes exist only on new instances, and they are locked by default.
* **Using the canary later requires, as separate owner approvals:**
  * merging and deploying #97 and this PR;
  * setting `CHAMPION_SHOP_CANARY_EXECUTION=enabled` and `CHAMPION_SHOP_CANARY_INSTALLATION_IDS=11`;
  * then the Phase 2C.2 gates A–I, each approved on its own.

  The file uploads (Gates A, B, E, G) and restarts (F, H) are performed by the owner or operator, outside this service.
