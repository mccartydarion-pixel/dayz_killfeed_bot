# CHAMPIONS® SHOP PHASE 2C.2 — PRE-EXECUTION REVIEW

Live server canary **preparation** for automatic console delivery (Champions, Nitrado service 19806451, organization 1, installation 11, game server 1). This phase is read-only.

* No file was written or uploaded, and no upload token was requested.
* No restart was performed or requested.
* No purchase, delivery or attempt record was created or changed.
* Automatic Shop delivery stays disabled.

Every production write and restart below is a separate owner gate (section 9).

Each finding is classified as follows:

| Class | Meaning |
|---|---|
| **VERIFIED LIVE** | Observed on the production server or API by a read-only request during this phase (2026-09-24/25 UTC). |
| **VERIFIED IN TEST** | Proven by a Go test in this PR. |
| **SUPPORTED BY DOCUMENTATION** | Bohemia's published scripts or Nitrado's documented behaviour. It has not been exercised here. |
| **UNVERIFIED** | Not yet shown. |
| **UNSUPPORTED** | Not available. |

Code:

| Path | What it holds |
|---|---|
| `internal/shop/canary` | Patch generator, drop-point rules, single-item preview, restart and unstage decision, fulfillment rule, gates, upload sequence and the migration proposal. It is non-executing and imports no network, database or Nitrado package (enforced by a test). |
| `internal/shop/nitradodelivery/attempt.go` | Adds `UNSTAGE_REQUIRED`. |
| `cmd/shop-canary-prepare` | The read-only report tool. |

## 1. Live baseline

The server has 18 slots, runs version 1.29.163709 and has its mission at `dayzps_missions/dayzOffline.chernarusplus`.

| Fact | Value | Class |
|---|---|---|
| Service type | `dayzps` (PlayStation) | VERIFIED LIVE |
| `enableCfgGameplayFile` | `1` (Nitrado settings) | VERIFIED LIVE |
| `cfggameplay.json` | 2924 bytes, LF line endings, tab indentation | VERIFIED LIVE |
| `cfggameplay.json` SHA-256 | `99bcc7d7c38283e3402bad1fd883a025aa9839cba01a78e578e21ad14e079217` | VERIFIED LIVE |
| `cfggameplay.json` modified | 2026-09-25 00:44:55 UTC (an owner edit) | VERIFIED LIVE |
| `WorldsData.objectSpawnersArr` | **empty** (`[]`). The misspelled `custom/The_Losst_City.json` reference seen in 2C.1 (SHA `8d989bc4…`, 2952 bytes) was **removed** by the owner, not corrected. | VERIFIED LIVE |
| `custom/The_Lost_City.json` | exists: 354,796 bytes, modified 2026-09-15. It is not referenced. | VERIFIED LIVE |
| `champion/` directory | absent. `champion/champion_shop_delivery.json` is absent and not referenced. | VERIFIED LIVE |
| Boots 16:26, 17:34, 18:42, 19:51 (server time) | each RPT logs `[::SpawnObjects] [ERROR] File "$mission:custom/The_Losst_City.json" does not exist`, about 11 s after the first `[CE]` line and about 35–40 s after the boot starts. The server still started normally. | VERIFIED LIVE |
| Boot 20:46:52 (after the edit) | no `[::SpawnObjects]` line at all, so the owner's empty array is live | VERIFIED LIVE |
| Current boot | `DayZServer_PS4_x64_2026-09-24_20-46-53.ADM`, accepted by boot authority at 00:54:24 UTC (about 7.5 min after the boot). The session is current and not ended. | VERIFIED LIVE |
| Restart cadence | 68 min between the four scheduled boots. The fifth came 55 min later (probably owner-triggered after the edit; the cause is UNVERIFIED). | VERIFIED LIVE (intervals) |

Two consequences shape the plan:

* **A missing spawner file is non-fatal.** The server logs an error and boots. This is VERIFIED LIVE over four boots.
* **A successful spawn logs nothing.** Only failures produce a `[::SpawnObjects]` line, so the RPT can never prove that an item exists. It can only fail to show an error.

## 2. Configuration patch (NOT applied)

The spec expected the misspelled Lost City reference to still be present. It is not: the owner already emptied the array. The patch is therefore computed from the live file:

* it corrects the spelling only if the misspelled reference is present;
* it adds the Champion entry only if it is absent;
* it **never re-adds Lost City by default**, because that would silently re-enable 354 KB of map objects. Re-adding it is an explicit owner option (`-readd-lost-city`).

