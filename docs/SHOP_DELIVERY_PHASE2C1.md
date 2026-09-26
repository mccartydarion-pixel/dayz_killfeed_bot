# Champion Shop - Phase 2C.1: console delivery capability discovery

Read-only discovery on the connected Champions service (PlayStation DayZ on Nitrado), 2026-09-24.
Continues `docs/SHOP_DELIVERY.md` (2A) and `docs/SHOP_DELIVERY_PHASE2B.md` (2B).

**Nothing was written, uploaded, restarted or changed.** Every live call was a GET or a signed
download. No purchase or delivery state changed, and automatic delivery stays disabled.

Labels: **VERIFIED LIVE** (observed on the Champions service today), **VERIFIED IN TEST** (local
fixtures only), **SUPPORTED BY DOCUMENTATION**, **UNVERIFIED**, **UNSUPPORTED**.

## Discovery code

* `internal/nitrado/service_facts.go`: `ServiceFacts`, `GameserverFacts` and `TokenFacts`.
  * They are GET only and **whitelist-decoded**.
  * The gameserver-details body contains FTP and MySQL credentials, the admin, RCON and server passwords, and a websocket token. None of these is ever unmarshalled.
* `internal/shop/capability` (`Discover`) works through a `Reader` interface that has five read methods and nothing else.
  * Every path is validated inside the service's own `/games/<account>` root (`SafePath`).
  * `ftproot/` and `noftp/` resolve to one canonical location (`Canonical`).
  * A `serverDZ.cfg`, if one is ever exposed, is reduced to `enableCfgGameplayFile` and the mission template (`ParseServerCfg`).
* `cmd/shop-capability-probe` prints the sanitized report. The physical account path is removed.
* **Tests (VERIFIED IN TEST, `-race`):**
  * binding refusal (a foreign service, a missing organization or installation, a non-numeric service, gameserver details for another service);
  * safe paths and aliases;
  * missing configuration: no mission folder, no `cfggameplay.json`, invalid JSON, an unsafe spawner path, an unsafe mission name;
  * unsupported capabilities (no file browser, no write role);
  * listing and download failures;
  * redaction: a realistic Nitrado body full of secrets never reaches the output;
  * read-only enforcement: the `Reader` method set is checked; `nitrado.Client` has no file-write method; `Discover` through the real client against a recording server issues only GETs and never calls upload, delete, restart or stop.

## 1. Verified server identity

| Finding | Class | Sanitized evidence |
|---|---|---|
| Service `19806451`, `gameserver`, status `active`, game `DayZ (PS4)` | VERIFIED LIVE | `GET /services/19806451` |
| Platform: `dayzps`, PlayStation, 18 slots, v1.29.163709, map `dayzOffline.chernarusplus` | VERIFIED LIVE | `GET /services/19806451/gameservers` |
| Installation binding: server row 1 is bound to service 19806451 (installation 11, organization 1) | VERIFIED LIVE (operationally) | The production worker for `server_id=1` reads this service's ADM (`adm_session_current server_id=1` logs), and the probe's binding check passed. The binding row itself is in the internal database and was not queried |
| The token's user is **not the service owner** (`is_owner=false`), and the service is not read-only | VERIFIED LIVE | Service details |
| Token scope `service` | VERIFIED LIVE | `GET /token` (scopes only) |

## 2. Mission path

| Finding | Class | Evidence |
|---|---|---|
| Mission setting: `dayzOffline.chernarusplus` | VERIFIED LIVE | `settings.config.mission` |
| Mission folder, canonical: `dayzps_missions/dayzOffline.chernarusplus` (23 entries) | VERIFIED LIVE | File-server list |
| It exists **only under `ftproot/`**. `noftp/dayzps_missions/…` lists nothing, so `ftproot` is the mount for mission files | VERIFIED LIVE | Both mounts listed |
| Game directory `dayzps/` lists only `ban.txt` and `whitelist.txt` | VERIFIED LIVE | File-server list |
| **`serverDZ.cfg` is not exposed** through the file browser on this console service. Its values are Nitrado-managed settings | VERIFIED LIVE (absence) | No `serverDZ*.cfg` listed |

## 3. Gameplay configuration

| Finding | Class | Evidence |
|---|---|---|
| `enableCfgGameplayFile = 1` | VERIFIED LIVE | `settings.config.enableCfgGameplayFile` (the serverDZ.cfg value Nitrado manages; the file itself is not readable) |
| `cfggameplay.json` present: 2,952 bytes, `version` 123, SHA-256 `8d989bc4e30a…` | VERIFIED LIVE | Download (the same digest as the 2B.2 baseline, so the file is unchanged since then) |
| `WorldsData.objectSpawnersArr` has **1 entry**: `custom/The_Losst_City.json` | VERIFIED LIVE | Parsed |
| That file **does not exist**. The real file is `custom/The_Lost_City.json` (354,796 bytes). The reference is still misspelled | VERIFIED LIVE | `custom/` listed |
| `champion/champion_shop_delivery.json` is not referenced and does not exist | VERIFIED LIVE | Parsed and listed |
| Other owner files in `custom/` are not referenced: `4_way_deathmatch_FIXED.json`, `DZB_Loadout_2/4/5.json` | VERIFIED LIVE | Listed |

