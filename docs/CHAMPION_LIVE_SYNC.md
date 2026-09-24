# Champion Live Sync (C.L.S.)

Champion's goal: detect changed Nitrado log content promptly, process every supported observation, persist it once, and notify the systems that depend on it. It must never claim an in-game change that DayZ has not written evidence of.

This document records the audit of the existing pipeline, the **phase 1** changes delivered in this branch, and the remaining phases. **Implemented** and **not yet implemented** are marked explicitly throughout.

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
| A4 | Only ADM is ingested. RPT, `script_*.log`, `crash_*.log` and `restart.log` are never read | Startup/shutdown, script errors and restart causes are invisible | **Parsers + fixtures delivered; live ingestion not yet** |
| A5 | One selected ADM stands for the whole server's log state | A slow or failing source cannot be isolated | Not yet (phase 2) |
| A6 | No change notification: website data is polled request/response | The website cannot learn about committed changes | Not yet (phase 3) |
| A8 | `kills.event_time` / `deaths.event_time` come from `Event.Timestamp` (never set, see A2), so they are NULL; the kill/death heatmaps join `player_location_events` on `observed_at = event_time` | Kill/death heatmaps can never match a row | **Not fixed here**: needs source identity on kills/deaths (phase 2) |
| A7 | The stale-source loop (`selected_stale` → same file re-selected) is bounded to one rediscovery per `staleProbeInterval`, with a direct-read probe first | Behaviour is correct but coarse; source-level freshness is not exposed per family | Documented; per-source freshness is phase 2 |

## 2. Source inventory (Champions, service 19806451, read-only, 2026-09-24)

These are actual files exposed by the connected PlayStation service. No filename below is invented. Nitrado API event records (tasks, restart requests via the API) are **not** files, and are listed separately.

| Family | Real name pattern | Canonical path | Count | Boot identity | Parser | Live ingestion |
|---|---|---|---|---|---|---|
| ADM | `DayZServer_PS4_x64_YYYY-MM-DD_HH-MM-SS.ADM` | `dayzps/config/…` (noftp + ftproot alias) | 65 | filename + `AdminLog started on …` header | `killfeed.ADMParser` (+ player lists, phase 1) | **yes** (existing engine) |
| RPT | `DayZServer_PS4_x64_YYYY-MM-DD_HH-MM-SS.RPT` | `dayzps/config/…` | 66 | `Current time:` + `Version` header | `livesync.ParseRPT` | no (phase 2) |
| Script log | `script_YYYY-MM-DD_HH-MM-SS.log` | `dayzps/config/…` | 66 | `Log … started at DD.MM. HH:MM:SS` | `livesync.ParseScriptLog` | no (phase 2) |
| Crash log | `crash_YYYY-MM-DD_HH-MM-SS.log` | `dayzps/config/…` | 2 | `Log … started at` + `USMI264, DD.MM YYYY HH:MM:SS` | `livesync.ParseScriptLog` | no (phase 2) |
| Restart log | `restart.log` | `ftproot/restart.log` → `restart.log` | 1 (157 KB) | pre-start check lines | `livesync.ParseRestartLog` | no (phase 2) |
| Ban list / whitelist | `ban.txt`, `whitelist.txt` | `dayzps/…` | 1 each | – | classified only | no |
| Nitrado tasks | `GET /services/:id/tasks` (API record, not a file) | – | 0 tasks | – | – | no |

**Per-source tracking fields.** Size, modified time and readable byte count are available from the file-server listing and download. Per-source checkpoints, last successful read and rotation history exist today **only for ADM** (the durable ADM checkpoint). The phase 2 watcher adds them for every family.

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

**Unknown is never a success:** unrecognized lines are preserved as `UNKNOWN` records with a redacted excerpt. Persisting envelopes, and publishing them through an outbox, is phase 3.

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
| Persistence errors, fatal crash (non-VM) | – | ⚠️ no real sample yet → UNKNOWN | – | ⚠️ | – |
| Anything else | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN | UNKNOWN |

## 7. Checkpoints, rotation and stale sources

