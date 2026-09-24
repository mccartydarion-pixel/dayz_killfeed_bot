# Champion Live Sync (C.L.S.)

Champion's goal: detect changed Nitrado log content promptly, process every supported observation, persist it once, and notify the systems that depend on it. It must never claim an in-game change that DayZ has not written evidence of.

This document records the audit of the existing pipeline, the **phase 1** changes (ADM player lists, session-scoped current location, parsers), the **phase 2** changes (continuous per-source watchers, durable checkpoints and records, session end on evidence, source-time freshness, the heatmap source join), and the remaining phase 3. **Implemented** and **not yet implemented** are marked explicitly throughout.

## 1. Audit: the existing lifecycle (before C.L.S.)

```
DayZ writes ADM ─► Nitrado file server (listing metadata can lag; observed 25+ min on 2026-09-24)
  ─► killfeed.Engine discovery (ListLogs walk, dedupe of ftproot/noftp aliases, newest-file selection)
  ─► poll loop: Stat/list metadata every NITRADO_POLL_INTERVAL (default 10s)
       ├─ unchanged ─► probeStaleSource (direct read) after 2 min; give up + rediscover after staleGiveUpAfter
       └─ changed   ─► delta read (NITRADO_DELTA_READ_MODE) or full ReadLog download
  ─► Tracker: complete lines + absolute end offsets, partial-line buffer, durable ADM checkpoint
  ─► ADMParser.ParseLine ─► processLineAt
       ├─ C.A.S.E. evidence (source-addressed, opt-in)
       ├─ in-memory Deduplicator (90s window)
       ├─ PersistenceQueue (kills/deaths/connect/disconnect; blocks until durable; checkpoint holds on failure)
       ├─ LocationQueue (fire-and-forget batches) ─► player_location_events ─► zones/UAV/radar
       ├─ Hit/Build/PvE/Connection publishers ─► Discord
       └─ PlayerTracker (in-memory presence) ─► presence/voice counters
  ─► SaaS API reads (player directory, locations, heatmaps) - request/response only, no change stream
```

Defects found in the audit:

| # | Finding | Effect | Phase 1 |
|---|---|---|---|
| A1 | ADM player-list blocks (`##### PlayerList log`, bare `Player "x" (id=… pos=<…>)`, `#####`) matched no sub-parser | Every five-minute position observation was dropped; presence never used them | **Fixed** |
| A2 | `Event.Timestamp` is never set outside tests, so `observed_at` is Champion's ingestion time | The location dedupe key (player, server, type, observed_at) never matched a replay. Backlog lines looked "fresh" | **Fixed for new rows**: each row carries its physical source, and dedupe is by source |
| A3 | "Current location" was `LatestLocation` across all history | An old-session position could be presented as current | **Fixed** (section 4) |
| A4 | Only ADM is ingested. RPT, `script_*.log`, `crash_*.log` and `restart.log` are never read | Startup/shutdown, script errors and restart causes are invisible | Phase 1: parsers. **Phase 2: ingested continuously** (section 7) |
| A5 | One selected ADM stands for the whole server's log state | A slow or failing source cannot be isolated | **Fixed (phase 2)**: one independent watcher per family, own timeouts and backoff |
| A6 | No change notification: website data is polled request/response | The website cannot learn about committed changes | Not yet (phase 3) |
| A8 | `kills.event_time` / `deaths.event_time` come from `Event.Timestamp` (never set, see A2), so they are NULL; the kill/death heatmaps join `player_location_events` on `observed_at = event_time` | Kill/death heatmaps can never match a row | **Fixed (phase 2)**: kills/deaths store the line's source file + offset; the heatmap joins on it (section 7.6) |
| A7 | The stale-source loop (`selected_stale` → same file re-selected) is bounded to one rediscovery per `staleProbeInterval`, with a direct-read probe first | Behaviour is correct but coarse; source-level freshness is not exposed per family | ADM loop unchanged; **per-family freshness exposed (phase 2)** |
| A9 | Nitrado's signed download URL **ignores `offset`/`count`** and returns the whole file with a plain 200 (verified read-only on Champions, 2026-09-24). `tryOffsetQuery` accepted any response no larger than the request, and `cmd/nitrado-delta-probe` probed from byte 0 on a 124-byte file | With `NITRADO_DELTA_READ_MODE=offset_query/auto`, a small file would be parsed as if it started at the checkpoint (misaligned lines). Production runs with the mode unset (`off`), so nothing was affected | **Fixed (phase 2)**: offset/count responses are trusted only with a `Content-Range` stating the offset; the probe never tests from byte 0 |

