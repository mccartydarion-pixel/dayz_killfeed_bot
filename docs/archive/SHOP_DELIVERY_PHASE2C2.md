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
* the ledger's (migration 0054, PR #97) `attempt_id = 'champion:d' || delivery_id || ':a' || attempt` CHECK, together with its tenant-bound foreign key (no delivery 0 exists), refuses it (VERIFIED IN TEST in PR #97).

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

## 5. Durable attempt ledger (implemented in PR #97)

The ledger proposal first drafted in this PR has been **removed from here**. It is implemented, registered and tested as migration `0054_shop_delivery_attempts` in PR #97 (`internal/database/shop_attempts.go`, `internal/repository/shop_attempt_repository.go`, `docs/SHOP_DELIVERY_PHASE2C3.md`). This PR defines no migration, so there is no duplicate schema and no competing state machine: `canary.LedgerMigrationName` only names the migration this canary depends on.

PR #97 goes beyond the original proposal:

* write-once evidence columns;
* physical-evidence requirements for `FULFILLED`, including the second boot after the verified unstage;
* a one-time human review resolution (`NOT_SPAWNED` / `SPAWNED`);
* custom SQLSTATEs mapped to typed errors;
* the Shop `Refund` and `Fulfill` integration, returning 409 `DELIVERY_ATTEMPT_ACTIVE`.

