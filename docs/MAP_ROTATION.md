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
   A map's picture is optional and is uploaded the same way: a JPEG, PNG or WebP file of at most
   400 KB. Champion stores it and the website shows it on the vote page from its own address, so it
   keeps loading (a pasted link to a picture on Discord stops working after about a day, and
   pictures from other sites do not load inside a Discord Activity). Saving without a new picture
   keeps the stored one; **Remove picture** deletes it.
4. Press **Check files**: Champion lists the `custom` folder and shows which map files it found,
   and for each map whether its spawn file is stored.
5. Choose how often the map changes (every 1, 2 or 3 restarts), the order (sequence or random),
   whether players vote, how long the vote runs, the Discord channel for the announcement and
   whether it pings `@everyone`. Optionally switch on **Fresh characters on every map switch**
   (off by default; see the section of that name below before using it).
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

## Fresh characters on every map switch

Off by default. An option per server (`wipeCharacters`, stored as
`map_rotation_settings.wipe_characters`).

**Why.** DayZ saves every character, with its position, in `storage_1/players.db` inside the
mission folder. After a map switch a player therefore logs in where they stood on the old arena
and has to die once to reach the new map's spawn points. With this option on, Champion removes the
saved characters when it switches the map, so everyone starts as a fresh spawn on the new map.

> **Not yet tested on a real server.** The three Nitrado calls this uses (stop the server, delete a
> file, start the server) are written from Nitrado's documentation and have not been run against a
> live Nitrado service. Until someone has tried the option on a real server and watched it work,
> treat it as unverified. It is also unverified that a DayZ console server on Nitrado starts
> cleanly without `players.db` and writes a new one, which is what the game does elsewhere. Try it
> first on a server where a failed start costs nothing.

**What is deleted.** Exactly one file: `<mission folder>/storage_1/players.db`. That is every
character: where they are, their health and everything they wear and carry. Nothing else is
deleted, ever: bases, tents, vehicles, stashes and everything stored in the world are in other
files of the storage folder and are not touched, and neither is the storage folder itself. Players
keep what is in their base; they lose what is on their body.

**What happens**, right after a switch's two files are written and verified (so about 5 minutes
before a known scheduled restart):

1. Champion looks for `storage_1/players.db`. If the folder or the file is not there (for example
   nobody has joined since the last wipe), the server is **not** stopped; the switch's message says
   "Saved characters could not be cleared: the saved-characters file was not found." and staff are
   alerted. The switch stays successful and the rotation goes on.
2. It stops the server and asks Nitrado for the server's status every 5 seconds, for up to 2
   minutes, until it reads `stopped`. A server that does not stop keeps its file.
3. It deletes the file, once, and lists the folder again. Only a listing without the file counts
   as "cleared".
4. It starts the server again, **whatever happened in steps 2 and 3**, and watches the status until
   it reads `restarting` or `started` (up to 2 minutes; the start is requested again, up to 3 times,
   while the server stays down).

So the server goes down for a short time a few minutes **before** the scheduled restart, comes up
on the new map with fresh characters, and is restarted again by its schedule shortly after. Players
see two restarts close together.

The Discord posts are unchanged. The outcome is added to the switch's message, for example "Both
files were written and verified. Saved characters were cleared, so everyone spawns fresh.", and
`lastSwitch.charactersCleared` says it as a value.

**Counting restarts.** Champion's own start in step 4 is an extra boot. So that "every N restarts"
still means N scheduled restarts, every boot seen within **20 minutes** after that start belongs to
the same restart:

- the first boot seen after the switch makes the new map the current map, as always;
- a further boot inside the 20 minutes (the scheduled restart) is remembered as the current boot
  and the period is counted from it, but it does not add to the restart count;
- inside the 20 minutes nothing is decided, voted on or written, so the few minutes between the
  two boots never become a period of their own;
- a boot later than 20 minutes after Champion's start is counted like any other.

With the option off none of this applies and restarts are counted exactly as before. A side effect
worth knowing: a manual restart in those 20 minutes is not counted either.

**When something goes wrong.** The switch itself stays successful (the two files are in place) in
every case below.

| What | Result |
| --- | --- |
| The file or the storage folder is not found, Nitrado cannot be reached, or the server's status cannot be read | The server is not stopped. Characters not cleared; staff alert (warning). The rotation goes on. |
| The server does not report `stopped` within 2 minutes | Nothing is deleted. The server is started again. Characters not cleared; staff alert (warning). |
| Nitrado refuses the delete, or says yes but the file is still listed, or the folder cannot be listed afterwards | The server is started again. Characters not cleared; staff alert (warning). |
| The server cannot be seen starting again | **The rotation stops** with "The server was stopped to clear characters and could not be started again. Start it in Nitrado." and staff get a critical alert. Saving the settings starts the rotation again. |
| The bot crashes or is redeployed in the middle | The step it was at is stored on the switch before the step is taken (`wipe_state`: `STOP_REQUESTED`, `DELETED`, `START_REQUESTED`, then `DONE` or `FAILED`). On its next tick the worker never stops the server and never deletes again; it starts the server (unless it is already starting), closes the switch and reports the characters as cleared only if that had been verified before the crash. This also happens when the rotation was switched off, stopped or suspended in the meantime. If the server still cannot be seen starting, or the server's Nitrado connection cannot be opened for 30 minutes, the rotation stops with the message above and staff get the critical alert. |

