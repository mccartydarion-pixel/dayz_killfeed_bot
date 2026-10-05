# Server name sync

Champion shows one name per game server (`game_servers.display_name`): on the website, on Discord
boards, in feeds and in the `{{server_name}}` embed variable. That name follows the server's name
on Nitrado, unless the owner typed a name in Champion.

## The rule

| Where the name came from | `display_name_custom` | What the sync does |
|---|---|---|
| Nitrado (connect, or an earlier sync) | `false` | Replaces it when the Nitrado name changes |
| Typed by the owner in Champion (`PUT /admin/server/name`) | `true` | Never replaces it |

`provider_display_name` always holds the last name read from Nitrado, custom or not (`NULL` until
the first read).

A name stops being custom when the owner

- sets it to exactly the current Nitrado name, or
- clears it (`{"name": ""}`): the server shows its Nitrado name again. This is refused with
  `400 INVALID_REQUEST`, as an empty name always was, while Champion has not read a Nitrado name
  for the server yet.

Reconnecting or re-selecting the same Nitrado service keeps a custom name.

## Which Nitrado field

One function decides, for both connect flows (website and `/server select`) and for the sync:
`nitrado.ServerName` (`internal/nitrado/server_name.go`). The first non-empty of:

1. `data.gameserver.settings.config.hostname` of `GET /services/:id/gameservers` - the DayZ
   `hostname` setting the owner edits in the Nitrado panel.
2. `data.gameserver.query.server_name` of the same response - the name the running server
   announces (absent while the server is down).
3. `details.name` of `GET /services`.
4. `details.server_name` of `GET /services` (legacy).

The connect flows read 1-4; the sync reads only the gameserver response (1-2), so it costs one
request per server. When the sync finds neither field it changes nothing, so a name taken from 3
or 4 at connect time is kept.

The name is cleaned before it is stored (`nitrado.CleanServerName`): control characters and runs
of whitespace become one space, the ends are trimmed, and it is cut to 100 bytes - the same limit
the rename endpoint applies.

## The worker

`internal/app/server_name_sync.go`, started from `App.Run`.

- First pass 2-7 minutes after start, then every 20 minutes plus up to 5 minutes of jitter.
- Runs in one process only: the one holding the leader lock (docs/MULTI_PROCESS.md). A process that
  takes the lock over starts with the same 2-7 minute delay.
- Targets: active `game_servers` rows with provider `NITRADO` and a service ID. A server whose
  installations are all suspended is skipped.
- Credential: the organization's Nitrado credential, else the guild's own connection. A server
  with no usable credential is skipped.
- One `GET /services/:id/gameservers` per server per pass, 500 ms apart, through the shared
  Nitrado client (its 429 retry and rate-limit telemetry apply). After a failed read, the other
  servers of the same credential wait for the next pass.
- A failed read, a response that does not name the requested service, or an empty name writes
  nothing. A name is never blanked.
- The custom check is part of the `UPDATE` itself, so a rename that races with a pass wins.
- Logs: `component=server_name_sync` (`server_renamed`, `pass_done`). Tokens are never logged.

## How long a change takes

A name changed on Nitrado reaches the database within one interval: at most about 25 minutes
(plus the length of a pass), if the read succeeds. Then:

- Website: on its next request.
- Feed cards and `{{server_name}}`: the next card (the 5-minute name cache is cleared on a change).
- Boards and panels: on their own next refresh. Nothing is re-posted because of a rename.

## API

`DayZServerSummary` (the installation and dashboard responses) carries `displayNameCustom`
(boolean) and `providerName` (string or `null`). `PUT /admin/server/name` returns
`{ name, displayNameCustom, providerName }`.

## Servers that existed before migration 0124

A rename in Champion has always been recorded in `admin_audit_log` (`SERVER_NAME_EDIT`, with the
new name). Migration `0124_server_name_sync` marks a server custom when its current name is the
name of a successful audited rename. Every other existing name is treated as a Nitrado name and
follows Nitrado from the first pass. An audit row whose installation was deleted since cannot be
matched; such a server is treated as not custom.
