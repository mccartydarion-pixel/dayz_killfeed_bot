# DayZ Killfeed

A Phase 1 foundation for a DayZ killfeed backend built for Nitrado-hosted PlayStation DayZ servers. The project is designed for Railway deployment, PostgreSQL-ready persistence, and a modular future killfeed pipeline.

## Purpose

This project is the starting point for a reliable DayZ killfeed system that will eventually:

- discover Nitrado game servers
- connect to the appropriate DayZ server
- acquire and process log data incrementally
- normalize events into a killfeed model
- publish events to Discord
- persist state and world data in PostgreSQL

## Requirements

- Go 1.25+
- PostgreSQL-ready architecture
- Discord bot integration
- Nitrado API integration
- Structured logging with `log/slog`
- Railway-friendly config and runtime behavior

## Environment variables

Copy `.env.example` to `.env` for local development, then fill in values as needed.

```env
APP_ENV=development
HTTP_PORT=8080
PORT=8080

DISCORD_TOKEN=
DISCORD_APPLICATION_ID=
DISCORD_GUILD_ID=

NITRADO_TOKEN=
NITRADO_SERVICE_ID=

KILLFEED_CHANNEL_ID=

DATABASE_URL=
CREDENTIAL_ENCRYPTION_KEY=

DISCORD_PRESENCE_ENABLED=true
DISCORD_PRESENCE_ROTATION_SECONDS=45
DISCORD_PRESENCE_MODE=dynamic
```

Discord bot presence (all optional - these are the defaults if unset):

- `DISCORD_PRESENCE_ENABLED` - set to `false` to disable the bot's Discord activity/status entirely.
- `DISCORD_PRESENCE_ROTATION_SECONDS` - how often the presence rotates in dynamic mode. Clamped to 30-300 seconds; missing or invalid values fall back to 45.
- `DISCORD_PRESENCE_MODE` - `static` or `dynamic`.
- `HEATMAP_DISCORD_INTERVAL_MINUTES` - how often the Discord PvP heatmap summary refreshes. Clamped to 5-1440 minutes; missing or invalid values fall back to 30.
  - `static`: always shows **Competing in Competitive DayZ**.
  - `dynamic`: rotates through Competing in Competitive DayZ, live player/server counts (only when a server is actually connected - never a fabricated 0), Playing Champions® Killfeed, Watching Live PvP Activity, and Competing in DayZ Leaderboards. The bot's Discord status (online/idle/dnd) also reflects overall application health, debounced so a brief blip never flaps it.

Important notes:

- `PORT` is preferred for Railway deployment.
- `HTTP_PORT` is used as a local fallback.
- `DATABASE_URL` is optional during Phase 1 and should be configured in production via Railway Variables.
- `CREDENTIAL_ENCRYPTION_KEY` is required for `/server connect`. It must be a stable 32-byte
    value or base64-encoded 32-byte value. Generate one once, store it as a deployment secret,
    and do not rotate it casually because existing encrypted Nitrado credentials depend on it.
- Secrets must never be committed to the repository.

PowerShell example for generating a base64 key:

```powershell
[Convert]::ToBase64String([byte[]](1..32 | ForEach-Object { Get-Random -Maximum 256 }))
```

Set the resulting value as `CREDENTIAL_ENCRYPTION_KEY` in Railway or the deployment
environment, then redeploy the bot. `/server connect` will be enabled after startup.

## Local development

```bash
go mod tidy
cp .env.example .env
# edit .env with your values
./dayz-killfeed
```

## Run

```bash
go run ./cmd/server
```

## Build

```bash
go build ./...
```

## Docker usage

```bash
docker build -t dayz-killfeed .
docker run --rm -p 8080:8080 --env-file .env dayz-killfeed
```

## Current Phase 4.9 functionality

- typed configuration with startup validation (`PORT` preferred for Railway, `HTTP_PORT` local fallback)
- structured logging via `slog` (`LOG_LEVEL=debug` enables checkpoint debug logs)
- Nitrado REST client with retries and error handling
- Nitrado service discovery, DayZ service matching, and configured-service verification
- sanitized recursive inspection of the live service payload for file/log capability fields
- conservative log candidate discovery (filename, path, size, modified, inferred type)
- Discord bot connection with guild, killfeed channel, and permission validation
- HTTP server with `/health` (no external calls) and `/api/v1/status` (live sanitized runtime state)
- killfeed polling engine with incremental reads, offset tracking, duplicate prevention,
  partial-line buffering, truncation and rotation detection
- graceful shutdown using `signal.NotifyContext`
- Railway-aware `PORT` and `0.0.0.0` binding behavior
- multi-server runtime with one isolated worker, parser, tracker, persistence queue,
    and guild-scoped Nitrado credential per active `game_servers` row
- secure `/server connect`, `/server services`, `/server select`, `/server disconnect`,
    `/server repair`, and `/server status` onboarding flow
- guild-scoped competitive commands, persistent welcomer configuration, and pending-only
    account linking with destructive-action confirmation
- `/ready` readiness endpoint and worker health reporting

## Validation

The normal suite is self-contained:

```bash
go test ./...
go vet ./...
go build ./...
```

PostgreSQL integration tests are explicit and never use an unspecified database:

```bash
$env:TEST_DATABASE_URL = "postgres://.../dayz_killfeed_test"
$env:ALLOW_INTEGRATION_DB_TESTS = "true"
go test -tags=integration ./...
```

Live Discord and Nitrado verification still requires real credentials and a dedicated
test guild/service. The application reports those dependencies through `/ready` and
`/api/v1/status`; they are not fabricated as passing in local CI.

## Notes

This project intentionally avoids inventing undocumented Nitrado endpoints or DayZ log syntax. The architecture is designed to support future log streaming, PostgreSQL-backed persistence, and multi-server scaling without relying on local state.