Limits worth knowing:

- After a crash the server is restarted once more than strictly needed in some cases (Champion
  cannot know whether its stop request arrived, so it always asks for a start).
- "Seen starting" is Nitrado's status reading `restarting` or `started`. Champion does not wait for
  the game to finish loading.
- A scheduled restart that arrives while the server is stopped for the wipe is not something
  Champion can prevent. The wipe begins about 5 minutes before a known restart and normally takes
  one to two minutes.
- The staff alert for a wipe is sent once, when it happens. If Discord or the bot is down at that
  moment it is not sent later; the switch's message still says what happened.
- It runs at most once per switch, only after both files are verified, and never for a switch that
  failed or was rolled back.

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

These messages carry no map picture: neither a pasted picture address nor an uploaded picture is
put into them.

`<site>` is `CHAMPION_SITE_BASE_URL` (default `https://championshp.vip`), the setting every other
link to the website already uses.

## Safety rules for the file writes

The switch is `internal/maprotation/mapswitch`. It is the only code besides
`internal/shop/missionwrite` that calls the Nitrado write primitives
(`nitrado.RequestUploadToken`, `nitrado.PostUpload`), and `TestWriteCapabilityIsIsolated` in
`internal/shop/missionwrite/isolation_test.go` was extended on purpose to allow exactly this
package, to allow it only those two calls (never `Mkdir`), and to require that only
`internal/app/map_rotation_worker.go` imports it.

The same test covers the one delete Champion can make. `nitrado.DeleteFile` may be called only by
`internal/maprotation/charwipe` (fresh characters, above), which only
`internal/app/map_rotation_worker.go` may import. Inside that package one function,
`deletePlayersDB`, makes the call, and it refuses every path that is not exactly
`<located mission folder>/storage_1/players.db`.

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
  in the database, not on the server.) The only file Champion ever deletes is
  `storage_1/players.db`, by a separate procedure and only with the owner's "fresh characters"
  option on.
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
             imageUrl: string|null, imageUploaded: boolean, imageBytes: number,
             imageVersion: string|null, enabled, position }
