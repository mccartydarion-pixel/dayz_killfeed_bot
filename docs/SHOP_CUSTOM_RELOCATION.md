# Champion Shop — Relocating the delivery file into custom/

**This is prepared, not executed.** Relocation Gates C and D each need their own dry run, plan ID and owner approval.

## 1. Why

Gate B (2026-09-29, plan `mw-5bb8e7511b5db7f33006c35c`) wrote `cfggameplay.json` correctly: `WRITTEN_VERIFIED`, 2992 bytes, `aa5c8d9a…`. The first boot after it (`DayZServer_PS4_x64_2026-09-29_07-15-38`) logged:

```
[::SpawnObjects] :: [ERROR] :: File "$mission:champion/champion_shop_delivery.json" does not exist, check the provided path
```

The read-only investigation found the following:

| Evidence | Classification |
|---|---|
| The game runs on a Windows host from a local copy, `C:\SERVICES\<svc>_local\dayzps\mpmissions\dayzOffline.chernarusplus` (RPT: "Adding relative directory … under name mission"). The API file server (`/games/<acct>/ftproot/dayzps_missions/…`) is a separate storage copied to it. | VERIFIED LIVE (RPT) |
| Our API edit of the existing `cfggameplay.json` reached the game: it named the new entry. | VERIFIED LIVE |
| `custom/The_Lost_City.json` loads without error. The `champion/` folder and file are identical in owner, group and permissions (`40775` / `100664`), yet never reached the game in dozens of boots since 2026-09-26. | VERIFIED LIVE |
| "On Nitrado, [`custom`] is the only user-created folder that's whitelisted for loading external files like JSONs." | Community documentation ([DayZ BoosterZ wiki](https://www.dayzboosterz.com/wiki/page/how-to-add-object-spawner-files)), not Nitrado's own |
| An API-created file **inside** `custom/` reaches the game. | **UNVERIFIED.** Proven only by the release gate (section 5). |

## 2. Code changes (no automatic migration, no startup write)

* `nitradodelivery.ArtifactRelPath` is now `custom/champion_shop_delivery.json` (`ArtifactDir = "custom"`). It is used by the plan, staging, unstaging, rollback plans, preview and the capability probe.
* `nitradodelivery.LegacyArtifactRelPath` is `champion/champion_shop_delivery.json`: the Gate A/B location, left in place and unreferenced once Gate D has run.
* `nitradodelivery.BackupDir` is `champion/backup`. Config backups are **never** in `custom/`, so they never reach the game.
* `canary.ProposeRelocation` replaces the single legacy entry **in place** and changes no other byte. It refuses unless the legacy entry occurs exactly once and the `custom/` entry does not occur.
* `missionwrite`:
  * **`gate-c-create-custom`** writes the built-in 20-byte empty file into the **existing** `custom/`. It never creates a folder (`ErrParentMissing`) and never overwrites.
  * **`gate-d-relocate-reference`**:
    * first writes a **fresh verified backup** of the current config (`champion/backup/cfggameplay.json.<sha12>.bak`);
    * then re-verifies the config, the `custom/` file (exactly empty) and the backup;
    * then uploads the relocated config and reads it back.
  * `gate-b-rollback` restores **any** verified config backup by its SHA-256.
  * `gate-a-create-empty` and `gate-b-reference` are completed and retired: the command line refuses them. Their journal entries and consumed plan IDs stay as history.
* `shop-canary-prepare` reports both Champion file locations and the Gate D relocation proposal.

## 3. Required change outside this PR: the attempt ledger

Migration 0054 constrains `shop_delivery_attempts.artifact_path = 'champion/champion_shop_delivery.json'`, and the repository inserts that literal. Nothing compares it with the code's path, so relocation Gates C and D do not depend on it.

A canary **delivery attempt** (from the purchase gate on) must record the real path. That needs a separate, owner-approved schema migration, deployed before any purchase:
* **Proposal:** a new migration widens the CHECK to `IN ('champion/…', 'custom/…')`.
* The repository then writes `nitradodelivery.ArtifactRelPath`.

It is **not** included here: a schema migration runs automatically on deploy.

## 4. Gates

| Gate | Writes | Preconditions (all re-verified live at execution) | Proof |
|---|---|---|---|
| **C** `gate-c-create-custom` | `custom/champion_shop_delivery.json` (new, 20 bytes) | `custom/` exists; the destination is absent; the config and spawner list are exactly as approved | read-back 20 bytes, `328c4d64…`; config unchanged; no restart |
| **D** `gate-d-relocate-reference` | `champion/backup/cfggameplay.json.<sha12>.bak`, then `cfggameplay.json` | Gate C verified (the `custom/` file is exactly empty); the config has the approved SHA-256 and exactly one legacy entry; no conflicting backup | backup read-back; config read-back with the approved size and SHA-256; spawners `["custom/The_Lost_City.json","custom/champion_shop_delivery.json"]`; every referenced file present; no restart |

Each gate has its own plan ID. The journal refuses reuse of any consumed ID (Gate A `mw-d6d756eb…`, Gate B `mw-5bb8e751…`).

**Rollback.** `gate-b-rollback` restores either:
* the fresh post-Gate-B backup (the state before Gate D); or
* the original `4d000807…` backup (the state before Gate B).

Each rollback needs its own dry run and approval. Gate C's file is harmless while unreferenced.

## 5. Release gate (before any purchase)

After Gate D, the **next normal boot** must be confirmed from the server's own logs and the bot's boot authority. Its RPT must contain:
* CE startup reached;
* **no** `[::SpawnObjects]` error, and no parse error, for `custom/champion_shop_delivery.json` or any other spawner file.

A successful spawn logs nothing: the pre-Gate-B baseline boot had zero spawner lines. Only then may the ledger migration (section 3) and the purchase gate be proposed.