## 2. Source inventory (Champions, service 19806451, read-only, 2026-09-24)

These are actual files exposed by the connected PlayStation service. No filename below is invented. Nitrado API event records (tasks, restart requests via the API) are **not** files, and are listed separately.

| Family | Real name pattern | Canonical path | Count | Boot identity | Parser | Live ingestion |
|---|---|---|---|---|---|---|
| ADM | `DayZServer_PS4_x64_YYYY-MM-DD_HH-MM-SS.ADM` | `dayzps/config/…` (noftp + ftproot alias) | 65 | filename + `AdminLog started on …` header | `killfeed.ADMParser` (+ player lists, phase 1) | **yes** (existing engine) |
| RPT | `DayZServer_PS4_x64_YYYY-MM-DD_HH-MM-SS.RPT` | `dayzps/config/…` | 66 | `Current time:` + `Version` header | `livesync.ParseRPT` | **yes (phase 2)**, newest boot's file |
| Script log | `script_YYYY-MM-DD_HH-MM-SS.log` | `dayzps/config/…` | 66 | `Log … started at DD.MM. HH:MM:SS` | `livesync.ParseScriptLog` | **yes (phase 2)**, newest boot's file |
| Crash log | `crash_YYYY-MM-DD_HH-MM-SS.log` | `dayzps/config/…` | 2 | `Log … started at` + `USMI264, DD.MM YYYY HH:MM:SS` | `livesync.ParseScriptLog` | **yes (phase 2)**, newest file |
| Restart log | `restart.log` | `ftproot/restart.log` → `restart.log` | 1 (157 KB) | pre-start check lines | `livesync.ParseRestartLog` | **yes (phase 2)** |
| Nitrado server log | `server.log` (ftproot `dayzps/config` only, 5.2 MB) | – | 1 | – | none (format not yet sampled) | no: not a DayZ log family; listed only |
| Ban list / whitelist | `ban.txt`, `whitelist.txt` | `dayzps/…` | 1 each | – | classified only | no |
| Nitrado tasks | `GET /services/:id/tasks` (API record, not a file) | – | 0 tasks | – | – | no |

**Per-source tracking fields.** Size, modified time and readable byte count are available from the file-server listing and download. Since phase 2 every family has a durable checkpoint, last read/growth time, record counts and rotation history (`live_sync_sources`, section 7).

**Mounts lag independently.** On 2026-09-24 10:24 UTC the `noftp` listing already showed the 05:51:41 boot's files while the `ftproot/dayzps/config` listing did not list them at all. The watchers merge both mounts per canonical file and take the larger size.

**Real format notes (from the fixtures in `internal/livesync/testdata`, sanitized):**
* **RPT** lines are ` H:MM:SS.mmm message`. Exception blocks (`Reason:` and stack frames) have **no clock prefix**. The shutdown sequence is `[Server] :: termination in: N` … `ENGINE : Destroying game` … `--- Termination successfully completed ---`.
* **Script and crash logs** carry `SCRIPT (E): Virtual Machine Exception` blocks: `Reason: [::Fn] :: [ERROR] :: msg`, `Function: 'Fn'`, `Stack trace:`, frames. Crash logs end with `CLI params:`, which carries the server address. It is **never** kept.
* **restart.log** has two line formats: `Thu, 24 Sep 2026 04:13:32 -0400 msg` (states its offset) and `2026-09-24 08:13:29 msg`. The second is UTC: every such line pairs with an offset line written seconds later, and it is recorded as `clock=utc_inferred`. It holds restart requests (Webinterface/WINDOWS), host reboots, "Website Client Admin request", and a pre-start `[DayZTypesLimiter]` line about 40 s before every boot.
* **The server runs with `-VMErrorMode log_only`**, so a script VM exception (for example the misspelled `custom/The_Losst_City.json` spawner path) produces a `crash_*.log` on every boot, while the server keeps running.

