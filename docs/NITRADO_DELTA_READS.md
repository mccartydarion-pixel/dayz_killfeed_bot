# Nitrado Delta ADM Reads (Champion Performance Phase 1.5)

This document is the design record for the partial-read ("delta") path added in Phase 1.5, which
follows on from `docs/PERFORMANCE.md` section 2 flagging full-file re-download as the largest
remaining performance lever after Phase 1. It covers: the current full-read behavior it sits
alongside, the three partial-read mechanisms investigated, capability detection, the checkpoint and
rotation/truncation rules a delta read must never violate, the fallback chain, the rollout flag, and
the read-only live-probe process an operator runs before enabling it against a real service.

**Core safety rule, unchanged from the task that produced this phase: zero missed ADM bytes, zero
duplicate events beyond the existing dedupe. Any uncertainty falls back to the existing full read.**
Correctness was prioritized over bandwidth savings throughout; every design choice below explains
which correctness property it protects.

## 1. Current full-read behavior (unchanged, still authoritative)

`internal/nitrado.Client.ReadLog` downloads the **entire** current ADM file on every poll where
`Tracker.ShouldReadAgain` detects a size or modified-time change (`internal/killfeed/tracker.go`).
Parsing was already incremental before this phase - `Tracker.DrainCompleteLinesWithOffsets` only
processes the byte range after `LastByteOffset` - so the only waste was in the network transfer, not
CPU. `ReadLog` and the full-read block in `Engine.pollSelected` (`internal/killfeed/engine.go`) are
**byte-for-byte unchanged by this phase** and remain the authoritative path: every delta mechanism
below falls back to it on any failure or uncertainty.

`Tracker.LastByteOffset` is the safe byte position immediately after the last fully-processed ADM
line. `Tracker.LineBuffer` holds bytes not yet resolved into a complete line (typically an unfinished
final line). On restart, `DurableCheckpoint.PendingPartialLine` seeds `LineBuffer` for display/
diagnostic purposes, but it is fully overwritten by the very next poll's own read regardless of
mode - both the full-read path (`content[readOffset:]`) and the delta path (`result.Data`, read
starting at `readOffset`) produce the identical byte sequence "everything from the last processed
offset onward," so a delta read never needs special-case handling for a pending partial line as long
as it always requests starting at `readOffset` (never at `readOffset + len(pending partial)`).

## 2. Partial-read mechanisms

Investigated and implemented in `internal/nitrado/partial_read.go`, in this preferred order (per the
task's research into Nitrado's own file-server client):

1. **SEEK** (primary): `GET /services/{serviceID}/gameservers/file_server/seek?file=&offset=&length=&mode=raw`.
   Same signed-token-envelope shape as the normal download endpoint - `fetchSignedURL` (shared with
   `ReadLog`) fetches the token, then the returned signed URL is fetched for raw bytes. Requires
   HTTP 200 from the signed URL fetch.
2. **OFFSET_QUERY** (secondary): reuses the **existing normal-download token endpoint** (the exact
   one `ReadLog` already calls), then appends `?offset=&count=` to the returned signed URL before
   fetching it. If the server ignores these parameters and returns the whole file (more bytes than
   requested), this is explicitly rejected as unsupported rather than risk feeding a wrongly-offset
   buffer into the parser.
   **Live finding (2026-09-24, Champions, read-only):** Nitrado ignores `offset`/`count` and returns
   the whole file with a plain `200` and no `Content-Range`, so a file SMALLER than the requested
   count used to pass the size check and be treated as starting at the offset. OFFSET_QUERY is now
   trusted only when a `Content-Range` states the requested offset - on Nitrado today it is therefore
   unsupported. `cmd/nitrado-delta-probe` previously probed a 124-byte ADM from byte 0 (a false
   SUPPORTED); it now always probes the file's second half. Keep `NITRADO_DELTA_READ_MODE` unset.
3. **RANGE** (third fallback only - never the starting point, per explicit instruction): a standard
   `Range: bytes=<offset>-<offset+length-1>` request against the normal download signed URL.
   - `206 Partial Content` is accepted **only** if `Content-Range` parses and its `start` matches the
     requested offset.
   - `200 OK` is **always** treated as unsupported - the server ignored `Range` and returned the
     whole file; a 200 response is never trusted to start at the requested offset.
   - `416 Range Not Satisfiable` is treated as unsupported (not proof of anything - just no new data
     yet at that offset), not a fatal error.

