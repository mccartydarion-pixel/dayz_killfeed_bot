# C.A.S.E. Phase 2G.4 — One-shot operator verification (NOT activated)

This standalone Go command is an optional, offline operator tool. It is NOT a new HTTP endpoint, startup hook, cron job, GitHub Action, Railway service, or production variable. It uses only an operator-supplied PostgreSQL URL. Never paste the URL into GitHub, PR discussion, Discord or a shell argument. It does not run migrations.

## Two-step flow

1. From an operator machine that already has approved database connectivity, run:
   `DATABASE_PUBLIC_URL=<from secure local environment> go run ./cmd/case-shadow-once -mode preview -guild <database guild id> -server <database game server id> -limit 2`
   Preview uses a read-only repeatable-read transaction. Record the printed scope, evidence count and 64-character fingerprint. It does not print player names, raw ADM paths, or DB credentials, and it does not write a ledger row.

2. The separately authorized operator may invoke the same binary with `-mode execute -expected-fingerprint <exact preview fingerprint> -ack BLOCKED_DIAGNOSTICS_ONLY` and a *process-local* `CASE_SHADOW_ONESHOT_ALLOWED=1` setting. The same `-guild`, `-server` and `-limit` values MUST be retained. The command selects the live bounded window again; a changed fingerprint aborts. It validates the selected IDs twice, inserts only BLOCKED prerequisite diagnostics using the 2G.2 ledger writer, then reads back and checks the evidence-ID set. No score, finding, alert, case, Discord message or enforcement is created. It exits after one attempt; it has no persistent gate or recurring schedule.

3. For independent acceptance, use the ordinary authorized C.A.S.E. Detectors dashboard's shadow history plus scoped Evidence tab. Repeat the same command with exactly the same fingerprint: the printed evaluation ID should be unchanged, and record count should not increase. This last step requires an operator with appropriate permissions and is NOT established by command-local readback alone.

Never place database credentials, fingerprint confirmations or operator secrets into a GitHub Action unless the corresponding execution environment is explicitly reviewed and authorized. The existing backend CI has no production secrets and must stay that way.

## Important limitations

The command is not executable through the current Railway connector. It requires a separately approved operator environment with DB access. This PR only adds an operator utility and tests; it does not establish that any real production shadow evaluation has run. An external agent must not execute it merely because it is available. Deployment of the bot is not necessary to execute this command from a checked-out, verified source. Do not merge/deploy solely to create a production evaluation record.

Although a preflight fingerprint protects the selected evidence IDs, new ingested records can arrive immediately after the final preflight; this is a pinned diagnostic *snapshot*, not proof of whole-source completeness or continuous movement. The operator must document any source/window movement and abort if the scope has changed before execution. ADM clocks remain date-less seconds and event-triggered positions are not validated movement samples. CASE-MOV-001 stays BLOCKED regardless of result.

The operator should preserve the command version, limited IDs and fingerprint, two exact scoped readbacks, deployment status, and authorization record in a secure runbook. Never print a raw source path, player verdict or credentials in the public log.

## Rollback

The command changes no service, Nitrado or Discord configuration. If the process fails before the DB transaction commits, no evaluation is added. If a post-commit readback fails, the BLOCKED record may already be durable; inspect it read-only rather than retrying blindly. Avoid deleting or editing evidence to disguise a failed verification. The ledger's unique fingerprint ensures repeat calls of the same valid snapshot do not add records.