## 3. ADM player lists (phase 1, implemented)

```
04:35:42 | ##### PlayerList log: 1 players                                     -> PLAYER_LIST_HEADER (declared=1)
04:35:42 | Player "Ceiyxe" (id=… pos=<4621.1, 8397.2, 319.6>)                -> PLAYER_LIST_ENTRY
04:35:42 | #####                                                              -> PLAYER_LIST_FOOTER
```

* **Coordinate conversion.** ADM order is `<x, z, altitude>`; canonical world coordinates are `x`, `y = altitude`, `z`. For Ceiyxe's sample: x = 4621.1, y = 319.6, z = 8397.2 (`Position.MapX/Altitude/MapZ`).
* **Every entry is stored,** including repeated stationary ones, as a `player_location_events` row with `event_type = 'PLAYER_LIST'`. Each row carries:
  * `source_file` (canonical ADM identity);
  * `source_offset` (byte offset at the end of the line);
  * `source_local_time` (server-local, zone-less);
  * `snapshot_ref` (`<file>@<header offset>`).
* **Identity.** Each snapshot is identified by its source file plus header offset, and records its declared and observed player counts.
* **Routine entries never create killfeed, connection or Discord messages.** They are handled before the deduplicator, persistence, evidence and every publisher.
* **Presence.** Only a **complete** snapshot (header, all declared entries and footer) reconciles the in-memory tracker:
  * listed players are online;
  * tracked-but-unlisted players are not.

  An incomplete snapshot (count mismatch, a missing footer superseded by the next header, or a stray `#####`) changes nobody's presence. "Complete and empty" and "missing" are different things. Snapshot reconciliation sends no connect/disconnect notice, and it does not change `player_server_activity` (phase 2).
* **The activity heatmap** now also counts player-list positions, sampled at most once per player, minute and cell (its existing rule). That makes it a genuine presence map instead of a map of combat and connect events only.
* **Zone and UAV evaluation** receives player-list positions like any other location batch, so a stationary player inside a zone is evaluated every five minutes.

## 4. Current-session authority (phase 1, implemented)

| Concept | Identity |
|---|---|
| Historical observation | any `player_location_events` row |
| Current server boot | the ADM file the engine currently reads (`server_adm_sessions.adm_file`, written on selection and rotation; ftproot/noftp aliases are the same session) |
| Current ADM file | same as above |
| Current player session | rows from the current ADM file at or after the player's latest `CONNECT` row in that file (byte order, so no timezone is needed) |
| Currently connected | `player_server_activity.currently_connected` (connect/disconnect persistence path) |
| Observation freshness | `ageSeconds` / `freshness` from `observed_at`, plus `sourceLocalTime` from the line |

`CurrentLocation` returns a row only when **all** of these hold:
* the player is connected;
* the row is from the current ADM file;
* the row is at or after their latest connect in that file.

Otherwise the result is **UNKNOWN**. A new boot therefore makes every player UNKNOWN until they are observed in the new file. An unsourced (pre-phase-1) row is never current. History is preserved and exposed separately as `lastKnownLocation`.

```
   boot A (file A)                       boot B (file B)
   CONNECT@100 ─ LIST@200 ─ LIST@500 │   (no rows yet)        CONNECT@50 ─ LIST@150
   current = LIST@500                │   current = UNKNOWN    current = LIST@150
                                     │   lastKnown = LIST@500 (HISTORICAL)
```