None of these three has been exercised against a live Nitrado service in this environment (no
credentials here, and this phase's own instructions forbid an automatic live run - see section 7).
They are built exactly to the shape described, defended by response validation, never by assumption.

Every returned partial result passes through `validatePartialData`, which is the one place body
length is checked before being trusted (a shorter-than-requested response is treated as a legitimate
EOF, never an error).

## 3. Capability detection and caching

`Capability` is one of `UNKNOWN`, `SEEK_SUPPORTED`, `OFFSET_QUERY_SUPPORTED`, `RANGE_SUPPORTED`,
`FULL_ONLY`. State is cached process-wide, keyed by Nitrado `serviceID` (`globalCapabilityCache` in
`partial_read.go`) - capability is a property of the remote service, not of any one `*Client`
instance, so every client talking to the same service shares the same proven/failed state.

- In `auto` mode, a previously-proven capability is tried first each call, with the other two
  mechanisms available as a same-poll fallback if that specific call fails transiently.
- Three consecutive failures for a mechanism demote it; three consecutive failures of the last-known
  capability demote the service to `FULL_ONLY`.
- A service marked `FULL_ONLY` is not re-probed on every 10s poll - a `probeCooldown` of 10 minutes
  gates re-probing, so a genuinely unsupported or misconfigured service does not get hammered with
  failing requests forever.

## 4. Rollout flag

`NITRADO_DELTA_READ_MODE` (parsed by `nitrado.ParseDeltaMode`): `off` (default), `auto`, `seek`,
`offset_query`, `range`. An empty or unrecognized value fails closed to `off` - a typo must never
silently enable an unverified transport change. `auto` tries seek -> offset_query -> range in
priority order (with the cached capability tried first once proven). The single-mechanism modes exist
for controlled testing/rollout only - they do not implicitly fall back to another mechanism, only to
the full read.

**This phase ships with the default at `off`.** `ReadLog` stays untouched and authoritative; enabling
delta mode for a service is a separate, deliberate rollout decision an operator makes after running
the live probe (section 7) against that service.

## 5. Engine integration

`internal/killfeed.DeltaSource` is an optional interface (mirrors the existing `StatSource` pattern):

```go
type DeltaSource interface {
    ReadDelta(ctx context.Context, serviceID, path string, fromOffset, targetSize int64, mode nitrado.DeltaMode) (*nitrado.PartialReadResult, bool)
}
```

`*nitrado.Client` implements it. `Engine.pollSelected` tries a delta read only when there is a prior
offset to resume from (`oldOffset > 0`) and genuine growth (`current.Size > oldOffset`) - a cold/first
read of a file always goes through the full, already-proven `ReadLog` path (the existing cold-start
protection, see section 6). `Engine.tryDeltaPoll` is a fully separate, additive method that
duplicates the full-read block's tracker/parse/checkpoint/report logic, operating on offset-relative
`tail []byte` (`= result.Data`) instead of the full-read path's absolute-offset-0 `content []byte`.
This duplication - rather than refactoring the two paths to share code - is deliberate: the proven
full-read path is never touched, so it carries zero risk of regression from this change.

`ReadDelta` (in `internal/nitrado`) reads `fromOffset..targetSize` in bounded, strictly sequential
chunks (`maxChunkBytes` = 256 KiB, `maxChunksPerPoll` = 64) - never concurrent, never reordered. Each
chunk's `StartOffset` is re-validated against the expected running offset before being appended, as
defense in depth on top of each mechanism's own validation. `targetSize` is the remote size captured
once at the start of the poll cycle; a chunk loop reads up to that captured size and stops even if the
remote file keeps growing during the read - new growth is picked up on the next poll, not chased
within this one. The whole `ReadDelta` call fails as a unit on any chunk failure (`ok=false`), never
returning a partial/best-effort result - the caller's only correct response to `ok=false` is to fall
straight through to the unchanged full-read path in the same poll.

## 6. Checkpoint, rotation and truncation rules

- **Checkpoint restore**: `oldOffset` for a delta request is always `Tracker.LastByteOffset` as of the
  start of that poll - never an offset adjusted for a pending partial line. Restart resume is
  regression-tested end to end (`TestEngineDeltaCheckpointRestartResumesSafely`,
  `internal/killfeed/engine_delta_test.go`): a checkpoint persisted by one engine instance is loaded
  by a second, simulating a process restart, and the second instance fetches only the bytes appended
  after the checkpoint's offset - no replay, no skip, no duplicate.
- **Cold start**: unchanged. A brand-new engine with no existing checkpoint still seeds at the file
  tail on first selection (`startAtTail`/`cold_start_baseline_required` in `engine.go`) exactly as
  before this phase - delta mode never bypasses this protection, since delta reads only ever trigger
  when `oldOffset > 0`.
- **Truncation**: `current.Size < Tracker.LastByteOffset` is detected and handled by the existing
  `Tracker.ResetForRotation` logic **before** the delta-read branch is reached (`pollSelected`
  resets `oldOffset` to 0 first) - a truncated file is never fed a stale, now-invalid offset into a
  seek/offset-count/Range request. `TestEngineDeltaTruncationNeverSendsInvalidDeltaRequest` covers
  this.
- **Rotation**: a rotated file (new path) is detected by the existing metadata-stat-404 -> rediscovery
  flow before any delta read is attempted against it; an old file's byte offset is never carried into
  a new file's delta request. `TestEngineDeltaRotationNeverCarriesOldOffsetIntoNewFile` covers this,
  asserting no delta call anywhere requested the old file's offset against the new path.

## 7. Live probe tool (read-only, not run in this environment)

`cmd/nitrado-delta-probe` is a read-only diagnostic executable using the existing environment's
Nitrado credentials (`NITRADO_API_TOKEN`, `NITRADO_SERVICE_ID` or equivalent - see the tool's own
`-h`). It never prints credentials, performs GET/download requests only (no writes, no server
restarts, no settings changes), and reports:

- Whether SEEK, OFFSET_QUERY and RANGE are each supported for the configured service's live ADM file.
- A byte-for-byte validation: full-download a segment of the live file, partial-read the same segment
  via each supported mechanism, and assert `partialBytes == fullBytes[offset:offset+len(partialBytes)]`
  exactly - never relying on HTTP status alone as proof of correctness.

**This tool was not executed against a real Nitrado service in this session** - no live credentials
were available in this sandboxed environment, and the task's own instructions require explicit user
authorization before running it against a customer's real service. An operator should run it (`go run
./cmd/nitrado-delta-probe -service <id>`) against a real, non-production-critical service before
changing `NITRADO_DELTA_READ_MODE` away from `off` for that service, and treat its report as the
gating evidence for that rollout decision.

## 8. Test coverage summary

- `internal/nitrado/partial_read_test.go`: httptest-based coverage of all three mechanisms (206
  correct, 200 ignores Range, 416, malformed/mismatched Content-Range, short body as valid EOF,
  network failure), the fallback chain, capability caching (no re-probing once a capability is
  proven) and the circuit breaker, `ParseDeltaMode`/`parseContentRange` parsing, and `ReadDelta`'s
  sequential chunking, target-size stop, and all-or-nothing chunk failure behavior.
- `internal/killfeed/engine_delta_test.go`: engine-level correctness, including the byte-for-byte
  oracle (`TestEngineDeltaByteForByteOracle` - a `DeltaModeOff` engine and a `DeltaModeAuto` engine
  driven through the identical synthetic growth sequence must produce identical parsed events, event
  order, and final tracked offset) and a 1000-poll load comparison
  (`TestEngineDeltaLoadComparisonBytesTransferred`) with real, measured (not fabricated) results - see
  section 9.
- Both packages pass `go test -race`.

## 9. Measured bandwidth reduction

`TestEngineDeltaLoadComparisonBytesTransferred` simulates 1000 polls, each appending one 8-byte line,
tracking cumulative bytes actually transferred under full-download-every-changed-poll behavior versus
`Engine.DeltaStats().BytesReceived` under delta mode, with identical final parsed-event counts in both
modes. Measured result from this environment:

```
full=4005001 bytes, delta=7993 bytes, reduction=99.8%, events full=1000 delta=1000
```

This is a synthetic worst-case-for-full-download scenario (every poll has exactly one new line), not a
production traffic estimate - it demonstrates the mechanism is correct and the saving is real, not a
production bandwidth forecast.

## 10. Deliberately deferred (in scope per the task, not silently omitted)

- **Stale-probe and alt-source paths** (`Engine.probeStaleSource`, `adm_alt_probe.go`,
  `adm_source_scan.go`) still use the existing full-read logic unchanged. These are secondary,
  lower-frequency paths; the task explicitly allows keeping a full read "if still required for a
  particular decision." Wiring delta reads into them is a follow-up, not attempted this phase to keep
  the change surface (and risk) bounded to the primary poll path, which carries the overwhelming
  majority of ADM download volume.
- **Admin performance snapshot wiring**: `Engine.DeltaStats()` (cumulative bytes received and full-
  read bytes avoided) exists but is not yet surfaced through `GET /api/admin/health`'s `performance`
  object (`docs/PERFORMANCE.md` section 11) - see that document's changelog for whether a later phase
  wired it in.
