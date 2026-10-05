# Deploys

How a release reaches production, where the time goes, what the bot checks about itself after it
starts, and how it shuts down. Production is one Railway service (`dayz_killfeed_bot`, one
replica) built from the `Dockerfile` at the repository root.

## Where the 11 minutes go

Measured on 5 October 2026 from Railway's deployment record, the Railway build log, the bot's own
log and the GitHub Actions run of the same commit (`0d66f2d`). Times are UTC.

| When | What | Took |
| --- | --- | --- |
| 11:01:05 | Push to `main`; Railway creates the deployment and **waits for CI** | |
| 11:01:06 - 11:10:08 | GitHub Actions `backend-ci` on the pushed commit | 9 min 02 s |
| 11:10:48 | Railway starts the build (40 s after CI finished) | |
| 11:10:48 - 11:11:48 | Docker build and image push | 60 s |
| 11:11:55.8 | New process connects to the database | |
| 11:11:57.2 | New process is ready (migrations checked, Discord connected, 22 commands registered, worker started) | 1.5 s |
| 11:11:57.9 | Railway's health check passes on the first try | |
| 11:12:01 | Deployment `SUCCESS` | 10 min 56 s in all |
| 11:12:02.6 - 11:12:03.6 | Old process receives SIGTERM and shuts down cleanly | 1.0 s |

So **9 min 43 s of the 10 min 56 s (89 %) is Railway waiting for GitHub Actions**, not building.
The second deployment measured that day (10:21:42 to 10:32:31) has the same shape: its CI run took
10 min 04 s.

Inside the CI run (same commit): job set-up 40 s, format 1 s, build 10 s, vet 11 s, unit tests
24 s, race tests 21 s, **integration tests 7 min 11 s**.

Inside the 60 s Docker build: base images and `go mod download` came from Railway's layer cache
(0 s), `COPY . .` 1.2 s, `go build` 22 s, copying the binary into the runtime image 17 s, export
and push 16 s.

The two processes overlapped for about 8 seconds (11:11:55.8 to 11:12:03.6). During an overlap
only one of them runs the boards, reminders and schedulers (the leader lock,
[MULTI_PROCESS.md](MULTI_PROCESS.md)); the new process asks for the lock every 15 seconds, so it
takes those over at most 15 seconds after the old one lets go.

## What was changed in the repository

- **`.dockerignore`** (new). The build context no longer includes `docs/`, `*.md`, `.github/`,
  tests (`*_test.go`, `testdata/`) or `.env*`. A commit that only touches those leaves the
  `COPY . .` layer unchanged, so Railway reuses the cached `go build` layer (22 s measured) if it
  builds at all. Nothing in the binary reads those files (there is no `go:embed`).
- **CI runs as four parallel jobs** (`.github/workflows/backend-ci.yml`) instead of one after the
  other: `test` (format, build, vet, unit and race tests - no database) and the integration tests
  in three groups by package (`internal/repository`, `internal/factionstats`, every other
  package), each with its own throwaway PostgreSQL. The same commands run on the same code; the
  three groups together are exactly `./...`. Railway waits for the workflow run as a whole, so
  the run now takes as long as its slowest job instead of the sum. **Estimated, not yet
  measured on GitHub:** about 5 to 6 minutes instead of 9 to 10, which would bring a deploy to
  about 7 minutes. The estimate comes from running the three groups here on fresh databases:
  `internal/repository` 262 s, every other package 200 s (`internal/app` is 70 s of it),
  `internal/factionstats` 43 s.
- What still holds the run at 5 minutes is **one test**:
  `TestQueryHygieneIndexesServeTheirQueries` in `internal/repository` took 263 of that package's
  308 seconds here. Nearly all of it is its clean-up, one `DELETE FROM guilds` that cascades
  through the rows the test seeded. It was left alone in this change (the repository package was
  being worked on elsewhere); making that clean-up cheap would bring CI to about 3.5 minutes and
  a deploy to about 5.
