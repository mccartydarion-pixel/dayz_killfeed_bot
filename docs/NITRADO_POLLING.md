# Nitrado polling: rate-limit telemetry and adaptive speed

## What Nitrado offers

Nitrado's REST API (`api.nitrado.net`) has no push, websocket or webhook for game logs. The ADM and
RPT files are read through the gameserver file server:

| Endpoint | What it does |
| --- | --- |
| `file_server/list` | List a directory, with each file's size and modified time. |
| `file_server/stat` | Stat several files. |
| `file_server/size` | Get one file's size. |
| `file_server/download` | Get a signed URL for the whole file. |
| `file_server/seek` | Get a signed URL for a byte range (`offset`, `length`, `mode=raw\|lines`). |

Every API response carries `X-RateLimit-Limit`, `X-RateLimit-Remaining` and `X-RateLimit-Reset`
for the token, and Nitrado answers `429` once the budget is spent. This is what Nitrado's official
PHP client (`nitrado/NitrAPI-PHP`) reads.

## Telemetry (`internal/nitrado/ratelimit.go`)

The bot records the rate-limit headers from the requests it already makes. It never sends a
request of its own just to measure, so measuring costs nothing.

Each token is tracked under a 12-character SHA-256 prefix (`nitrado.TokenKey`). The token itself is
never stored or logged.

Kept per token:

- the last limit, remaining count and reset time Nitrado reported;
- API requests in the last hour, broken down by operation (`gameservers/file_server/list`, …);
- `429` responses;
- signed-URL downloads.

Where to see it:

- **Owner Hub API:** `GET /api/admin/nitrado-usage` (platform admins only). It returns every token
  plus the poll settings in effect.
- **Logs:** each token gets an `event=rate_budget` line at most every 5 minutes.
- **Warning:** `event=rate_budget_low` is logged once when less than 20% of the budget is left.

## Fewer requests per tick

A poll tick can list the selected log's directory up to three times: the boot scan, the newer-log
check, and the size/modified check. A listing is now reused for 750 ms, so those three cost one
request.

Some calls always skip the cache and ask Nitrado:

- Writes (upload token, upload, mkdir) clear the cache.
- `ListEntries`, which shop deliveries use to read back what they wrote.

## Adaptive poll rate (`Engine.pollingInterval`)

| Situation | Poll rate |
| --- | --- |
| Token budget low (under 20% left, before the reset time) | `max(3 × base, 30s)` |
| Log changed in the last 5 minutes, and the budget is known with at least half left | fast rate (`NITRADO_POLL_INTERVAL_FAST`, default `3s`; `off` disables it) |
| Otherwise | base rate (`NITRADO_POLL_INTERVAL`, default `10s`) |

The fast rate is only used after Nitrado's headers have been seen. If Nitrado never sends them, the
bot polls exactly as before.

## Not changed

**Delta reads** (`NITRADO_DELTA_READ_MODE`) are still off. They would read only the new bytes of a
changed log instead of downloading the whole file again. They have never been checked against a
live Nitrado service: see `docs/NITRADO_DELTA_READS.md` and `cmd/nitrado-delta-probe`. Run the probe
against a real server before turning them on.
