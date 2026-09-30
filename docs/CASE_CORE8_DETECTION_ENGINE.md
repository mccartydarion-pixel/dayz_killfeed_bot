# C.A.S.E. Core Eight — detection engine

Status: **offline engine implemented and unit-tested. No production caller. No detector is active.** All eight catalog entries stay `BLOCKED` (Xbox PC: `UNSUPPORTED`), so the engine cannot produce `canNotify: true`, and `enforcement` is always `DISABLED`.

This is the implementation for Phases 2–6 of the [Core Eight roadmap](CASE_CORE_ADVANCED_ROADMAP.md). It adds no ninth module and does not change polling, killfeed, economy, billing, verification, `/setup` or Discord delivery.

## Code map

| File | Role |
| --- | --- |
| `internal/caseintel/core8_telemetry.go` | Per-module telemetry contract (`RequiredTelemetry`) and `AssessCore8Health`, which feeds the existing `AssessDetectorHealth` state machine |
| `internal/caseintel/core8_engine.go` | `Finding` evidence record, `IncidentKey`, `ConfirmByStaff`, the shared VALIDATE stage (`finalize`) and staging `DefaultCore8Params` |
| `internal/caseintel/core8_movement.go` | Teleport, Skywalk, Undermap, No-Clip over `PositionSample` and a per-installation `MapModel` |
| `internal/caseintel/core8_detectors.go` | Base Boost, Dupe, PC Detection (Xbox), Suspicious Logins |
| `internal/discord/case_core8_alert.go` | `BuildCASECore8StaffEmbed`, the staff alert layout. It is not connected to any sender |

## Pipeline (same for all eight)

1. **Gate.** `AssessCore8Health` checks every feed the module needs. `DISABLED`, `UNSUPPORTED` and `ERROR` stop the evaluation. A stale or lagging feed returns `SUSPENDED` (health `DEGRADED`). A missing or unverified feed returns `INSUFFICIENT_EVIDENCE`. Either way there are no findings.
2. **Observe.** Each input sample must have an evidence ID, the exact player, a *trusted* event time (`TimeTrusted`), and an observation time no more than `MaxSampleLag` after it. Anything else is counted under `exclusions` and dropped. Ingestion time is never used as event time.
3. **Correlate.** Detector-specific rules plus legitimate-explanation exclusions. Every exclusion is counted by reason code.
4. **Validate.** Candidates are deduplicated by `IncidentKey`, and one evidence row supports at most one observation. The owner's sensitivity then sets how many independent observations are needed (through the existing `AssessInvestigation` thresholds). If the count is met, the findings become `SUSPICIOUS` / `PENDING_STAFF_REVIEW` and the result is `REVIEW_CANDIDATE`. Otherwise they stay `OBSERVED`.
5. **Notify.** This is not in the engine. `canNotify` needs a `REVIEW_CANDIDATE`, a catalog mode of `VALIDATED_SHADOW` and health `ACTIVE`. None of these is currently possible. Delivery must reuse `casealert.Evaluate` → `caseoutbox`, keyed by `IncidentKey`.

### Evidence tiers

- `OBSERVED`: activity was recorded.
- `SUSPICIOUS`: repeated, validated observations met the owner's threshold.
- `STAFF_CONFIRMED`: can only be set by `ConfirmByStaff(finding, reviewerID)`, and only from `SUSPICIOUS`. No detector can emit it, and it never triggers enforcement.

### Idempotency

`IncidentKey` = SHA-256 over scope (guild, installation, server), detector ID and version, player, and the sorted evidence IDs. Replayed, shuffled or duplicated inputs produce the same keys, so polling retries, reconnects, service restarts and journal recovery cannot create a second incident. Different installations always produce different keys. The Discord dedup relies on the existing outbox key uniqueness.

### Sensitivity

Relaxed ≥ Balanced ≥ Strict ≥ minimum evidence. Sensitivity changes **only** the number of independent observations needed. It never relaxes sample validity, exclusions or telemetry health. Without approved thresholds the result is `OBSERVATION_ONLY` with `THRESHOLD_NOT_VALIDATED`. Physical limits (`Core8Params`) are separate from sensitivity. `DefaultCore8Params` holds staging values that have not been validated against real false-positive data.

## Detectors

| Module | Engine logic | Built-in exclusions | Required telemetry |
| --- | --- | --- | --- |
| Base Boost | A build action inside a verified registered base radius (clamped to min/max) by a player who is not the owner, an authorized player, or in an authorized faction **at event time**. Watchtower and elevated-structure placements are labelled. `AffectedBaseID` is kept for future owner notices | Owner, guests, owner/authorized faction, unknown faction membership, action before registration, unverified base | ADM, build actions, base registry |
| Skywalk | A run of ≥ N consecutive samples in one life, spanning ≥ duration, at ≥ offset above verified terrain | `STRUCTURE` zones (towers, rooftops, custom builds), vehicles or unknown vehicle state, single samples, off-model points, respawn | ADM, positions, terrain, structures, vehicle state |
| Dupe | The same persistent item ID acquired twice with no release, near a reconnect or verified restart | No item evidence means `ITEM_EVIDENCE_UNAVAILABLE` and no findings, however many reconnects. Also: unverified provenance, no reconnect/restart context | ADM, sessions, restarts, item transactions |
| PC Detection (Xbox) | Signature-verified, trusted attestation reporting PC on an Xbox installation | The input type has no name, movement or controller fields. Non-Xbox installation, untrusted or missing attestation → `UNSUPPORTED` | Platform attestation |
| No-Clip | Consecutive samples ≤ max interval apart whose path crosses a verified solid by ≥ penetration depth within its altitude band | `ENTRANCE` zones, sparse samples (never interpolated), vehicles, respawn, missing altitude, paths over the solid | ADM, positions, structure geometry |
| Undermap | A run of samples ≥ depth below verified terrain | `UNDERGROUND` zones (bunkers, tunnels), `STRUCTURE` zones, shallow terrain noise, single samples | ADM, positions, terrain, structures |
| Suspicious Logins | A burst of ≥ N rapid disconnect→connect or connect→connect pairs within a window. This is a security review item, not a cheating verdict. `RelatedIncidentKeys` are carried forward | Reconnects within a verified restart ± grace, ordinary low-count reconnects | ADM, sessions, restarts |
| Teleport | Consecutive trusted samples ≥ min distance apart at an implied speed above the on-foot limit | Gaps over the max gap (missing movement is not evidence), respawn, restart, vehicle or unknown vehicle state, `TELEPORT_EXEMPT` zones, untrusted or stale samples | ADM, positions, vehicle state, restarts |

