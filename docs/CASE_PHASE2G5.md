# C.A.S.E. Phase 2G.5 — Evidence admissibility and console ADM coverage

**Status: evidence-quality work; no detector activation.** This branch starts from the CI-green Phase 2G.4 staging head, but introduces no Railway configuration, bot integration, migration, public endpoint, scheduled job or sanction.

## What has been demonstrated

The separate `champions-case-staging` PostgreSQL completed an entirely synthetic one-shot evaluation, an independent SQL readback, and an idempotent replay. Those tests establish diagnostic plumbing and storage isolation, **not movement-cheat detection** and **not production evidence completeness**. Production `genuine-education` and Champions killfeed are unchanged.

## Observability contract

`AuditAdmissibility` is a pure bounded audit of already-retained records. It reports counts and explicit blockers, without returning source names, player identities or unfiltered ADM lines. The source file identifier is used only as an internal grouping key; offsets are meaningful **only within that file**.

| Available datum | Defensible interpretation | Interpretation that remains prohibited |
|---|---|---|
| Source ID + byte end offset + SHA-256 | Exact retained line address and changed-content collision detection | Continuous source coverage, or elapsed time between offsets |
| ADM `HH:MM:SS` | Clock-string formatting and ordering diagnostics within a file | UTC event timestamp, subsecond duration, travel speed or suspicious movement |
| Ingestion timestamp | When a retained observation was persisted | When the gameplay action occurred |
| X/Z coordinates on selected events | Recorded coordinate on that event, if both present | Continuous location tracking or interpolated path |
| Connect/death/respawn markers | Observed markers inside a bounded source window | Complete lifetime/session when other lines are filtered or absent |
| Multiple ADM files | Distinct sources with separate offset address spaces | Automatic cross-file stitching or uninterrupted coverage |

A gap between retained byte offsets is **not** evidence of dropped ADM lines: the collector intentionally filters event types. A decreasing clock may be a day rollover. A same-second pair is not a zero-duration traversal. Either case is a quality diagnostic, never a player suspicion.

## Automated contract checks

- Reject invalid sample limit or over-limit input; report empty samples explicitly.
- Distinguish malformed source addresses, same-address replay and same-address changed-hash collision; compare valid SHA-256 hexadecimal case-insensitively.
- Count valid/invalid clock strings and missing/partial coordinate pairs independently for subject, actor and target. Treat non-finite PostgreSQL float8 values (NaN or either infinity) as unusable coordinates and report `NON_FINITE_COORDINATE`, not a complete position.
- Compare adjacent clock strings only after grouping and ordering by byte offset **within** a source. No cross-source clock arithmetic.
- Report bounded window edges and multiple-source ambiguity.
- Expose only fixed aggregate event categories (hit, kill, observed boundary, other) over the retained bounded sample. Categories sum to `observationCount`; they do not estimate missing or unrecorded ADM events.
- Unconditionally report `MovementDetectorStatus=BLOCKED`, `SafeSpeedPairs=0`, `Enforcement=DISABLED`, including when all recorded coordinates and clock strings look valid.
- No score, player verdict, finding, alert, Discord action or sanction is created.

## Persisted-evidence audit (CI only)

`CaseEvidenceRepository.AuditCaseEvidenceAdmissibility` executes a bounded, tenant-and-server-scoped read-only repeatable-read transaction of up to `limit+1` persisted event rows; the extra row marks a truncated window. It projects source address, clock and **separate subject, actor and target** X/Z pairs. It counts complete, partial and absent positions for each role independently; it never combines axes from different people, returns player identity, or exposes canonical source names to a web route. It calls the same pure audit without touching detection or enforcement.

A disposable-PostgreSQL integration test seeds three synthetic observations spanning two ADM sources and one foreign-server record, then verifies per-server and per-guild isolation, the bounded page edge, incomplete coordinate pairs, invalid clock text, midnight/clock decrease ambiguity, and the permanently blocked movement gate. Database-only tests use `TEST_DATABASE_URL` plus `ALLOW_INTEGRATION_DB_TESTS=true`; no live credentials or data enter CI.

## Authorized source-integrity view

The existing `GET .../admin/anti-cheat/integrity` response now includes `evidenceAdmissibility`. It reuses the existing `PLAYER_LOCATION_VIEW` capability, selected installation server scope, read limiter and audit event; no new route or permission bypass is introduced. The server reads at most 200 retained rows plus one truncation marker using a read-only repeatable-read SQL transaction. No additional Nitrado polling occurs.

