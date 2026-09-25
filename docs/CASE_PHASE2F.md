# C.A.S.E. Phase 2F — Evidence Continuity & Collection Assurance

Scope: read-only, per-installation continuity reporting built from the existing ADM worker and Phase 2B evidence. No Nitrado write, live server restart, extra polling, detector, score, or enforcement.

The existing protected Integrity endpoint adds a bounded summary of up to ten recently ingested ADM sources. For each source, the report gives retained line count, hits, kills, respawns, connects, disconnects, highest retained source offset, and latest ingestion time. Source identities are pseudonymized, with no raw paths, ADM lines, private DayZ identity strings, or credentials. All reads require the existing PLAYER_LOCATION_VIEW permission and are scoped by both guild and server.

A matching selected-source reference with retained records is classified RETAINED_EVENTS_OBSERVED. If no match is found in the bounded source list, status is NO_RETAINED_EVENTS_IN_RETURNED_SOURCES, **not** no gameplay or proof of collection failure. Missing worker, inactive collector, or unreported source is UNKNOWN. Matching checkpoint and remote file size is CHECKPOINT_REPORTED_NOT_FULL_COVERAGE: C.A.S.E. intentionally stores only selected event types, so source-byte gaps are not automatically missing evidence.

The report intentionally does not claim continuous coverage, all-day uptime, exact session durations or real-time movement. No speed pairs are derived. RestartRecovery remains NOT_TESTED_ON_LIVE_SERVER until a separately authorized controlled experiment is performed. ReplayProtection names the existing source-address-plus-line-hash mechanism; its current unit/integration regression tests verify same-content separate offsets, exact replay idempotence, and changed-hash collision rejection.

Acceptance: build/vet/unit/race/disposable-PostgreSQL CI; verify source-scoped aggregates under two installations; verify blank/unknown states; verify no duplicate evidence after local replay; confirm the current Champions source is selected and staff dashboard displays the source report. Live gameplay/restart coverage remains a separate release gate.
