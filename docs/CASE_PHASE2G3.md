# C.A.S.E. 2G.3 — Bounded shadow diagnostics and history

This is an explicitly invoked, **default-off** internal diagnostic runner, not a continuously active detector. It selects at most 50 already-persisted ADM evidence rows from the most recently ingested source, ordered by source byte offset within that source. It never stitches ADM sources or infers elapsed gameplay time. It calls the 2G.2 blocked-only ledger, which revalidates same-server evidence inside the write transaction and derives blockers from the registered detector.

The runner has no production caller, environment flag, scheduled job, HTTP write route or Discord integration. `Enabled` must be explicitly supplied to the internal call. No live flags, suspicion, scores, cases, alerts, sanctions or player action. CASE-MOV-001 stays BLOCKED.

Read-only `GET .../admin/anti-cheat/shadow-history` requires existing PLAYER_LOCATION_VIEW, tenant and selected game server scope, rate limiting and audit. It returns bounded newest-first blocked diagnostics, exact evidence IDs, blocker codes and cursor. Evidence details remain protected by the existing scoped evidence endpoint. Empty history means no recorded evaluations, not zero cheating.

Release gates: exact-commit Go CI and disposable-PostgreSQL integration, bounded replay test, cross-server isolation, authorization, pagination, migration 0056 already present. Deploy backend before any website history view. A future separate controlled operational activation procedure requires explicit authorization and new evidence-quality validation; this release does not activate its runner.
