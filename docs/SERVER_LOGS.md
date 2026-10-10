# Server logs

The server owner can read the game server's own log files in the Server Hub (Controls → Server
logs). Champion reads them from Nitrado when asked and keeps no copy.

## What is shown

The files Nitrado lists in the server's log directory (`<game path>/config`), newest server start
first:

| Kind | File | What it holds |
| --- | --- | --- |
| `adm` | `DayZServer_*_x64_<start>.ADM` | The admin log: connections, hits, kills, positions |
| `rpt` | `DayZServer_*_x64_<start>.RPT` | The engine report: start-up, warnings, errors |
| `script` | `script_<start>.log` | Script errors and warnings |
| `crash` | `crash_<start>.log` | Written when the server reports a crash |

Nothing else in the directory (configs, ban and whitelist files) is listed or readable here.

## Access

`SERVER_LOGS_VIEW`, Owner only. The files name every player, their platform ids and their
positions. Opening a file writes one audit row (`SERVER_LOG_VIEWED`, target `server_log:<name>`).

## API

```
GET <admin base>/server-logs
GET <admin base>/server-logs/content?file=<name>&before=<byte offset>
```

`content` returns at most 1 MiB ending at `before` (the end of the file when `before` is left
out), cut to whole lines. `start` and `end` are the byte range returned; pass `start` back as
`before` to read the part above it. `hasEarlier` is false once the top of the file is reached.

The size is the one in Nitrado's listing, which Nitrado refreshes in steps of about five minutes
(docs/NITRADO_POLLING.md), so the newest lines of a running server can be a few minutes behind.

## Safety

- A request names a file, never a path. The name must be a server log name and must match a file
  in Nitrado's listing; the path read is the one from that listing.
- Text is redacted before it leaves the bot: the start-up command line and the crash log's
  "CLI params" line (address, port, config file), the Nitrado service account name wherever it
  appears, IP addresses, and any value written as a password, secret, token or API key.
- Reads are Nitrado seek reads of 256 KiB. When Nitrado refuses a seek read the whole file is
  downloaded instead, up to 8 MiB; a larger file then returns `NITRADO_UNAVAILABLE`.
