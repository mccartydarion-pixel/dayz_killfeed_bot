# C.A.S.E. core: offline staff-card fixture

This standalone branch extracts **only** the fixed synthetic design fixture and local JSON printer from old stacked PR #116 onto current `main`, independently of obsolete branches #112/#113. It is a development preview, not evidence acceptance or a live finding.

`go run ./cmd/case-staff-preview` writes one Discord-compatible embed JSON to stdout. It has no network client, credentials, server/player input, route, HTTP endpoint, worker wiring, send method, scheduler or delivery outbox. The card is explicitly marked DEMO ONLY, BLOCKED, safe speed pairs zero and enforcement DISABLED, with a fixed date and no player identity. No one is accused and no Discord message is sent.

The *separate* operational publisher allowlist and guild-scoped state fix live in independent draft PR #126. Do not merge the old stacked #116 wholesale. Future case delivery requires a separately reviewed tenant-scoped case store, eligible detector, authenticated staff approval, durable idempotent outbox and private CASE_ALERTS route; none is authorized here. #114 still requires source-matching real retained evidence and CASE-MOV-001 remains blocked.
