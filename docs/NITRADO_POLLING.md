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

## Measuring a kill's trip to Discord (`internal/killfeed/poll_timing.go`)

These come from data the bot already has (Nitrado's modified time and the Discord delivery
ledger), so measuring sends nothing extra. All three appear under `timing` and `delivery` in
`GET /api/admin/nitrado-usage`, and each server logs an `event=adm_timing` line every 5 minutes.

| Part | Field | What it tells you |
| --- | --- | --- |
| Nitrado writes the log | `timing[].writeGap` (p50/p90/max) | Time between two consecutive modified times seen for a server's log. If this stays well above the poll interval, Nitrado writes in batches, and polling faster cannot help. It can only resolve gaps down to the poll interval in effect. |
| Bot notices the write | `timing[].detectLag` | Our clock when a change is noticed minus Nitrado's modified time. The modified time has 1-second resolution, and clock differences between Nitrado and us show up here too. Lags over 10 minutes (backlogs) are dropped. |
| Bot posts to Discord | `delivery[]` | Noticed → posted per feed, from the existing delivery ledger. |

The game writing the ADM line → Nitrado's file changing is not visible: ADM lines carry only the
server's local time of day.

## Live Sync tail reads (`internal/livesync/tail.go`)

Live Sync (RPT, restart, script and crash logs) used to download each whole file every 30–120 s
probe, and whenever the listing showed growth. It now reads only the bytes after its checkpoint once
that is proven safe for the Nitrado service:

1. **Verifying:**
   - Every read is the usual full download, plus a partial read (`file_server/seek`) from one byte
     before the checkpoint.
   - The two must match byte for byte.
   - After 3 matches that covered new bytes, the service is trusted (`event=tail_read_trusted`).
2. **Trusted:**
   - Reads are partial only.
   - The first returned byte must equal the last byte already read, so a replaced file is never
     stitched onto the old checkpoint. Reading one byte early also means a quiet file returns that
     byte, so an empty read at the end of the file never comes up.
   - Every 20th read is a verifying read again.
3. **Disabled:**
   - Any mismatch switches tail reads off for that service until restart
     (`event=tail_read_disabled`), and full downloads continue.
   - A failed partial read falls back to a full download and returns the service to verifying.

The first read of a file, a checkpoint of 0, and a listing smaller than the checkpoint (replaced or
truncated) always use a full download.

Request count is unchanged: a partial read is a token request plus a fetch, just like a download.
What drops is the bytes per read, from the whole file to the new lines.

## Not changed

**ADM delta reads** (`NITRADO_DELTA_READ_MODE`) are still off for the killfeed engine. The Live
Sync verification above can show that seek works on a service before turning them on. They would read only the new bytes of a
changed log instead of downloading the whole file again. They have never been checked against a
live Nitrado service: see `docs/NITRADO_DELTA_READS.md` and `cmd/nitrado-delta-probe`. Run the probe
against a real server before turning them on.