**Boot inference is not by filename alone** when better evidence exists. `livesync.CorrelateBoots` anchors a boot on in-file startup evidence, in this order:
1. the RPT `Current time:`;
2. the ADM `AdminLog started`;
3. the script and crash log headers.

A filename timestamp only anchors when nothing better exists, and such a boot is marked `Confirmed = false`. Restart requests attach as the boot's cause when they come at most 5 minutes before it. One restart is counted once, however many sources report it. In phase 1 the correlator is a tested library; the engine's session identity is the selected ADM file.

**Phase 2: a session ends on written evidence, before the new ADM is even visible.** `server_adm_sessions` gains `ended_at`, `ended_reason` and `ended_evidence` (source id + offset, never a raw line). The live sync watchers end the recorded session when DayZ or Nitrado has written proof that it is over:

| Evidence | Source | Ends sessions that started before |
|---|---|---|
| `--- Termination successfully completed ---` | RPT | the line's server-local time |
| `[DayZTypesLimiter]` pre-start check, host reboot | restart.log | the line's server-local time (stated offset) |
| a newer boot's RPT / script / crash file is listed | file name stamp | the stamp minus 2 minutes (files of one boot are stamped seconds apart) |

A restart **request** ends nothing (the server keeps running through its countdown). An ended session is never `CURRENT`: `CurrentLocation` returns `UNKNOWN` immediately, even while the ADM engine is still on the old file. The engine re-recording the same file keeps the end; a file whose boot stamp is older than the recorded session (a lagging mount alias) never replaces it; a genuinely new ADM opens a new session.

## 5. Canonical event envelope (phase 1, type delivered)

`livesync.Envelope` contains:
* `EventID`: deterministic from scope, source, offset and category, so a replay keeps its id;
* the organization / installation / server scope;
* `Family`, `SourceID` (canonical) and `BootID`;
* `PlayerSessionID`;
* `SourceLocalTime`, and `SourceUTC` only when the source states its offset;
* `ObservedAt` and `IngestedAt`;
* `Offset`, `Category`, `Payload` and `Parser` (`cls-1.0`);
* `Evidence`: a redacted excerpt of at most 240 characters, with ids, IPs and the service account removed;
* `Status`: `PARSED`, `PARTIAL` or `UNKNOWN`.

**Unknown is never a success:** unrecognized lines are preserved as `UNKNOWN` records with a redacted excerpt.

**Phase 2: envelopes from RPT, script, crash and restart.log are persisted** as `live_sync_records` (section 7.3), parser `cls-1.1`. Publishing them through an outbox and a change stream is phase 3.

## 6. Parser coverage matrix

| Category | ADM | RPT | Script | Crash | restart.log |
|---|---|---|---|---|---|
| Kill / hit / death / connect / disconnect / build | ✅ existing | – | – | – | – |
| Player-list snapshot | ✅ phase 1 | – | – | – | – |
| Boot started | ✅ header | ✅ `Current time` | ✅ header | ✅ header | pre-start check (evidence) |
| Shutdown countdown / complete | – | ✅ | – | – | – |
| Engine destroy | – | ✅ | ✅ `~DayZGame()` | – | – |
| Script exception | – | ✅ (Reason + frames) | ✅ | ✅ | – |
| Object-spawner error (missing mission file) | – | ✅ | ✅ | ✅ | – |
| Missing model / config warning | – | ✅ | – | – | – |
| Central Economy | – | ✅ component only | – | – | – |
| Network player removed | – | ✅ gamertag (id redacted) | – | – | – |
| Restart requested / host reboot / client admin | – | – | – | – | ✅ |
| Localization, A2S queries, resource leaks | – | ✅ counted (noise) | – | – | – |
| Model/geometry warnings, engine start-up, spawner config | – | ✅ phase 2 (`MODEL_GEOMETRY_WARNING`, `ENGINE_STARTUP`, `SPAWNER_CONFIG`) | – | – | – |
| Stop requested / automated restart | – | – | – | – | ✅ phase 2 (`STOP_REQUESTED`, `AUTOMATED_RESTART`) |
| Persistence errors, fatal crash (non-VM) | – | ⚠️ no real sample yet → UNKNOWN | – | ⚠️ | – |
| Anything else | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |

