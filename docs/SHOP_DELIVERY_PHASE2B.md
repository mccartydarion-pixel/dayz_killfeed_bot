# Champion Shop - Phase 2B: Nitrado automatic delivery feasibility

Research record and non-executing prototype for delivering a purchased item at a player's requested position on a **console DayZ** server hosted by Nitrado.
Phase 2A (`docs/SHOP_DELIVERY.md`) is unchanged: purchases, delivery records, X/Z coordinates, map scope, idempotency, fulfillment and refunds.
**Automatic delivery stays disabled.** Nothing in this phase writes to Nitrado, restarts a server, or changes a production delivery state.

Classification used throughout:

| Label | Meaning |
|---|---|
| **SUPPORTED BY DOCUMENTATION** | Stated in an official Nitrado or Bohemia source (quoted below) |
| **VERIFIED IN TEST** | Exercised in this repository's tests (local fixtures only) |
| **UNVERIFIED** | Plausible, but not confirmed by an official source or a live test |
| **UNSUPPORTED** | No official mechanism exists |

Nothing here is **VERIFIED IN TEST** against a live server. No live experiment was run (section 9).

## 1. Sources

* Nitrado API documentation, `https://doc.nitrado.net` (read on 2026-09-24), sections "Gameserver - Files - *", "Gameserver - Restart", "Gameserver - Gameserver Details".
* Nitrado's official PHP SDK, `github.com/nitrado/NitrAPI-PHP`, `lib/Nitrapi/Services/Gameservers/FileServer/FileServer.php`, at commit `112b3a1365c8fc959a4323d6ab00ce4b536070c0`.
* Bohemia Interactive's published DayZ game scripts, `github.com/BohemiaInteractive/DayZ-Script-Diff`:
  * `scripts/3_game/objectspawner.c`;
  * `scripts/3_game/cfggameplayhandler.c`;
  * `scripts/3_game/dayzgame.c` (`GlobalsInit`);
  * `scripts/5_mission/mission/missionserver.c` (`OnInit`).
* Bohemia's official Central Economy files, `github.com/BohemiaInteractive/DayZ-Central-Economy`: `dayzOffline.{chernarusplus,enoch,sakhal}/cfggameplay.json`, `init.c`.
* The Bohemia community wiki (`community.bistudio.com`) was not readable during this research: it sits behind a bot check, and bot checks are not bypassed. Every DayZ fact below therefore comes from the script and config sources above.

## 2. Existing system (audit)

* **Installation to Nitrado binding.** `installations.game_server_id` points to `game_servers.provider_service_id`, the Nitrado service ID.
  * The binding is set by `handleSelectDayZServer`, which re-verifies live that the service belongs to the connected Nitrado account.
  * The account's token is stored encrypted per organization (`SaaSCredentials`).
  * A delivery snapshots `game_server_id` (`shop_deliveries`).
* **`internal/nitrado` today is read-only for files:**
  * file-server `list`, `download` (signed URL) and `seek`;
  * `GET /services`;
  * `restart` / `stop` (Client Admin, typed confirmation; `docs/CLIENT_ADMIN.md`).
* **There is no upload, copy, move or delete call in this codebase.** This phase adds none.
* **`shop.DeliveryAdapter` / `shop.DisabledAdapter`** (Phase 2A) are unchanged. The new prototype adapter implements the same interface and is also disabled.

## 3. Nitrado file writes