## 4. Object-spawner availability

| Finding | Class | Evidence |
|---|---|---|
| **The console server processes `objectSpawnersArr` at boot.** The current boot's RPT (`…_13-02-05.RPT`) logs one `[::SpawnObjects]` error for the misspelled file | **VERIFIED LIVE** | This was UNVERIFIED in 2B. DayZ on this PS server reads the array and tries to load each file |
| A Champion spawner file would be loaded, if the owner added its path to `objectSpawnersArr` | UNVERIFIED | By the same mechanism, but no Champion entry exists |
| Item behaviour on console (quantity, attachments, physics, persistence) | UNVERIFIED | Needs the authorized experiment (2B §9) |
| A dedicated Champion file is possible without touching owner spawners: add one array entry beside the owner's | SUPPORTED BY DOCUMENTATION + VERIFIED LIVE (the array accepts entries) | 2B §4 |

**Owner action recommended, independent of the Shop:** fix `custom/The_Losst_City.json` to `custom/The_Lost_City.json` in `cfggameplay.json`. Every boot currently logs a spawner error, and the intended Lost City objects are not spawned. Champion did not change it.

## 5. Nitrado read and write capability

| Capability | Class | Evidence |
|---|---|---|
| File-browser feature (`has_file_browser`), FTP, backups | VERIFIED LIVE | Gameserver features |
| List and download (signed URL) | VERIFIED LIVE | 8 reads in the probe, plus the ADM/RPT reads the engine makes every poll |
| Role `ROLE_WEBINTERFACE_FILEBROWSER_READ` | VERIFIED LIVE | Service roles (21) |
| Role `ROLE_WEBINTERFACE_FILEBROWSER_WRITE` is listed | SUPPORTED BY DOCUMENTATION | Listed on the service, but **a role is not proof of write**. No write was attempted |
| **Write (upload) capability** | **UNVERIFIED** | Only a real, authorized upload proves it. The token belongs to a non-owner user (`is_owner=false`) |
| Offset/partial reads | UNSUPPORTED | Nitrado ignores `offset`/`count` (Live Sync finding A9) |
| Upload token, raw POST, overwrite, errors, rate limits | SUPPORTED BY DOCUMENTATION / official SDK | 2B §3. Unverified live |

## 6. Altitude sources

| Data | Class | Evidence |
|---|---|---|
| X, Y (altitude), Z from ADM positions: all **1,251** stored location rows across 20 located players carry altitude | VERIFIED LIVE | Read-only SaaS API, counts only |
| Server-local source time and `occurredAt` (restart.log offset): 301 rows (those since Live Sync phase 1) | VERIFIED LIVE | Same |
| Five-minute `PLAYER_LIST` positions: 11 rows | VERIFIED LIVE | Same |
| Current-session authority (boot file, player session, ended sessions UNKNOWN) | VERIFIED LIVE | Live Sync 2.1 acceptance, 2026-09-24 |
| Player identity: the ADM id resolves to the player row | VERIFIED IN TEST / live in the directory | `player_location_events.player_id` |
| Right now: 0 players online, so no position is CURRENT | VERIFIED LIVE | Directory `currentLocationStatus` |

**Mode A, admin-verified drop point: supportable with existing data.**
* An admin stands at a spot, and the ADM `PLAYER_LIST` or event row records x/y/z with source identity. The earlier verified example is `<4621.1, 8397.2, 319.6>` → `[4621.1, 319.6, 8397.2]`.
* Storing it as a named point needs a small admin feature. That is not built.

**Mode B, player-position delivery: data exists, but it is weaker.**
* A position is only as fresh as the last ADM event or 5-minute player list.
* ADM visibility on Nitrado can lag minutes, and the player moves.
* Only a `CURRENT` session position within a strict age (for example ≤ 5 minutes, `timeBasis=SOURCE`) and a radius check should ever be eligible.

**Recommendation: Mode A first.** No altitude is ever invented or computed from a map.

## 7. Restart detection and duplicate prevention

Evidence from the Live Sync 2.1 acceptance (natural restarts, 2026-09-24):

