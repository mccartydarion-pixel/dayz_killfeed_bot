# C.A.S.E. Phase 2B — Evidence Collection Engine

Status: opt-in observation-only. No detectors, scores, cases, or enforcement.

## Collection

The same per-server ADM polling goroutine and durable checkpoint serve killfeed and C.A.S.E. Evidence is captured before legacy semantic dedupe, so two identical-looking hit lines at different byte offsets are separate observations. The source address includes guild, server, canonical ADM path (normalizing ftproot/noftp aliases), and complete-line ending byte offset. A SHA-256 hash of the line is stored, never the raw line.

Exact source replay is idempotent. Changed content at the same source address is a hard integrity error. A DB write error stops checkpoint advancement, so no hole is silently declared complete. This opt-in reliability choice can delay killfeed polling if the evidence DB is down; monitor before enabling.

Captured types: hits, kills, connect, disconnect, death, suicide, respawn, unconscious, conscious. Time-of-day is stored as clock-only; no UTC date or timezone is fabricated. Ingestion timestamp and evidence ID represent ingestion order, not necessarily event-time intervals. Player identities exposed to staff are scoped internal IDs. Positions use corrected MapX/MapZ/Altitude mapping. No raw ADM line, Nitrado token, or private DayZ identity string is returned.

## Lifecycle and exclusions

Explicit CONNECT, DISCONNECT, RESPAWN, DEATH and SUICIDE boundaries are retained. PLAYER_KILL denotes death of the target. UNCONSCIOUS and CONSCIOUS remain state events, not invented session resets. ADM source changes are evidence gaps/boundaries, not proof of a server restart. Missing connect/death evidence, vehicles and delayed transport remain possible. No movement detector is active.

## Persistence and API

Migration 0048 adds the scoped immutable evidence table and indexes without altering existing killfeed tables. No historical hit backfill is claimed and retention is not silently applied.

Both CASE_EVIDENCE_ENABLED=true and CASE_EVIDENCE_SERVER_IDS=<comma-separated internal game_servers IDs> are required to attach collection. An empty or malformed allowlist fails closed. Server IDs are from the scoped game_servers table, not Nitrado's provider service ID. Restart the server worker after changing these environment variables. A disabled collector does not imply zero hits.

GET .../admin/anti-cheat/evidence requires PLAYER_LOCATION_VIEW (Administrator) because detailed coordinates are sensitive. Supports playerId, before, limit 1–100, and returns source pseudonym/offset, clock, ingestion time, names, internal IDs, nullable combat fields and positions. Every query scopes both guild and server and reads are audited. Overview hit totals use ingestion time.

## QA

Build, vet, unit, race and disposable-Postgres integration tests. Required scenarios: same-text distinct hits; replay idempotence; offset conflict rejection; mount alias equivalence; retry after persistence failure; cross-installation isolation; permission gating; incomplete coverage labelled accurately.

C.A.S.E. does not infer Cronus/XIM, shots missed, or continuous movement from these observations.