| Question | Finding | Label |
|---|---|---|
| Endpoint | `POST https://api.nitrado.net/services/:id/gameservers/file_server/upload`, parameters `path` (target directory) and `file` (target file name) | SUPPORTED BY DOCUMENTATION |
| Authentication | `Authorization: Bearer <token>`, or the token as a GET parameter. Permissions `ROLE_WEBINTERFACE_FILEBROWSER_READ` and `ROLE_WEBINTERFACE_FILEBROWSER_WRITE`. Required service status `SERVICE_STATUS_ACTIVE` | SUPPORTED BY DOCUMENTATION |
| Response | `{"status":"success","data":{"token":{"url":"http://<host>:8080/upload/","token":"<uuid>"}}}` - a one-time upload URL and token, **not** the upload itself | SUPPORTED BY DOCUMENTATION |
| Upload mechanism | The API docs do not describe the second request. The official SDK (`writeFile`, `uploadFile`) POSTs the raw file bytes to `token.url` with headers `token: <token>` and `content-type: application/binary` | Official SDK - UNVERIFIED live |
| Overwrite | SDK `writeFile`: "File will be overwritten if it's already existing". No conditional write (no ETag or version check) | Official SDK |
| Supported paths | Any path inside the service's file browser. Examples in the docs are `/games/<user>/ftproot/...` | Paths: SUPPORTED BY DOCUMENTATION. The **console DayZ mission-folder path is UNVERIFIED** |
| File-size limit | Not documented. Only a service quota exists (`quota.block_softlimit` / `block_hardlimit` in gameserver details) | UNVERIFIED |
| Errors | Token request: `401` invalid token, `429` rate limit, `503` maintenance. The upload URL's error contract is undocumented (the SDK reads a JSON `message`) | Partly documented |
| Server must be stopped? | Not stated by Nitrado. DayZ reads the spawner files only at server start (section 4), so writing while the server runs changes nothing until the next start | Nitrado: UNVERIFIED. DayZ side: from scripts |
| Active after restart only? | Yes, for the object-spawner mechanism (section 4) | From Bohemia scripts |
| Related endpoints | `DELETE .../file_server/delete` (`path`), `POST .../copy` (`source_path`, `target_path`, `target_name`), `POST .../move`, `GET .../stat` (`size`, `modified_at`), `GET .../size`, `GET .../download` (token + URL) | SUPPORTED BY DOCUMENTATION |
| Restart | `POST .../gameservers/restart` (`message`, `restart_message`), permission `ROLE_WEBINTERFACE_GENERAL_CONTROL` | SUPPORTED BY DOCUMENTATION |
| Token scopes | Whether customers' connected tokens carry `FILEBROWSER_WRITE` | UNVERIFIED (a read-only token would make uploads impossible) |

**Security note.** `GET /services/:id/gameservers` (gameserver details) returns `credentials.ftp.password` and `credentials.mysql.password` in its response body. Any future code that calls it must never log, store or forward that response.

## 4. DayZ item delivery mechanism

**The mechanism that places an arbitrary item at exact coordinates is the object spawner.**