| Signal | Latency observed | Meaning |
|---|---|---|
| `restart.log` pre-start check | Detected 6–13 s after it was written (~40 s before the boot) | Nitrado is starting a new DayZ process |
| RPT `Termination successfully completed` | Minutes (RPT bytes arrive late) | The old boot is over |
| The new boot's ADM listed and accepted by Champion | Nitrado listing ~7.5–9 min after boot; Champion 1.1 s after listing | The new boot exists |
| The new boot's RPT spawner lines (`[::SpawnObjects]`) | Minutes | **The spawner of the new boot ran**: the strongest evidence |

**The spawner file must stay staged until the new boot has read it.** DayZ loads it during mission initialisation, seconds after the boot. Unstaging on the pre-start signal alone could remove the entry before it is read. The item would not spawn, which is safe but not delivered.

The right trigger to unstage is the first evidence that the **new boot** is running: its RPT or ADM with a newer boot stamp, or best, its spawner lines. Restarts on Champions are about 55–70 minutes apart, so detection within ~10 minutes leaves a wide margin.

**The risk is a second start within that window** (a crash loop, or an owner restarting twice). That start would spawn again. Mitigation: unstage at the first new-boot evidence, and send the attempt to `FAILED_REVIEW` if two boot stamps are seen before the unstage is verified.

**Reconciliation with the prototype** (`nitradodelivery/attempt.go`):
* The prototype has `FILE_STAGED → AWAITING_RESTART → RESTART_OBSERVED → VERIFICATION_REQUIRED → FULFILLED`, with unstaging as an action inside the `RESTART_OBSERVED` step. `UNSTAGED` already means "removed before any start".
* **Proposal (not implemented):**
  * add `UNSTAGE_REQUIRED` between `RESTART_OBSERVED` and `VERIFICATION_REQUIRED`, so `VERIFICATION_REQUIRED` is reachable only after the unstage has been verified;
  * `UNSTAGE_REQUIRED → FAILED_REVIEW` when the unstage cannot be verified before another boot;
  * `RESTART_OBSERVED` records the new boot's stamp from Live Sync (`server_adm_sessions`, `live_sync_records`).
* A restart is not proof of spawn, and an upload is not proof of delivery. `FULFILLED` stays human-verified.

## 8. Existing prototype validation

`go test -race ./internal/shop/...` passes. Covered, VERIFIED IN TEST:
* immutable plans;
* tenant and installation validation;
* approved classnames;
* X/Y/Z bounds and the altitude requirement;
* fingerprints;
* Stage/Unstage idempotency;
* deterministic rendering;
* diff and rollback planning;
* the attempt ledger (one open attempt, no retry after an uncertain attempt, compare-and-set under 64 concurrent attempts);
* `Reconcile`;
* the adapter is disabled (`Enabled()=false`, `Deliver` returns `ErrAutomaticDeliveryDisabled`).

No executable delivery path exists: the adapter has no Nitrado client, and nothing calls upload or restart.

## 9. Remaining blockers

1. **Write capability is UNVERIFIED.** The token user is not the service owner, and a listed role is not proof.
2. **Upload is not exercised:** token request, raw POST, overwrite semantics, error contract and rate limits.
3. **The owner must add `champion/champion_shop_delivery.json` to `objectSpawnersArr`**, and should fix the misspelled Losst entry at the same time. Champion will not edit owner files.
4. **Console item behaviour after spawning is unverified:** physics, persistence, stack or ammo quantity, attachments.
5. **The durable attempt table and `UNSTAGE_REQUIRED` state:** migration and product decision.
6. **The admin drop-point feature (Mode A) is not built.**
7. **There is no automated proof of pickup.** Fulfillment stays a human confirmation.

## 10. Conditions for Phase 2C.2

Phase 2C.2 (controlled, single-item canary) may start only with **explicit written authorization**, and requires:

* the owner chooses the target server: a disposable test server, or Champions with explicit acceptance of the risk;
* the owner adds the Champion artifact to `objectSpawnersArr` themselves (and fixes the Losst reference);
* one admin-verified drop point, with x/y/z from an ADM row that has source identity;
* one harmless item (`BandageDressing`), one order, one attempt;
* one authorized upload of Champion's own file, with the verified backup and download-verify steps (2B §8);
* **no Champion-initiated restart**: the owner's schedule or the owner restarts;
* unstaging at the first new-boot evidence, then physical verification (2B §9) including the second-restart duplicate check.

**Readiness decision: NOT READY to execute 2C.2 now.** Discovery removed the biggest unknown: the console server does run `objectSpawnersArr`. It also confirmed the mission path, `enableCfgGameplayFile=1`, a documented write role, and altitude data. But write capability is unverified, and blockers 3 to 6 are open. With the owner's written authorization and blocker 3 done, 2C.2 can proceed as the controlled canary above.
