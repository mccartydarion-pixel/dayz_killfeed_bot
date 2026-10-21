# Stadium: a tournament arena spawned on the owner's DayZ server

The Stadium is a walled arena with two gated corners (Red to the west, Blue to the east), a gun
locker room and a clothing room outside each gate, cover inside, stands at the corners, a
commentary tower and flags. The owner places it on the website, previews it, and presses **Build**:
Champion writes one object-spawner file into the server's mission `custom/` folder and references
it from `cfggameplay.json`. The arena appears at the next server restart and is rebuilt at every
restart after that, until the owner presses **Remove**.

Code: `internal/stadium` (the layout, pure), `internal/stadium/stadiumwrite` (the guarded write),
`internal/app/saas_api_stadium.go` (the routes), `internal/repository/stadium_repository.go`,
migration `0131_stadiums`.

## 1. Read this first: what the game does with the file

Everything below follows from Bohemia's object spawner (`scripts/3_game/objectspawner.c`,
`cfggameplayhandler.c`), verified in `docs/SHOP_DELIVERY.md` and by the Shop canary:

* With `cfggameplay.json` enabled, every file listed in `WorldsData.objectSpawnersArr` is read
  **once per server start**. Each entry is created at the **exact** `[x, y, z]` given, `y` being the
  altitude. **There is no ground snapping.** An object given a wrong altitude floats or sinks.
* So the site must be **flat**: an airfield runway, a large field, a car park. On a slope the far
  end of an 80 m arena is metres above or below the ground. The altitude is never invented: it comes
  from a position the server itself logged (the position lookup), or the owner types one read from
  the server log.
* `enableCEPersistency` is false: the objects are not saved and spawn again at every start. An item
  a player picks up persists in that player's inventory like any item; what is left on the ground
  is gone at the next restart and a fresh kit spawns.
* On Nitrado console services the game host receives user files **only from `custom/`**
  (`docs/archive/SHOP_CUSTOM_RELOCATION.md`), which is why the file lives there.
* A restart is needed and Champion never performs one here (`requiresRestart: true`).

Honest caveats:

* **Object sizes and origins are not verified in-game.** The layout is drawn with a standard 20 ft
  ISO container (6.06 x 2.44 m), a 20 m castle wall and guesses for the stairs (6 x 3 m) and tower
  (8 x 8 m). Model origins are assumed to be at the footprint centre. Expect to nudge the per-kind
  altitude offsets after the first build.
* **Yaw.** From community spawner files: `ypr[0]` is a compass bearing of the model's forward axis,
  clockwise from north. Containers are long along their forward axis (yaw 0 stands north-south);
  castle walls are long along their X axis (yaw 0 runs east-west). Which way the stairs and the
  tower "face" is a guess (documented in the object list).
* Class names of the structures were taken from community spawner files that run on console; the
  item class names of the built-in kits are vanilla types. A custom item name is only checked for
  shape and is reported as unverified in the preview: a misspelt class spawns nothing.

## 2. Layout

