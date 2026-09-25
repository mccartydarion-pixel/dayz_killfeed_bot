# CHAMPIONS® SHOP PHASE 2C.2 — PRE-EXECUTION REVIEW (revised)

This is the live-server canary **preparation** for automatic console delivery. The target is Champions:

| Binding | Value |
|---|---|
| Nitrado service | 19806451 |
| Organization | 1 |
| Installation | 11 |
| Game server | 1 |

This phase is read-only:

* Nothing was uploaded, and no upload token was requested.
* No restart was performed or requested.
* No purchase, delivery, attempt or product record was created or changed.
* No migration was registered or deployed.
* Automatic Shop delivery stays disabled.

Every production step below is a separate owner gate (section 8).

This revision applies the owner's pre-execution corrections:

* the empty Champion file is created first;
* Lost City is a separate decision;
* the canary-record design is settled;
* the ledger is finalized;
* the six restart milestones are distinct;
* the drop point must be re-validated on the day.

**Classes used for every finding:**

| Class | Meaning |
|---|---|
| **VERIFIED LIVE** | Observed on production by a read-only request (2026-09-24/25 UTC). |
| **VERIFIED IN TEST** | Proven by a unit test, or by the integration test against CI's disposable PostgreSQL. |
| **SUPPORTED BY DOCUMENTATION** | Bohemia scripts or Nitrado documentation. |
| **UNVERIFIED** | Not yet shown. |
| **UNSUPPORTED** | Not available. |

**Code:**

| Path | Contents |
|---|---|
| `internal/shop/canary` | Non-executing preparation code. An import-guard test keeps network, database and Nitrado packages out. |
| `internal/shop/nitradodelivery/attempt.go` | The `UNSTAGE_REQUIRED` state. |
| `cmd/shop-canary-prepare` | The read-only report tool. |
| `internal/shop/canary/ledger_integration_test.go` | Applies the ledger proposal inside a transaction that is always rolled back. |

## 1. Live baseline

| Fact | Value | Class |
|---|---|---|
| `cfggameplay.json` | 2924 bytes, SHA-256 `99bcc7d7c38283e3402bad1fd883a025aa9839cba01a78e578e21ad14e079217`, LF, tabs; last modified 2026-09-25 00:44:55 UTC (owner edit) | VERIFIED LIVE |
| `objectSpawnersArr` | empty `[]` | VERIFIED LIVE |
| `champion/champion_shop_delivery.json` | absent; `champion/` absent | VERIFIED LIVE |
| `custom/The_Lost_City.json` | exists, not referenced: 354,796 bytes, SHA-256 `c7bfea2c…8390`, valid spawner JSON with 791 objects | VERIFIED LIVE |
| Missing referenced spawner file | Non-fatal: four boots logged `[::SpawnObjects] … The_Losst_City.json does not exist` and ran normally | VERIFIED LIVE |
| Boot after the owner's edit (20:46:52 server time) | no `[::SpawnObjects]` line | VERIFIED LIVE |
| Spawner timing | runs about 35–40 s after the boot starts, about 11 s after the first `[CE]` line | VERIFIED LIVE |
| Success logging | a successful spawn logs nothing; only failures are logged | VERIFIED LIVE (errors only) |
| Restart cadence | 16:26 → 17:34 → 18:42 → 19:51 (68 min apart), then 20:46 (55 min; likely owner-triggered, cause UNVERIFIED) | VERIFIED LIVE (intervals) |
| Boot authority | current boot `DayZServer_PS4_x64_2026-09-24_20-46-53.ADM`, accepted about 7.5 min after the boot | VERIFIED LIVE |
| Current-boot ADM (read at 01:16 UTC) | 3 player lists; latest at 21:02:32 server time (01:02:32 UTC), 1 player, position with altitude | VERIFIED LIVE (counts only; no name or coordinate recorded) |

## 2. Corrected file-preparation sequence

**Problem.** If `cfggameplay.json` referenced the Champion file before it existed, any restart in between would read a missing file. That was already shown to be non-fatal, but it is untidy and it hides real errors.

