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
```

Important notes:

- `PORT` is preferred for Railway deployment.
- `HTTP_PORT` is used as a local fallback.
- `DATABASE_URL` is optional during Phase 1 and should be configured in production via Railway Variables.
- Secrets must never be committed to the repository.

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

## Current Phase 2.2 functionality

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

## Planned Phase 3

Phase 3 will implement the real DayZ event parser and kill reconstruction based on the
live log sample captured during Phase 2.2. No event parsing exists yet by design.

## Notes

This project intentionally avoids inventing undocumented Nitrado endpoints or DayZ log syntax. The architecture is designed to support future log streaming, PostgreSQL-backed persistence, and multi-server scaling without relying on local state.