The `UNSTAGE_REQUIRED` change to `internal/shop/nitradodelivery/attempt.go` is byte-identical in both PRs, so whichever merges second rebases without conflict. The operator tooling that drives the ledger for the canary is Phase 2C.4 (a separate PR stacked on #97).

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
| **C** | Deploy the ledger (PR #97, migration 0054) | database: `shop_delivery_attempts`, `shop_delivery_attempt_events`, triggers, `uq_shop_deliveries_id_tenant` | PR #97 approved and merged; its CI green | `schema_migrations` row; refunds and fulfils without attempts unchanged | Any regression: drop the triggers, then the tables |
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
3. Approve PR #97 (migration 0054) and its deploy (Gate C).
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
| Config preservation with Lost City already enabled (live since 2026-09-25) | `TestShopPatchKeepsEnabledLostCity` |
| Ledger migrations on existing data, concurrent startups (PR #97) | `TestShopLedgerMigrationsOnExistingData`, `TestConcurrentStartupsMigrateOnce` (integration) |
| State machine and reconcile | `nitradodelivery` tests |
| Read-only and redaction | `TestPackageIsNonExecuting`, `TestOutputsCarryNoCredentials`, 2C.1 `TestReaderIsReadOnly` |

## 10. CI

PR #95: `backend-ci` runs build, vet (including the integration tag), unit tests, race tests, and integration tests on a disposable PostgreSQL 16. The status of the latest commit is reported in the PR.

## 11. Live operations performed in this revision (read-only only)

* Capability discovery and `shop-canary-prepare`: GETs and signed downloads of `cfggameplay.json`, the Champion path (absent) and `custom/The_Lost_City.json`.
* A listing of the mission folder and `custom/`.
* Reads of the recent RPT files and the current ADM (counts only).
* `GET /api/admin/live-sync`.

## 12. Verdict (original revision; superseded by section 13)

**BLOCKED — AWAITING OWNER DECISIONS**

The preparation is complete, but nothing can run until the owner decides:

1. Whether to authorize Gate A, the first write.
2. The canary record (B recommended) and the canary product.
3. Whether and when to merge and deploy PR #97 (Gate C).
4. The operator, the availability window, and manual vs scheduled restart for Gate F.
5. The drop-point spot. The observation itself must be fresh on the day.
6. Lost City: re-enable or leave off (independent).

## 13. Final canary preparation (2026-09-26)

This section supersedes the live values in sections 1 and 8 where they differ.

### 13.1 What changed since the review

| Item | Status |
|---|---|
| Gate C (ledger, migration 0054, PR #97) | **Done.** Deployed 2026-09-26 (Railway `997fb02e`); `cmd/shop-ledger-verify -phase 0054`: all PASS. |
| Operator service and evidence (migration 0055, PR #98) | **Deployed** 2026-09-26 (Railway `23d3b0a1`, commit `a849ee6`). `-phase 0055`: all PASS. 55 migrations; 0 attempts, history and evidence rows; economy totals unchanged; canary API `executionLocked: true`; mutation refused with 423. |
| Canary execution lock | **Closed.** Neither `CHAMPION_SHOP_CANARY_EXECUTION` nor `CHAMPION_SHOP_CANARY_INSTALLATION_IDS` is set. The startup log reports `execution_enabled=false installations=0`. |
| The Lost City | **Re-enabled by the owner** on 2026-09-25. The last verified live `cfggameplay.json` (2026-09-25, `docs/SHOP_LEDGER_ROLLOUT.md` section 0) is 2951 bytes, SHA-256 `4d000807963a…`, `objectSpawnersArr = ["custom/The_Lost_City.json"]`. |

What follows from this:

* **Section 1's `cfggameplay.json` facts and Gate B's hashes in section 8 are obsolete.** Section 1 records `99bcc7d7…` and an empty array; Gate B pins `99bcc7d7…` → `7c66bf0d…`. Gate B must instead use the current and proposed SHA-256 printed by a fresh `cmd/shop-canary-prepare` run on the day.
* **Gate B keeps the Lost City reference.**
  * `ProposePatch` appends `champion/champion_shop_delivery.json` after every existing entry and changes no other byte (`TestShopPatchKeepsEnabledLostCity`).
  * The expected result is `["custom/The_Lost_City.json", "champion/champion_shop_delivery.json"]`.
* **The `LOST_CITY` gate is not needed.** `ProposeLostCityRestore` reports "nothing to change" on the current file, before and after the Shop reference.
* **The empty Champion file is unchanged:** 20 bytes, SHA-256 `328c4d64bb81bdbdddad2431a12b6197182f5fa5646bf16c7e0e8d437bc8dc5f`.

### 13.2 Fresh live read (owner-run, read-only)

This revision did **not** re-read the live server: the session was not permitted to use the production Nitrado token.

The owner runs it from the repository folder:

```
railway run -s dayz_killfeed_bot go run ./cmd/shop-canary-prepare -service 19806451 -org 1 -installation 11 -game-server 1
```

It only makes GET requests and signed downloads. It requests no upload token, and prints no token, signed URL or account path.

Accept the output only if all of these hold:

* the mission is the Chernarus mission already verified in 2C.1;
* the Gate A line reads `current Champion file: not readable (absent)`, or the file is present with SHA-256 `328c4d64…`;
* the Gate B `current` line is the file the owner expects. An owner edit changes its SHA-256, which is fine, but that value is what Gate B pins;
* `objectSpawnersArr` goes from `["custom/The_Lost_City.json"]` to `["custom/The_Lost_City.json" "champion/champion_shop_delivery.json"]`, and the only change is `add champion/champion_shop_delivery.json`;
* `[LOST_CITY]` reports no change, and `custom/The_Lost_City.json` parses as a spawner file.

The boot identity is read on the day of Gate D, because the drop point must come from the current boot. It is `bootAuthority.acceptedBoot` from `GET /api/admin/live-sync`.

### 13.3 The BandageDressing canary plan

* **Item.** One `BandageDressing`, quantity 1, built through the production plan validator (`nitradodelivery.NewPlan`).
* **Drop point.** It must be verified for the current boot (`VerifyDropPoint`): tenant, `chernarusplus`, altitude from the ADM, current-boot source, and freshness.
* **Product.** 1 point, FINITE stock 1, purchase limit 1, active only for the purchase window.
* **Purchase.** A normal Shop purchase by the owner (Gate D) at the drop point's X/Z. The delivery must be within 0.5 m of the drop point.
* **Staged file.** Its content and SHA-256 depend on the real delivery ID (`champion:d<id>:a1`). They are computed from the real record after Gate D (`PreviewSingleItem` with `Delivery` set).
* **No placeholder delivery.** A preview built on delivery 0 is refused for production by `ValidateProductionAttemptID`; the ledger's CHECK constraint and foreign key refuse it too.
* **Unstaged content.** The empty file (`328c4d64…`), which is also the rollback content.
* **Recording.** With 0055 deployed, gates D–I are recorded through the operator API (`/shop/canary/attempts`): create attempt, evidence, advance, review and fulfil.
  * Physical evidence kinds need `IN_GAME_OBSERVATION`.
  * Fulfilment needs a named pickup confirmation.
  * A review needs a `REVIEW_OBSERVATION`.

### 13.4 Authorization gates, in order

Each gate is a separate, explicit owner approval.

| # | Gate | What it does | Touches |
|---|---|---|---|
| 1 | **A — create the empty Champion file** | One upload of the 20-byte empty spawner file, read back as `328c4d64…`. Nothing references it yet. | Nitrado file (new) |
| 2 | **B — reference it** | Back up `cfggameplay.json`, upload the patch pinned to the fresh current SHA-256, and read back the proposed SHA-256. The next boot (a scheduled one is fine: the file is empty) must show no Champion spawner error. | Nitrado `cfggameplay.json` |
| 3 | **Open the execution lock** | Set `CHAMPION_SHOP_CANARY_EXECUTION=enabled` and `CHAMPION_SHOP_CANARY_INSTALLATION_IDS=11` on `dayz_killfeed_bot`. This redeploys the bot only, never Champions. | Railway variables |
| 4 | **D — canary purchase** | The owner buys the 1-point canary at the fresh drop point, and the attempt is created (`PLAN_CREATED`). | Shop records (1 point) |
| 5 | **E — stage the file** | After `CheckStagingReadiness` passes, replace the empty Champion file with BandageDressing ×1 and read back the staged SHA-256. | Nitrado Champion file |
| 6 | **F — restart** | An owner-triggered restart (recommended: shortest exposure) or the next scheduled one. The operator records the milestones. | Champions restart |
| 7 | **G — unstage** | Restore the empty file and verify it before any further boot (`UNSTAGE_REQUIRED` → `VERIFICATION_REQUIRED`). | Nitrado Champion file |
| 8 | **H — second restart and no-respawn check** | One more boot. In game, no new bandage appears at the drop point. | Champions restart |
| 9 | **I — physical confirmation and fulfilment** | Record the named in-game observation and pickup as evidence. Then mark the attempt, delivery and purchase `FULFILLED` in one transaction. | Shop records |
| 10 | **Close the execution lock** | Unset both variables. This redeploys the bot only. | Railway variables |

Rollback at every step is as in section 8.

Automatic delivery stays disabled throughout: `nitradodelivery.PrototypeAdapter.Enabled = false`, and there is no worker.

### 13.5 Remaining blockers

1. The owner's fresh, read-only `shop-canary-prepare` run (13.2), which pins Gate B's hashes.
2. Owner approval of Gate A, the first live write. Write capability stays UNVERIFIED until then.
3. For Gate F: the operator, the availability window, and manual vs scheduled restart. The drop point on the day.
4. Merging this PR. It contains code and documents only, no migration, and nothing in it executes.