| Fact | Evidence | Label |
|---|---|---|
| `cfggameplay.json` is read at server start, in `MissionServer.OnInit` via `CfgGameplayHandler.LoadData()` | `missionserver.c`, `cfggameplayhandler.c` | from Bohemia scripts |
| It is only used when `serverDZ.cfg` sets `enableCfgGameplayFile = 1` (`g_Game.ServerConfigGetInt("enableCfgGameplayFile")`) | `cfggameplayhandler.c` | from Bohemia scripts |
| `WorldsData.objectSpawnersArr` lists spawner JSON files relative to the mission folder (`"$mission:" + path`) | `objectspawner.c`; the key exists (empty) in the official Chernarus, Livonia and Sakhal `cfggameplay.json` | from Bohemia scripts and files |
| A spawner file is `{"Objects":[{"name","pos":[x,y,z],"ypr":[yaw,pitch,roll],"scale","enableCEPersistency","customString"}]}` | `ObjectSpawnerJson` / `ITEM_SpawnerObject` | from Bohemia scripts |
| Spawning runs once per server start, from `DayZGame.GlobalsInit()` after the Central Economy initialises. There is no reload path | `dayzgame.c`, `objectspawner.c` | from Bohemia scripts |
| Items are created with `CreateObjectEx(name, pos, ECE_SETUP \| ECE_UPDATEPATHGRAPH \| ECE_CREATEPHYSICS \| ECE_NOLIFETIME \| ECE_DYNAMIC_PERSISTENCY, RF_IGNORE)` at the **exact `pos`**. No surface snapping is requested | `objectspawner.c` | from Bohemia scripts |
| With `enableCEPersistency: false` the object has no CE lifetime and dynamic persistency: saved only once a player interacts with it. With `true` it gets normal CE lifetime and persistence | `objectspawner.c` | from Bohemia scripts (the exact persistence behaviour is UNVERIFIED live) |
| A name containing `/` or `\` is treated as a static P3D model and restricted to plants and rocks. A classname creates an entity | `objectspawner.c` (`VALID_PATHS`) | from Bohemia scripts |
| A spawn failure prints `Object spawner failed to spawn <name>` to the RPT log | `objectspawner.c` | from Bohemia scripts |
| Console servers run the same scripts and honour `enableCfgGameplayFile` / `objectSpawnersArr` | The published scripts are the PC build; the official Central Economy files are shared across platforms | **UNVERIFIED** |
| Nitrado console services let the owner set `enableCfgGameplayFile` and edit the mission folder | not documented | **UNVERIFIED** |

**Item details:**
* **Classnames:** DayZ config classnames such as `M4A1`, `Mag_STANAG_30Rnd` or `AmmoBox_556x45_20Rnd`. The prototype accepts only `^[A-Za-z][A-Za-z0-9_]{1,63}$` and only names on the product's owner-approved allowlist.
* **Quantity:** one spawner entry per unit.
* **Not controllable by the spawner (UNVERIFIED):** stack or ammo quantity, condition, and **attachments or loaded magazines**. The spawner has no fields for them. A magazine or ammo box has to be a separate entry.
* **Altitude:** `pos` needs a real `y`, section 6.

**Rejected mechanisms:**
* **`events.xml` / `cfgeventspawns.xml`:** Central Economy events choose positions and loot at random within configured groups. They cannot put one purchased item at one requested position for one buyer. UNSUPPORTED for targeted delivery.
* **`init.c` scripting** (for example a script that polls a file and spawns at runtime):
  * whether console servers allow a modified `init.c` is UNVERIFIED;
  * it would run Champion-authored script on a customer server.

  Not proposed.

## 5. Delivery options

| Option | Status | Notes |
|---|---|---|
| **A. Restart-based delivery** (object spawner) | Mechanism from Bohemia scripts. Console and Nitrado parts **UNVERIFIED** | The only candidate. The staged entry spawns at the next server start. That can be the owner's own scheduled restart, so Champion never needs to restart a server |
| **B. Configuration reload** | **UNSUPPORTED** | No reload path exists in the scripts. `cfggameplay.json` and the spawner files are read once per start |
| **C. Immediate delivery** | **UNSUPPORTED** | Nitrado has no spawn API. RCON on console is not available through this integration. There is no supported runtime mechanism. **Immediate spawning is not possible with verified tools** |

## 6. Altitude requirement

The object spawner uses `pos` exactly. A wrong `y` could place an item in the air or under the terrain. Whether physics drops a floating item is UNVERIFIED. Champion stores only X/Z (Phase 2A) and has no terrain-height data, so the prototype refuses a plan without a verified `y` (`ErrAltitudeRequired`).

Candidate sources for `y`:
1. **Owner-defined drop points (recommended):** an admin stands at a spot. The ADM log records the full position (`player_location_events` has correct x/y/z since migration 0045), and the point is saved with its verified altitude. Players pick a drop point rather than arbitrary X/Z.
2. The buyer's own recent ADM-recorded position, with the delivery restricted to within a few metres of it.

Arbitrary map coordinates without an altitude source are **not deliverable automatically**. They stay `MANUAL_COORDINATE` for staff.

## 7. Proposed delivery adapter (prototype, `internal/shop/nitradodelivery`)

* **`NewPlan(PlanInput) (Plan, error)`** builds an **immutable** plan. Fields are unexported, getters copy slices, and no credential field exists. It contains:
  * delivery ID, purchase ID, organization, installation and game-server IDs;
  * Nitrado service ID, map, and the product/item snapshot;
  * quantity, `[x, y, z]`, and the validated classname;
  * attempt number and `AttemptID()` (`champion:d<delivery>:a<attempt>`);
  * `Fingerprint()` (SHA-256 of every planned fact);
  * expected artifact `ArtifactPath()` = `champion/champion_shop_delivery.json` (relative to the mission folder; a constant, never from a request);
  * `RequiredAction()` = `OWNER_CONFIRMED_RESTART`.

  **Validation, in order:**
  1. The tenant: organization and installation of the scope, the delivery and the binding.
  2. The Nitrado service ID format.
  3. The game server matches the binding.
  4. The delivery is `MANUAL_COORDINATE`, `MANUAL_READY`, and its purchase is `PENDING_FULFILLMENT`. Anything refunded, cancelled or fulfilled is refused.
  5. The map is supported and equals the installation's current map.
  6. The coordinates are on the map.
  7. The altitude is verified and within -50..1500.
  8. The classname is well-formed and approved.
  9. The quantity is 1..10 units.
* **Spawner file:**
  * `ParseSpawnerFile` refuses the Champion file if any entry lacks a `champion:` tag. That means someone else edited it, and it is never overwritten.
  * `Stage` / `Unstage` add or remove exactly one attempt's entries (tagged `customString`), idempotently.
  * `Render` is deterministic.
  * `Diff` is a line diff for review.
  * The file is capped at 50 staged objects.
* **`PlanRollback`** gives before/after SHA-256, a backup name under `champion/backup/`, and the ordered backup, verify, write, verify, restore steps.
* **`PrototypeAdapter`** implements `shop.DeliveryAdapter`: `Enabled()` is false and `Deliver` returns `ErrAutomaticDeliveryDisabled`.
  * `Propose(plan, currentFile)` returns a `Proposal`: the plan, the current and proposed SHA-256, the proposed file, the diff, the required action, the rollback plan, the blockers, and steps all prefixed `NOT EXECUTED:`. `Executed` is always false.
  * It has **no Nitrado client and no database access**, so it cannot write or restart anything.

**File layout (proposal).** Champion writes only its own file, `champion/champion_shop_delivery.json`. The server owner adds `"champion/champion_shop_delivery.json"` to `objectSpawnersArr` in their `cfggameplay.json` once, by hand, and enables `enableCfgGameplayFile`. Champion never edits `cfggameplay.json` or any other owner file.

## 8. Duplicate prevention, recovery and rollback

Because **every staged entry spawns again on every server start**, safety depends on removing it right after the first start. A durable attempt ledger stops a crash or retry from staging twice.

States: `PLAN_CREATED` → `FILE_PREPARED` → `FILE_STAGED` → `AWAITING_RESTART` → `RESTART_OBSERVED` → `VERIFICATION_REQUIRED` → `FULFILLED`. The terminal side states are:
* `ABANDONED`: never written;
* `UNSTAGED`: written, then proven removed before any start;
* `FAILED_REVIEW`: an uncertain outcome that a human resolves.

The transitions are in `CanAdvance`. **`RESTART_OBSERVED` never goes straight to `FULFILLED`.** An uploaded file is not proof of delivery, and a restart is not proof of delivery.

Ledger rules (`MemoryLedger`, the prototype of the durable table):
* at most one open attempt per delivery;
* no new attempt after a fulfillment;
* **no new attempt after any attempt that is not proven to have spawned nothing.** Only `ABANDONED` and `UNSTAGED` allow a retry;
* attempt numbers follow history;
* state changes are compare-and-set.

`Reconcile(state, fileStillContainsAttempt, restartSinceStaging)` is the restarted-worker procedure. It never re-stages. Anything that may have reached a server start goes to verification or review.

Proposed durable table (**not migrated in this phase**):

```sql
CREATE TABLE shop_delivery_attempts (
  id BIGSERIAL PRIMARY KEY,
  delivery_id BIGINT NOT NULL REFERENCES shop_deliveries(id),
  organization_id BIGINT NOT NULL, installation_id BIGINT NOT NULL,    -- composite FKs as in 0049
  attempt INT NOT NULL, state TEXT NOT NULL, plan_fingerprint TEXT NOT NULL,
  artifact_sha_before TEXT, artifact_sha_after TEXT, backup_name TEXT,
  staged_at TIMESTAMPTZ, restart_observed_at TIMESTAMPTZ, unstaged_at TIMESTAMPTZ, verified_by_user_id BIGINT,
  UNIQUE (delivery_id, attempt)
);
-- at most one open attempt per delivery
CREATE UNIQUE INDEX ON shop_delivery_attempts(delivery_id) WHERE state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW');
```

**Refund interaction:** a refund must be refused, or must unstage first, while an attempt is `FILE_STAGED` / `AWAITING_RESTART`. The prototype refuses to plan anything for a refunded delivery.

**Recovery and rollback** (`PlanRollback`; every step is a description):
1. Download the Champion file and record its SHA-256.
2. Upload that exact content as `champion/backup/<file>.<sha12>.bak`, download it again, and compare digests. This is the **verified backup**.
3. Immediately before writing, download again. If the digest changed, stop. There is no conditional write, so this re-check is the only protection against concurrent owner edits, and it has a small race window.
4. Upload the staged content, download it, and compare to the expected digest. **On a partial upload or mismatch, re-upload the backup and verify.**
5. Only then `AWAITING_RESTART`: the owner restarts, or their schedule does.
6. **Right after the start is observed, unstage and verify.** How to detect a start is UNVERIFIED. Candidates: service status polling, or the new ADM log file each start creates.
7. **Failed restart** (the server does not come back): unstage, verify, then send the attempt to `FAILED_REVIEW`. Nothing is retried.
8. **Uncertain attempt** (a crash between steps): `Reconcile` decides. If a start may have happened, it goes to `VERIFICATION_REQUIRED` and the player and staff confirm.

Only the Champion-owned file is ever written. A mission file is never deleted or replaced, and the owner's `cfggameplay.json` is only edited by the owner.

**Multiple queued orders:** several attempts can be staged in the one Champion file. They all spawn at the next start, and each is unstaged by its own tag.

**Persistence and cleanup:** with `enableCEPersistency: false`, an item nobody touched is not saved (UNVERIFIED live). After unstaging, an unclaimed delivery disappears at the following restart, so the player must collect it within that restart window.

## 9. Test-server experiment (NOT conducted - needs explicit authorization)

**Prerequisites:**
* a disposable, user-owned console DayZ server on Nitrado, not a customer's;
* a Nitrado token with `FILEBROWSER_WRITE` for it;
* one authorized test player;
* written authorization for exactly the steps below.

**Steps:**
1. **Read-only discovery.** List the mission folder and record its path. Record `enableCfgGameplayFile` and whether `cfggameplay.json` exists.
2. **Owner setup, by hand.** Enable `enableCfgGameplayFile = 1` and add `champion/champion_shop_delivery.json` to `objectSpawnersArr`.
3. **Choose one location.** The test player stands at an open, flat spot. Read the ADM position (x, y, z) from the log, and record a screenshot with the in-game position.
4. **One item.** `BandageDressing`: small and harmless.
5. **One order.** Buy one `MANUAL_COORDINATE` product at those x/z, with that y as the verified altitude. Run `Propose`, review the diff, and follow the rollback plan's backup steps.
6. **Upload the proposed file (the only write).** Download it and verify the SHA-256.
7. **One controlled restart** by the owner.
8. **Unstage immediately** after the server is back, and verify.

**Physical verification** (all required):
* the RPT log has no `Object spawner failed to spawn BandageDressing`;
* the test player walks to the location and sees the item, with a screenshot or video;
* the player picks it up and it is in their inventory;
* after a **second** restart (the entry is unstaged), there is no second bandage at the location. This checks duplicate prevention;
* an unclaimed control item placed the same way is gone after the second restart. This checks the cleanup assumption.

**Abort** at any unexpected step: restore the backup and verify.

## 10. Security requirements (enforced by the prototype where it can)

* **Tenant isolation and installation ownership:** scope, delivery and binding must agree.
* **Nitrado service identity:** the plan's game server must be the installation's bound server, and the service ID is numeric.
* **Authorized paths only:** a constant `champion/` artifact path. No path from any request.
* **No secret in plans:** the plan has no credential field. Gameserver-details credentials must never be logged.
* **No writes after refund or cancellation:** the plan is refused.
* **No automatic customer-server restarts:** the required action is `OWNER_CONFIRMED_RESTART`, and nothing calls restart.
* **No duplicate attempts:** enforced by the ledger rules and the compare-and-set advance.

## 11. Tests (`internal/shop/nitradodelivery`, local fixtures only)

The tests cover:
* plan validation and immutability;
* invalid classnames: P3D paths, JSON injection, unapproved names;
* invalid coordinates and altitude;
* wrong organization, installation or game server;
* refunded, cancelled or fulfilled deliveries;
* unsupported or changed maps;
* spawner file generation, field names, idempotent re-staging and exact unstaging;
* refusal of a foreign or broken file;
* the diff;
* rollback planning;
* the disabled adapter and dry-run output;
* the attempt ledger: duplicates, uncertain attempts, retry only after proven non-spawn, no retry after fulfillment;
* 64 concurrent attempts and compare-and-set races (under `-race`);
* crash reconciliation.

## 12. Remaining blockers (why automatic delivery stays disabled)

1. The two-step upload (token, then raw POST) has not been exercised against a real Nitrado server.
2. Unverified on console Nitrado services:
   * `enableCfgGameplayFile` and `objectSpawnersArr`;
   * the mission-folder path;
   * whether customer tokens include `FILEBROWSER_WRITE`.
3. A verified altitude source is required. The recommendation is owner-defined drop points.
4. Unverified on console: object-spawner item behaviour (quantity, attachments, physics, persistence).
5. Restart detection for immediate unstaging is not designed or verified. Two quick restarts before unstaging would spawn twice.
6. There is no automated proof of pickup. Fulfillment stays a human confirmation.
7. The durable attempt table and its states need a migration, and an explicit product decision before any activation.
