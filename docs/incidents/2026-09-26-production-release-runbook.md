# Reliability release: production preflight and runbook (PR #108)

**Status: PREFLIGHT INCOMPLETE — NOT AUTHORIZED.** Nothing was merged, deployed, restarted or migrated. No production variable was read or changed, and no secret was read.

| | |
|---|---|
| Production baseline | `605f1ec` (`main`, unchanged) |
| Release head | `6f6988b` |
| Owner decision | launch with `KILLFEED_DELIVERY_MODE=immediate` |

## 1. Pre-merge checks

| # | Check | Status | Evidence / what is missing |
|---|---|---|---|
| 1 | Latest PR head and CI | **PASS** | head `6f6988b`; GitHub `test` check success; base `main` = `605f1ec`, no conflict. The PR is still a **draft**; the owner marks it ready. Runtime code at `6f6988b` is identical to `05e02c9` (`git diff 05e02c9 6f6988b` outside `docs/` and tests is empty). |
| 2a | Production database backup | **BLOCKED** (no Railway access) | Owner: confirm a Railway backup newer than the release window, **or** take `pg_dump -Fc` of production to secure storage before the merge. |
| 2b | Migration `0056`/`0057` compatibility | **PASS** (tests) | Both are additive (nullable columns plus a partial index; one new table). `605f1ec`'s full integration suite passes on the `0057` schema (1,712 tests, 0 failures). Both migrations are validated from a fresh `0001` and against the `0055` schema. Lock cost is read from R2 below. |
| 3 | Read-only production evidence | **BLOCKED** (no database access) | Run `2026-09-26-evidence-queries.sql` **and** `2026-09-26-release-preflight-queries.sql` (new; validated against the `0055` and `0057` schemas; `READ ONLY` transaction, 15 s timeout) and keep the output. |
| 4 | Discord routes and permissions | **BLOCKED** (Discord denied) | R4 gives the routes. The bot needs View Channel, Send Messages, Embed Links, Read Message History and Manage Messages on the combat channel; Manage Channels for the counter; Manage Roles above the Verified role; and the Server Members intent. Confirm in the Discord UI or with read-only API calls. |
| 5 | Conflicting legacy routes | **BLOCKED** (needs R3–R5) | Any R5 row is a finding to resolve before the merge (see section 2). |
| 6 | Immediate delivery and journal recovery on the exact release commit | **PASS (synthetic only)** | On `6f6988b`: `-race` ×3 for the immediate/journal/rollback tests; integration against Postgres: `TestJournalSurvivesSIGKILL` (real SIGKILL), `TestJournalStrictCapacityBlocksUntilOldestRemoved` and the other journal tests. **No live run exists**: staging was never deployed. |
| 7 | Production Railway service tracks `main` | **BLOCKED** (no Railway access) | Owner: service → Settings → Source: repo `mccartydarion-pixel/dayz_killfeed_bot`, branch `main`, auto-deploy on. |
| 8 | Rollback to `605f1ec` | **PREPARED** | Section 5. |

## 2. Release risks that need an owner decision before authorization

1. **Leftover rotating cards (visible, certain).** At shutdown, `605f1ec`'s rotating feed posts one final batch of up to 10 cards per feed (`Run` → `flush()` on `ctx.Done()`). The new process has no journal rows for those cards, so it never removes them. The combat channel will show the old batch **plus** the new window: up to 10 + 10 per feed. The per-feed limit of 10 applies to cards the new process manages.

   Options:

   * **(a) Recommended.** Right after acceptance step A1, a moderator deletes the pre-release feed cards (everything posted before the deploy time) in the combat channel.
   * (b) A code change that deletes unjournaled cards at the first immediate start. This is not implemented. It would have to tell kill and death cards apart from PvE cards and the starter card in the same channel, so it needs its own review and tests.

   Rollback has the mirror case: `605f1ec` does not remove the immediate cards.
2. **No live evidence.** Staging was never deployed (no Railway or Discord access). The production deploy is the first live run of immediate mode, the journal and the strict window. The acceptance checks in section 4 are the first real measurements.
3. **Shared combat channel.** With a `KILLFEED` route, kills and deaths post to one channel, each with its own window: up to 20 managed cards (10 kills + 10 deaths). If R5 reports `LEGACY_DEATH_CHANNEL_SHADOWED`, the legacy death channel stops receiving cards.
4. **Deploy overlap.** If Railway runs the old and new containers together during the switch, both poll ADM. Each kill is still published once: publishing happens only after a successful non-duplicate insert, and the unique key is `(guild_id, event_fingerprint)`. The counter and the role reconciler may act twice during the overlap.
5. **Existing VERIFIED links** keep `role_sync_status` NULL (see R7) and are not reconciled. Only links verified after the release are retried automatically.

## 3. Production configuration (applied only after authorization)