## 7. Continuous multi-source ingestion (phase 2, implemented)

### 7.1 Shape

```
                      ┌──────────── killfeed.Engine (ADM) ── unchanged, own goroutine ──────────────┐
Nitrado file server ──┤                                                                              │
 (one HTTP client,    └──────────── livesync.Supervisor (one per server worker) ─────────────────────┤
  no shared lock)          ├─ dir lister: lists the ADM dirs + ftproot root every 20 s → snapshots   │
                           ├─ RPT watcher      ─┐ each: own goroutine, own 60 s per-call timeout,    │
                           ├─ restart.log      ─┤ own failure backoff (probe × 2ⁿ, ≤ 10 min),       │
                           ├─ script log       ─┤ own durable checkpoint                             │
                           └─ crash log        ─┘                                                    │
                                   │ read → parse complete lines after checkpoint → ONE transaction: │
                                   ▼ INSERT live_sync_records … ON CONFLICT DO NOTHING + checkpoint  │
                           session evidence ─► server_adm_sessions.ended_at ◄── ADM engine selection ┘
```

* **Directories are discovered, never guessed.** The ADM files Nitrado lists give the log directories (both mounts); the `ftproot` root is listed because the ADM path shows the `/<root>/{noftp,ftproot}/` layout, and `restart.log` is read only when that listing returns it.
* **One current file per family**: the newest filename boot stamp across both mounts (`restart.log` is a single file). The same logical file on both mounts is one source; the copy with the larger listed size wins.
* **Independence.** Nobody waits on the lister: families read its last snapshot. A family keeps probing its durable current file directly even when listings fail. Tested: a hung RPT download does not delay restart.log (`TestStalledFamilyDoesNotBlockOtherFamilies`) or the real ADM engine sharing the same remote (`TestStalledRPTWatcherCannotBlockADM`).
* **Kill switch:** `LIVE_SYNC_WATCHERS=off` disables the watchers; the ADM engine is unaffected either way.

### 7.2 Stale listings and reads

A family reads when **either** the listing shows its file larger than the last read **or** its direct-read probe interval has passed (RPT and restart.log 30 s, script 60 s, crash 120 s). The probe finds new bytes even when Nitrado's listing size and modified time are stale (`TestWatcherFindsNewBytesDespiteStaleListing`), and the health view reports `listingBehindBytes` when a direct read found more than the listing shows.

Reads are **full downloads** parsed from the checkpoint: Nitrado ignores offset/count (finding A9), so no partial read is trusted. Real costs measured read-only on 2026-09-24: RPT 414–416 KB in 1.2–2.8 s, restart.log 157 KB in 1.3 s, script/crash logs < 2 KB in 0.7 s. Parsing a full 420 KB RPT takes ≈ 40 ms (`BenchmarkParseRPTRealSize`); steady-state reads parse only the bytes after the checkpoint.

**Request budget per server** (steady state): 3 listings / 20 s, plus per family one token + one download per probe interval ≈ 0.35 Nitrado requests/s on top of the ADM engine.

### 7.3 Durable checkpoints and records