**Implemented (existing engine, unchanged):**
* durable ADM checkpoint (file, offset, pending partial line);
* no checkpoint advance past a failed durable persistence;
* truncation and rotation handling;
* ftproot/noftp alias checkpoint reuse;
* delta reads with full-read fallback;
* the direct-read stale probe before rediscovery;
* at most one full rediscovery per `staleProbeInterval` while stuck.

**Phase 1 addition.** Replay safety for location observations moved from the in-memory 90 s deduplicator and an ingestion-time key to the physical source identity. Replaying the same bytes (same file and offset) is an exact database no-op, while two distinct stationary observations are both kept.

**The livesync parsers** return the consumed byte count and never consume a trailing partial line. A delta read seeded with the boot time produces the same records and absolute offsets as one full read (tested).

**Not yet (phase 2):** per-source checkpoints and watchers for RPT, script, crash and restart logs, each with independent failure counts, backoff, request budgets and priority. The priority order is: active ADM, current-session data, critical RPT errors, boot records, then historical diagnostics.

## 8. API contract changes (phase 1)

| Endpoint | Change |
|---|---|
| `GET …/admin/players` and `…/admin/players/online` | `currentLocation` is now **session-scoped** (absent = unknown). New: `currentLocationStatus` (`CURRENT` / `UNKNOWN`) and `lastKnownLocation` (the newest observation ever, possibly historical) |
| `GET …/admin/players/{id}/locations/current` | **new**: `{ "status": "CURRENT" \| "UNKNOWN", "location": locationDTO \| null }` (`PLAYER_LAST_LOCATION_VIEW`) |
| `GET …/admin/players/{id}/locations/latest` | unchanged meaning (last known); now marks `sessionScope` |
| every `locationDTO` | new `sessionScope` (`CURRENT_SESSION` / `HISTORICAL`) and `sourceLocalTime` (server-local, no zone); `eventType` may be `PLAYER_LIST` |

**Not yet (phase 3):**
* the installation-scoped change stream or revision API;
* the transactional outbox for committed changes;
* reconnect and missed-event recovery.

Until then, the website polls the endpoints above.

## 9. Latency

The engine was not rebuilt, so ADM detection latency is unchanged: metadata is polled every 10 s (`NITRADO_POLL_INTERVAL`), a direct probe runs after 2 min without metadata change, and rediscovery is bounded.

**Player-list observations** are available within one poll of Nitrado exposing the bytes. That is a gain from "never" to about one poll cycle plus the location batch interval (≤ 500 ms). **No sub-second claim is made:** Nitrado has no push mechanism, and its listing metadata was observed lagging the file by 25+ minutes (`probeStaleSource` exists for exactly that).

## 10. Recovery and diagnostics

**Crash or restart of the Champion process:**
* the ADM checkpoint resumes from the last durable offset;
* location rows replay as source-identity no-ops;
* the current session is re-recorded on the next selection.

**Diagnostics:**
* `Engine.PlayerListStats()` reports snapshots (complete/incomplete), entries, the last snapshot's id and player count, and presence added/removed;
* `component=livesync` logs `player_list_snapshot`, `adm_session_current` and `adm_session_header`;
* `component=presence` logs `player_list_reconciled`.

## 11. Remaining unsupported and pending work

1. **Phase 2 — source watchers.** Per-source watchers for RPT, script, crash and restart logs, with:
   * durable per-source checkpoints and per-source freshness health;
   * request budgets and priority scheduling;
   * RPT and restart evidence feeding `CorrelateBoots`;
   * DB presence (`player_server_activity`) reconciled from complete snapshots.
2. **Phase 3 — persistence and fanout:**
   * envelope persistence with a transactional outbox;
   * fanout to consumers without double credit or Discord replay;
   * an installation-scoped change stream with a conditional-polling fallback.
3. **Samples still needed:** persistence errors and non-VM fatal crashes (no real sample yet; they parse as UNKNOWN until one exists).
4. **Automatic Shop spawning remains disabled** (docs/SHOP_DELIVERY_PHASE2B.md).
