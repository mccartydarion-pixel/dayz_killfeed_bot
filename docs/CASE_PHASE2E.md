# C.A.S.E. Phase 2E — Source Integrity

This phase exposes a read-only, authorized snapshot of the **existing** ADM worker and its persisted evidence. It does not poll Nitrado again, select an ADM file, change checkpoints, or create a detector.

## API

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/anti-cheat/integrity`

Reuses the installation's server-side guild/server scope, the `PLAYER_LOCATION_VIEW` capability, the admin read limiter and an audit record. Raw Nitrado paths are never returned: source identities are SHA-256 pseudonyms. Selected, accepted and newest-listed file identities remain separate.

## Interpretation

- `sourceState` is from the existing classified ADM source-health snapshot. HEALTHY refers to polling/source progress, **not** completeness of all game events or evidence of a clean player.
- QUIET means polling is operating while the selected source is unchanged. SOURCE_LAGGING indicates the selected source appears not to advance despite other activity; transport failures and stalled workers have separate statuses.
- Worker availability is `false` if the app has no live engine for that server. Byte counts and timestamps are nullable when absent, not misleading zero.
- Remote metadata and checkpoint snapshots are independently synchronized. They do not form an atomic byte-for-byte proof of continuity.
- Evidence counts come from already stored Phase 2B source events filtered by guild and server. The latest evidence source, offset and ingestion time come from one database row. These are ingestion observations, not precise gameplay times.
- Absence of evidence does not prove absence of gameplay. Source changes do not prove server restarts. Metadata unchanged does not prove an exploit.
- `elapsedTimeTrusted=false`, `movementDetectorStatus=BLOCKED`, `detectorsEnabled=false`, `enforcement=DISABLED`. Do not compute speed from ADM clock or ingestion times.

## QA and rollout

Unit checks: missing worker, empty metadata, quiet source, active source lag, source pseudonyms, no detector activation. PostgreSQL integration: scoped count/latest evidence and unauthorized user. Existing source selection and killfeed delivery are unchanged.

Deploy backend only after build, vet, unit, race and integration tests; verify Railway health, then deploy the website Integrity tab. Do not change the Champions collector allowlist. This endpoint does not resolve ADM clock-only timestamp limitations.
