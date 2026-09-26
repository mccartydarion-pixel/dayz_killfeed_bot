# Online counter and PSN link check: investigation and fix

Two live issues on Champions (Nitrado service `19806451`, DayZ PS4, 18 slots):

1. `/link` and the Link Username panel always answered **LINK CHECK UNAVAILABLE**.
2. The online voice channel showed a count that did not match the 2 players connected (expected `Online: 2/18`).

This was diagnosed from code and tests only. No production reads or writes were made.

## Do they share a failure?

Mostly no. They have separate root causes. The only link between them is the per-boot presence handling (section 3), which inflated the counter and also let phantom activity sessions keep accruing time.

| | Player count | Link verification |
|---|---|---|
| Source before | In-memory ADM `PlayerTracker` (connect/disconnect lines) | `player_server_activity` for the server `ConnectedServerID` picks |
| Source after | Nitrado `query.player_current`/`player_max`, then a recent complete ADM player list, otherwise unknown | Same table, but the server is resolved the same way as the public counter |

## 1. LINK CHECK UNAVAILABLE: root cause

`ServerRepository.ConnectedServerID` kept only rows with `LOWER(status) IN ('connected','ready','active')`. Different flows write different values to `game_servers.status`:

* `/server connect` writes `CONNECTED`.
* The SaaS installation flow (`UpsertForInstallation`, `saas_api_nitrado.go`) writes `ONLINE` or `OFFLINE`, taken from Nitrado's service status.

Champions is bound through the SaaS flow (installation 11, `game_servers` row 1). Its row therefore never matched, and `Request` returned `ErrLinkCheckUnavailable` before it read any activity. The killfeed was unaffected because workers start from `active` alone.

Reproduction: `TestLinkRequestSucceedsForSaaSOnboardedServer` fails on the old query with `no connected server for guild N` and passes with the fix.

**Fix**
* The link check now resolves its server with the same rule as the public counter: the guild's `selected_public_server_id` if that server is active, otherwise the only active server. The status label is ignored.
* The single "unavailable" answer is split into distinct outcomes. Each wraps its old parent error, so `errors.Is` checks in existing callers still match:

| Outcome | Error | Player sees |
|---|---|---|
| Malformed name (mention, URL, wrong length) | `ErrInvalidUsername` | INVALID PLAYSTATION USERNAME |
| Name never seen | `ErrPlayerNotFound` | PLAYER NOT FOUND |
| Name known, no time on this server | `ErrPlayerNotObserved` | NOT OBSERVED ON SERVER |
| Less than 5 minutes | `*PlaytimeShortfallError` | MORE PLAYTIME REQUIRED (for example, "3m 20s of the required 5m") |
| No active server | `ErrInstallationNotConfigured` | SERVER NOT CONFIGURED |
| Several servers, none selected | `ErrServerSelectionRequired` | SERVER SELECTION REQUIRED |
| Database or activity read failed | `ErrActivityUnavailable` | LINK CHECK UNAVAILABLE |
| 5 minutes or more | none | PENDING VERIFICATION (the existing disconnect/reconnect challenge, then @verified) |

* `/link` and the panel now use one message table (`linkErrorMessage`). The @verified role assignment and `/admin verify-link` are unchanged.

## 2. Incorrect online count: root causes

1. **The wrong source of truth.** The counter showed the ADM tracker, which is rebuilt from connect and disconnect lines. That tracker is not a live count:
   * **Server restart:** DayZ writes no disconnect lines on shutdown. The engine switched to the new boot's ADM with `presence_retained=true`, so every player from the previous boot stayed "online".
   * **Bot restart or first connect:** the tracker starts empty and resumes from the checkpoint or the log tail. Players who joined earlier were missing until the next complete player list.
2. **Renames ran on the ADM pipeline.** `OnPlayersChanged` called `Publish` and then `Reconcile` synchronously, which did a channel GET and an immediate edit on every presence change. Discord allows 2 renames per channel per 10 minutes. discordgo's default is to sleep through a 429's Retry-After, which can be minutes. That stalled ADM and kill processing, and the counter lagged behind.
3. **Channel flip-flop.** Every presence change re-bound the counter to the legacy GuildSetup channel, overriding the `ONLINE_COUNTER` route. When both existed, updates alternated between two channels.

**Fix**
* A new counter loop (`internal/app/online_counter.go`) runs on its own goroutine. It evaluates every 60s, or sooner (no more than once per 15s) when presence changes:
  1. **Nitrado** `GET /services/:id/gameservers`: `query.player_current` and `player_max`, whitelist-decoded. Credentials in the same body are never decoded. A `stopped` or `suspended` server is a known 0.
  2. **ADM tracker**, but only while a complete ADM player list reconciled it within the last 11 minutes.
  3. Otherwise **unknown**. The channel keeps its last value for 10 minutes, including after a bot restart, and then shows `⚪・Online: ?/18`. It never shows a made-up 0.
* The channel name is `🟢・Online: 2/18`. Older `Online Players: N` names are still parsed and migrated. Setup and repair still recognise the channel in every format, so no duplicate channel is created.
* `VoiceChannelCounter` spaces successful renames at least 5 minutes apart, coalesced to the latest value. Renames go through `ChannelRename`, which sets `WithRetryOnRatelimit(false)`, so a 429 returns at once and the retry is scheduled from Retry-After. The ADM callback now only pokes the loop.
* The `ONLINE_COUNTER` route always wins over the legacy channel.

## 3. Server restart handling

* When the engine selects a **verified newer boot** (the ADM filename stamp is strictly newer than the accepted boot), it clears the tracker, clears the "last complete player list" proof, and fires `OnNewBoot`. The old file's tail is drained before this happens (`drainRotationTail` runs before `selectLog`).
* `OnNewBoot` closes open `player_server_activity` sessions for the server (`ResetConnectedForRestart`). Phantom rows stop accruing observed time, and time already accrued is kept.
* Other source switches (the same boot on another mount, a stale-source switch, unstamped files) still keep presence, as before.

## Installation mapping

Documented in `docs/SHOP_DELIVERY_PHASE2C1.md` and verified live in that phase: Nitrado service `19806451` maps to `game_servers.id = 1`, installation 11, organization 1. It was not queried again for this change. Read-only checks to run before deploying:

```sql
SELECT id, guild_id, provider_service_id, status, active, organization_id FROM game_servers WHERE provider_service_id = '19806451';
SELECT id, discord_guild_id, selected_public_server_id FROM guilds WHERE id = (SELECT guild_id FROM game_servers WHERE id = 1);
```

Expected: exactly one active row for the service, with a `status` of `ONLINE` or `OFFLINE` (the value the old filter rejected), and `selected_public_server_id` = 1.

After deploying, the admin pipeline diagnostics report `counter_source`, `nitrado_player_current`, `tracker_count`, `tracker_matches_nitrado` and `counter_desired_name`.

## Known gaps (not changed here)

* Bot status ("N Players Online"), the server status board and the ADM monitor still read the ADM tracker. The restart reset removes their phantoms, but they can still lag after a bot restart until the next player list.
* A bot restart closes open activity sessions (`ResetConnectedForRestart` at worker start). A player who stays online across a bot deploy accrues no link playtime until they reconnect.