| | |
|---|---|
| Current SHA-256 | `99bcc7d7c38283e3402bad1fd883a025aa9839cba01a78e578e21ad14e079217` (2924 bytes) — VERIFIED LIVE |
| Proposed SHA-256 | `7c66bf0dce23e4baf27819f6df366b594d562bc1098314f15f86aa8b99bb5de9` (2960 bytes) — computed from the live bytes, VERIFIED IN TEST for the method |
| Change | `objectSpawnersArr: [] -> ["champion/champion_shop_delivery.json"]` |
| Affected files | `cfggameplay.json` MODIFY (Gate A); `champion/champion_shop_delivery.json` CREATE (Gate B) |

Exact diff (every other byte is unchanged; the old closing line is 8 spaces followed by `],`):

```
 		"objectSpawnersArr": [
-        ],
+			"champion/champion_shop_delivery.json"
+		],
```

The generator rewrites only the array's byte span, keeping CRLF or LF as found. It refuses the patch when:

* the key is missing, or occurs more than once;
* the array is not an array of strings;
* the file is invalid JSON;
* the result is not deep-equal to the current file in every other key (`ErrPatchNotMinimal`).

Re-running it on the patched file returns `ErrNoChange`. All of this is VERIFIED IN TEST, and a test pins the byte-exact output for the live formatting.

**Backup:**

1. Download the file and confirm the current SHA-256.
2. Upload the same bytes to `champion/backup/cfggameplay.json.99bcc7d7c382.bak`.
3. Read the backup back and confirm the SHA-256.
4. Keep a second, local copy.

Whether an upload creates the missing `champion/` directory is **UNVERIFIED**. If it does not, the backup goes to a mission-root name, which the server never reads.

**Rollback:** re-upload the backup and confirm SHA `99bcc7d7…`. It takes effect at the next start. The Champion file may remain, because an unreferenced file is never read.

## 3. Drop point

Rules (`VerifyDropPoint`, VERIFIED IN TEST). A drop point is accepted only when all of these hold:

* It is a **server-written ADM observation**: a `PLAYER_LIST` row or an ADM event.
* It has a canonical source file and offset.
* It has a **source altitude**. An altitude is never invented and must lie in the range −50 to 1500.
* It is on the installation's map (`chernarusplus`), and the organization and installation match.
* Its ADM file **is the current boot session** from `server_adm_sessions`, and that session has not ended. A position from a previous boot is never current.
* It is at most 20 minutes old.

The conversion is ADM `<x, z, altitude>` → spawner `pos [x, altitude, z]`.

**Current status: NO VERIFIED DROP POINT.**

* The 2B.3 candidate is ADM `<4621.1, 8397.2, 319.6>` → `[4621.1, 319.6, 8397.2]`. It comes from `DayZServer_PS4_x64_2026-09-24_04-15-07.ADM`, and `VerifyDropPoint` rejects it with `ErrDropPointPreviousBoot` against the current session. It has no source identity in the database either.
* A usable point needs a player (the owner) standing at the intended spot during the current session. That produces a fresh `PLAYER_LIST` row with altitude, captured immediately before Gate B.

## 4. Single-item test: BandageDressing ×1

The plan is built by the production validator, `nitradodelivery.NewPlan`. The class and quantity are fixed at `BandageDressing` and 1. The values below are a **SIMULATED PREVIEW**: a synthetic delivery with ID 0 at the 2B.3 position. They are not a verified-live result, and every one of them changes with the real drop point and the real delivery record.

| | Simulated value |
|---|---|
| Attempt ID | `champion:d0:a1` (the real one is `champion:d<canary delivery id>:a1`) |
| Fingerprint | `b768ea33f4ca8d4057e57458666c8ec3dbeb792827e13923335c927078eb9994` |
| Staged file SHA-256 | `b8be6bb52fce1e4d3faaf3797a3b07533f00b8ab12e90046a0e4ad7198bb1476` |
| Unstaged file SHA-256 | `328c4d64bb81bdbdddad2431a12b6197182f5fa5646bf16c7e0e8d437bc8dc5f` (`{"Objects": []}`) |

Staged spawner JSON (Gate B uploads this content, creating the file):

```json
{
  "Objects": [
    {
      "name": "BandageDressing",
      "pos": [4621.1, 319.6, 8397.2],
      "ypr": [0, 0, 0],
      "scale": 1,
      "enableCEPersistency": false,
      "customString": "champion:d0:a1:u1"
    }
  ]
}
```

The real file is rendered with one number per line by `Render`; the hashes above are computed on the rendered bytes.

* **Stage diff:** absent → the content above.
* **Unstage diff:** the object is removed, leaving `"Objects": []`.
* **Rollback before any start:** upload the unstaged content, verify SHA `328c4d64…`, and move the attempt to `UNSTAGED`. No delete call is needed.
* **Rollback after a start:** none. Unstage (Gate D), then verify.