Local frame: `u` east, `v` north, metres from the centre; rotated clockwise by `yawDeg` (a
positive yaw turns the arena's east axis towards south, like a compass) and moved to
`(centerX, centerZ)`. Every object's altitude is `altitudeY` plus its kind's offset. Inner arena
sizes: SMALL 60 x 40 m, MEDIUM 80 x 60 m (default), LARGE 100 x 80 m (`L` east-west, `W`
north-south).

| Piece | Class | Where |
| --- | --- | --- |
| North and south walls | `Land_Castle_Wall1_20` | 20 m segments corner to corner at `v = +-W/2`, yaw 0 |
| East and west sides | `Land_Castle_Wall1_20`, `Land_Container_1Bo` | The gate opening (20 m, centred on `v = 0`) is left open and narrowed to 8.9 m by a closed container on each side (`v = +-7.5`, yaw 0, standing north-south). Between that container and the corner: MEDIUM one 20 m wall (`v = +-20`, yaw 90); SMALL two more containers (`+-13.56`, `+-19.62`), no wall; LARGE two more containers and a wall at `+-32.65`. On SMALL and LARGE the side reaches 2.65 m past the corner. |
| Wings (one per gate, mirrored) | `Land_Container_1Aoh` x4, `Land_Container_1Mo` x2 | Two walk-through tunnels of two open containers end to end along `u` (0.3 m gap), from `u = +-(L/2+2)` to `+-(L/2+14.42)`: the **gun locker room** at `v = +3.4` and the **clothing room** at `v = -3.4`, yaw 90. A closed container at `v = +-7.5` (yaw 90, centred on the wing) as a side wall. |
| Kit items | kit class names | On each tunnel floor, from the gate end outwards: two columns (`v` +-0.6 of the tunnel axis), 1.1 m apart along `u`. Up to 22 items fit at that pitch; a bigger custom kit is packed closer (the preview warns) down to 40 items. |
| Map pillar | `StaticObj_Furniture_school_map` x4 | `(+-0.35, 0)` and `(0, +-0.35)`, yaw 90/270/0/180, facing outwards |
| Cover LIGHT (default) | `Land_Roadblock_WoodenCrate` x12, `staticobj_roadblock_cncblocks_long` x2 | Four clusters of three crates (a 1.5 m triangle) at `(+-L/6, +-W/5)`; block rows at `(0, +-W/3)`, yaw 0 |
| Cover HEAVY | adds `Land_Wreck_Uaz` x2, blocks x2 | Wrecks at `(+-L/4, 0)`; block rows at `(+-L/3, 0)`, yaw 90 |
| Stands | `Land_Castle_Stairs` x4 | `(+-(L/2+5), +-(W/2+3))`: 5 m past the side, so a stand clears the side pieces that overshoot the corner. The north pair faces south (yaw 180), the south pair north (yaw 0) - "facing the long wall" is a guess at the model's front. |
| Commentary tower | `Land_Mil_ATC_Small` | `(0, W/2+12)`, yaw 180 (facing the arena) |
| Flags | `StaticObj_Furniture_flag_chernarus_pole` x4 | `(+-(L/2+17), +-9)` |

Object counts with the defaults (LIGHT cover, M4_ONLY, RED_BLUE_CORNERS, every extra; 60 items):
**SMALL 117, MEDIUM 115, LARGE 125.** MEDIUM structures: 12 walls, 4 gate containers, 8 open
containers, 4 side containers, 4 maps, 12 crates, 2 block rows, 4 stairs, 1 tower, 4 flags.

Kits (per tunnel; the item preview counts both wings):

* `lockerKit` `M4_ONLY`: 2x `M4A1`, 4x `Mag_STANAG_30Rnd`, 2x `AmmoBox_556x45_20Rnd`,
  2x `M4_T3NRDSOptic`, 2x `M4_RISHndgrd`, 2x `M4_MPBttstck`, 2x `BandageDressing`, 2x `Morphine`.
  `M4_PLUS_SIDEARM` adds 2x `FNX45`, 2x `Mag_FNX45_15Rnd`. `CUSTOM`: `customLockerItems`.
* `clothingKit` `RED_BLUE_CORNERS`: red wing `TrackSuitJacket_Red`, `TrackSuitPants_Red`; blue wing
  `TrackSuitJacket_Blue`, `TrackSuitPants_Blue`; both `BallisticHelmet_Black`, `PlateCarrierVest`,
  `TacticalGloves_Black`, `CombatBoots_Black`, each x2. `TACTICAL`: `TTSKOJacket_Camo`,
  `TTSKOPants`, `BallisticHelmet_Green`, `PlateCarrierVest`, `TacticalGloves_Black`,
  `CombatBoots_Black` x2 in both wings. `CUSTOM`: `customClothingItems` in both wings.
* Custom lists: at most 40 entries, each `{className, count}` with a class name matching
  `^[A-Za-z][A-Za-z0-9_]{1,63}$` and a count of 1..40, at most 40 items in total per room. Names
  outside the verified list are accepted and listed in `preview.warnings`.

Limits: at most 500 objects; every object must lie inside the map (`internal/dayzmap`: Chernarus
15360 m, Livonia 12800 m); the altitude must be within -50..1500 m (the Shop's sanity range).

The file: `{"Objects": [...]}` with two-space indent and a trailing newline, objects in layout
order, each `{name, pos: [x, altitude, z], ypr: [yaw, 0, 0], scale: 1, enableCEPersistency:
false, customString: "champion:stadium:v1:<index>"}`. Coordinates carry three decimals.

## 3. API

Base `/api/saas/organizations/{organizationID}/installations/{installationID}/stadium`. The
standard chain: service auth, acting user, organization **OWNER or ADMIN** (`403 FORBIDDEN`
otherwise; a MEMBER or a faction role grants nothing), the installation in the organization
(`404 NOT_FOUND`). A platform admin's "view as customer" session is read-only: its `GET`s work,
every `POST`/`PUT` is `403 FORBIDDEN` (`docs/OWNER_OPS.md`). Reads share the admin read budget,
writes the admin action budget (`429 RATE_LIMITED`).

| Route | Returns |
| --- | --- |
| `GET` | `{"stadium": Stadium}` |
| `PUT` body = `Params` | Saves the configuration as `DRAFT` (a `BUILT` stadium stays `BUILT` with `dirty: true` while the saved params differ from the built file; a `REMOVED` one becomes `DRAFT`). Returns `{"stadium": Stadium}` with a fresh preview. Validation errors are `400 VALIDATION_ERROR` with a plain message (`INVALID_REQUEST` for a body that is not JSON or over 64 KB). |
| `POST /position` body `{"playerName"?: string}` | `{"position": {"x", "z", "altitudeY", "observedAt", "playerName", "source": "ADM"}}`: the latest ADM-recorded position **with its recorded altitude** within the last 24 hours, of the acting user's VERIFIED linked player on this server, or of the named player (case-insensitive, must have been seen on this server). Otherwise `404 NOT_FOUND` "no recent position ...". |
| `POST /build` | `{"outcome": Outcome, "stadium": Stadium}`. Writes the file and references it (section 4). |
| `POST /remove` | `{"outcome": Outcome, "stadium": Stadium}`. Writes the empty file `{"Objects": []}` and keeps the reference, so nothing spawns after the next restart. |
| `GET /file` | The current layout as `application/json`, `Content-Disposition: attachment; filename="champion_stadium.json"`, for a manual upload. `404` when nothing is saved. |

`POST /build` and `POST /remove` answer `200` with the outcome whenever the procedure ran, even
when it failed (the outcome says why). A build makes some fifty Nitrado calls and can take up to
a minute (the handler allows 90 s); the website should wait for it rather than time out early. They answer an error only when it cannot start:
`409 CONFLICT` with no saved configuration or while another build of the same installation runs,
`400 INVALID_REQUEST` when the installation has no DayZ server, `503 NITRADO_UNAVAILABLE` when the
organization's Nitrado connection cannot be opened. Every save, build and remove is audited
(`STADIUM_SAVE`, `STADIUM_BUILD`, `STADIUM_REMOVE`).

### Params

```json
{
  "mapKey": "chernarusplus",
  "centerX": 4618, "centerZ": 10439,
  "altitudeY": 339.2, "altitudeSource": "ADM:<player>@<RFC3339>" ,
  "yawDeg": 0,
  "size": "SMALL|MEDIUM|LARGE",
  "cover": "NONE|LIGHT|HEAVY",
  "lockerKit": "M4_ONLY|M4_PLUS_SIDEARM|CUSTOM", "customLockerItems": [{"className": "M4A1", "count": 2}],
  "clothingKit": "RED_BLUE_CORNERS|TACTICAL|CUSTOM", "customClothingItems": [],
  "extras": {"stairs": true, "commentaryTower": true, "flags": true, "mapPillar": true},
  "offsets": {"wallY": 0, "containerY": 0, "itemY": 0.05, "towerY": 0, "miscY": 0}
}
```

`centerX`, `centerZ` and `altitudeY` are required. Everything else has a default (MEDIUM, LIGHT,
M4_ONLY, RED_BLUE_CORNERS, every extra on, `itemY` 0.05, the other offsets 0,
`altitudeSource` `MANUAL`); a field left out of `extras` or `offsets` keeps its default. Enums
are case-insensitive, `yawDeg` is normalised to 0..360, offsets are limited to +-5 m,
`altitudeSource` is `MANUAL` or `ADM:...`. Unknown fields are refused. `mapKey` may be left out:
the installation's configured map (the Shop delivery map, which the live map uses too) is filled
in; with none configured the layout is checked against Chernarus bounds and the preview warns.
The stored and returned `params` are always complete.

### Stadium

```json
{
  "status": "NONE|DRAFT|BUILT|REMOVED",
  "params": Params | null,
  "preview": Preview | null,
  "build": {"builtAt": "RFC3339|null", "fileSha256": "hex|null", "objectCount": 115, "referenced": true, "lastOutcome": Outcome | null} | null,
  "dirty": false,
  "updatedAt": "RFC3339|null",
  "removedAt": "RFC3339|null"
}
```

`build` is present once anything was built, removed or attempted (`lastOutcome` is the last build
or remove outcome, successful or not). `dirty` is true on a `BUILT` stadium whose saved params no
longer render to the built file.

### Preview

```json
{
  "objectCount": 115,
  "footprint": [[x, z], [x, z], [x, z], [x, z]],
  "zones": [{"kind": "ARENA", "name": "Arena", "polygon": [[x, z], ...], "center": [x, z]}],
  "classes": [{"className": "Land_Castle_Wall1_20", "count": 12}],
  "items": [{"className": "M4A1", "count": 4, "room": "LOCKER_ROOM"}],
  "warnings": ["..."]
}
```

Polygons are in map coordinates after rotation, four corners, counter-clockwise from the local
south-west corner. `footprint` is the site's bounding rectangle plus 3 m. Zone kinds: `ARENA`,
`GATE_RED`, `GATE_BLUE`, `WING_RED`, `WING_BLUE`, `LOCKER_ROOM_RED`, `CLOTHING_ROOM_RED`,
`LOCKER_ROOM_BLUE`, `CLOTHING_ROOM_BLUE`, `COMMENTARY` (when the tower is on), `STAND_NW`,
`STAND_NE`, `STAND_SW`, `STAND_SE` (when the stairs are on). `items` counts both wings per room
kind (`LOCKER_ROOM`, `CLOTHING_ROOM`), sorted by room then class.

### Outcome

```json
{
  "status": "BUILT|REMOVED|FAILED|UNCERTAIN",
  "file": "custom/champion_stadium.json",
  "sha256": "<hex of the file that was (to be) written>",
  "objectCount": 115,
  "fileVerified": true,
  "referenced": true,
  "steps": [{"name": "write the stadium file", "result": "PASS|FAIL|SKIPPED|UNVERIFIED", "detail": "..."}],
  "requiresRestart": true,
  "message": "...",
  "configBackup": "champion/backup/cfggameplay.json.<sha12>.bak",
  "configSha256": "<hex>",
  "writes": 5,
  "at": "RFC3339"
}
```

`BUILT`: the file is verified on the server and `cfggameplay.json` references it. `REMOVED`: the
empty file is verified. `FAILED`: nothing harmful happened; every write was verified or proven
unchanged, and `message` says what to do (`fileVerified` with `referenced: false` means the file is
on the server but the configuration write failed: build again). `UNCERTAIN`: a read-back could
not prove the state; `message` says which file to check in the Nitrado file browser before the
next restart. `steps` never carry a token, a URL, an account path or file content.

## 4. The write (what Build does, in order)

The owner pressing **Build** (or **Remove**) is the approval of this one procedure; nothing runs on
a schedule or at start-up. The Shop's gates are strictly operator-run, single-use-authorised steps
with their own tool and journal (`docs/SHOP_GATE_A_UPLOAD.md`), so the Stadium is modelled as its
own approved action with the same rules, in `internal/stadium/stadiumwrite` (the isolation test
in `internal/shop/missionwrite` allows exactly this package the write primitives, reachable from
`internal/app/saas_api_stadium.go` alone):

1. Find the mission folder by its `cfggameplay.json` (`maprotation.Locate`). None: **FAILED** with
   the message to enable "cfggameplay.json" in Nitrado's General Settings and restart once.
2. Download `cfggameplay.json` and compute the edit (`maprotation.EditChampionSpawner`): only the
   text of `WorldsData.objectSpawnersArr` changes, every other byte is kept; a file that cannot be
   edited safely is refused. `champion_stadium.json` is a reserved name a map rotation can never
   remove.
3. List `custom/` and the current `custom/champion_stadium.json` (existence from directory
   listings, never from a download error). A file holding objects Champion did not write is never
   overwritten.
4. Record the server's boot identity (newest `DayZServer_*.ADM` across the log mounts) and status.
5. The stadium file: skipped when it already has exactly this content. Otherwise create `custom/`
   when missing (one mkdir, verified by listing), re-read the file, the configuration, the boot and
   the status immediately before the write (any change: **FAILED**, nothing written), then one
   upload token and one transfer, and an independent read-back decides: equal to the payload -
   verified; unchanged after a refused request - **FAILED**; anything else - **UNCERTAIN**, stop.
6. The reference (build only, when missing): a verified backup first -
   `champion/backup/cfggameplay.json.<sha12>.bak`, the exact current bytes read back and compared
   (the same backup the Shop's `gate-b-rollback` restores; folders created when missing; never
   inside `custom/`) - then the re-verification, one upload of the edited configuration and the
   read-back. **UNCERTAIN** names the backup to restore by hand.
7. Report whether the server restarted during the build (informational).

A remove writes the empty file the same way and leaves the configuration alone (an empty
referenced file is harmless; a missing one makes the game log an error at boot). The outcome is
recorded on the stadium row and answered; the stadium's status follows it (`BUILT`, `REMOVED`, or
unchanged on `FAILED`/`UNCERTAIN`). One build per installation runs at a time.

## 5. Doing it by hand

`GET .../stadium/file` downloads the file. Then, on Nitrado:

1. General Settings: tick **Enable cfggameplay.json** (console services). Restart once if it was off.
2. File browser: in the mission folder (`dayzps_missions/dayzOffline.chernarusplus` or the
   server's mission), create `custom/` if it does not exist and upload `champion_stadium.json`
   into it.
3. Edit `cfggameplay.json`: in `WorldsData`, add `"custom/champion_stadium.json"` to
   `objectSpawnersArr` (keep every other entry), for example
   `"objectSpawnersArr": ["custom/champion_stadium.json"]`.
4. Restart the server. The arena appears about 30-40 seconds into the boot.

To take it down by hand, replace the file's content with `{"Objects": []}` (or remove the entry
from `objectSpawnersArr` and the file) and restart.

## 6. Position lookup and altitude

The altitude is the third value of the ADM `pos=<x, z, altitude>` the server writes for every
player every five minutes and on every event (`internal/killfeed`, `player_location_events.y`).
`POST .../stadium/position` returns the newest such row of the last 24 hours that carries an
altitude, for the owner's linked player or a named player of this server. The website records
where the altitude came from in `altitudeSource` (`ADM:<player>@<observedAt>`). Stand where the
centre of the arena should be, wait for the next player-list line (up to five minutes), then look
it up.

## 7. Database

Migration `0131_stadiums`: table `stadiums` - `installation_id` (primary key),
`organization_id` (composite foreign key with the installation, cascade), `params` JSONB,
`status` (`DRAFT`, `BUILT`, `REMOVED`), `file_sha256`, `object_count`, `built_at`, `removed_at`,
`last_outcome` JSONB, `created_at`, `updated_at`. Positions are read from
`player_location_events`; nothing else is written.

## 8. Tests

* `internal/stadium` (unit): determinism, the structure allowlist for every built-in combination,
  object counts per size, mirror symmetry, rotation (a 90 degree yaw moves the west gate north and
  every object with it), map-bound and altitude refusal, custom-item validation and warnings,
  parsing defaults, the file rendering and read-back parsing, the altitude range pinned to the Shop's.
* `internal/stadium/stadiumwrite` (unit, the real Nitrado client against the fixture's mission
  folder): a build writes, backs up and references and a second build changes nothing; `custom/`
  is created; a missing `cfggameplay.json` fails with the Nitrado-settings message before any
  write; partial, refused and claimed-but-unchanged transfers; a configuration edited or a server
  restarted between the inspection and the write; a foreign file; remove; an already referenced
  file needs no backup.
* `internal/app/saas_api_stadium_integration_test.go` (routes + PostgreSQL + the fixture): the
  chain and tenant isolation, validation, drafts and dirty, the download, the position lookup
  (linked player, named player, 24 h window, altitude required, another server's player), build
  writes and references, rebuild, read-back mismatch, remove, view-as cannot build, member
  forbidden, Nitrado unavailable, no cfggameplay.json.
* `internal/maprotation`: the reserved file name and `EditChampionSpawner`.
* `internal/shop/missionwrite` `TestWriteCapabilityIsIsolated`: only `stadiumwrite` may call the
  write primitives for the stadium, and only `saas_api_stadium.go` may import it.
