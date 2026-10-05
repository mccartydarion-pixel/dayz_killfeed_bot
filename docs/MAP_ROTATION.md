# Map rotation with a player vote

For DayZ console servers hosted on Nitrado. A server owner sets up 2 to 5 maps; Champion makes a different one the active map every 1, 2 or 3 restarts, in
sequence or at random. Before a switch it can ask the players: the bot posts "Pick the next map" in
Discord with a link to the Champion website, where players with a linked gamertag vote.

A map is two files:

- a **map file**: a JSON object-spawner file, for example `arena1.json`. The owner uploads it by
  hand into the server's `custom` folder on Nitrado and gives Champion its name. Champion never
  uploads, edits or deletes it.
- a **spawn file**: an XML file in the format of `cfgplayerspawnpoints.xml`, for example
  `arena1_spawns.xml`. The owner uploads it **on the Champion website**. Champion stores its
  contents in its database (`map_rotation_maps.spawn_xml`) and keeps the file's name for display.
  It is not read from the server's `custom` folder, and a spawn file lying there is ignored.

To make a map active it changes two files in the
server's mission folder, before the restart that should load them:

1. `cfggameplay.json`: in `WorldsData.objectSpawnersArr`, the entries that are map files of this
   server's rotation are removed and `custom/<mapFile>` of the chosen map is put in their place.
   Every other entry (a shop file, a loadout file, anything the owner added) and every other key is
   kept exactly; only the text of that one array changes.
2. `cfgplayerspawnpoints.xml`: its content is replaced by the chosen map's stored spawn file.

## It is off until three things are on

| Level | Default | Who turns it on |
| --- | --- | --- |
| Feature flag `map_rotation` | off | Champion (Owner Hub, per installation), or `CHAMPION_MAP_ROTATION_ENABLED=true` for every installation |
| Plan entitlement `map_rotation` | Champion plans | Only matters when `CHAMPION_PLAN_GATING_ENABLED` is on: Survivor (`NORMAL`) does not include it |
| Platform owner access | - | For an installation of an organization a platform owner owns, both rows above are satisfied with no override and on any plan (docs/ADMIN_API.md "Platform owner access"). An explicit OFF override still wins. The owner's own switch below is unaffected. |
| The owner's `enabled` setting | off | The server owner, in the settings below |

With any of them off, or while Champion has suspended the installation, the worker does nothing for the installation: no Nitrado call, no state
change, no Discord post, no file write. Merging this feature changes nothing for an existing server.

**Turning it on for one test server first.** Leave `CHAMPION_MAP_ROTATION_ENABLED` unset. In the
Owner Hub open the installation, Feature flags, and switch **Map rotation** on, or call
`PUT /api/admin/installations/{installationID}/flags/map_rotation` with
`{"reason": "map rotation test", "enabled": true}` (docs/ADMIN_API.md "Feature flags"). The override
takes effect within 30 seconds. Sending the same call without `enabled` removes it again.

## Owner setup

1. Upload each map's map file (`.json`) into the mission's `custom` folder on Nitrado (the folder
   next to `cfggameplay.json`). File names may use letters, digits, `.`, `_` and `-`, up to 80
   characters.
2. Make sure the server already runs with `cfggameplay.json` enabled and that the file has a
   `WorldsData` section.
3. In the Champion dashboard add the maps (name, map file name, optional picture), in the order
   they should rotate, and upload each map's spawn file (`.xml`, at most 1 MB). Champion checks the
   file when it is uploaded: well-formed XML, root `<playerspawnpoints>`, at least one `<pos x z>`.
   To change a map's spawn points later, upload a new file; saving without one keeps the stored one.
4. Press **Check files**: Champion lists the `custom` folder and shows which map files it found,
   and for each map whether its spawn file is stored.
5. Choose how often the map changes (every 1, 2 or 3 restarts), the order (sequence or random),
   whether players vote, how long the vote runs, the Discord channel for the announcement and
   whether it pings `@everyone`.
6. Switch it on. At least 2 maps must be enabled.

Champion counts restarts from the moment it is switched on. It does not know which map is live
before its own first switch, so "current map" is empty until then.

## Timing

A restart is a new server boot, detected the way the rest of Champion detects it: the ADM log file
the server writes changes (`server_adm_sessions`). With "every N restarts" the map changes on the
Nth restart after the last switch; the time before that restart is the **final period**.

In the final period Champion decides the next map and writes the two files:

- **The restart schedule is known** (Nitrado's scheduled restart tasks, the same reading the live
  map's clock shows): the vote opens `voteMinutesBeforeRestart + 5` minutes before the restart and
  closes 5 minutes before it. The files are written when it closes.
- **The schedule is not known** (Nitrado lists no restart task): the vote opens as soon as the
  final period begins and stays open for `voteMinutesBeforeRestart` minutes. The files are written
  when it closes. If Nitrado cannot be asked at all, Champion decides nothing and asks again a
  minute later.
- **No vote**: the rotation decides and the files are written at the same points (5 minutes before
  a known restart, otherwise `voteMinutesBeforeRestart` minutes into the period).
- Nothing is written less than 2 minutes before a known restart; the switch then waits for the
  next period.

Who decides, in this order: a staff member's choice (`POST .../map/rotation/next`, for the next
switch only), then the vote, then the rotation. With no votes the rotation decides. A tie goes to
the map the rotation reaches first. A vote offers every enabled map except the current one when
that leaves at least two; otherwise every enabled map.

The map becomes "current" when Champion sees the restart after a successful switch. If a switch
fails, the map stays, the decision is kept and the switch is tried again in the next period without
a new vote. After 3 failed switches in a row the rotation stops until the owner saves the settings
again.

Limits worth knowing: restarts are counted one at a time as Champion sees them, so two restarts
within a few seconds, or restarts while the bot is down, count as one. A manual restart counts like
any other.

## Discord

Posted in the configured announce channel (none configured: nothing is posted):

- when a vote opens: "Pick the next map" with the options, the closing time and the link
  `<site>/vote/<installationID>`. With `pingEveryone` the message starts with `@everyone` and only
  that mention is allowed; otherwise the message cannot ping anyone;
- when the vote closes: "Next map: X" with the votes per map;
- after the restart that loaded it: "Map changed to X".

A failed switch goes to staff through the existing staff alert route (`ADMIN_ALERTS`, alert kind
`MAP_ROTATION_FAILED`), never to the public channel. Each post is sent once; one that Discord
refuses is tried again each minute while it is still current.

`<site>` is `CHAMPION_SITE_BASE_URL` (default `https://championshp.vip`), the setting every other
link to the website already uses.

## Safety rules for the file writes

The switch is `internal/maprotation/mapswitch`. It is the only code besides
`internal/shop/missionwrite` that calls the Nitrado write primitives
(`nitrado.RequestUploadToken`, `nitrado.PostUpload`), and `TestWriteCapabilityIsIsolated` in
`internal/shop/missionwrite/isolation_test.go` was extended on purpose to allow exactly this
package, to allow it only those two calls (never `Mkdir`), and to require that only
`internal/app/map_rotation_worker.go` imports it.

- **Validate everything first.** The stored spawn file is checked again before the server is
  asked anything: it must be there, be at most 1 MB and be well-formed XML with the root
  `<playerspawnpoints>` and at least one `<pos x z>` (the same check as at upload). Then three
  files are listed and downloaded from Nitrado: the two live files and the map file.
  `cfggameplay.json` must be valid JSON with a `WorldsData` object whose `objectSpawnersArr`, if
  present, is a list of strings. The map file must be a JSON object. All three must exist, be
  non-empty and be within the size limits (1 MB, 1 MB, 16 MB). If anything fails, nothing is
  written, the switch is recorded as `FAILED` with a plain-language reason and staff are alerted.
- **No stored spawn file, no switch.** A map saved before spawn files were uploaded on the website
  has nothing stored. A switch to it fails before anything is sent to the server, with "Upload a
  spawn file for <map> on the Map rotation page. Nothing was changed.", and staff are alerted like
  for any failed switch.
- **A switch keeps the spawn file it started with.** When a switch begins, the map's stored spawn
  file is copied onto the switch's row (`map_rotation_switches.spawn_xml`) in the same statement
  that creates the row. Every attempt of that switch writes this copy, so a switch finished after a
  crash writes what its first attempt wrote even if the owner uploaded another file in between. The
  copy is dropped when the switch finishes.
- **Backup before the first write.** The previous content of both live files is stored on the
  switch's database row (`map_rotation_switches.prev_gameplay`, `prev_spawns`) before anything is
  uploaded. If it cannot be stored, nothing is written. A stored backup is never replaced. The
  newest five switches of an installation keep their backup.
- **One attempt, then read back.** Each file is uploaded once and judged only by downloading it
  again. The file is also re-read immediately before its upload; if it changed since validation,
  nothing is sent.
- **Roll back.** If the second file cannot be written and verified, the first is restored from the
  backup and the restore is verified (`ROLLED_BACK`). If a restore cannot be verified the switch is
  `FAILED`, staff get a critical alert and the rotation stops until the owner saves the settings.
- **File names are untrusted.** A map file name and the name of an uploaded spawn file must match `^[A-Za-z0-9._-]{1,80}$`, have
  the right extension (`.json`, `.xml`), contain no `..` and not start with a dot. Champion's own
  `champion_shop_delivery.json` cannot be a map. Names are checked when saved and again before a
  switch. Every path is built inside the service's own file-browser root. (The spawn file's name
  is only shown; it is never used as a path.)