## 5. File-write readiness

Write capability is **UNVERIFIED**. The token's roles list `FILEBROWSER_WRITE` (SUPPORTED BY DOCUMENTATION), but no upload has been performed or authorized. Nitrado has **no conditional write**: there is no If-Match and no version check. The sequence (`UploadSequence`, VERIFIED IN TEST for its structure):

1. READ the target; abort unless its SHA-256 is the expected before-digest (or the file is absent, as expected).
2. WRITE the backup.
3. READ the backup back and verify it.
4. READ the target again immediately before writing; abort if it changed.
5. WRITE: request a single-use upload token and POST the content. A failed response does not prove that nothing was written.
6. READ the target back. If it is neither the after-digest nor the before-digest, restore the backup and verify.
7. RECORD the attempt transition, with both digests, only after step 6 verified.

**Residual risk:** an owner edit between steps 4 and 5 would be overwritten. Step 6 detects it, and the backup restores it. The mitigation is a declared no-edit window during each write gate.

## 6. Restart detection and unstage rules

`DecideRestart` is VERIFIED IN TEST. The timings are VERIFIED LIVE:

* The restart.log pre-start line is seen 6–13 s after it is written, about 40 s before the boot.
* The spawner runs about 35–40 s after the boot starts.
* The new boot's ADM is accepted about 7.5–9 min after the boot.
* The RPT of a running boot is readable.

The rules:

* **A pre-start line alone never unstages.** The entry must still be in the file when the new boot's spawner runs.
* **New-boot identity comes only from boot authority.** A boot counts only if it is accepted (source-authoritative), is not the boot that was current when staging was verified, and started after the staged read-back was verified.
* **Whether the spawner processed the file:**
  * `FAILED` if the RPT names the Champion file in a `[::SpawnObjects]` error;
  * `NO_ERROR_REPORTED` if CE init was reached without such an error — **never proof of an item**;
  * `UNKNOWN` otherwise.
* **First new boot:** RESTART_OBSERVED → **UNSTAGE_REQUIRED at once.** The window before the next scheduled start is about 55–68 min minus about 9 min of ADM latency.
* **Entries remaining:** the unstaged file must be read back with the unstaged SHA-256 before verification (`FileContainsAttempt=false`).
* **A second new boot before the verified unstage** means a possible double spawn: **FAILED_REVIEW**, no retry.
* **A boot that started within 10 min before staging** is ambiguous: FAILED_REVIEW. Staging must therefore wait at least 10 min after a boot.
* **The server restarts on its own schedule.** A staged entry spawns at the next scheduled start, whether or not the owner acts, so Gate C must be approved together with Gate B's timing.

## 7. Attempt state machine and durable ledger (proposal)

`nitradodelivery` now has this flow (VERIFIED IN TEST):

`PLAN_CREATED → FILE_PREPARED → FILE_STAGED → AWAITING_RESTART → RESTART_OBSERVED → UNSTAGE_REQUIRED → VERIFICATION_REQUIRED → FULFILLED`

* `RESTART_OBSERVED` can no longer go straight to `VERIFICATION_REQUIRED`.
* `UNSTAGE_REQUIRED` can go only to `VERIFICATION_REQUIRED` or `FAILED_REVIEW`.
* After a crash, `Reconcile` sends a started-while-staged attempt to `UNSTAGE_REQUIRED` if the entry is still in the file, or to `VERIFICATION_REQUIRED` if it is gone. It never re-stages.
* `Fulfill` is the only path to `FULFILLED`. It requires `VERIFICATION_REQUIRED`, a clean second start, and a named `IN_GAME_OBSERVED` confirmation. A restart, an upload or a log line never fulfils.

The durable table `shop_delivery_attempts` and the history table `shop_delivery_attempt_events` exist as `canary.ProposedAttemptMigrationSQL`, name `0054_shop_delivery_attempts`. **They are not registered in `internal/database/migrations.go`**, and a test enforces that. The proposal includes:

* a state CHECK with all 11 states;
* `UNIQUE (delivery_id, attempt)`;
* a partial unique index allowing one open attempt per delivery;
* CHECKs that `VERIFICATION_REQUIRED` and `FULFILLED` require `unstage_verified_at`, and that `FULFILLED` requires `verified_by`;
* a compare-and-set transition statement scoped by organization and installation.

Status: UNVERIFIED on a real database. Deploying it needs its own approval.

**Failure handling:**

| Situation | Result |
|---|---|
| Upload response lost | Step 6 read-back decides; `Reconcile` promotes the attempt to FILE_STAGED if the entry is present, or abandons it if absent. |
| Worker crash while awaiting restart | `Reconcile` together with `DecideRestart`. |
| File changed by someone else | Stop; FAILED_REVIEW if it was already staged. |
| Two boots before the verified unstage | FAILED_REVIEW. |
| Spawner error naming the Champion file | Unstage, then review. |

