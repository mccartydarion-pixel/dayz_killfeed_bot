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

---

## 7. Final release gate (owner decision: controlled manual cleanup, no automatic channel clearing)

**Gate status: NOT READY FOR EXECUTION.** Each row records only what was actually observed.

| # | Gate item | Observed status | Evidence |
|---|---|---|---|
| 1 | CI on the latest head | **PASS (observed)** | `abdf7c8`: GitHub `test` check success; `main` = `605f1ec` (no conflict) |
| 2 | Recoverable production backup | **NOT OBSERVED** | no Railway or database access in this session |
| 3 | Both read-only SQL files on production | **NOT RUN** | no production database access. Both files were validated only on local copies of the `0055` and `0057` schemas. |
| 4 | Conflicting feed routes / legacy channels resolved | **NOT OBSERVED** | depends on R3–R5 from item 3 |
| 5 | Discord permissions; existing feed message IDs | **NOT OBSERVED** | `discord.com` is denied by the network policy. The leftover card IDs **cannot exist before the deploy** (see 7.1). |
| 6 | The merge triggers the expected Railway deployment | **NOT OBSERVED** | no Railway access |
| 7 | Migrations `0056`/`0057` and rollback compatibility | **PASS (tests, observed locally)** | `605f1ec` suite on the `0057` schema: 1,712 tests, 0 failures. Migrations applied from `0001`, and preflight SQL run on the `0055` and `0057` schemas. Not observed against production data. |
| 8 | Written deployment and rollback sequence | **PREPARED** | sections 4 and 5, refined in 7.2 |
| 9 | Variable before the merge; old application healthy afterwards | **PREPARED; NOT OBSERVED** | `605f1ec` does not read `KILLFEED_DELIVERY_MODE` (verified in its source). Its health after the change can only be observed at execution. |
| 10 | Remove only the identified pre-release message IDs | **PREPARED** | 7.1 |

### 7.1 Identifying the pre-release feed cards by exact message ID

**Timing matters.** `605f1ec` deletes its current batch and posts a final batch of up to 10 cards per feed when it receives the shutdown signal (`Run` → `flush()` on `ctx.Done()`). The cards to remove are therefore created *during* the switch. Any ID list taken before the deploy would name cards the old process deletes itself.

Capture is read-only (`GET /channels/{id}/messages?limit=100` with the production bot token, via `railway run` so the token is never printed):

1. **Before step 2** (baseline B0): list the combat channel (and the legacy death channel, if R5 shows one in use). Record ID, author ID, timestamp, embed title and footer. This is evidence only; nothing is deleted from it.
2. **After the new deployment passes A1** (list L1): a message is a **pre-release feed card** only if **all** of these hold:
   * its author is the production bot;
   * its ID is **not** in `SELECT message_id FROM discord_feed_cards WHERE message_id IS NOT NULL` (the new process's own cards);
   * its embed is a kill or death card: footer text ends with `EVERY KILL TELLS A STORY`, and the title is a kill title or `☠️ PLAYER DEATH` / `💀 SUICIDE`;
   * it was created before the new process's first `posted_at` for that channel.

   Excluded:
   * the starter card (title `🔫 COMBAT FEED`);
   * PvE cards (description-only amber embeds, no title or footer), which are not rotating-feed cards;
   * panels and anything by another author.
3. **Owner review.** The exact ID list, with timestamp and title for each, goes to the owner. Nothing is deleted without explicit approval of *that list*.
4. **Delete** each approved ID with a single `DELETE /channels/{channel}/messages/{id}`: never a bulk or "clear channel" call. Then re-list and confirm that exactly those IDs are gone and every excluded message remains.

Expected size: at most 10 kill + 10 death cards per server worker, plus any older cards a past failed delete left behind (listed separately for the owner to decide).

### 7.2 Sequence with Railway auto-deploy from `main`

1. Run items 2–6 and record the output. Stop on any unresolved R5 finding. Capture B0 and note `T0`.
2. Set `KILLFEED_DELIVERY_MODE=immediate` on the production bot service only and apply it. Railway redeploys the **current** `605f1ec` image. Observe: `/health` OK; the deployment is still `605f1ec`; rotating cards still post at the cycle; no crash loop. If it is unhealthy, unset the variable and stop.
3. Mark PR #108 ready and merge it. Merging creates a commit on `main`, and Railway auto-deploys it; if "Wait for CI" is enabled, it deploys only after `main`'s checks pass. Note the merge SHA; A1 must report exactly that SHA.
4. The new process starts in immediate mode, applies `0056`/`0057` under the migration advisory lock, then starts its workers. Run A1–A10.
5. After A1 passes: capture L1, build the candidate list (7.1), get owner approval, delete by ID, verify.
6. On any **C** failure: section 5 (roll back in Railway; revert the merge with a new commit so auto-deploy does not bring the release back; then unset the variable). Clean up the immediate cards with the same by-ID procedure (7.1). Their IDs come directly from `discord_feed_cards` (`message_id`, `removed_at IS NULL`).

### 7.3 Access needed to complete the gate

* Network access for this session: `backboard.railway.app` and `discord.com` (environment settings).
* A Railway token for the production project in the environment settings (`RAILWAY_TOKEN`). Items 2, 6 and 9 need it, and running both SQL files through `railway run` against the production database needs it too.

  If you prefer to run items 2–6 yourself, share the outputs; secret values are not needed.

Production variables, the merge, the deploy, migrations, restarts and message deletion all remain **not executed** until explicit authorization.

---

## 8. Update: `main` moved (merged into the PR, 2026-09-29)

`main` advanced from `605f1ec` to `f4d956e` (31 C.A.S.E. commits, #104–#155) while this release waited. That changes the following.

* **Merge.** `main` was merged into the PR branch with a merge commit; no history was rewritten. The only conflict was the migration list.
* **Migrations renumbered.** `main` now has `0056_case_shadow_evaluations`, `0063_case_review_outbox_skeleton` and `0064_case_build_evidence` (`0057`–`0062` are reserved by another billing candidate). This release's migrations were renamed to **`0065_player_link_role_sync`** and **`0066_discord_feed_cards`**, with unchanged SQL, and listed after `main`'s. Neither was ever applied anywhere outside tests, so the rename is safe. Everywhere above that says `0056`/`0057` for this release now means `0065`/`0066`.
* **Production baseline is unknown here.** Railway auto-deploys `main`, so production may now run `f4d956e`, not `605f1ec`. Step 1 must record the SHA production actually runs (from the Railway deployment or `/api/runtime/status` if present). **That SHA is the rollback target.**
  * **C.A.S.E. preservation.** The release changes no C.A.S.E. file. Every C.A.S.E. change comes from `main` as-is, and its migrations run in `main`'s order.
  * **Rollback compatibility.** Checked against `main` (`f4d956e`): its full integration suite on the release schema (`0066`) passes, 45 packages, 1,856 tests, 0 failures.
  * **Merged tree.** Build and vet are clean; unit tests pass in 45 packages; integration on a fresh database (`0001`–`0066`) passes 46 packages, 2,015 tests, 0 failures; `-race` passes for app, killfeed and discord. The preflight SQL is validated on the `0064` and `0066` schemas.
* **`605f1ec` facts still hold for `f4d956e`.** `f4d956e` does not read `KILLFEED_DELIVERY_MODE` (not present on `main`). Its rotating feed still posts a final batch at shutdown, so the by-ID leftover-card procedure (7.1) is unchanged.
