# C.A.S.E. Phase 2G.4 — Controlled Shadow Verification

## Scope and evidence

This change validates the *deployed code path* against synthetic ADM-shaped records in a disposable PostgreSQL integration database. It does **not** claim to have run a production shadow evaluation, to have copied real player data, or to have proved a movement cheat detector.

Checks:
- Explicit default-off gate makes no write; a limit over 50 fails.
- Most recent persisted ADM source is selected without mixing sources; selection uses source byte offsets, not invented elapsed gameplay time.
- Same snapshot replays to one evaluation ID and two source-linked evidence records.
- A changed snapshot has a different ID, and subsequent replay remains idempotent.
- Each evidence ID is resolvable within the selected tenant/server.
- History cursor pagination, empty end, access control, bounds, and read-only behavior.
- No finding, score, warning, case, sanction, continuous runner, or Discord action.

Production remains unchanged: no ad-hoc DB credentials or SQL access are exposed through the Railway connector, and there is no existing one-off process execution facility. Do not add a public debug endpoint, repurpose the production service, restart Nitrado, or enable a scheduled job merely to produce a sample record.

## Remaining live acceptance gate

To claim that actual persisted **production** evidence has been evaluated: establish a separately audited, operator-invoked, single-use execution path with exact server scope and max evidence window, verify the preflight evidence IDs/source, execute only BLOCKED diagnostics, and independently read back the same evaluation/evidence IDs through the authorized history endpoint. Run twice to demonstrate duplicate count remains unchanged. Capture deployment and source identity, rollback and audit evidence. Until then the live history may truthfully be empty, and this phase's production-replay gate is NOT VERIFIED.

CASE-MOV-001 remains BLOCKED because event-triggered, date-less-second ADM positions do not prove elapsed travel time or continuous movement.
