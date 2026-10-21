# Ranked identity and dedupe audit (source inspection)

Scope: read-only inspection of the PR #160 code path. No production data or live ADM fixtures were inspected. This is a source finding, not proof that an ADM `id=` value is globally stable across Nitrado servers or PlayStation/Xbox accounts.

## Observed path

1. `internal/killfeed/parser.go` extracts an opaque `id=` token from each player's ADM metadata into `PlayerRef.ID`.
2. `PersistenceQueue.upsertPlayer` sends that token to `PlayerRepository.UpsertPlayer`, which keys `players` on `(guild_id, dayz_player_id)`. A missing ID yields no player row.
3. `game_servers.platform` exists, and the SaaS installation path sets it explicitly. The legacy `ServerRepository` upsert preserves its existing platform on conflict; rank ingestion must read and validate the authoritative server row rather than infer platform from a name.
4. `kills` records the guild and optional server ID and enforces `UNIQUE(guild_id, event_fingerprint)`. The fingerprint uses type, event clock/date, participant ADM IDs, weapon and distance; it does not include a server identity.
5. The persistence queue returns early on `ErrDuplicate`, then invokes the postprocessor only for a newly inserted kill. This prevents a normal same-guild replay from reaching postprocessing twice.

## Consequences for Ranked

- A guild player ID is not a global identity. A proposed `(platform, ADM id)` key needs evidence that the same console player presents the same opaque ADM ID across servers and that it cannot collide with another player. Do not assume `id=` is a PSN/Xbox account ID or join by gamertag.
- One guild can contain multiple game servers. Because the kill unique key omits `server_id`, indistinguishable same-second kills on separate servers could collapse at the existing source. Do not alter this existing dedupe as part of RP without an isolated compatibility and replay investigation.
- The same physical Nitrado server could be represented in different guilds: the existing guild-scoped fingerprint would allow two kill rows. A global award ledger therefore needs canonical physical server identity plus a source event identity, and must reject or reconcile duplicate service registrations before any global award.
- RP processing should consume persisted kill IDs from a durable worker/reconciliation cursor, not depend solely on a best-effort postprocessor. The award table must have unique keys for global and local scope and be safely replayable after a crash.
- Unknown platform, missing player IDs, ambiguous cross-server identity, and uncertain duplicate source events must be excluded from RP with an inspectable reason. Existing killfeed/stats continue normally.

## Evidence needed before live RP

- Sanitized ADM lines from the same consenting console account on two participating servers, plus different accounts and both supported platforms. Confirm `id=` behavior without storing raw account tokens in public logs.
- A two-server same-guild fixture with concurrent same-second kills, and a duplicate-import fixture for one physical server across guilds. Determine whether source-file position can serve as a stable event identifier across imports; a local offset alone is not global.
- Database audit for duplicate `provider_service_id` registrations and unknown platform values, read-only and aggregate-only. Do not expose player identities in the report.
- Crash and delayed/out-of-order log replay that reconciles each season total to the immutable ledger and applies the five-minute attacker/victim cooldown deterministically by event time.