VoteOption { mapId, name, imageUrl: string|null, imageVersion: string|null, votes }
Vote       { id, status: "OPEN"|"CLOSED", opensAt, closesAt, options: VoteOption[], totalVotes }
AdminView  {
  available, reason: string|null,
  enabled, everyRestarts: 1|2|3, order: "SEQUENCE"|"RANDOM",
  voteEnabled, voteMinutesBeforeRestart: 5..120, pingEveryone, announceChannelId: string|null,
  wipeCharacters: boolean,
  maps: MapEntry[],
  current: { mapId|null, name|null, since|null },
  next: { mapId|null, name|null, decidedBy: "ROTATION"|"VOTE"|"STAFF"|null, switchAt|null, restartsUntilSwitch|null },
  vote: Vote|null,
  lastSwitch: { at, mapId|null, name|null, ok, message, charactersCleared: boolean|null }|null,
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
| `PUT adminBase/map/rotation` | `{ enabled, everyRestarts, order, voteEnabled, voteMinutesBeforeRestart, pingEveryone, announceChannelId, wipeCharacters?, maps: [{ id?, name, mapFile, spawnFile, spawnXml?, imageUrl, imageData?, removeImage?, enabled }] }` | `AdminView`. Array order is rotation order; 0 to 5 maps; a map with `id` is updated, one without is created, a stored map left out is removed. Switching on needs 2 enabled maps. |
| `POST adminBase/map/rotation/check` | none | `AdminView` with `filesCheck` filled: lists the server's `custom` folder (a read) and reports which configured map files exist. Nothing is looked up on Nitrado for the spawn files. |
| `POST adminBase/map/rotation/next` | `{ mapId: number\|null }` | `AdminView`. Sets the staff choice for the next switch; `null` clears it. |
| `GET /api/saas/player/servers/{installationID}/map/vote` | | `PlayerView`. Any signed-in website user; `linked` says whether they can vote. |
| `POST /api/saas/player/servers/{installationID}/map/vote` | `{ mapId }` | `PlayerView`. One vote per linked player per vote; voting again changes it. |
| `GET /api/saas/network/servers/{installationID}/map-images/{mapID}` | | The map's stored picture as bytes (not JSON). Service bearer only, no acting user, like the public live map. See "The picture". |

Notes on the views: `next` is filled once the next map is known: a staff choice at once, a sequence
without a vote in advance, a vote or a random order when decided. `switchAt` is the scheduled
restart that will load it, when known. `vote` is the open vote, or the vote that decided the
current period (status `CLOSED`) until the restart. `filesCheck` holds the last check of each map
and is cleared for a map whose map file name changes. `spawnFileFound` means the map's spawn file
is stored in Champion, as it is now (the same as the map's `spawnUploaded`).

**Fresh characters.** `wipeCharacters` (boolean) is the option described in "Fresh characters on
every map switch". In the `PUT` body it may be left out, which means `false`: a client that does
not know the field switches the option off when it saves. `GET` always returns it.
`lastSwitch.charactersCleared` is read-only: `null` when clearing was not attempted for that switch
(the option was off, or the switch did not succeed), `true` when `players.db` was deleted and
verified gone, `false` when it was not (the reason is in `lastSwitch.message`). `lastSwitch.ok`
stays `true` for a switch whose files were written even when the characters were not cleared.

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
- The request body of the `PUT` is limited to 10,660,536 bytes, about 10.2 MiB (five files of 1 MB, half as much again
  because a file is larger as a JSON string, five pictures of 400 KB as base64, and 64 KB for the
  rest). A larger body is a `VALIDATION_ERROR`.

**The picture.** A map's picture is uploaded and stored in Champion. `imageUrl` (a pasted address)
still works as before and is kept for maps that have one; a client shows the stored picture when
`imageVersion` is not null and falls back to `imageUrl` otherwise.

- `imageData` is the picture file in standard base64 (with padding, no `data:` prefix). Left out,
  null or empty, the stored picture is kept. Sent, it replaces it.
- The picture is at most 400 KiB (409,600 bytes) decoded and must be a JPEG, PNG or WebP image.
  Champion decides the type from the bytes; SVG, GIF and everything else are refused. Each failure
  is a `VALIDATION_ERROR` that names the map, for example "Map 2 (Dust): the picture must be a
  JPEG, PNG or WebP image".
- `removeImage: true` deletes the stored picture. With `imageData` in the same map the new picture
  is stored and `removeImage` has no effect.
- A map removed from the list loses its picture with it.
- The bytes are never part of a JSON view. `imageUploaded` says whether a picture is stored,
  `imageBytes` how large it is (0 when none) and `imageVersion` is the first 16 hex characters of
  the SHA-256 of the bytes (null when none). A vote option's `imageVersion` is read from the map as
  it is now, not as it was when the vote opened: a picture uploaded or removed during a vote shows
  at once, and it is null when the map has been removed.
- `GET /api/saas/network/servers/{installationID}/map-images/{mapID}` answers `200` with the bytes
  and `Content-Type` (`image/jpeg`, `image/png` or `image/webp`), `X-Content-Type-Options: nosniff`,
  `Cache-Control: public, max-age=31536000, immutable` and `ETag: "<imageVersion>"`. It answers
  `404 NOT_FOUND` (the standard JSON error) when the installation, the map or the picture does not
  exist or the map belongs to another installation. It does not depend on the rotation being
  switched on, on the feature flag or on the plan. The website serves it to players at
  `<site>/api/live/<installationID>/map-image/<mapID>?v=<imageVersion>`.

| Code | HTTP | When |
| --- | --- | --- |
| `VALIDATION_ERROR` | 400 | A field is out of range, a file name or picture address is not allowed, a picture is too large or not a JPEG, PNG or WebP image, a spawn file is missing, empty, too large or not a spawn point file, a map id is unknown, the channel is not a text channel of this Discord server; on a vote, the map is not an option. |
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

Migration `0127_map_rotation_wipe_characters` (additive): `map_rotation_settings.wipe_characters`
(the option, default false) and `wipe_restart_at` (when Champion last started a server after
clearing characters; the restart count uses it), and on `map_rotation_switches` `wipe_state`
(default `NONE`), `wipe_state_at`, `wipe_note` (the outcome as a sentence) and
`characters_cleared` (null when not attempted).

Migration `0128_map_rotation_map_images` (additive, three nullable columns on `map_rotation_maps`):
`image_data` (`BYTEA`, the uploaded picture), `image_type` (its content type) and `image_version`
(the first 16 hex characters of the SHA-256 of the bytes). `image_url` stays.

## Code

| Where | What |
| --- | --- |
| `internal/maprotation` | The rules, with no way to write: file names, the `cfggameplay.json` edit, the spawn file check, rotation order, vote count, restart counting (with the 20-minute rule after a wipe restart) and the planner (`Plan`). |
| `internal/maprotation/mapswitch` | The two-file switch with backup, read-back and rollback. |
| `internal/maprotation/charwipe` | Fresh characters: stop, delete `players.db`, start, and the resume after a crash. |
| `internal/repository/map_rotation_repository.go` | Storage. |
| `internal/app/map_rotation.go` | The API. |
| `internal/app/map_rotation_worker.go` | The worker (once a minute) and the Discord posts. |

## Before enabling it on a real server

- "Fresh characters on every map switch" stops and starts the server and deletes a file through
  Nitrado calls that have not been run against a live Nitrado service. Leave it off until it has
  been tried on a test server (see its section).
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
