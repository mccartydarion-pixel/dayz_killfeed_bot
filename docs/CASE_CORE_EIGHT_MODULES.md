# C.A.S.E. core — eight client modules

Owner-selected replacement scope for the client-facing C.A.S.E. detector list. The protected detector-readiness read displays these eight names in this order. Every module is **BLOCKED** and the read returns no findings, cases, scores or enforcement. A catalog entry is a product definition, not proof that PlayStation or Xbox exposes its required data.

| Module | Intended observation | Current evidence gap / exit study |
| --- | --- | --- |
| Base Boost Detection | Unauthorized building inside or around another player's base | A build-action parser and staff feed exist, but those lines are not retained as C.A.S.E. evidence and real ADM wording has not been verified. Need verified source-addressed actions, ownership, geometry and guest/faction/raid permissions. Connect/hit/kill positions alone cannot establish building. |
| Skywalk Detection | Unexplained movement above valid terrain/structures | Need continuous server-authoritative positions and map/structure geometry, plus vehicle, fall and desync counterexamples. |
| Dupe Detection | Duplicated item creation | Need authoritative item identity and inventory transaction provenance; a reconnect or kill is insufficient. |
| PC Detection (Xbox) | Unapproved PC client participation on an Xbox server | Need trusted platform attestation available to the server. An Xbox account name, behavior or network address does not prove client platform. PlayStation installations should show this as unavailable. |
| No-Clip Detection | Unexplained traversal through collision | Need continuous server-authoritative positions, collision geometry and legitimate transition/lag exclusions. |
| Undermap Detection | Player occupancy below valid map geometry | Need continuous positions and validated terrain/building elevations, with spawn and falling exclusions. |
| Suspicious Logins | Staff-reviewable unusual account/session access | Connect/disconnect lines alone do not establish identity compromise. Need trusted session/device/network context, a baseline and privacy/false-positive review. This is a security review alert, not a cheating verdict. |
| Teleport Alerts | Unexplained location jump | Need trusted game-event elapsed time, continuous position/source coverage and respawn/vehicle/admin/map exceptions. An ADM event pair is not enough. |

The old `CASE-MOV-001` movement-timing prerequisite is an internal, permanently blocked diagnostic used by historical fixture/ledger checks; it is retired from the **client-facing** list. Existing records remain readable, and no detector IDs or evidence histories are rewritten. There is no automatic conversion of an old fixture into a new module.

## Advanced Edition within the same eight

The catalog lists the owner's proposed advanced capabilities per module. Base Boost adds custom zones, faction permissions, repeated intrusion review and private owner notifications. Skywalk adds terrain comparison, structure exclusions, repeated positions and snapshots. Dupe adds restart/reconnect correlation, item evidence when available and incident timelines. PC Detection (Xbox) requires verified platform evidence before any platform allegation. No-Clip adds map-aware geometry, building exclusions and movement reconstruction. Undermap adds underground exclusions, terrain elevation and repeated confirmation. Suspicious Logins adds session history, reconnect patterns, restart awareness and related incidents. Teleport adds trusted distance/elapsed-time analysis, vehicle/respawn exceptions and historical movement comparison. These are requirements, not implemented live detectors.

All eight share the intended **Observe → Correlate → Validate → Notify** investigation flow. Observe never accuses. Correlate joins exact-installation events and legitimate explanations. Validate independently checks current source, poller lag, required fields, provenance, exclusions and duplicate evidence. Notify is a separate private, default-off capability requiring an authenticated case and delivery receipt. A neutral staff review candidate never establishes a violation.

The offline `AssessInvestigation` policy models the three owner modes: Relaxed, Balanced (default) and Strict. It requires an approved per-module threshold profile and a shared minimum evidence floor; no numeric production threshold is supplied by the catalog. All modes require the same source, provenance, module-validation and exception standards. No mode can recover missing telemetry or enable sending; threshold values require independent false-positive validation and owner controls before use.

Detector health must be read from the protected, exact-installation source/continuity aggregate rather than inferred from runtime log lines. A stale source, behind poller or missing required fields yields `DEGRADED / SUSPENDED` with the affected module, reason and action to suspend conclusions. The offline policy implements this suspension; the live health panel and case sender are not wired to it. A preview mode selection does not change server configuration.

The offline `AssessDetectorHealth` classifier models `ACTIVE`, `DISABLED`, `DEGRADED`, `INSUFFICIENT_EVIDENCE`, `UNSUPPORTED` and `ERROR`. Since every current catalog module is `BLOCKED`, it cannot report `ACTIVE` even when fixture inputs claim validation. Timestamps, freshness limits and poller delay are explicit inputs for future protected readback. It has no production caller or dashboard binding.

The protected `/anti-cheat/integrity` response also projects `pipelineHealth` from its exact-installation worker snapshot and retained evidence readback. `DEGRADED` identifies an absent worker, stalled/lagging source, transport error or stale poll; `INSUFFICIENT_EVIDENCE` identifies an unconfigured collector, unaccepted source, quiet current source with no retained events, or historical-only evidence. `CURRENT_SOURCE_OBSERVATIONS` requires a matching selected/accepted source and a retained event with source address after the observed source change. It still says `conclusionsSuspended: true`: the reads are bounded and non-atomic, and this state does not validate any detector or prove ADM completeness. No runtime detector consumes this result.

The existing protected `/anti-cheat/sessions` reconstruction now includes a bounded `loginObservations` summary for Suspicious Logins. It counts repeated connect lines without a recorded disconnect **within the same ADM source** and links their retained evidence IDs. It discloses pagination truncation and leaves rapid reconnect timing and restart context unverified. A normal recorded disconnect/reconnect or a source boundary is not classified as suspicious. This is a staff-only source-order observation, not a login detector, case or notification. It reuses the existing exact server/player query, rate limit, authorization and audit rather than introducing another poller.

## Offline detection engine

The per-module Observe → Correlate → Validate logic, telemetry contracts, evidence record, incident key and staff alert layout are in [CASE_CORE8_DETECTION_ENGINE.md](CASE_CORE8_DETECTION_ENGINE.md). The engine has no production caller. It cannot activate a module or notify while the catalog modes above remain `BLOCKED`/`UNSUPPORTED`.

## Build order

1. Verify current-source retained evidence and protected health/continuity.
2. Establish a trusted data contract and legitimate counterexamples for each module. Unsupported modules remain visibly unavailable.
3. Validate one module in isolated real shadow evaluation with false-positive review. Then build authenticated case review, private delivery and owner controls in that order.

The read-only catalog does not change polling, killfeed, economy, login collection, detection, case admission, Discord alerts or sanctions. Activation and production merge remain separately controlled.
