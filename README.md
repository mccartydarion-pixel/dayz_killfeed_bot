# Champion bot (DayZ killfeed)

The backend of Champion: a Discord bot and HTTP API for DayZ console servers hosted on Nitrado.
It is one Go program (`cmd/server`) that:

- reads each connected server's admin log from Nitrado and turns it into kills, deaths, hits,
  connections, positions and build actions;
- posts and maintains the Discord side: killfeed and other feeds, leaderboards, bounties, ranked
  seasons, panels, slash commands and the `/setup` channel layout;
- stores everything in PostgreSQL (the bot owns the database schema and its migrations);
- serves the HTTP APIs the Champion website uses: the customer API (`/api/saas/...`), the Owner
  Hub API (`/api/admin/...`) and a runtime status API.

It runs many Discord servers and game servers in one process, each with its own isolated worker
and its own encrypted Nitrado credential.

**Documentation: [docs/README.md](docs/README.md)** is the index of every document, grouped by
area. The helper programs in `cmd/` are described in [docs/TOOLS.md](docs/TOOLS.md).

## Requirements

- Go 1.25 or newer
- PostgreSQL (16 is what CI uses)
- A Discord bot token

## Configuration

All configuration is environment variables. [`.env.example`](.env.example) lists every variable
the bot reads, with a comment and its default. For local development:

```bash
cp .env.example .env
# edit .env
```

The bot loads `.env` at start-up if the file exists. In production the same variables are set in
the hosting platform (Railway), never in a file.

- Only `DISCORD_TOKEN` is required to start.
- `DATABASE_URL` is needed for almost everything beyond starting up.
- `CREDENTIAL_ENCRYPTION_KEY` is required for `/server connect`. Generate it once, store it as a
  deployment secret and do not change it: the stored Nitrado credentials are encrypted with it.
- `PORT` is used when set (Railway sets it); `HTTP_PORT` is the local fallback. The server binds
  `0.0.0.0`.
- Feature switches are off by default. A blank value always means "default" or "off".

Never commit a real token, key, password or id. `.env` is ignored by git.

## Run

```bash
go run ./cmd/server
```

Database migrations run automatically at start-up (`internal/database/migrations.go`).

Useful endpoints once it is running:

| Endpoint | What it is |
| --- | --- |
| `GET /health` | Liveness. Makes no external call. |
| `GET /ready` | Readiness, including worker health. |
| `GET /api/v1/status` | Sanitized live runtime state. |

## Docker

```bash
docker build -t champion-bot .
docker run --rm -p 8080:8080 --env-file .env champion-bot
```

`Dockerfile` builds only `cmd/server`. `Dockerfile.nitrado-fixture` builds the synthetic Nitrado
API used for isolated staging (see [docs/TOOLS.md](docs/TOOLS.md)); never deploy it next to
production.

## Tests

The normal suite needs nothing but Go:

```bash
gofmt -l .        # must print nothing
go build ./...
go vet ./...
go test ./...
```

The integration tests need a **disposable** PostgreSQL database. They migrate it and write to it,
so never point them at a real one. They only run when all of this is set:

```bash
export TEST_DATABASE_URL="postgres://user:password@localhost:5432/champion_test?sslmode=disable"
export ALLOW_INTEGRATION_DB_TESTS=true
go test -tags integration -p 1 -count=1 ./...
```

`-p 1` matters: every package migrates and writes the same database. With `REQUIRE_INTEGRATION_DB=1`
a missing database is a failure instead of a skip.

CI (`.github/workflows/backend-ci.yml`) runs exactly these steps on every pull request and on
`main`, plus race tests for `internal/discord`, `internal/killfeed` and `internal/app`, against a
throwaway PostgreSQL 16.

Checks against real Discord, Nitrado or Stripe are not part of the test suite. They need real
credentials and a dedicated test server; the operator tools for them are in
[docs/TOOLS.md](docs/TOOLS.md).

## Layout

| Path | What is there |
| --- | --- |
| `cmd/server` | The bot. |
| `cmd/*` (everything else) | Operator and test helpers, see [docs/TOOLS.md](docs/TOOLS.md). None of them is part of the bot. |
| `internal/app` | Start-up, workers and every HTTP handler. |
| `internal/killfeed`, `internal/nitrado`, `internal/livesync` | Reading and parsing the server logs. |
| `internal/discord`, `internal/presentation` | Discord publishers, commands and card design. |
| `internal/repository`, `internal/database` | SQL and migrations. |
| `internal/config` | Environment variables. |
| `docs/` | Documentation; start at [docs/README.md](docs/README.md). |

## Principles the code keeps to

- Nothing is invented: the bot only reports what the server log or an official Nitrado endpoint
  actually says.
- Every customer's data is scoped to its own organization, installation and game server.
- Anything that writes to a game server, charges money or messages players is off until an owner
  switches it on.
