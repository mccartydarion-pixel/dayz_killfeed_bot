# Champion Shop — Gate B: reference the Champion file in cfggameplay.json

Gate B is the only overwrite of a server-owner file in the canary. It appends `champion/champion_shop_delivery.json` to `WorldsData.objectSpawnersArr` in `cfggameplay.json`.

**This document prepares Gate B. It has not been executed.**

Gate B reuses the guarded write machinery from Gate A (`docs/SHOP_GATE_A_UPLOAD.md`): plan IDs, the anchored single-use journal, the https `*.nitrado.net` trust boundary, no retries, and independent read-back.

## 1. Operations

| Operation | Writes | Payload |
|---|---|---|
| `gate-b-reference` | 1. `champion/backup/` (only if absent); 2. the server backup `champion/backup/cfggameplay.json.<sha12>.bak`; 3. `cfggameplay.json` | Derived from the **live** bytes by `canary.ProposePatch`: one appended entry, every other byte unchanged. A caller-supplied payload is refused. |
| `gate-b-rollback` | `cfggameplay.json` | The verified server backup, whose SHA-256 must equal the original. A **separate plan and a separate authorization**. |

## 2. Refusals (before anything is written)

* The service, binding or mission differs.
* `cfggameplay.json` does not have the expected SHA-256 at planning time, at execution time, or immediately before the overwrite.
* `objectSpawnersArr` is not exactly the expected list. The Lost City reference must be present.
* The config already references the Champion file.
* The Champion file is missing, or is not exactly the 20-byte empty file (`328c4d64…`).
* The derived patch is anything other than exactly one appended `champion/champion_shop_delivery.json`.
* A **different** file exists at the backup path. An identical backup is reused, not rewritten.
* The plan ID does not match the fresh plan, was already used (Gate A's consumed plan ID can never authorize Gate B), or the destination has an outstanding `STARTED`/`UNCERTAIN` entry.
* The command line only: the owner-side local copy `%APPDATA%\champion-shop\backups\cfggameplay.json.<sha12>.bak` is missing or does not match. The dry run creates and verifies it.

## 3. Execution sequence

1. Lock, then check the journal. Fresh plan; its ID must equal the authorization. Journal `STARTED`.
2. **Backup first.** Create `champion/backup` if needed, upload the exact original bytes, and read them back. Any failure stops the run with `NOT_WRITTEN`, leaving `cfggameplay.json` untouched. A backup file with unexpected content is reported for the owner to inspect.
3. **Re-verify immediately before the overwrite:**
   * the config has the original SHA-256;
   * the Champion file is still the empty file;
   * the backup still matches.
4. One upload token and one transfer of the patched bytes.
5. **Independent read-back** decides the outcome:

| Read-back | Outcome |
|---|---|
| The patched size and SHA-256 | `WRITTEN_VERIFIED` |
| Still the original, and the request was refused | `NOT_WRITTEN` |
| Still the original, but success was reported | `UNCERTAIN` |
| Anything else (partial, foreign) | `UNCERTAIN`; the outcome names `gate-b-rollback` and the backup path |
| Unreadable | `UNCERTAIN` |

6. **Post-write checks:**
   * `objectSpawnersArr` equals `["custom/The_Lost_City.json","champion/champion_shop_delivery.json"]`;
   * every referenced spawner file is present;
   * the Champion file is unchanged;
   * no new boot, and the gameserver status is unchanged.

Gate B takes effect at the next server start. Until then the running server keeps the configuration it booted with. At that start the spawner reads the empty Champion file and spawns nothing.

## 4. Rollback

* **After `WRITTEN_VERIFIED`** (owner decision): run `gate-b-rollback` with the current (patched) SHA-256, spawners `["custom/The_Lost_City.json","champion/champion_shop_delivery.json"]` and payload SHA-256 = the original. It needs its own dry run, plan ID and approval.
* **After `UNCERTAIN`:**
  1. Stop.
  2. Inspect read-only.
  3. The owner records a resolution (`-resolve`).
  4. Then a `gate-b-rollback` dry run against the file as it now is, with `-expect-spawners '[]'` if the file is no longer valid JSON. It needs its own approval.
* **Owner-side copy:** `%APPDATA%\champion-shop\backups\cfggameplay.json.4d000807963a.bak` can be uploaded by hand in the Nitrado file browser if the API is unavailable.

## 5. Tests (disposable stand-in)

| Scenario | Test |
|---|---|
| The backup is created and verified before a byte-exact patch; exact order and bodies; post checks; re-run and reuse refused | `TestGateBBackupThenByteExactPatch` |
| An existing identical backup is reused | `TestGateBReusesAnExistingVerifiedBackup` |
| Stale state: config changed; Champion file missing or not empty; Lost City gone; already referenced; conflicting backup; custom payload; changed after approval; changed between the backup and the overwrite | `TestGateBStaleStateRefusal` |
| Backup failures (mkdir, token, 507, partial, mismatched read-back) leave the config untouched | `TestGateBBackupFailureLeavesConfigUntouched` |
| Interrupted upload: partial, dropped before or after storing, success claimed but unchanged, read-back mismatch; `UNCERTAIN` blocks; no retry | `TestGateBInterruptedUploadAndReadBackMismatch` |
| Rollback after a verified Gate B and after a partial upload; Gate B's plan ID cannot authorize it; a missing or tampered backup is refused | `TestGateBRollbackProcedures` |
| Gate A's consumed plan ID never authorizes Gate B | `TestGateANeverAuthorizesGateB` |
