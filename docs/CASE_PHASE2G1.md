# C.A.S.E. Phase 2G.1 — Detector Framework and Shadow Readiness

This release installs an inert, versioned detector registry and read-only, scoped prerequisite API. It deliberately does not start a background evaluation worker, create cases, store findings, score players, enable Watch Mode or send enforcement commands.

## Safety contract

- CASE-MOV-001 version 0.1.0 is BLOCKED: no trusted elapsed gameplay time, no validated continuous movement samples, no proven full source coverage and no validated vehicle/respawn/teleport/admin exclusions.
- ADM HH:MM:SS and database ingested_at are **not** substitutes for trusted event time. Neither healthy source selection nor source-byte/checkpoint equality changes these limitations.
- The prerequisite evaluator returns explicit blockers, zero evidence IDs and empty findings. No fabricated shadow output, suspicion or player verdict.
- Registry definitions are copy-on-read; a caller cannot mutate global mode.
- EvidenceFingerprint is a deterministic, tenant- and server-scoped hash of detector ID/version and unique positive evidence IDs. This is a primitive for later idempotent persisted evaluation, NOT itself a stored result and not suitable as a verdict.
- The endpoint `GET .../admin/anti-cheat/detector-readiness` requires existing PLAYER_LOCATION_VIEW, installation guild/server scope, rate limiting and audit. No mutation route exists.

## Remaining release gates

Before any detector can execute in shadow mode: define persistent source-addressed evaluation storage with scoped foreign keys and uniqueness; build a separately gated evaluation runner; attach immutable evidence IDs, version and provenance; test idempotence, cross-tenant isolation, bounded replay and rollback. Then prove each rule's input prerequisites with actual data. Enforcement stays disabled by design.

No Nitrado, Discord, server, collector or source-selection configuration changes.

## Release validation

Do not promote on stale CI results: inspect check runs for the exact final commit after any deployment-integration configuration change. The Go test job and the Cloudflare build check are independent gates; a stale failed check on an older commit is not proof that a newer build passed. The live killfeed runs on Railway, not this inert readiness route. Verify Railway health after release.