**Correction.** The empty Champion file is created first (Gate A). It is referenced second (Gate B). The single item is written only at Gate E.

A scheduled restart at any point therefore meets one of three safe states:

* no reference at all;
* a reference to an existing empty file, which spawns nothing (SUPPORTED BY DOCUMENTATION: `objectspawner.c` iterates `Objects`; to be VERIFIED LIVE at Gate B by the next boot's clean RPT);
* the staged file, but only while an operator is present (Gates E–G).

**The empty file** (`EmptyArtifact`, VERIFIED IN TEST): 20 bytes, SHA-256 `328c4d64bb81bdbdddad2431a12b6197182f5fa5646bf16c7e0e8d437bc8dc5f`.

```json
{
  "Objects": []
}
```

The same bytes serve three purposes:

* what Gate A creates;
* what Gate G (unstage) restores;
* the rollback content of Gate E.

**Gate B guard.** `CheckReferencePrecondition` (VERIFIED IN TEST) allows the reference only when the Champion file exists and is byte-exactly the empty file. It refuses a missing file, a staged file, a foreign entry, or a non-canonical `{"Objects":[]}`. The tool shows the live state: absent, so the guard currently refuses (VERIFIED LIVE).

**Reference patch (Gate B, NOT applied):**

| | |
|---|---|
| Current SHA-256 | `99bcc7d7c38283e3402bad1fd883a025aa9839cba01a78e578e21ad14e079217` (2924 bytes) |
| Proposed SHA-256 | `7c66bf0dce23e4baf27819f6df366b594d562bc1098314f15f86aa8b99bb5de9` (2960 bytes), computed from the live bytes |

```
 		"objectSpawnersArr": [
-        ],
+			"champion/champion_shop_delivery.json"
+		],
```

**Patch guarantees (VERIFIED IN TEST):**

* Only the array's byte span changes.
* Existing entries are never removed or edited, including a misspelled Lost City entry.
* The generator refuses when the file is not deep-equal in every other key.
* CRLF files keep CRLF.
* It is idempotent (`ErrNoChange`).

**Backup:** `champion/backup/cfggameplay.json.99bcc7d7c382.bak`, verified by read-back, plus a local copy. Whether an upload creates a directory is UNVERIFIED. Gate A proves it for `champion/`; if it fails, the owner creates the folder in the Nitrado file browser.

**Rollback:** re-upload the backup and confirm SHA `99bcc7d7…`. It takes effect at the next start.

**Upload sequence (all writes, `UploadSequence`):**

1. READ the target and check the expected digest.
2. WRITE the backup.
3. READ the backup back and verify it.
4. READ the target again immediately before writing.
5. WRITE: token, then POST.
6. READ back; restore the backup on a mismatch.
7. RECORD the result.

**Residual risk:** Nitrado has no conditional write (UNSUPPORTED). An owner edit between steps 4 and 5 is detected at step 6 and restored from the backup. Mitigation: a declared no-edit window during each write gate. Write capability remains **UNVERIFIED** until Gate A's read-back.

## 3. Lost City — a separate owner decision

The Shop patch never touches Lost City (VERIFIED IN TEST). Re-enabling the map is its own proposal (`ProposeLostCityRestore`) and its own gate (`LOST_CITY`). It is never combined with a Shop gate.

The exact change against the current live file (VERIFIED LIVE inputs, NOT applied):

| | |
|---|---|
| Current SHA-256 | `99bcc7d7c38283e3402bad1fd883a025aa9839cba01a78e578e21ad14e079217` |
| Proposed SHA-256 | `d32505e07c715fcdb8bb00a18d2db61e910beb0ab8d9353578ca5f2ca54e76be` (2949 bytes) |

```
 		"objectSpawnersArr": [
-        ],
+			"custom/The_Lost_City.json"
+		],
```

**Behaviour:**

* If the Shop reference is applied first, the Lost City change appends after it. The hash must be recomputed on the day with `cmd/shop-canary-prepare`.
* A misspelled `The_Losst_City.json`, if it reappears, is corrected in place.
* Effect: 791 map objects are re-created at every start.

## 4. Canary record: design decision

`champion:d0:a1` is a **preview identity only**, and three independent layers refuse it:

* `ValidateProductionAttemptID` (VERIFIED IN TEST);
* `PreviewSingleItem` refuses a real delivery with ID ≤ 0 (VERIFIED IN TEST);
* the proposed ledger's `attempt_id = 'champion:d' || delivery_id || ':a' || attempt` CHECK, together with its tenant-bound foreign key (no delivery 0 exists), refuses it (VERIFIED IN TEST, integration).

| | A. Dedicated isolated canary record | B. Legitimate test purchase (existing Shop flow) |
|---|---|---|
| In `shop_deliveries` | Impossible without a `shop_purchases` row: `purchase_id` is a NOT NULL FK. That row needs `total_points > 0` and an idempotency key, so it would be a fabricated paid purchase with no `SHOP_PURCHASE` debit, and `ReconcileShop` would flag `MISSING_DEBIT`. | Created by `Purchase()`: purchase, debit and delivery `MANUAL_READY` in one transaction, idempotent and reconciled. |
| In a separate table | The ledger's tenant-bound FK to `shop_deliveries` cannot reference it; `NewPlan` requires a real `ShopDelivery`; the refund guard does not apply. It would need a parallel code path, so the canary would test something other than production. | The same code path real orders will use: `NewPlan`, the ledger FK, the refund and fulfil guards. |
| Refund / rollback | Ad hoc | The existing refund flow, blocked by the ledger while the item may be on the server |
| Cost | none | 1 point from the owner's linked player (`price_points > 0` is enforced) |
| **Compatible with current database guarantees** | **No** | **Yes** |

**Recommendation: B.**

* The owner creates a product such as "Canary BandageDressing": 1 point, delivery policy `MANUAL_COORDINATE`, `stock_mode FINITE` with stock 1, `purchase_limit 1`, active only for the purchase minute, so no other player can buy it.
* The owner buys it while standing at the drop point, entering the drop point's X/Z.
* `PreviewSingleItem` requires the delivery to be within **0.5 m** of the verified drop point, exactly one item, quantity 1, the correct tenant, and an open delivery (VERIFIED IN TEST).

Nothing is created until the owner approves Gate D.

## 5. Durable attempt ledger (proposal `0054_shop_delivery_attempts`)

`canary.ProposedAttemptMigrationSQL` is **not registered** in `internal/database/migrations.go`; `TestMigrationIsProposalOnly` enforces this. `TestProposedLedgerMigration` (integration tag) runs all registered migrations on CI's disposable database and applies the proposal inside a transaction that is always rolled back, then checks:

| Guarantee | Mechanism | Result |
|---|---|---|
| Tenant isolation | FK `(delivery_id, organization_id, installation_id)` → a new unique index on `shop_deliveries`; the insert guard re-checks the tenant; the CAS is scoped by org and installation | Cross-tenant insert refused; another tenant's CAS moves nothing |
| Compare-and-set | `ProposedTransitionSQL` with `WHERE attempt_id AND state=expected AND tenant` | Wrong expected state: 0 rows; repeated CAS: 0 rows |
| State machine | `BEFORE UPDATE` guard allows only the transitions of `nitradodelivery.CanAdvance`, including `UNSTAGE_REQUIRED`; the identity is immutable; terminal rows are immutable | Skipped states, identity edits and terminal edits refused |
| Evidence at each stage | CHECKs: staged needs digest, time and boot; restart needs the new boot; verification and `UNSTAGED` need a verified unstage (time and digest); `FULFILLED` needs `verified_by`; `FAILED_REVIEW` needs a reason | Each missing piece refused |
| Duplicate prevention | partial unique index (one open attempt per delivery); the insert guard refuses after any attempt that is fulfilled or under review; the attempt number must follow history; the insert locks the delivery row | Second open attempt, retry after `FAILED_REVIEW`, and wrong number refused; retry after `UNSTAGED` allowed |
| Refund restrictions | trigger on `shop_deliveries`: no `CANCELLED` (refund) or manual `FULFILLED` while an attempt is in `FILE_PREPARED`…`VERIFICATION_REQUIRED`; `PLAN_CREATED → FILE_PREPARED` requires an open delivery and a `PENDING_FULFILLMENT` purchase | Refund refused while in flight or during verification; refund allowed at `PLAN_CREATED` and after `FAILED_REVIEW` (a human decision); a refunded attempt can never be prepared; no attempt on a refunded delivery |
| Fulfillment integrity | deferred constraint trigger: a `FULFILLED` attempt commits only with its delivery `FULFILLED` | Refused alone; accepted together |
| Audit | every transition is written to `shop_delivery_attempt_events` with a mandatory `champion.actor` | Missing actor refused; 7 events for a full run |
| Crash recovery | every field `Reconcile` and `DecideRestart` need is durable; `ProposedOpenAttemptsSQL` lists open attempts per tenant | Reads exactly the open attempt |

The class for this whole table is **VERIFIED IN TEST** once PR #95's integration job is green (section 10).

**Deployment is its own step (Gate C):** a separate code change that registers the migration, plus a deploy, with no file write. Before that change:

* `Refund` must map the guard's `check_violation` to a clear 409 ("an automatic delivery attempt is in progress"). Today it would surface as a generic error.
* Gate I needs a single transaction that moves the attempt to `FULFILLED` and fulfils the delivery and purchase.

## 6. Restart, exposure and cleanup verification

**Exposure is real from the moment the item is staged.** Any start spawns it, including a scheduled one. `CheckStagingReadiness` (VERIFIED IN TEST) refuses Gate E unless:

* a named operator has **confirmed availability** from now for at least `MinOperatorWindow` = 2 × the observed 68-min interval (136 min). That covers a scheduled restart, boot acceptance (about 9 min) and the unstage after it;
* the current boot is accepted by boot authority and is at least 10 min old (the staging quiet period);
* no restart.log pre-start line is pending;
* the drop point observation is at most 20 min old.

**Recommended timing:** stage shortly after a boot is accepted, then have the owner restart at once (Gate F, manual). The unstage then completes about 10 min after the new boot, long before the next scheduled restart.

**The six milestones are separate evidence** (`Milestones.Check`, VERIFIED IN TEST):

| # | Milestone | Evidence | What it does *not* prove |
|---|---|---|---|
| 1 | Restart initiated | restart.log pre-start line, or the owner's restart | a boot happened; **never** a reason to unstage |
| 2 | New boot accepted | boot authority accepts a boot that started after the verified stage | that the spawner ran |
| 3 | Spawner processing attempted | that boot's RPT reached CE init with no `[::SpawnObjects]` error naming the Champion file | an item exists: **an absent error is not delivery** |
| 4 | Item physically observed | in game, a named observer sees the bandage at the drop point, and it is **picked up** | — |
| 5 | Entry removed | the empty file read back with its SHA, timestamped **before** the next boot starts | — |
| 6 | No additional spawn | a later accepted boot, then an in-game check: **no new** bandage at the drop point | — |

The original bandage does **not** have to remain at the drop point; it is expected to have been collected. A milestone is not reached when:

* an in-game observation has no named observer;
* the entry was removed only after the second boot started;
* the no-respawn check came before the second boot.

`DecideRestart` handles the remaining cases:

* A pre-start line alone keeps the entry staged.
* An unaccepted boot is ignored.
* The first accepted boot leads to `UNSTAGE_REQUIRED`.
* Two boots before a verified unstage lead to `FAILED_REVIEW`.
* A boot inside the quiet period leads to `FAILED_REVIEW`.

`Fulfill` accepts only all of the following:

* state `VERIFICATION_REQUIRED`;
* a named `IN_GAME_OBSERVED` confirmation;
* a pickup (who, and when, not before the sighting);
* the in-game no-respawn check;
* all six milestones complete.

Log evidence, restarts and uploads never fulfil (VERIFIED IN TEST).

## 7. Drop point: re-validation on the day

* **Old coordinates.** The 2B.3 coordinates `<4621.1, 8397.2, 319.6>` are an **illustrative sample only**. `VerifyDropPoint` rejects them against the current session (`ErrDropPointPreviousBoot`).
* **Current boot.** The current boot has a source-authoritative ADM player list with altitude (VERIFIED LIVE, section 1). It is someone's current position, not an owner-chosen spot, and it was already 13 min old when read, so it is **not** a drop point.

**On the day:**

1. The owner stands at a flat, open spot.
2. The next five-minute `PLAYER_LIST` line records the position with altitude, from the current accepted boot.
3. `VerifyDropPoint` accepts it only when it is:
   * a server-written ADM row with source file and offset;
   * from the current, un-ended session;
   * on `chernarusplus`;
   * for the correct tenant;
   * at most 20 min old.
4. The owner buys the canary at that X/Z (Gate D).
5. Plan, attempt ID, fingerprint and staged and unstaged hashes are recomputed from the real delivery. The test shows they differ from the preview.
6. `CheckStagingReadiness` re-checks freshness immediately before Gate E.

**Preview values (SIMULATED, synthetic delivery 0 at the sample position; never usable in production):**

| | |
|---|---|
| Attempt ID | `champion:d0:a1` |
| Fingerprint | `b768ea33…9994` |
| Staged SHA-256 | `b8be6bb5…1476` |
| Unstaged SHA-256 | `328c4d64…dc5f` (the empty file) |

## 8. Revised execution plan (each gate needs its own explicit approval)

| Gate | Action | Affected path / record | Precondition | Evidence to proceed | Rollback condition and action |
|---|---|---|---|---|---|
| **A** | Create the empty Champion file | `champion/champion_shop_delivery.json` (CREATE) | file absent (or already exactly empty: skip) | read-back SHA `328c4d64…`; write capability VERIFIED LIVE | Upload fails or the directory is not created: stop; the owner creates the folder or deletes the file |
| **B** | Reference it | `cfggameplay.json` (MODIFY), `champion/backup/cfggameplay.json.99bcc7d7c382.bak` (CREATE) | `CheckReferencePrecondition` passes on a fresh read; config still `99bcc7d7…` | read-back `7c66bf0d…`; the next boot (a scheduled one is fine) shows no Champion spawner error | Mismatch, or an error at the next boot: restore the backup (`99bcc7d7…`) |
| **C** | Deploy the ledger | database: `shop_delivery_attempts`, `shop_delivery_attempt_events`, triggers, `uq_shop_deliveries_id_tenant` | integration test green; Refund 409 mapping ready; a separate PR and deploy | `schema_migrations` row; refunds and fulfils without attempts unchanged | Any regression: drop the triggers, then the tables |
| **D** | Canary purchase | canary product (owner), `shop_purchases`, `shop_deliveries`, `SHOP_PURCHASE` ledger row | fresh drop point (owner at the spot); product at 1 point, FINITE stock 1, limit 1 | `PENDING_FULFILLMENT` + `MANUAL_READY`, within 0.5 m, a real ID | Wrong coordinates or any doubt: refund through the normal flow |
| **E** | Stage BandageDressing ×1 | `champion/champion_shop_delivery.json` (REPLACE) | `CheckStagingReadiness` passes (operator window, quiet period, no pre-start, freshness); attempt `FILE_PREPARED`; file reads as the empty file | read-back of the staged SHA; attempt `AWAITING_RESTART` | Before any boot: restore the empty file, verify it, attempt `UNSTAGED` |
| **F** | First restart and observation | server restart (owner) or the next scheduled one | operator present | milestones 1–4 | None; go to G at once |
| **G** | Unstage | `champion/champion_shop_delivery.json` (REPLACE with empty) | `UNSTAGE_REQUIRED`; file still the staged SHA | milestone 5 before the next boot; attempt `VERIFICATION_REQUIRED` | File not the staged SHA, or a second boot first: `FAILED_REVIEW` |
| **H** | Second restart and in-game check | server restart | G verified | milestone 6 | A new bandage appears: `FAILED_REVIEW` |
| **I** | Fulfil | attempt `FULFILLED`, delivery and purchase `FULFILLED` (one transaction) | `Fulfill` accepts the confirmation | database rows | Item never observed: `FAILED_REVIEW` (refund by decision) |
| **LOST_CITY** (optional, independent) | Re-enable the map | `cfggameplay.json` | owner decision; hash recomputed on the day | read-back SHA; the next boot shows no Lost City error | Restore the backup |

**Owner authorization requests, in order:**

1. Approve Gate A (one upload, creating the empty file).
2. After A is verified, approve Gate B (backup plus configuration upload).
3. Approve the separate ledger PR and deploy (Gate C).
4. Choose the canary record (B recommended), create the canary product, and approve Gate D at the drop point.
5. Name the operator and the window, and choose manual or scheduled restart for F.
6. Approve E, F, G, H and I one at a time, each after the previous gate's evidence.
7. Separately, decide Lost City.

## 9. Tests

All VERIFIED IN TEST:

* `go test ./...` passes: 39 packages.
* `go test -race` passes for `internal/shop/...`, `internal/livesync`, `internal/discord`, `internal/killfeed` and `internal/app`.

| Requirement | Tests |
|---|---|
| Sequencing and reference guard | `TestPatchEmptyArrayMinimalAndExact`, `TestReferenceRequiresTheEmptyChampionFile` |
| Config preservation, exact diff, CRLF, refusal | `TestShopPatchPreservesEverythingElse`, `TestPatchRefusesUnsafeInputs` |
| Lost City separation | `TestLostCityRestoreIsSeparate` |
| Drop point (tenant, map, altitude, source, current boot, freshness) | `TestDropPointRules` |
| Canary plan, placeholder refusal, real-delivery recompute and match | `TestSingleItemPreview` |
| Restart decisions | `TestRestartRules` |
| Milestones | `TestMilestonesAreDistinct` |
| Staging readiness and operator window | `TestStagingReadiness` |
| Physical fulfillment with pickup | `TestFulfillOnlyOnPhysicalConfirmation` |
| Gate order and properties; upload sequence | `TestGatesAndUploadSequence` |
| Ledger not registered | `TestMigrationIsProposalOnly` |
| Ledger guarantees (PostgreSQL) | `TestProposedLedgerMigration` (integration) |
| State machine and reconcile | `nitradodelivery` tests |
| Read-only and redaction | `TestPackageIsNonExecuting`, `TestOutputsCarryNoCredentials`, 2C.1 `TestReaderIsReadOnly` |

## 10. CI

PR #95: `backend-ci` runs build, vet (including the integration tag), unit tests, race tests, and integration tests on a disposable PostgreSQL 16. The status of the latest commit is reported in the PR.

## 11. Live operations performed in this revision (read-only only)

* Capability discovery and `shop-canary-prepare`: GETs and signed downloads of `cfggameplay.json`, the Champion path (absent) and `custom/The_Lost_City.json`.
* A listing of the mission folder and `custom/`.
* Reads of the recent RPT files and the current ADM (counts only).
* `GET /api/admin/live-sync`.

## 12. Verdict

**BLOCKED — AWAITING OWNER DECISIONS**

The preparation is complete, but nothing can run until the owner decides:

1. Whether to authorize Gate A, the first write.
2. The canary record (B recommended) and the canary product.
3. Whether and when to deploy the ledger (Gate C, a separate PR).
4. The operator, the availability window, and manual vs scheduled restart for Gate F.
5. The drop-point spot. The observation itself must be fresh on the day.
6. Lost City: re-enable or leave off (independent).