- **Two targets only.** A switch writes `cfggameplay.json` and `cfgplayerspawnpoints.xml` in the
  mission folder and nothing else. It never creates a folder and never deletes. (The backup lives
  in the database, not on the server.)
- **Never while off.** The flag, the plan and the owner's switch are checked at the start of every
  pass and again immediately before a switch.
- **Idempotent.** A switch is a row in `map_rotation_switches`: `PENDING` before anything is
  written, then `APPLIED`, `FAILED` or `ROLLED_BACK`. A unique index allows one `PENDING` switch
  per installation. A file that already has the wanted content is not written again, so a retry
  after a crash finishes the switch instead of applying it twice. A `PENDING` switch is carried on
  for 30 minutes and at most 3 attempts; after that nothing more is written, it is closed as
  `FAILED` and, if a write may have happened, the rotation stops for a person to look. One bot
  process works an installation at a time (a lease on the settings row).
- **Nothing sensitive is logged.** Log lines carry ids, statuses and counts. File contents, upload
  tokens and upload URLs are never logged and never appear in a message. That includes the
  uploaded spawn files: the API never returns them, and the audit log records only a map's file
  name and the size of what is stored.

## Permissions and plan

All admin routes need the client-admin capability `MAP_ROTATION_MANAGE` (level Owner). It is
returned by `.../admin/me` like every other capability.

The plan gate is the entitlement key `map_rotation` (internal/entitlements), a Champion-plan
feature like the perk store. Champion's plan model has no separate "Deathmatch" tier today; when
one exists, this key is the one place to move.

## API

`adminBase` is `/api/saas/organizations/{organizationID}/installations/{installationID}/admin`.
Errors use the standard shape `{"error": {"code", "message"}}`.

```
MapEntry   { id, name, mapFile, spawnFile, spawnUploaded: boolean, spawnBytes: number,
             imageUrl: string|null, enabled, position }
VoteOption { mapId, name, imageUrl: string|null, votes }
Vote       { id, status: "OPEN"|"CLOSED", opensAt, closesAt, options: VoteOption[], totalVotes }
AdminView  {
  available, reason: string|null,
  enabled, everyRestarts: 1|2|3, order: "SEQUENCE"|"RANDOM",
  voteEnabled, voteMinutesBeforeRestart: 5..120, pingEveryone, announceChannelId: string|null,
  maps: MapEntry[],
  current: { mapId|null, name|null, since|null },
  next: { mapId|null, name|null, decidedBy: "ROTATION"|"VOTE"|"STAFF"|null, switchAt|null, restartsUntilSwitch|null },
  vote: Vote|null,
  lastSwitch: { at, mapId|null, name|null, ok, message }|null,
  filesCheck: { mapId, mapFileFound, spawnFileFound, checkedAt }[]
}
PlayerView { enabled, linked, serverName,
  current: { mapId, name }|null,
  next: { mapId, name, decidedBy }|null,
  vote: (Vote & { myVote: number|null })|null }
```

| Route | Body | Result |
| --- | --- | --- |
| `GET adminBase/map/rotation` | | `AdminView`. `available: false` with a `reason` when the flag is off, the plan does not include it or no DayZ server is selected. |
| `PUT adminBase/map/rotation` | `{ enabled, everyRestarts, order, voteEnabled, voteMinutesBeforeRestart, pingEveryone, announceChannelId, maps: [{ id?, name, mapFile, spawnFile, spawnXml?, imageUrl, enabled }] }` | `AdminView`. Array order is rotation order; 0 to 5 maps; a map with `id` is updated, one without is created, a stored map left out is removed. Switching on needs 2 enabled maps. |
| `POST adminBase/map/rotation/check` | none | `AdminView` with `filesCheck` filled: lists the server's `custom` folder (a read) and reports which configured map files exist. Nothing is looked up on Nitrado for the spawn files. |
| `POST adminBase/map/rotation/next` | `{ mapId: number\|null }` | `AdminView`. Sets the staff choice for the next switch; `null` clears it. |
| `GET /api/saas/player/servers/{installationID}/map/vote` | | `PlayerView`. Any signed-in website user; `linked` says whether they can vote. |
| `POST /api/saas/player/servers/{installationID}/map/vote` | `{ mapId }` | `PlayerView`. One vote per linked player per vote; voting again changes it. |

