# C.A.S.E. core — console signal feasibility and first finding admission

This is the scope-frozen feasibility review under issue #124. **No detector is authorized or enabled by this document.** Data visibility in the existing PlayStation ADM collector is not the same as validated cheat detection.

## Present source contract (verified from main source)

The existing C.A.S.E. evidence collector records only the selected ADM event types in `internal/killfeed/evidence.go`: connect, disconnect, respawn, death, kill, hit, unconscious, regained consciousness, and suicide. A retained record has the server/guild scope, canonical ADM source identity, byte end offset, SHA-256 of the raw line, date-less ADM clock, optional subject/actor/target coordinates and event-specific values. The collector runs inside the existing poller and waits for evidence storage acknowledgement before advancing its checkpoint. **No additional ADM polling is authorized.**

The production bounded audit has shown a historical 200-record page from six sources and a quiet new 124-byte accepted source. The 200-row page is truncated and is not full source coverage; current-source matching retained evidence still needs acceptance in #114.

| Already discussed signal | Present observable inputs | Current detector decision | Evidence needed before future admission |
| --- | --- | --- | --- |
| Movement, teleport, speed, skywalk and underground | Intermittent event positions and date-less second-resolution ADM clocks | BLOCKED | Validated gameplay elapsed time, continuous role-specific positions, source continuity and tested respawn/vehicle/admin/map exceptions; do not infer speed or altitude exploit from event positions alone. |
| Suspicious hit or kill sequence | Filtered hit/kill lines with optional weapon, zone, damage, player roles, source address | OBSERVATION ONLY | Confirm actual console event semantics and quality on current source, valid event sequence with independent non-cheat explanations, reviewed rules, legitimate edge cases, and measured false-positive behavior. A high hit count is not cheating evidence. |
| Repeated connect/respawn or boundary sequence | Selected connect/disconnect/death/respawn records | OBSERVATION ONLY | Validate log ordering, bounded source windows, role identity and missing events. No accusation from a gap, same-second pair, repeated connect or source change. |
| Base boosting/intrusion | Existing operational zone/radar events are separate from player cheat evidence | NOT A CHEAT DETECTOR | Establish reliable zone geometry, map object semantics, false-positive exclusions and a separately authorized service definition. Keep operational ADMIN_ALERTS separate from C.A.S.E. reviewed findings. |
| Duplication, inventory injection or economy exploits | No inventory transaction/authoritative item-creation telemetry established in this ADM evidence allowlist | UNSUPPORTED | Independent verified authoritative inventory/action source, not inference from kills, death, disconnect or inventory outcome. |
| Controller inputs, aim automation or recoil scripts | No controller input/aim-vector/recoil telemetry | UNSUPPORTED | An independently available trustworthy input/aim data source. Never label a player based on hit rate or aggregate headshot count. |

## Release selection gate

**No first cheating detector has passed eligibility yet.** The nearest existing domain for further study is hit/kill sequence *source-quality analysis*, because those event types are actually retained; that does not imply an anti-cheat verdict or a commitment to a detector. Perform an isolated synthetic audit first; then separately approved staff-only real-source observation. Record definition, provenance, exclusion model, rate of unverified evidence, counterexamples and false-positive review *before* considering shadow evaluation.

The current registry contains only CASE-MOV-001 with permanently BLOCKED prerequisites. Do not add an enabled detector to satisfy a roadmap percentage.

## Staff review model gate (offline only)

To progress independent infrastructure while #114 is open, a separate pure in-memory case review reducer may be developed using synthetic fixtures. It must not be connected to production DB, HTTP routes, queues, Discord, evaluator, scheduled jobs, live source data or enforcement.

A future real case admission requires:
- explicit reviewed and eligible detector/version, source-quality snapshot and same-scope evidence IDs with verified provenance; no BLOCKED or demo evaluation can be admitted;
- independently authenticated and authorized staff for **every** state transition; no event can grant its own permission;
- immutable case identity, tenant/installation/server scope, evidence references and append-only audit history;
- explicit neutral statuses and human-entered review outcomes; no automatic accusation or sanction;
- database transactions, tenant isolation and an idempotent delivery contract separately reviewed before any live publisher.

No value from a synthetic fixture establishes live findings or permits sending a Discord cheating alert. This phase changes no production behavior.