## 8. Gates (each one is a separate approval)

| Gate | Action | Writes / restarts | Key preconditions | Evidence before the next gate |
|---|---|---|---|---|
| **A** | Back up and patch `cfggameplay.json` | write | Live SHA still `99bcc7d7…`; verified backup; no-edit window | Read-back SHA `7c66bf0d…`; next boot shows no unexpected spawner error |
| **B** | Upload the staged Champion file (BandageDressing ×1) | write | A verified; fresh current-session drop point; canary delivery record exists (its own approval); ledger deployed or the operator records each transition; ≥10 min since the boot; no pending pre-start | Read-back SHA equals the staged SHA; attempt AWAITING_RESTART |
| **C** | First restart (manual, or the next scheduled one) | restart | B verified; an operator is available to unstage within about 50 min | Boot authority accepts the new boot; the RPT reached CE init with no Champion spawner error |
| **D** | Upload the unstaged file | write | UNSTAGE_REQUIRED; the file still has the staged SHA | Read-back of the unstaged SHA before any further start |
| **E** | Second restart | restart | D verified | Later boot with no Champion error; exactly one bandage at the drop point |
| **F** | Final fulfillment | record | VERIFICATION_REQUIRED; E clean | Named in-game observation |

## 9. Tests

Everything below is VERIFIED IN TEST. `go test ./...` passes (39 packages). `go test -race` passes for `internal/shop/...` and `internal/livesync`.

| Requirement | Test |
|---|---|
| Tenant isolation | `TestDropPointRules` (other installation), `TestSingleItemPreview` (binding mismatch → `ErrWrongTenant`), `NewPlan` tests |
| Mission path | 2C.1 `capability` tests; the tool resolves through `Discover` and `SafePath` |
| Config preservation and exact diff | `TestPatchEmptyArrayMinimalAndExact`, `TestPatchPreservesEverythingElse`, `TestPatchRefusesUnsafeInputs` |
| Altitude requirement | `TestDropPointRules` (none or absurd), `NewPlan` |
| Single-item plan | `TestSingleItemPreview` |
| Attempt idempotency and duplicate prevention | `TestAttemptLedgerPreventsDuplicates`, `TestConcurrentAttemptsOnlyOneWins`, plus the fingerprint determinism in `TestSingleItemPreview` |
| Backup and rollback | Patch backup and rollback name the verified digest; `TestGatesAndUploadSequence` |
| Boot-source authority | `TestRestartRules` (unaccepted boot ignored; pre-start alone keeps the entry staged; quiet period) and `TestDropPointRules` (previous boot, ended session) |
| Uncertain-state reconciliation | `TestReconcileAfterCrash` (new UNSTAGE_REQUIRED cases), `TestRestartRules` (two boots → FAILED_REVIEW) |
| Fulfillment only on physical confirmation | `TestFulfillOnlyOnPhysicalConfirmation` |
| Read-only enforcement | `TestPackageIsNonExecuting`; 2C.1 `TestReaderIsReadOnly` (nitrado has no write method) |
| Credential redaction | `TestOutputsCarryNoCredentials`; the tool prints only canonical paths |
| Migration not deployed | `TestMigrationIsProposalOnly` |

## 10. What this phase did live (read-only only)

* Ran capability discovery and `shop-canary-prepare`: GETs and signed downloads only.
* Downloaded `cfggameplay.json`.
* Listed the mission folder and `custom/`.
* Read the last five RPTs.
* Made one GET to `/api/admin/live-sync` for boot authority.

No upload token was requested. Nothing was written.

## 11. Open decisions for the owner

1. Re-add `custom/The_Lost_City.json`? It is not included by default.
2. How the canary delivery record is created: a real zero-point purchase, or a dedicated canary record. Either needs its own approval.
3. Deploy the attempt ledger (`0054`) before Gate B, or run the canary with operator-recorded transitions.
4. Whether Gate C is a manual restart or the next scheduled one.

## 12. Verdict

**BLOCKED — REMAINING REQUIREMENTS**

The preparation code, patch, rules, gates and tests are complete and ready for review. Execution cannot begin until:

1. **A verified drop point exists.** It must be a current-session, server-written ADM position with altitude, captured just before Gate B; no such point exists today.
2. **The owner decides the section 11 items**: Lost City, the canary delivery record, the ledger or manual recording, and the restart mode.
3. **Write capability is proven.** It stays UNVERIFIED until the first authorized upload (Gate A) is read back with the expected SHA-256.