| Variable | Action |
|---|---|
| `KILLFEED_DELIVERY_MODE` | set to `immediate` on the **production bot service only** |
| `NITRADO_API_BASE_URL` | stays **unset** (with it set and `APP_ENV` other than `staging`, the service refuses to start) |
| Every other variable, including `CASE_EVIDENCE_*`, Stripe, Discord, database and Nitrado | **unchanged** |

**Coordination:** `605f1ec` does not read `KILLFEED_DELIVERY_MODE` (verified: the string is absent from its source). So setting it first cannot change current behaviour, and the new application is guaranteed to start in immediate mode.

## 4. Execution sequence (after written owner authorization)

1. Record the evidence: run both SQL files (checks 3–5) and resolve any R5 finding. Confirm the backup (2a) and the source branch (7). Record the pre-deploy time `T0`.
2. Set `KILLFEED_DELIVERY_MODE=immediate` on the production bot service and apply it. Railway restarts `605f1ec`, which ignores the variable. Confirm it is healthy (`/health`) and still `605f1ec`.
3. Mark PR #108 ready for review and merge it to `main`. Railway auto-deploys; the new process applies migrations `0056` and `0057` at startup.
4. **Live acceptance** (stop at any critical failure, marked **C**, and go to section 5):

| # | Check | How (read-only) | Pass |
|---|---|---|---|
| A1 **C** | Correct commit and mode | `/api/runtime/status` → `build.commit` = the merge commit, `build.killfeedDeliveryMode = immediate`, `build.nitradoSource = nitrado`; `schema_migrations` max = `0057` | all true |
| A2 **C** | Kill cards appear promptly | next real kills: `feed card delivered` log lines, and the Discord message time vs the ADM event | detect→publish p95 < 5 s (the ADM poll adds up to 10 s end to end) |
| A3 | 10 cards per feed | combat channel: cards posted after `T0`, per feed | ≤ 10 kill and ≤ 10 death cards |
| A4 | Journal records and replay | `SELECT feed_key, COUNT(*) FILTER (WHERE message_id IS NULL) AS queued, COUNT(*) FILTER (WHERE removed_at IS NULL AND message_id IS NOT NULL) AS shown FROM discord_feed_cards GROUP BY 1`; ledger `replayed` after the next ordinary restart | shown ≤ 10 per key; queued returns to 0 |
| A5 **C** | Live player count | counter channel name vs Nitrado's live query (`health.playersSource`) | matches; never a fabricated 0 |
| A6 | Linking and the Verified role | one real `/link` → VERIFIED, role assigned (`health.verifiedRoles` shows `ASSIGNED`) | pass |
| A7 **C** | No stale-ADM-source errors | `health.pipelineStatus`; logs for `WRONG_OR_INACTIVE_ADM_SOURCE` | none after the first 10 minutes |
| A8 **C** | No Discord permission or rate-limit loops | ledger `CONFIG_FAULT` / `RATE_LIMITED` counts; logs | none repeating |
| A9 **C** | No duplicates | Discord cards vs `kills`/`deaths` rows since `T0` (unique by fingerprint) | one card per event |
| A10 **C** | C.A.S.E. unaffected | R9 row counts keep growing as before; C.A.S.E. endpoints and pages respond; `CASE_EVIDENCE_*` unchanged | unchanged behaviour |

Record every result as PASS, FAIL or NOT OBSERVED. There are no assumed passes: a check with no traffic yet (for example no kill or no `/link` yet) is NOT OBSERVED until one happens.

## 5. Rollback to `605f1ec`

**Triggers:** any failing **C** check, a crash loop, or failed migrations.

1. In Railway, roll the production service back to the last `605f1ec` deployment. `605f1ec` ignores `KILLFEED_DELIVERY_MODE`, so the variable can stay for this step.
2. Revert the merge on `main` (`git revert -m 1 <merge>`) so the next auto-deploy does not redeploy the release. This is a new commit; history is not rewritten.
3. Unset `KILLFEED_DELIVERY_MODE` afterwards, for clarity (this restarts `605f1ec`).
4. **Data:** migrations `0056` and `0057` stay; they are additive and compatible (`605f1ec`'s suite passes on that schema). No down-migration and no data restore are needed unless the database itself is damaged, in which case restore the backup from 2a.
5. **Discord:** the immediate process's last window (up to 10 cards per feed) stays in the channel. Delete it manually, as in risk 1.
6. Verify: `/health` is OK, rotating cards appear at the 10-minute cycle, the counter is correct, and the R9 C.A.S.E. counts are unchanged.

## 6. What the owner must provide

1. Written authorization for the section 4 sequence.
2. Railway and database access for checks 2a, 3, 5 and 7, or run them and share the output (never secrets).
3. A decision on risk 1: (a) or (b).
4. Acknowledgement of risk 2 (no live staging evidence).