## Tests (actual)

Run with `go test ./internal/caseintel/ ./internal/discord/`. This is synthetic fixture coverage only. It does not certify live detection.

- Legitimate gameplay: walking, one normal reconnect, Xbox attestation, item release and re-acquire, climbing over a wall, shallow terrain noise, builds outside the radius.
- Suspicious patterns: one per detector.
- Missing, unverified or unsupported telemetry, stale feeds, polling lag, processing errors, invalid scope.
- Stale samples, untrusted clocks, future events, player mismatch.
- Repeated events and sensitivity (Strict/Balanced/Relaxed); Strict cannot bypass evidence; unapproved thresholds.
- Custom-map exclusions (structures, bunkers, entrances, exempt zones) and an unverified custom map.
- Server restart boundaries (Teleport, Logins, Dupe context).
- Polling recovery: replayed, shuffled and duplicated input gives identical incident keys.
- Cross-installation isolation of incident keys.
- Every catalog module stays non-`ACTIVE` and `canNotify` stays false.
- Staff alert layout: required fields, mention and code-fence neutralisation, no verdict language, unvalidated findings are not rendered.

Not tested here: Discord delivery failures (owned by the existing `caseoutbox` tests; this engine sends nothing), real console telemetry, and a real staging installation.

## Console telemetry limitations (blocking release)

These are why every module stays blocked. They are properties of the data source, not of the engine.

1. **No trusted event time.** ADM clocks are `HH:MM:SS` with no date and no sub-second resolution. So `TimeTrusted` cannot be true for ADM-derived samples, and every time-dependent rule (Teleport, No-Clip, Skywalk/Undermap duration, Login bursts, Dupe windows) has no qualifying input. A trusted clock needs, for example, a verified file-start anchor plus monotonic offsets, validated in staging.
2. **Positions are event-triggered or periodic.** Hit, kill, connect and build lines, plus optional `PlayerList` snapshots (typically minutes apart), are far too sparse for No-Clip. They may support Skywalk, Undermap or Teleport once item 1 is solved and the snapshot cadence is verified. Retaining `PlayerList` positions as C.A.S.E. evidence has not been verified.
3. **No vehicle state.** Teleport and Skywalk exclude samples whose vehicle state is unknown, which is currently all of them.
4. **No terrain or collision model.** `internal/dayzmap` holds verified bounds only. Elevation grids and collision volumes per map revision, plus owner-registered custom structures, still need to be sourced and verified.
5. **No item identity or inventory transactions** in ADM. Dupe stays `ITEM_EVIDENCE_UNAVAILABLE`.
6. **No platform attestation** on console. PC Detection (Xbox) stays `UNSUPPORTED`.
7. **Restart schedule.** Needs a verified source (host API or owner schedule). An ADM gap is not a restart.
8. **Base registry.** Draft [#157](https://github.com/mccartydarion-pixel/dayz_killfeed_bot/pull/157) (migration 0070) is unmerged. A real ADM build line sample is still unverified.

## Dashboard and marketplace

- The protected `/anti-cheat/detector-readiness` read now includes `requiredTelemetry` for each module's health entry: one `{kind, status, reason}` per feed in `RequiredTelemetry`. Status is one of `CURRENT`, `STALE`, `UNAVAILABLE`, `NOT_CONFIGURED`, `UNVERIFIED` or `UNSUPPORTED`. Only the ADM feed can be `CURRENT`, and only from the live worker snapshot. The field is additive: module `state` and `reasons` are unchanged. The dashboard should list these feeds so an owner can see exactly what each detector is missing. A module must never show as operational unless its state is `ACTIVE`, which the owner toggle alone cannot achieve.
- Owner sensitivity storage stays in draft [#170](https://github.com/mccartydarion-pixel/dayz_killfeed_bot/pull/170). The engine consumes it as `EvalContext.Mode` / `Thresholds`.
- Security Marketplace products (#156) are not detectors. Base Boost findings carry `AffectedBaseID`, so a future purchased owner notice can be built on the separate player-facing path. The engine never sends player-facing messages.

## Release gate per detector

A module can move from `BLOCKED` to `VALIDATED_SHADOW` only after all of the following:

1. Its required telemetry is available, verified and fresh through protected exact-installation readback in isolated staging.
2. Real legitimate-play counterexamples have been reviewed for false positives.
3. Thresholds are approved.
4. The owner has separately authorized it.

Until then the engine runs only in tests.