Notes on the views: `next` is filled once the next map is known: a staff choice at once, a sequence
without a vote in advance, a vote or a random order when decided. `switchAt` is the scheduled
restart that will load it, when known. `vote` is the open vote, or the vote that decided the
current period (status `CLOSED`) until the restart. `filesCheck` holds the last check of each map
and is cleared for a map whose map file name changes. `spawnFileFound` means the map's spawn file
is stored in Champion, as it is now (the same as the map's `spawnUploaded`).

**The spawn file.** `spawnFile` is the uploaded file's name (required, a `.xml` name as above) and
is only shown. `spawnXml` is the file's text. The contents are never returned: `spawnUploaded` says
whether they are stored and `spawnBytes` how large they are (0 when nothing is stored).

- A map with nothing stored (a new map, or one saved before spawn files were uploaded on the
  website) must send `spawnXml`. Without it the save is refused and nothing is saved:
  `VALIDATION_ERROR`, "Map 2 (Dust): upload a spawn file".
- `spawnXml` left out for a map that has a stored file keeps that file. Sent, it replaces it.
- `spawnXml` must not be empty, is at most 1 MB (1,048,576 bytes) and must be a spawn point file
  (well-formed XML, root `<playerspawnpoints>`, at least one `<pos x z>`). Each failure is a
  `VALIDATION_ERROR` that names the map.
- The request body of the `PUT` is limited to 7,929,856 bytes, about 7.6 MiB (five files of 1 MB, half as much again
  because a file is larger as a JSON string, and 64 KB for the rest). A larger body is a
  `VALIDATION_ERROR`.

| Code | HTTP | When |
| --- | --- | --- |
| `VALIDATION_ERROR` | 400 | A field is out of range, a file name or picture address is not allowed, a spawn file is missing, empty, too large or not a spawn point file, a map id is unknown, the channel is not a text channel of this Discord server; on a vote, the map is not an option. |
| `INVALID_REQUEST` | 400 | The body is not JSON, or no DayZ server is selected. |
| `ADMIN_FORBIDDEN` | 403 | The caller does not hold `MAP_ROTATION_MANAGE`. |
| `PLAN_FEATURE_REQUIRED` | 403 | A write, and the plan does not include map rotation. |
| `FORBIDDEN` | 403 | A write, and the feature flag is off for this installation. |
| `NOT_LINKED` | 403 | A vote from a user without a verified gamertag link on this server's Discord. |
| `VOTE_CLOSED` | 409 | A vote when none is open (or the feature is off). |
| `NITRADO_UNAVAILABLE` | 503 | The file check could not read the server. |
| `DISCORD_UNAVAILABLE` | 503 | A new announce channel could not be checked. |

`imageUrl` must be an `https` address of at most 500 characters, or null. `name` is 1 to 60
characters. Saving, the file check and the staff choice are written to the admin audit log
(`MAP_ROTATION_SAVE`, `MAP_ROTATION_FILES_CHECK`, `MAP_ROTATION_NEXT_SET`).

## Data

Migration `0122_map_rotation` (additive): `map_rotation_settings` (settings and rotation state, one
row per installation), `map_rotation_maps`, `map_rotation_votes`, `map_rotation_vote_options`,
`map_rotation_ballots` (one row per vote and player) and `map_rotation_switches` (the switch log and
backups).

Migration `0123_map_rotation_spawn_upload` (additive, two nullable `BYTEA` columns):
`map_rotation_maps.spawn_xml` (the uploaded spawn file; `spawn_file` stays as its name) and
`map_rotation_switches.spawn_xml` (the copy a switch in flight works with). Maps that existed
before it have no stored spawn file: their owner uploads one on the Map rotation page, and until
then a switch to such a map fails without touching the server. `map_rotation_maps.spawn_file_found`
is still written by the file check (as "stored") but the API reads the stored contents instead.

## Code

| Where | What |
| --- | --- |
| `internal/maprotation` | The rules, with no way to write: file names, the `cfggameplay.json` edit, the spawn file check, rotation order, vote count, restart counting and the planner (`Plan`). |
| `internal/maprotation/mapswitch` | The two-file switch with backup, read-back and rollback. |
| `internal/repository/map_rotation_repository.go` | Storage. |
| `internal/app/map_rotation.go` | The API. |
| `internal/app/map_rotation_worker.go` | The worker (once a minute) and the Discord posts. |

## Before enabling it on a real server

- The owner's map files and spawn files are used as they are. Champion checks their shape, not
  whether the map works in game.
- Maps set up before spawn files were uploaded on the website need their spawn file uploaded once.
  Until then a switch to such a map fails (nothing is written) and counts towards the 3 failed
  switches after which the rotation stops.
- A save with spawn files can be several megabytes. The website (and anything between it and the
  bot) must let a request body of about 8 MB through, and the bot's HTTP server reads a request
  within 5 seconds.
- If a file is edited by hand in the seconds between Champion's last read and its upload, that
  edit is overwritten (Nitrado has no conditional write). The backup on the switch row has the
  content Champion read.
- A restart that arrives while the two files are being written can load one new and one old file.
  Champion does not start a switch less than 2 minutes before a restart it knows about; it cannot
  know about a manual restart.
- The first switch on a server removes nothing it does not own: an entry for one of the maps that
  the owner wrote differently from `custom/<mapFile>` (another folder) is left in place.