* `live_sync_sources` (one row per family + canonical file): `checkpoint_offset` (end of the last complete, committed line), `backfill_until`, `read_size`, `active`, last read / growth time, record count.
* `live_sync_records`: every complete line, recognized **or `UNKNOWN`**, with `source_file` (canonical), `source_offset` (end of line), category, status, `delivery`, boot id, `source_local_time`, `source_utc` (only when stated), `visible_after`, `detected_at`, `persisted_at` (commit clock), bounded redacted `evidence`, JSON payload and parser version. `event_id` is deterministic, `UNIQUE (server_id, event_id)`.
* **Records and the checkpoint commit in one transaction.** A failed commit leaves both unchanged and the same bytes are read again; the deterministic id makes the retry a no-op (`TestCommitFailureRetriesWithoutLossOrDuplicates`, `TestLiveSyncCommitIsTransactionalAndIdempotent`).
* A trailing partial line is never consumed; a bare time-of-day line carries no observation and is not stored; a file smaller than its checkpoint (truncated or replaced) restarts from byte 0.
* **Rotation:** when a newer boot's file is listed, the old file gets one final drain read (retried on its own schedule if it fails) and is retired (`active = false`); the new file is attached from byte 0.
* **Resume:** a restarted Champion loads `live_sync_sources` and continues from each checkpoint (`TestResumeFromDurableCheckpoint`).
* **Retention:** high-volume categories (`UNKNOWN`, model/engine/localization/config/CE/query noise, headers) 3 days; everything else 30 days. A live RPT is ≈ 4,100 lines per boot, 87% of them start-up noise (measured on the 05:51:41 boot).

### 7.4 LIVE versus BACKFILL (never "new" just because it was read late)

A record is `BACKFILL` when its bytes existed before Champion first listed the file, or when its event time is known and more than 10 minutes older than the read. Otherwise it is `LIVE`. The event time is the source's own UTC (restart.log states its offset), or its server-local time converted with the UTC offset **learned from restart.log** (`live_sync_server_clock`); with neither, only the byte-position rule applies.

