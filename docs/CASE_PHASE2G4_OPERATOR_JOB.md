# C.A.S.E. 2G.4 — Isolated operator-job readiness and acceptance

Status: **NOT APPROVED FOR PRODUCTION EXECUTION**. This is an operator plan, not an activation. The existing `cmd/case-shadow-once` remains an offline tool, with no bot startup hook, public route, cron, or automatic enforcement.

## Baseline observed 2026-09-27

- Production bot: Railway project `genuine-education`, service `dayz_killfeed_bot`, successful deployment of `main` at `9d4d9c8889500d3a3c561a11af457996def51c99` (Phase 2G.3).
- The 2G.4 utility is on PR #112, head `c22ae997a6039435fb366f3f34ba7a0c18f82dc2`; backend-ci run 36301939601 succeeded.
- Railway exposes no existing operator one-shot command execution in this project; connector does not offer SQL inspection or raw credentials. No production shadow replay has been performed.
- Separate `champions-case-staging` project exists. Its reliability QA bot, QA PostgreSQL and Nitrado fixture have no reported deployment; the existing billing QA service and Postgres must not be repurposed.

## Isolated staging execution environment (2026-09-27)

- Dedicated QA service `case-shadow-oneshot-qa` in the separate Railway project `champions-case-staging` (project `0cb1c55e-71c6-4865-96a8-dc7c7e7b3dc9`, service `98eb3f02-aca3-4d2e-bab9-5debb158d79f`). Its environment is named `production` within this **staging project**; it is NOT the live `genuine-education` project. Inert start command, restart NEVER, no cron, no public domain and no production credentials.
- Dedicated PostgreSQL `Postgres-BqU0` (service `a12e5f66-542c-4689-b822-3072392bd2b0`) is provisioned in this staging project with its own private network and volume. It is not the existing billing or reliability Postgres. A template provisioning no-op staged patch was discarded without changing data.
- `Dockerfile.case-shadow-once` builds the offline verifier and separate staging-only fixture initializer, **not** the bot server. Default entry remains inert. Verify the actual Railway build log names `./cmd/case-shadow-once`; do not rely on the configured Dockerfile path alone, as a prior config-triggered deployment still built the old Dockerfile before a controlled redeploy.
- `case-shadow-qa-setup` is manually invoked, never a startup or cron path. It rejects anything except the exact staging Railway project/service IDs, an ephemeral `CASE_QA_SETUP_ALLOWED=1` gate, and a private DB hostname matching the dedicated database service's `RAILWAY_PRIVATE_DOMAIN` reference. It migrates that isolated DB and inserts only two synthetic evidence rows. Its output contains internal fixture scope IDs and counts, never a DSN or player identity.
- The QA service must reference **only** `Postgres-BqU0.DATABASE_URL` as `DATABASE_URL` and `Postgres-BqU0.RAILWAY_PRIVATE_DOMAIN` as `CASE_QA_DATABASE_HOST`; set variables on the QA service alone with redeploy skipped. Do not use `DATABASE_PUBLIC_URL`. Review the reference names before any one-shot execution.
- The sole authorized staging pre-deploy command, if needed for a single manual deployment, is `sh -c 'CASE_QA_SETUP_ALLOWED=1 /app/case-shadow-qa-setup'`. Retain the inert start command and restart NEVER; remove the pre-deploy command immediately after its execution and before any further redeploy. Because Railway pre-deploy commands repeat on subsequent deployments, never leave this configured unattended. No auto-execute command or enforcement flag is permitted.
- After setup: require exactly two synthetic evidence rows and zero evaluations. Run an explicit read-only preview from a reviewed one-shot path with limit 2 and the emitted fixture guild/server IDs. Do NOT carry staging hashes into production; independent readback and an explicit second execution approval are still required.

## Staging status boundary

The dedicated infrastructure and package have been created. A green CI/test of an isolated image is not a database rehearsal. Until schema seeding, preview, BLOCKED-only execution and independent exact scoped readback are performed on this dedicated QA database, staging replay remains **NOT VERIFIED**. Production replay remains **NOT VERIFIED**.

## Required design before any write

1. Do not alter the live bot service, deploy PR #112 solely to collect a record, add a public endpoint, expose a database, or use an automatic cron/retry policy.
2. First verify on isolated fixture PostgreSQL. The operator command must be compiled from a pinned reviewed commit and invoked once, with no Discord token, Nitrado token or Stripe credentials. A long-lived service that auto-runs `execute` is prohibited.
3. Production connectivity, if subsequently approved, must use a dedicated short-lived operator environment/job and scoped database account; avoid passing DSNs as CLI arguments or putting secrets in GitHub, logs or PRs. Use private networking where available. Prefer a read-only account for preview and a separate restricted writer for the single approved ledger transaction. Confirm permissions against schema 0056; reject migration/schema drift.
4. Before operator execute, document owner authorization, exact database environment, guild ID, game-server ID, bounded limit (start at 2; never above 50), tool commit, deployment baseline, evidence IDs, fingerprint and PLAN_HASH in a restricted record. Never expose names, raw ADM path, player identities, credentials or source content in public logs.
5. Preview is read-only, repeatable-read. Require `MODE=preview`, the intended exact scope, positive evidence count, fingerprint and PLAN_HASH. Stop on missing/ambiguous server scope, unexpected source/window movement, missing migration or an unavailable independent history reader.
6. Only after separate authorization, run `execute` from the same pinned binary with the same guild/server/limit, both exact expected hashes, process-local `CASE_SHADOW_ONESHOT_ALLOWED=1`, and `-ack BLOCKED_DIAGNOSTICS_ONLY`. Never persist this gate as an always-on production environment variable. A mismatch aborts.
7. Capture the command-local evaluation ID, then independently inspect the authorized scoped C.A.S.E. shadow history and evidence endpoint. Confirm BLOCKED status, four prerequisite blockers, exact evidence IDs and same server. Re-run only when the first transaction's outcome is known; an identical pinned snapshot must return the same evaluation ID and not add a record.
8. Check unchanged killfeed, Discord, Nitrado polling, source/checkpoint, production vars and runtime status. Record failure and stop on any discrepancy. Do not delete ledger or source evidence to conceal failure.

## Explicit acceptance / stop conditions

PASS requires exact-head CI, disposable-Postgres regression, reviewed migration 0056, isolated rehearsal, approved short-lived access and job, preview/execute hash match, independent two-pass readback, idempotence, and unchanged live runtime. A CLI-local readback alone is insufficient.

STOP if any of these are absent, if the selected source changes before execute, if database access is over-privileged, if the operator run can recur without another approval, if independent history access is unavailable, or if production health deviates. In particular, a failed post-commit readback may leave one durable BLOCKED row; investigate read-only instead of blindly retrying.

`CASE-MOV-001` version 0.1.0 remains BLOCKED. Date-less ADM seconds and event-triggered locations do not establish trusted elapsed time or continuous movement. This acceptance proves safe evidence-linked diagnostics only, not cheating detection. No finding, score, alert, case, ban or player action is authorized.

## Recovery and evidence retention

A preview changes no database state. If execute fails before commit, its transaction rolls back. After commit, preserve the immutable diagnostic and review it rather than deleting it. Remove the temporary operator job/credentials when finished; do not alter live service, bot variables or production delivery mode. Save private run metadata, sanitized command version, scope, hashes, evaluation ID, two readback outcomes, approval record, and deployment baseline.
