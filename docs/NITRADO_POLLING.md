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

The interval runs from the start of one poll to the start of the next, so a 2s interval polls every
2s rather than every 2s plus the poll's own time (about 0.5–0.9s in production). After a slow poll
the bot still waits at least 250 ms. The discovery backoff is unchanged: it is a full wait after
each attempt.

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

## Which mount is fresher (`internal/livesync/mount_compare.go`)

Nitrado exposes each log under two mounts, `noftp` and `ftproot`. The killfeed reads `noftp`. The
Live Sync lister already lists both mounts every 20 seconds, so comparing them sends nothing extra.

For every file present in both mounts, the lister logs `event=mount_lead` each time the file grows:

| Field | Meaning |
| --- | --- |
| `leader` | The mount that showed the new bytes first (`noftp`, `ftproot`), or `tie` when both showed them in the same pass |
| `lag_ms` | How long the other mount took to reach the same size. Resolution is one listing pass (`list_every_ms`) |
| `family`, `file`, `size` | Which log, and the size the leader showed |

This only measures. Nothing reads it to choose a mount. A pass in which any directory failed to
list is skipped, so a failed listing never looks like a mount that fell behind.

## Verified tail reads (`internal/nitrado/tail_trust.go`)

Live Sync (RPT, restart, script and crash logs) and the killfeed ADM reader both used to download
each whole file on every change. They now read only the bytes after their checkpoint once that is
proven safe. Trust is kept per Nitrado service and shared by both readers, so matches from either
one count toward it.

1. **Verifying:**
   - Every read is the usual full download, plus a partial read (`file_server/seek`) from one byte
     before the checkpoint.
   - The overlapping bytes must match byte for byte. A file can grow between the two reads, so only
     the bytes both reads cover are compared; extra bytes on one side are not a mismatch.
   - After 3 matches that covered new bytes, the service is trusted (`event=tail_read_trusted`).
   - For ADM, the partial read runs after the kills from the full download are posted, so verifying
     never delays the feed.
2. **Trusted:**
   - Reads are partial only.
   - The first returned byte must equal the last byte already read, so a replaced file is never
     stitched onto the old checkpoint. Reading one byte early also means a quiet file returns that
     byte, so an empty read at the end of the file never comes up.
   - Every 20th read is a verifying read again.
3. **Disabled:**
   - Any mismatch switches tail reads off for that service until the bot restarts
     (`event=tail_read_disabled`, with `source=livesync` or `source=adm`), and full downloads
     continue.
   - A failed partial read falls back to a full download and returns the service to verifying.

Trust survives restarts. Each service's match count and trusted flag are saved in
`nitrado_tail_trust` (migration `0110`) and restored at startup (`event=tail_trust_restored`).

- A restored trusted service verifies its first read again before it goes back to tail-only reads.
- Saved trust is dropped after 7 days unless a passed recheck refreshes it.
- A disabled service is saved as untrusted, so it starts verifying again after a restart.

Full downloads are always used for the first read of a file, a checkpoint of 0, a listing smaller
than the checkpoint (replaced or truncated), and for ADM a log rotation or a checkpoint the reader
did not itself write in this process.

Tail reads use Nitrado's `file_server/seek` only, and each request asks for exactly the bytes the
file is known to hold (up to the listing's size, or the full download's length when verifying).
Production showed why (2026-10-02, one service):

- the download URL ignores `offset`/`count` and a `Range` header, and returns the whole file, so
  neither is a partial read;
- `seek` answered HTTP 500 ("temporary Nitrado failure") when asked for 256 KiB from a checkpoint a
  few KiB from the end of the file.

Live Sync only tail-reads when the directory listing gave the file's size.

A failed partial read logs `event=partial_read_failed` with the method and a reason (status, error
kind, Nitrado's message, or the network error). It is logged at most once per service and method
every 30 minutes and never includes a URL, since signed download URLs carry credentials. When every
method has failed three times in a row, `event=partial_reads_paused` is logged and the service
uses full downloads for 10 minutes before trying again.

Request count is unchanged: a partial read is a token request plus a fetch, just like a download.
What drops is the bytes per read, from the whole file to the new lines.

## Not changed

**ADM delta reads** (`NITRADO_DELTA_READ_MODE`) are a separate, older switch and stay off. When
they are on, they take priority over the verified tail reads above. See
`docs/NITRADO_DELTA_READS.md` and `cmd/nitrado-delta-probe`.
