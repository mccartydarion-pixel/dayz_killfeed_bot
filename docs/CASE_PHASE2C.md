# C.A.S.E. Phase 2C — Read-only Player Session Intelligence

This phase builds **bounded reconstruction of observed activity**, not anti-cheat verdicts, trusted event timestamps, continuous player tracking or inferred speed. It does not add migrations, alter ADM polling, or change enforcement.

## Input and ordering

Source: existing `case_evidence_events` captured in Phase 2B. The authorized installation determines both `guild_id` and `server_id`; a required internal `playerId` is matched against subject, actor or target IDs. All queries include guild and server. A query returns the newest 1–500 matching rows by database ID (default 200), fetching one extra record to expose an older-page cursor. This is a **bounded ingestion window** and may omit earlier history.

Reconstruction groups observations by their canonical ADM source. **Within a source only**, physical `source_end_offset` determines observation order. Distinct sources are never joined, even when their wall clocks appear adjacent. Groups are displayed by latest ingestion ID for convenience; that does not establish chronology across files. ADM `HH:MM:SS` is a clock-only display field. `ingested_at` is database ingestion time, not exact in-game timing.

No duration, speed, shot/miss accuracy, Cronus/XIM determination, or confidence/verdict is calculated.

## Connection windows

- `PLAYER_CONNECT` starts an explicitly observed connection window.
- First event without a connect is an **unknown start**, flagged `MISSING_CONNECT_IN_WINDOW`.
- `PLAYER_DISCONNECT` explicitly ends the window.
- Repeated connects without an observed disconnect close the prior window with an ambiguity marker; this is not proof of repeated reconnects.
- End of the source/read window without disconnect is `UNOBSERVED_END`, not an inferred disconnect.
- A source rotation is an unstitched boundary; it is not automatically a restart.

## Life observation windows

- `PLAYER_RESPAWN` starts a recorded respawn observation; prior unclosed life is marked ambiguous.
- `PLAYER_KILL` ends a life **only for the target**; the attacker is not considered dead.
- `PLAYER_DEATH` ends the subject's observed life.
- `SUICIDE_ACTION` records the action, not confirmed death; it does not close a life.
- Events following recorded death but preceding recorded respawn receive `OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER`. This includes DayZ's legitimate kill-before-hit log emission pattern. It does **not** imply gameplay after death or cheating.
- Missing connect/death/respawn/disconnect records are labeled explicitly. No event is inserted to fill a gap.

## Route

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/anti-cheat/sessions?playerId=<internal_id>&limit=200&before=<id>`

Requires existing `PLAYER_LOCATION_VIEW` capability and the admin read rate limiter. Every row is server/guild scoped. Reads are audited as `CASE_SESSIONS_VIEWED`. Source paths and raw ADM lines remain internal; the response exposes hashed source references and physical offsets. There are no mutation endpoints.

## Release and acceptance

Backend CI: `go build ./...`, `go vet ./...`, unit/race tests, and disposable-Postgres integration tests. Web: production-grade Next.js preview build. Verify with actual captured evidence: one connected player, death/respawn/disconnect, actor-only kill, two hits with same second, separate ADM boot sources, unknown start/end, and pagination. Confirm no cross-server leakage and no unauthorized coordinates.

Roll out backend before website. The C.A.S.E. collector stays restricted to its current Champions allowlist; this phase does not change collection flags. Detectors and enforcement stay disabled.