# C.A.S.E. Phase 2A — Observation API

## Goal

Connect the Champions Anti-Cheat Security Engine website to **actual server-scoped records** without making cheating claims. This phase intentionally does not add a detector or mutate a player account.

The existing ADM parser provides event-triggered kills, hits, connections, respawns, and positions, but the durable tables do not retain every parsed event. The API reads only records that already exist in PostgreSQL. It does **not** scrape Nitrado again, reparse ADM in a second poller, or synthesize missing fields.

## Route and authorization

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/anti-cheat/overview`

- Existing website service bearer token and acting-user header.
- Existing `requireCapability(..., PLAYER_DIRECTORY_VIEW)` resolves installation and staff membership/role server-side.
- Every query includes **both** scoped `guild_id` and `server_id`; a second installation in the same guild cannot leak records.
- Existing admin read rate limiter, ten-second request timeout, and audit event `CASE_TELEMETRY_VIEWED`.
- No write or punishment routes exist.

## Response semantics

`mode=OBSERVATION_ONLY`, `detectorsEnabled=false`, `enforcement=DISABLED`. Cases, alerts and watchlist are genuinely empty arrays.

`telemetry.killEvents24h` counts persisted kills with a valid ADM source timestamp inside the UTC window. A row without `event_time` is excluded from that count rather than misclassified as a source-timed kill.

`telemetry.locationSamples24h` counts event-triggered ADM location samples with `observed_at` in the same window. These samples are **not** continuous movement telemetry.

`telemetry.hitEvents24h=null`: hits are parsed but not retained in the existing durable dataset. `null` means unmeasured; `0` would be an incorrect claim. Shot misses, controller inputs, and continuous positions are not available.

`recentEvents` returns up to 20 newest persisted kill records for the same guild/server. Each includes source timestamp and provenance (`ADM_EVENT_TIME` or `INGESTED_AT` fallback), player record IDs and display names, recorded weapon and nullable distance. No raw ADM line, credential, DayZ private ID, or speculative risk score is returned.

`status=COLLECTING` means qualifying persisted records were seen in the 24-hour window; it does **not** mean the poller is healthy. Otherwise status is `AWAITING_EVENTS`. Runtime pipeline health remains a separate concern.

## What this unlocks

The website can now show observed data, source coverage and honest empty states. Example cases stay behind explicit demonstration mode. Later phases will add a dedicated, durable per-line observation sink (including hits), idempotency based on ADM source cursor rather than lossy line-text hashing, session/reset boundaries, validated detectors, correlated evidence packages, and audited case actions.

Do not infer true hit accuracy without missed-shot counts; do not infer Cronus/XIM from ADM logs; do not treat high kill counts, distance, or one position change as proof.

## QA

- Run `go test ./internal/app ./internal/killfeed ./internal/repository` and the full backend CI suite.
- Verify an unauthorized user and a user without `PLAYER_DIRECTORY_VIEW` cannot read this route.
- Verify two installations in one guild show distinct server records.
- Verify an installation with no recent records returns zero counts, empty lists and `hitEvents24h: null`.
- Verify that an older kill without source timestamp uses `timestampSource: INGESTED_AT`.
- Confirm existing killfeed, Nitrado polling, location history, player stats, and Discord behavior are unchanged.

No migration or new environment variable is needed for this API.
