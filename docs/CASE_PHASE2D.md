# C.A.S.E. Phase 2D — Evidence Quality & Detector Readiness

**Scope:** read-only diagnostics on Phase 2B evidence and Phase 2C player session reconstruction. No migration, new collector, scoring, detector activation, case creation or punishment.

## Evidence completeness

The reconstruction returns observed connection and life windows from up to 500 most-recently-ingested matching records, grouped by canonical ADM source and ordered by source byte offset within each file. Its new `quality` object exposes counts for unknown connection starts, unobserved ends, repeated connects, respawns without recorded death and observations occurring after death in source order. A source-file boundary and page boundary remain explicit, not automatically stitched. A missing boundary means no boundary appeared *in the returned observations*, not that a player exploited anything.

The evidence collector stores selected event types. Byte gaps between two retained observations may contain routine lines that were never meant for C.A.S.E.; the gap alone does not prove missing source lines or source loss. `coverageStatus=FILTERED_SOURCE_EVENTS_ONLY` makes that explicit.

## Timing gate

`timeStatus=CLOCK_ONLY_NO_TRUSTED_ELAPSED_TIME`, `movementDetectorStatus=BLOCKED` and `safeSpeedPairs=0` apply to the available ADM schema. The raw ADM HH:MM:SS is clock-only (no trusted date/timezone or subsecond precision). Log emission order can differ from gameplay ordering; ingestion timestamps measure delivery, not gameplay. Event-triggered positions cannot become continuous GPS by interpolation.

A same-second pair, invalid clock or clock decrease is a *source quality diagnostic*, not an exploit. Clock decreases may simply be a midnight rollover. No speed, movement verdict or confidence is calculated. CASE-MOV-001 requires a separately validated event-time source and vetted exceptions before a future shadow-mode release.

## Exact evidence deep links

`GET .../admin/anti-cheat/evidence?evidenceId=<positive_id>&playerId=<optional_id>` returns **only the exact matching record**, even if it lies beyond the 50-item page. It reuses the existing `PLAYER_LOCATION_VIEW` authorization, guild/server scoping, optional player scoping, read limiter and audit log. `before` cannot be combined with `evidenceId`. Empty results never fall back to nearby evidence; exact requests have no pagination cursor.

## Verification

- Pure reconstruction quality tests cover same-second lines, source changes, partial windows, midnight clock decrease, missing clock, and kill-before-hit source ordering.
- Database integration tests cover exact-ID reads beyond pagination, foreign server isolation, invalid selector combinations and unauthorized actor.
- Verify Sessions and Evidence links in the deployed UI for a player with more than 50 observations.
- Confirm the current collector allowlist remains unchanged.
- No raw private DayZ IDs, canonical Nitrado paths or credentials are exposed through these routes.

**Release order:** backend PR, successful CI and Railway deployment, then website PR and Vercel build. This phase does not turn on any detector or automated enforcement.