- **`time/tzdata` is embedded in the server binary** (`cmd/server/main.go`). The runtime image is
  `alpine:3.20`, which has CA certificates (TLS to Discord, Nitrado and Stripe works) but **no
  time zone database**; the heatmap API's `tz` parameter (`time.LoadLocation`) therefore refused
  every zone except UTC in production. The image itself is unchanged.

Not changed, on purpose:

- **BuildKit cache mounts** (`RUN --mount=type=cache`). Railway supports them only with an id of
  the form `id=s/<service id>-<path>`, with the service id written into the Dockerfile. This
  Dockerfile also builds the staging service, which has another id, and the most a warm Go build
  cache could save is part of the 22-second compile, about 3 % of a deploy. The risk of a failed
  build is not worth it.
- **The runtime image.** Still `alpine:3.20` with a non-root user.

## What only the owner can change (Railway dashboard)

These are settings of the `dayz_killfeed_bot` service. Their values on 5 October 2026 were read
from Railway, not guessed.

| Setting (Service > Settings) | Now | Suggestion |
| --- | --- | --- |
| **Wait for CI** (Source) | on | This is the 9-10 minutes. Turning it off makes a deploy take about 75 seconds. It is only safe if nothing reaches `main` without passing CI first: protect the branch in GitHub (Settings > Branches > require the `backend-ci` checks and "Require branches to be up to date before merging"). Every merge that day went through a pull request whose own CI run had already passed, so the run Railway waits for is a repeat. |
| **Watch Paths** (Build) | none | Add `/cmd/**`, `/internal/**`, `/go.mod`, `/go.sum`, `/Dockerfile`, `/.dockerignore`. A commit that changes none of these (documentation, CI, README) then does not deploy at all, so the bot is not restarted for it. With watch paths set, an empty commit no longer triggers a deploy; use "Deploy Latest Commit" in Railway instead. |
| **Healthcheck Path** (Deploy) | `/health` | Consider `/ready`. `/health` answers 200 as soon as the HTTP server listens. That is after start-up finished (below), so today it works as a readiness check, but it also answers 200 when the database could not be reached. `/ready` answers 200 only when the database and Discord are connected and the commands registered; with it, a release that cannot reach the database never replaces the running one. |
| **Healthcheck Timeout** | 120 s | Keep. Start-up takes about 2 s. |
| **Draining seconds** (`drainingSeconds`) | 30 | Keep. This is how long Railway waits after SIGTERM before it kills the old process; the bot needs about 1 s and bounds itself to 20 s. |
| **Overlap seconds** (`overlapSeconds`) | 0 | Keep at 0: a longer overlap means doubled killfeed posts ([MULTI_PROCESS.md](MULTI_PROCESS.md)). |
| **Replicas** | 1 (Amsterdam) | Keep at 1. |

The builder is shown as Railpack, but Railway uses the `Dockerfile` it finds at the repository
root (the build log starts with "load build definition from Dockerfile").

## Start-up and readiness

The HTTP server starts listening **last**: after the database is connected and migrated, the
leader election started, Discord connected, every command registered and every server worker
started. Until then Railway's health check gets no answer and keeps the old deployment serving.
A process that fails before that point (a migration error, Discord refusing the token) exits, and
Railway keeps the old deployment.

| Endpoint | Answers 200 when |
| --- | --- |
| `GET /health`, `GET /live` | the HTTP server is listening (start-up finished) |
| `GET /ready` | the database and Discord are connected and the required commands registered; 503 otherwise |

What "ready" does not prove: that the server workers have read a log yet (their first Nitrado
read happens seconds later), or that this process holds the leader lock (during the overlap the
old process still does). The self-check covers both.

## The deploy self-check

`internal/app/deploy_selfcheck.go`. After start-up the process checks itself every 5 seconds and
logs **one line** as soon as everything is in place:

```
deploy self-check HEALTHY: commit 0d66f2dfcdf8, 126 migrations applied, leadership LEADER, Discord gateway connected, 1/1 server workers started, 1 reading logs, ready 1.7s after start
```

(`component=startup`, `event=deploy_self_check`, with the same facts as separate fields:
`commit`, `migrations_applied`, `leadership`, `discord_gateway`, `workers_expected`,
`workers_started`, `workers_reading`, `ready_ms`, `healthy_after_s`.)

| Fact | Where it comes from | Healthy when |
| --- | --- | --- |
| commit | `RAILWAY_GIT_COMMIT_SHA` (else the Go build stamp) | always shown |
| migrations applied | rows in `schema_migrations`, counted after migrating | counted |
| leadership | the leader lock ([MULTI_PROCESS.md](MULTI_PROCESS.md)) | `LEADER`, or `NO_LOCK` (`SINGLETON_LEADER_LOCK=off`). `STANDBY` (another process holds the lock) and `ERROR` (the lock cannot be reached) are not |
| Discord gateway | the session's ready flag | connected |
| server workers started | workers running, against the active servers found at start-up | all of them |
| reading logs | workers whose last poll of the server log completed without a Nitrado failure streak | all started workers |
| ready | process start to "about to serve HTTP" | always shown |

Just after a deploy the line normally appears within about 20 seconds: the new process is
`STANDBY` until the old one exits and it takes the lock at its next 15-second attempt.

**If it is still not healthy 5 minutes after the process started**, it logs the same line at
error level with `result=UNHEALTHY` and a `problems` list, and tells the platform admins by
Discord DM - the Owner Hub's alert path, behind the same `alertsEnabled` switch as incidents
([OWNER_OPS.md](OWNER_OPS.md)). It then keeps checking for an hour; if the process recovers it
logs `result=RECOVERED` and, if it had sent the alert, says so in a second DM.

Limits, stated plainly: the alert is a Discord DM, so if the Discord gateway itself is what is
down it cannot be delivered and only the log line exists. Nothing is stored in the database; the
check belongs to the process and starts again with the next one. With `alertsEnabled` off (the
default) there is the log line and the status endpoint, no DM.

The same facts, live, are the `deploy` block of `GET /api/runtime/status`
([runtime-status-api.md](runtime-status-api.md)).

## Shutdown

Railway sends SIGTERM and kills the container `drainingSeconds` (30) later. On SIGTERM (or
Ctrl-C) the bot:

1. stops accepting HTTP requests at once (in-flight requests get up to 10 s);
2. gives up the leader lock, so the next process takes the singleton work over while this one is
   still flushing;
3. stops every server worker (at most 12 s);
4. drains the per-server persistence queues to the database;
5. waits for the killfeed and death feeds to post what they have queued - **bounded**: the whole
   of steps 2 to 5 gets 20 s, and the feeds always get at least 2 s;
6. closes the Discord session and the database pool, and logs `shutdown complete` with
   `duration_ms`.

Measured in production: 1.0 s from SIGTERM to `shutdown complete`.

What a shutdown can still lose:

- Kill and death cards in **immediate** delivery mode are journalled and replayed by the next
  process. In **rotating** mode (the default) a batch that cannot be posted inside the bound is
  not posted; the kills themselves are stored. Before this bound existed a stuck Discord request
  could hold the shutdown until Railway killed the process, with the same loss and without the
  database being closed cleanly.
- Staff alerts waiting in the in-memory admin-alert queue are dropped, and the economy and bounty
  feeds flush on shutdown but nothing waits for them, so a flush that takes longer than the rest
  of the shutdown is cut off. Both are small queues that are normally empty; making them durable
  is a larger change and has not been done.
- Slash commands that arrive in the last second may be answered by either process, or get
  Discord's "interaction failed" if the old process had already closed its session. The new
  process is connected before the old one is told to stop, so this window is the ~1 s shutdown.