The source-health snapshot, source-continuity query and admissibility audit are separate observations, **not one atomic view of live log state**. The report contains only aggregate counts and quality blockers; canonical ADM paths, source IDs, player identities, and coordinates are never returned through this new field. Empty rows explicitly mean `NO_OBSERVED_EVENTS`, not no cheating. Integration tests assert server isolation, unauthorized-user rejection, permanently blocked movement and absence of raw source paths.

## Evidence observation availability

The existing staff integrity response also provides `evidenceObservationStatus`, an explicit availability label that is **not** collector health or a player judgment. When the worker is absent it returns `WORKER_UNAVAILABLE` even if historical rows remain; when the collector is not configured it returns `COLLECTOR_NOT_CONFIGURED`. Without a selected source it is `SOURCE_UNVERIFIED`. With a selected source and no retained records it is `NO_RETAINED_EVENTS`, not `NO_CHEATING`; otherwise `RETAINED_EVENTS_OBSERVED` means retained observations exist, not that the collector is continuously healthy or complete. Existing independent source-state and transport fields remain authoritative for source-health context. An old observation is not a current gameplay signal.

## Evidence acceptance process for real console ADM

This branch has not fetched a current real ADM sample. A real-world observational gate must use the existing authorized and scoped C.A.S.E. evidence/integrity views, preferably the already-running collector; **do not create a second Nitrado poller**, expose raw source paths, export private player data to CI, or silently treat an empty server as an ingestion failure.

For an exact Champions server and documented observation window, privately record: running worker presence, collector flag, selected/accepted source reference match, most recent source/offset, checkpoint/remote size provenance, transport failures, retained event count, source changes, invalid clocks, boundary ambiguity, and source-address collision outcomes. Compare counts to the relevant retained event allowlist; do not claim full ADM-log coverage from filtered rows. An empty sample is `NO_OBSERVED_EVENTS`, not `NO_CHEATING`.

The reviewer must verify that source-health snapshots are not atomic across endpoints. Do not make a byte-perfect cross-snapshot claim. No public dashboard element should display canonical source paths, tokens or raw player identifiers.

## Staff acceptance capture (existing authorized view)

After review and a separately approved backend deployment, select the **Champions** installation, authenticate as an actor with `PLAYER_LOCATION_VIEW`, and issue the existing `GET .../admin/anti-cheat/integrity` request. Capture only the following *aggregate* fields in the restricted release record: `generatedAt`, `serverId`, `workerAvailable`, `sourceState`, `collectorConfigured`, `selectedIsAccepted`, `evidenceLines24h`, `latestEvidenceIngestedAt`, continuity status, and the `evidenceAdmissibility` aggregate counters/blockers. Do **not** paste private source paths, player data, DSNs, session cookies or Nitrado secrets into a public ticket, CI artifact or Discord.

The `evidenceAdmissibility` audit covers the **latest 200 retained records by ingestion ID**, not a 24-hour period; `evidenceLines24h` is a separate ingestion-time count. It is a filtered, truncated page when `windowTruncated=true`. Compare repeat observations to establish whether new retained events appear without interpreting snapshot differences as exact missing bytes. When the server is quiet, require `NO_OBSERVED_EVENTS` if the sample is empty, not `NO_CHEATING`; when the worker is unavailable, do not describe the source as healthy.

A production activation decision for this read-only response is separate from any production **shadow evaluation**. Do not copy the staging one-shot process to the live bot. This release only permits observation of data quality after routine code review and deployment; it never authorizes scores, Discord cheating alerts, cases, bans, or enforcement.

## Phase exit and non-exit criteria

This phase can validate that the quality classification and observational reporting are consistent for synthetic and separately authorized real samples. It **cannot** lift `CASE-MOV-001` merely because a sample is large or a staging replay passed. To consider movement inference later requires a separately verified dated, high-resolution gameplay event-time source, validated sampling continuity, source boundary treatment, and a tested exception model for respawn, vehicles, teleport/admin actions and map boundaries. Every criterion needs evidence and a new reviewed detector design.

Until then: no timing-derived speed, no risk score, no automatic punishment. Production one-shot replay is a separate approval and remains unverified.
