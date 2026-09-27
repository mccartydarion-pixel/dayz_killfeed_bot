# C.A.S.E. 2G.4 — Isolated operator-job readiness and acceptance

Status: **NOT APPROVED FOR PRODUCTION EXECUTION**. This is an operator plan, not an activation. The existing `cmd/case-shadow-once` remains an offline tool, with no bot startup hook, public route, cron, or automatic enforcement.

## Baseline observed 2026-09-27

- Production bot: Railway project `genuine-education`, service `dayz_killfeed_bot`, successful deployment of `main` at `9d4d9c8889500d3a3c561a11af457996def51c99` (Phase 2G.3).
- The 2G.4 utility is on PR #112, head `c22ae997a6039435fb366f3f34ba7a0c18f82dc2`; backend-ci run 36301939601 succeeded.
- Railway exposes no existing operator one-shot command execution in this project; connector does not offer SQL inspection or raw credentials. No production shadow replay has been performed.
- Separate `champions-case-staging` project exists. Its reliability QA bot, QA PostgreSQL and Nitrado fixture have no reported deployment; the existing billing QA service and Postgres must not be repurposed.

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