The same principle now applies to locations: every `locationDTO` carries `occurredAt` (DayZ's time in UTC, present only when the offset is known) and `timeBasis` (`SOURCE` / `INGESTION`). `ageSeconds` and `freshness` are measured from `occurredAt` when present, so a player-list line ingested an hour late is `STALE`, not `LIVE_RECENT`.

### 7.5 Freshness and diagnostics per family

`GET /api/admin/live-sync` (platform admin) returns, per running server:
* per family: `state` (`FRESH` = last successful read within 2× its probe interval; `LAGGING`; `FAILING` = three consecutive failures; `NO_SOURCE`), current canonical file, checkpoint, read and listed size, `listingBehindBytes`, last read / growth / failure time, safe error class, counts (reads, records, live, backfill, unknown, rotations) and a latency summary;
* the directory lister's last successful listing and error;
* the learned UTC offset and the recent session-end evidence;
* the recorded ADM session (file, start, `endedAt`, reason, evidence);
* stored-record statistics for the last 6 hours per family, and ADM latency from stored location rows.

`component=livesync` logs `source_attached`, `source_read`, `source_rotated`, `source_truncated`, `source_read_failed`, `adm_session_ended`, `server_clock_learned` and, every 5 minutes, one `source_health` line per family. No token, signed URL, physical path or raw line is logged or returned.

### 7.6 Kill/death heatmaps (finding A8)

Kills and deaths now store the ADM line's physical source (`source_file`, `source_offset`, `source_local_time`), exactly as the location rows written from the same line. The heatmap joins on `(server, source_file, source_offset, player)`, and filters time on `COALESCE(event_time, created_at)`. `event_time` stays NULL, because ADM lines carry no date. Legacy rows keep the old `observed_at = event_time` join. Kills and deaths recorded before this change remain unmatchable (they have no source identity), so kill and death heatmaps fill from new events onward.

### 7.7 ADM source authority (phase 2.1)

**Production findings (2026-09-24, Phase 2 acceptance).**
* A quiet boot's ADM is 124 bytes of header that never grows. Once the previous boot's ADM had been unchanged for `staleGiveUpAfter` (8 min), every poll took the stale branch and returned **before** the 10 s newer-file rescan. The new boot was found only by the 60 s full rediscovery, whose activity model ranks a never-growing file `STALE` after its first pass. It was accepted 10 min 43 s after the boot, and about 3 min after Nitrado first listed it.
* A `noftp` listing gap made discovery fall back to `ftproot` and select a **three-day-old** ADM (`newest_remote_modified`) for 33 s. It read that file from byte 0 through the live pipeline: header only that time, but a historical file with unrecorded kills would have been republished.

**Boot authority** (`internal/killfeed/boot_authority.go`). DayZ names each ADM after the boot's server-local start and writes the same time in its first line (`AdminLog started on … at …`). When the two agree (within 2 min), that stamp is the boot's identity, and it outranks listing activity:

| Rule | Mechanism |
|---|---|
| A verified **newer** boot is selected promptly, even quiet | a boot scan every `rescanInterval` (10 s) lists the directories discovery saw ADMs in (both mounts), verifies the newest newer stamp by reading its header, drains the old file's remaining complete lines and switches. It runs **before** the stale-source branch. Discovery applies the same rule (at startup it picks the newest verified boot) |
| An **older** boot is never selected | candidates older than the accepted boot are removed before any ranking or probing, and `selectLog` refuses one outright (`older_boot_refused`) |
| A listing gap keeps the accepted boot | when nothing admissible is listed, discovery re-selects the accepted boot (`accepted_boot_retained`) at its own checkpoint |
| No historical replay | an older file is never selected, so it is never read, checkpointed, or sent through a publisher |
| Aliases stay one source | `noftp`/`ftproot` copies of the accepted boot are the same boot, never "older" |

A newer-stamped file whose header contradicts its name, or cannot be read yet, is not promoted (`boot_candidate_unverified`); it is re-checked on the next scan.

**Session logging.** `adm_session_current` is logged only when the database accepts the session (`RecordADMSession` reports it). A refused older boot logs `session_rejected` with only the canonical file and reason.

**Worker and source health.**
* The worker heartbeat advances on every completed poll cycle; a quiet ADM is a working worker.
* The `nitrado` health component now reads the state snapshot's real key (`nitrado_connected`). It had read a non-existent `nitrado_authenticated`, which left Nitrado, the overall status and the bot's Discord presence permanently DEGRADED.
* A per-server `adm_source_<id>` component classifies the ADM source:

| State | Meaning | Health |
|---|---|---|
| `HEALTHY` | polls work and the ADM changed within 3 min | healthy |
| `QUIET` | polls work; the current boot's ADM has not changed | healthy |
| `SOURCE_LAGGING` | a newer boot has been listed > 30 s without being accepted, or players are online and the ADM has not advanced for 5 min | degraded |
| `TRANSPORT_ERROR` | 3+ consecutive Nitrado list/read failures | degraded |
| `WORKER_STALLED` | no poll cycle completed for 2 min | unhealthy |

`GET /api/admin/live-sync` adds `bootAuthority` (accepted boot and file, when accepted, the last newer boot listed and when, older candidates rejected, unverified candidates), `admSource` and `commandLineHeaderRecords`.

### 7.8 RPT command-line redaction (phase 2.1)

Parser `cls-1.2` stores the RPT's executable-path and command-line header lines (`== …`) as `LOG_HEADER` records with **no evidence** (`payload.redacted = command_line`). Parser `cls-1.1` had kept the game port and config file name, with the IP and service name already removed: one record per RPT. Migration `0052` deletes exactly those records: family RPT, category `LOG_HEADER`, parser `cls-1.1`, an `==` line containing `-port=`. Nothing else is touched. `commandLineHeaderRecords` in the diagnostics confirms none remain.

## 8. API contract changes (phase 1)

| Endpoint | Change |
|---|---|
| `GET …/admin/players` and `…/admin/players/online` | `currentLocation` is now **session-scoped** (absent = unknown). New: `currentLocationStatus` (`CURRENT` / `UNKNOWN`) and `lastKnownLocation` (the newest observation ever, possibly historical) |
| `GET …/admin/players/{id}/locations/current` | **new**: `{ "status": "CURRENT" \| "UNKNOWN", "location": locationDTO \| null }` (`PLAYER_LAST_LOCATION_VIEW`) |
| `GET …/admin/players/{id}/locations/latest` | unchanged meaning (last known); now marks `sessionScope` |
| every `locationDTO` | new `sessionScope` (`CURRENT_SESSION` / `HISTORICAL`) and `sourceLocalTime` (server-local, no zone); `eventType` may be `PLAYER_LIST` |
| every `locationDTO` (phase 2) | new `occurredAt` (DayZ time in UTC, only when the server offset is known from restart.log) and `timeBasis` (`SOURCE` / `INGESTION`); `ageSeconds`/`freshness` are measured from `occurredAt` when present |
| `…/locations/current` and directory `currentLocation` (phase 2) | also `UNKNOWN` once written evidence has ended the boot session (section 4) |
| `GET /api/admin/live-sync` (phase 2) | **new**, platform admin only: per-family freshness and diagnostics (section 7.5) |

**Not yet (phase 3):**
* the installation-scoped change stream or revision API;
* the transactional outbox for committed changes;
* reconnect and missed-event recovery.

Until then, the website polls the endpoints above.

## 9. Latency

Latency is **measured, never assumed**, separately for each stage:

| Stage | Definition | Where measured |
|---|---|---|
| DayZ event time | the time written in the line; UTC via restart.log's stated offset | `source_utc`, or `source_local_time` − learned offset |
| Nitrado visibility | somewhere in `(visible_after, detected_at]`: the previous read did not contain the bytes, this one did | `live_sync_records.visible_after` → `visibleWindow*` |
| Champion detection | the read that returned the bytes completed | `detected_at` (records), `observed_at` (ADM location rows) |
| Database persistence | the transaction that stored them committed | `persisted_at` (commit clock), `created_at` (location rows) |

Event → detection therefore **includes** Nitrado's own delay in exposing the bytes; the visibility window bounds how much of it the probe interval adds. The ADM engine is unchanged (metadata poll `NITRADO_POLL_INTERVAL`, 2 s on production, plus its stale-source probe). Its latency is measured from stored player-list and event rows once the offset is known.

**No sub-second claim is made:** Nitrado has no push mechanism, reads are full downloads, and its listing metadata has been observed lagging the file by 25+ minutes. The production numbers are reported per deployment from `GET /api/admin/live-sync`.

## 10. Recovery and diagnostics

**Crash or restart of the Champion process:**
* the ADM checkpoint resumes from the last durable offset;
* every live sync source resumes from its durable checkpoint;
* location rows and live sync records replay as source-identity no-ops;
* the current session is re-recorded on the next selection, and an end that evidence already proved is kept.

**Diagnostics:**
* `GET /api/admin/live-sync` (section 7.5);
* `Engine.PlayerListStats()` reports snapshots (complete/incomplete), entries, the last snapshot's id and player count, and presence added/removed;
* `component=livesync` logs `player_list_snapshot`, `adm_session_current`, `adm_session_header` and the watcher events in section 7.5;
* `component=presence` logs `player_list_reconciled`.

## 11. Remaining unsupported and pending work

1. **Phase 3 — fanout and change stream:**
   * a transactional outbox for committed records and location observations;
   * fanout to consumers without double credit or Discord replay;
   * an installation-scoped change stream with a conditional-polling fallback;
   * wiring `CorrelateBoots` into a persisted boot table fed by the stored records.
2. **Phase 2.1 (section 7.7):** ADM boot authority, health and the RPT command-line redaction are implemented. Nitrado's own listing delay for new files (≈ 7½ min observed) is outside Champion's control.
3. **Still open from phase 2's list:** DB presence (`player_server_activity`) reconciled from complete ADM snapshots; per-installation request budgets across many servers.
4. **Samples still needed:** persistence errors and non-VM fatal crashes (no real sample yet; they parse as UNKNOWN until one exists). `server.log` (ftproot only) is listed but its format has not been sampled, so it is not ingested.
5. **Kills and deaths recorded before phase 2** have no source identity and cannot appear on kill/death heatmaps.
6. **Automatic Shop spawning remains disabled** (docs/SHOP_DELIVERY_PHASE2B.md).
