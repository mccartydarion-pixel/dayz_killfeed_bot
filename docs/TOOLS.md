# Command-line tools in `cmd/`

`cmd/server` is the bot. Everything else in `cmd/` is a small helper program that a person runs
by hand from a terminal. None of them is part of the running bot, none is started by it, and the
production Docker image (`Dockerfile`) does not contain them.

CI compiles all of them (`go build ./...`) and runs the tests that three of them have, so they
must keep building. Apart from that, nothing runs them automatically.

## At a glance

| Tool | What it is | Can it change anything real? | Status |
| --- | --- | --- | --- |
| [`nitrado-fixture`](#nitrado-fixture) | A fake Nitrado API for isolated staging | No (it is a server that refuses writes) | In use: has its own Dockerfile |
| [`shop-mission-write`](#shop-mission-write) | Guarded write of one Shop file to a game server | **Yes: writes to Nitrado**, one approved file at a time | Operator tool, still needed |
| [`nitrado-delta-probe`](#nitrado-delta-probe) | Checks whether a server supports partial log reads | No (downloads only) | Operator tool, still useful |
| [`shop-capability-probe`](#shop-capability-probe) | Reports what a game server's files allow for Shop delivery | No (downloads only) | Operator tool, still useful |
| [`case-stripe-preflight`](#case-stripe-preflight) | Checks the C.A.S.E. prices in Stripe test mode | No (reads only, test keys only) | Operator tool, needed before C.A.S.E. sales |
| [`case-discord-qa`](#case-discord-qa) | Sends one synthetic test message to a QA Discord channel | **Yes: can post one Discord message** when explicitly confirmed | Operator tool, needed before C.A.S.E. sales |
| [`shop-canary-prepare`](#shop-canary-prepare) | Printed the plan for the Shop canary's file changes | No (downloads only) | Finished experiment |
| [`shop-ledger-verify`](#shop-ledger-verify) | Acceptance check for the Shop ledger rollout | No (read-only database; one harmless API probe) | Finished experiment |
| [`case-staff-preview`](#case-staff-preview) | Prints a demo C.A.S.E. staff card as JSON | No (no network at all) | Finished experiment |
| [`testdburl`](#testdburl) | Runs the integration tests against a Railway test database | **Yes: writes to the test database it is pointed at** | Superseded by CI |

General safety notes:

- Every tool reads its credentials from environment variables in the terminal it is run in. Load
  them privately; do not put them in a file in this repository or paste them into a chat.
- The tools that talk to Nitrado, Stripe or Discord use real services. "Read-only" below means
  the tool only sends requests that read.
- A tool marked "finished experiment" still builds and can still be run, but the job it was
  written for is done.

---

## nitrado-fixture

**Purpose.** A stand-in for the Nitrado API (`internal/nitrado/nitradofixture`). It serves a
synthetic game server with synthetic log lines, so a staging copy of the bot can run the whole
log pipeline without touching Nitrado or a real game server.

**When you would run it.** When setting up an isolated staging environment. The staging bot is
started with `APP_ENV=staging` and `NITRADO_API_BASE_URL` pointing at the fixture; the bot refuses
that variable in any other environment.

**What it touches.** Nothing outside itself. It holds no Nitrado credential and answers every
write with a refusal. Its `/_fixture/...` control endpoints add synthetic kills, deaths and
restarts.

**Settings.** `PORT` (default 8080), `FIXTURE_SERVICE_ID` (default 90000001),
`FIXTURE_CONTROL_TOKEN` (the secret for the control endpoints), `FIXTURE_KILLS_PER_MINUTE`
(default 0).

**Safety notes.** Set `FIXTURE_CONTROL_TOKEN`: without it the control endpoints are open to anyone
who can reach the service (the tool warns at start-up). Never deploy it next to production.

**Status: in use.** Built by `Dockerfile.nitrado-fixture`. The package behind it is also used by
the bot's own end-to-end tests. Background:
[incidents/2026-09-26-staging-infrastructure.md](incidents/2026-09-26-staging-infrastructure.md).

## shop-mission-write

**Purpose.** The one hand-operated way to write a file to a game server for the Shop: create the
Champion spawner file, point the server's `cfggameplay.json` at it, put one bought item into the
file (stage) or take it out again (unstage), or restore a saved copy of `cfggameplay.json`.

**When you would run it.** To set a server up for Shop delivery by hand, to deliver or clean up
one order by hand, or to put the configuration back after a problem. The automatic delivery worker
does the staging and unstaging itself when it is switched on; this tool remains the manual path.

**What it touches.** Without `-execute` it only reads the server and prints a plan and a plan ID.
With `-execute -authorize <plan ID>` it **writes one file to the game server through Nitrado**. For
stage and unstage it also reads one order from the database (`DATABASE_PUBLIC_URL`, opened
read-only). It writes a journal and a backup copy on the operator's own computer. It never restarts
a server and never writes the database.

**Safety notes.**

- Dry run is the default. A write needs the exact plan ID from a dry run, and a plan ID works once.
- It checks the file's current content, the server binding and the upload address before writing,
  reads the file back afterwards, and reports `WRITTEN_VERIFIED`, `NOT_WRITTEN` or `UNCERTAIN`. It
  never retries. On `UNCERTAIN`: stop, look, and record the outcome with `-resolve`.
- The payload is fixed by the operation; the tool never uploads a file you hand it.
- The two oldest operations (`gate-a-create-empty`, `gate-b-reference`) are completed and refused.
- Each write is meant to have the owner's approval first.

**Status: operator tool, still needed.** It is also required by a test:
`TestWriteCapabilityIsIsolated` (`internal/shop/missionwrite`) fails if this tool does not exist,
because the test proves that only this tool and the delivery worker can reach the write code.
Manuals: [SHOP_GATE_A_UPLOAD.md](SHOP_GATE_A_UPLOAD.md) (protocol, journal, exit codes),
[SHOP_CANARY_STAGING.md](SHOP_CANARY_STAGING.md) (stage and unstage),
[archive/SHOP_CUSTOM_RELOCATION.md](archive/SHOP_CUSTOM_RELOCATION.md) (create and relocate).

## nitrado-delta-probe

**Purpose.** Checks whether one Nitrado server supports reading only the new part of a log file,
and compares every partial read byte for byte with a full download.

**When you would run it.** Before setting `NITRADO_DELTA_READ_MODE` to anything other than `off`
for a server.

**What it touches.** Nitrado, read-only (`NITRADO_TOKEN`): it lists and downloads log files. It
prints neither the token nor any download address.

**Safety notes.** It downloads the newest log in full, so it costs a few Nitrado requests. If it
reports a byte mismatch, do not enable partial reads for that server.

**Status: operator tool, still useful** for as long as the partial-read option exists. Partial
reads are off by default. Manual: [NITRADO_DELTA_READS.md](NITRADO_DELTA_READS.md).

## shop-capability-probe

**Purpose.** Looks at one game server's files and reports, as JSON, what is there: the mission
folder, the configuration files, and whether the server can take the Shop's spawner file.

**When you would run it.** Before preparing a new server for Shop delivery, or when delivery or
map rotation fails on a server and you need to see how its files are laid out.

**What it touches.** Nitrado, read-only (`NITRADO_TOKEN`). It prints a sanitized report: no token,
no download address, no file contents, no account path.

**Safety notes.** The `-org`, `-installation`, `-game-server` and `-service` values must match the
installation's stored binding; the tool refuses a server that does not match.

**Status: operator tool, still useful.** The code it calls (`internal/shop/capability`) is used by
the bot itself. Background: [archive/SHOP_DELIVERY_PHASE2C1.md](archive/SHOP_DELIVERY_PHASE2C1.md).

## case-stripe-preflight

**Purpose.** Checks that the two C.A.S.E. add-on prices (Watch and Pro) exist in a Stripe **test**
account with the right amount, interval and labels. Optionally also checks the base plan catalog
and the staging webhook.

**When you would run it.** Before testing C.A.S.E. checkout in staging, and again before C.A.S.E.
is ever put on sale.

**What it touches.** Stripe, read-only (`CASE_STRIPE_TEST_SECRET_KEY`). It creates and changes
nothing.

**Safety notes.** It refuses any key that is not a test key (`sk_test_` or `rk_test_`). The price
and product ids it checks by default are written into the program as flag defaults; pass
`-watch-price`, `-watch-product`, `-pro-price` and `-pro-product` if the test account changes.

**Status: operator tool, needed before C.A.S.E. sales.** C.A.S.E. billing is built but switched
off. Manual: [CASE_STRIPE_TESTMODE_QA.md](CASE_STRIPE_TESTMODE_QA.md).

## case-discord-qa

**Purpose.** Proves that a separate QA bot can deliver a message to one private QA channel, by
sending a single clearly labelled synthetic message and reading it back.

**When you would run it.** Before turning on the paid C.A.S.E. Watch digest, to test Discord
delivery without using the production bot or real data.

**What it touches.** Discord, with a **separate QA bot token** (`CASE_DISCORD_QA_BOT_TOKEN`). Its
default mode (`preflight`) and `verify-existing` only read. `send-once` posts exactly one message.
No Stripe, no Nitrado, no database, no player data.

**Safety notes.**

- Sending needs two confirmations at once: an exact phrase on the command line and an exact value
  in the environment (`CASE_DISCORD_QA_ALLOW_SEND`).
- It refuses to send if the QA bot is the production bot, and it needs the production server and
  bot ids so it can refuse them. Using a channel inside the production server needs a second pair
  of confirmations and a list of production channels to refuse.
- It never retries. If the result is unclear, look for the message by its reference; do not send
  again.

**Status: operator tool, needed before C.A.S.E. sales.** The bot keeps one function only for this
tool (`VerifyCaseSyntheticQAChannel` in `internal/discord`). Manual:
[CASE_DISCORD_DELIVERY_RECONCILIATION.md](CASE_DISCORD_DELIVERY_RECONCILIATION.md).

## shop-canary-prepare

**Purpose.** Printed the exact proposed changes for the first hand-run Shop delivery (the
"canary"): the empty spawner file, the change to `cfggameplay.json`, and the order of steps.

**When you would run it.** It was run while preparing the canary. It would only be useful again
when preparing another server by hand in the same way.

**What it touches.** Nitrado, read-only (`NITRADO_TOKEN`). It never uploads and never asks for an
upload permission.

**Safety notes.** Read-only. Part of its output is about one specific custom map file on the
original canary server, which means nothing on another server.

**Status: finished experiment.** The canary it prepared is recorded as completed
([SHOP_DELIVERY_WORKER_DESIGN.md](SHOP_DELIVERY_WORKER_DESIGN.md)), and `shop-mission-write`'s dry
run prints the same proposals for the operations it performs. Background:
[archive/SHOP_DELIVERY_PHASE2C2.md](archive/SHOP_DELIVERY_PHASE2C2.md).

## shop-ledger-verify

**Purpose.** A one-time acceptance check run right after the Shop delivery ledger was first
deployed: the two database changes were applied once, nothing else changed, and the canary lock
was closed.

**When you would run it.** It was for that rollout. Run today it would report failures that are
not problems: it expects that no delivery attempt exists yet, and later deliveries create them.

**What it touches.** The database, in a read-only transaction (`DATABASE_PUBLIC_URL` or
`DATABASE_URL`). With `-api` it also calls the bot's canary API twice using `WEBSITE_API_SECRET`:
one read, and one request for a delivery that cannot exist, which is expected to be refused.

**Safety notes.** It cannot write to the database. The API probe is built so that it creates
nothing even if the lock were open. Its default organization and installation numbers are those of
the original rollout.

**Status: finished experiment.** Background:
[archive/SHOP_LEDGER_ROLLOUT.md](archive/SHOP_LEDGER_ROLLOUT.md).

## case-staff-preview

**Purpose.** Prints one fixed, made-up C.A.S.E. staff card as Discord-style JSON, to look at the
design.

**When you would run it.** Only to see that early demo card.

**What it touches.** Nothing. No network, no credentials, no input.

**Safety notes.** None needed. The card is marked as a demo.

**Status: finished experiment.** The real staff alert card is a different, later design
([CASE_STAFF_ALERTS.md](CASE_STAFF_ALERTS.md)). The demo card function exists in the bot's code
only for this tool. Background:
[archive/CASE_CORE_OFFLINE_STAFF_PREVIEW.md](archive/CASE_CORE_OFFLINE_STAFF_PREVIEW.md).

## testdburl

**Purpose.** Builds a test-database address from Railway's variables, checks that it is not the
same database as `DATABASE_URL`, connects, and then runs the whole integration test suite against
it.

**When you would run it.** It predates CI. CI now runs the same integration tests on every change
against a throwaway database, so there is normally no reason to run this.

**What it touches.** **The database it is pointed at**: the integration tests create tables and
write rows there. It reads `PGUSER`, `PGPASSWORD`, `PGDATABASE`, `RAILWAY_TCP_PROXY_DOMAIN`,
`RAILWAY_TCP_PROXY_PORT` and `DATABASE_URL`.

**Safety notes.** Its only guard is that it refuses to run when the test database and
`DATABASE_URL` are the same host, port and database name. It trusts that whatever the `PG...`
variables point at is disposable. Never run it with those variables pointing at a real database.
It hides passwords in what it prints.

**Status: superseded by CI.** Nothing refers to it: no Dockerfile, no workflow, no document
before this one, no test.
