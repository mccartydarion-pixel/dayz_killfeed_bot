# Champion Player API (Champion Access Model Phase 2, Part A/B)

The website's Player Hub needs to answer two questions for a Discord-authenticated user: **which
Champion servers do I have a legitimate history on**, and **what are my stats on one of them**.
This document is the authoritative contract for both. It reuses Champion's existing auth (Discord
acting-user identity, `player_links`) and existing data (`kills`, `deaths`, `bounties`,
`player_server_activity`, Faction Hub membership) - there is no second identity system and no
fabricated data anywhere in this surface.

## 1. A Player is not an organization member

Every other `/api/saas/organizations/{organizationID}/...` route requires organization
membership (OWNER/ADMIN/MEMBER). A Player has none of that - they are a tracked DayZ identity,
verified through `player_links`, exactly like the existing Champion Points economy's player-facing
routes (`docs/ECONOMY.md`: "players are not organization members"). Player routes therefore:

- live under `/api/saas/player/...`, never under an organization prefix;
- need only the standard service-auth + acting-user chain (no organization role check at all);
- never accept a `playerId` or gamertag from the request body/query as authority - identity is
  always resolved server-side through the acting Discord user's `player_links` row.

## 2. The authoritative identity chain

```
Discord user (app_users.discord_user_id, from the acting-user header)
  -> player_links row (guild_id, player_id, discord_user_id, status='VERIFIED')
  -> players row (guild_id, dayz_player_id) - one row per (guild, physical DayZ identity)
  -> installations (discord_guild_connection_id -> guilds.id = player_links.guild_id,
                     game_server_id = the specific DayZ server)
```

`player_links` is scoped **per guild** (`UNIQUE(guild_id, discord_user_id)`), not globally - the
same Discord user can hold different verified links in different guilds. One guild can back
**several installations** (one `game_servers` row each), so a verified link alone only proves
identity for the guild as a whole, not for any one specific server.

**"Legitimate association" with an installation therefore always requires BOTH:**

1. a `VERIFIED` `player_links` row for the installation's guild (identity), **and**
2. observed activity on that installation's own `game_server_id` - a `player_server_activity` row,
   or at least one `kills`/`deaths` row for that player on that `server_id` (proof of play).

Neither is used alone. Guild membership, faction membership, and an installation id the browser
merely supplied are never treated as proof (task Part A.3's explicit exclusions).

## 3. `GET /api/saas/player/servers`

Auth: service auth + acting user. No organization/installation id in the request.

Returns every installation the acting user has a legitimate association with (section 2), most
recently active first. An unverified user, or a verified user who has never been observed playing
anywhere, gets `{"items":[],"defaultInstallationId":null}` - a normal empty state, not an error.

```json
{
  "items": [
    {
      "installationId": 42,
      "serverName": "DE #3 | 1PP | Chernarus",
      "platform": "PLAYSTATION",
      "serverStatus": "READY",
      "discordGuildName": "Champion Test Community",
      "lastSeenAt": "2026-09-20T18:04:00Z",
      "linkedPlayerId": 981
    }
  ],
  "defaultInstallationId": 42
}
```

`organizationId` is **deliberately not included** - a Player is never an organization member and
has no legitimate use for one (task's own "? only if safe/needed" resolved to "not needed"; least
exposure). Field names otherwise mirror the existing admin `InstallationSummary`/`DiscordInfo`/
`ServerInfo` sources (`internal/adminrepo`) rather than inventing new ones.

### Default server rule

`defaultInstallationId` is the **most recently active** installation:
`player_server_activity.last_seen_at DESC`, `NULLS LAST` (an installation observed only via a
kill/death row, with no tracked session yet, sorts after any installation with a real session),
then installation id ascending as a stable, deterministic tie-break. This is exactly the order
`items` is returned in - `items[0]` is always the default when the list is non-empty.

## 4. `GET /api/saas/player/servers/{installationID}/stats`

Auth: service auth + acting user. `installationID` is a path parameter, but it is **only a lookup
key** - never proof of anything on its own. The backend independently verifies the caller's
association (section 2) before computing or returning anything.

```json
{
  "installationId": 42,
  "kills": 118,
  "deaths": 54,
  "kd": 2.185185185185185,
  "headshots": 31,
  "longshots": 6,
  "longestKillMeters": 412.7,
  "playtimeSeconds": 305820,
  "bountiesClaimed": 3,
  "bountyValue": 750,
  "faction": "Wolfpack",
  "lastSeenAt": "2026-09-20T18:04:00Z"
}
```

- `kd`: `deaths == 0 ? kills : kills / deaths` (the exact existing convention,
  `repository.PlayerProfile.KD()`).
- `playtimeSeconds`: whole seconds, not a duration string - chosen deliberately over the task's
  looser suggested name `playtime` for unambiguous website-side formatting.
- `faction`: the acting user's **current Faction Hub faction on this installation**
  (`hub_faction_members`, genuinely installation-scoped), or `null`. This is intentionally NOT the
  classic guild-wide faction system (`FactionRepository.GetActiveFactionForPlayer`) - see section 5.

### Error responses

| Condition | Status | Code |
|---|---|---|
| `installationID` does not exist (or has no DayZ server selected yet) | 404 | `NOT_FOUND` |
| Installation exists, but the acting user has no `VERIFIED` link for its guild | 409 | `PLAYER_IDENTITY_REQUIRED` (same stable code `internal/economy`'s `Me` endpoint already uses) |
| A verified link exists, but no observed activity on THIS specific server | 404 | `NOT_FOUND` (see section 6 - deliberately not a distinct code) |

## 5. Fields not included, and why

Reusing **actual authoritative data** (task's own instruction) means some intuitive fields are
left out rather than approximated:

- **`bestStreak` / `currentStreak`**: `player_combat_stats` (the only streak store,
  `internal/repository/streak_repository.go`) is keyed `(guild_id, player_id)` - **guild-wide**,
  with no server dimension at all. Showing it on a response scoped to one installation would blend
  in streak activity from any *other* server of the same guild, directly violating the installation
  isolation this endpoint exists to guarantee (section 6). Not included. This is a data-model gap,
  not an oversight - flagged as a product decision for a future phase (per-server streak tracking,
  or an explicit product decision to accept guild-wide streaks) rather than silently faked here.
- **Classic per-player faction** (`FactionRepository.GetActiveFactionForPlayer`): same problem -
  guild-wide, not server-scoped. The Faction Hub system's own membership table
  (`hub_faction_members`) IS genuinely installation-scoped, so `faction` above uses that instead;
  a player who has never joined a Faction Hub faction on this specific installation gets `null`,
  even if they hold a classic-system faction elsewhere in the guild.

## 6. Installation isolation (task section 8)

A Player viewing installation A must never receive installation B's personal stats, or even learn
that B exists via a different error shape. Enforced two ways:

1. Every stats query (`kills`, `deaths`, headshots, longshots, longest kill, bounty claims) filters
   by `server_id = <this installation's game_server_id>` explicitly, in addition to `guild_id` and
   `player_id` - never a bare guild-wide query (see section 5's contrast with the streak/classic-
   faction fields, which is exactly why they're excluded rather than shown un-scoped).
2. A verified link to the guild is not, by itself, enough to see a specific server's stats: if the
   player has never actually played on that server (no `player_server_activity` row, no kill, no
   death there), the endpoint returns the **same 404** an unknown installation id would - not a
   distinct "you're not associated with this one" code, which would otherwise let a caller enumerate
   which installations of a guild they belong to by comparing 404 vs. some other status.

Covered by `internal/app/saas_api_player_integration_test.go`'s
`TestPlayerStatsInstallationIsolation` (two servers, two different kill counts, each endpoint call
shows only its own) and `TestPlayerStatsNoObservedActivityOnThisServerReturns404`.

## 7. Performance

- `player_links(discord_user_id, status)` - new index (migration `0039_player_api_lookup_index`).
  Neither existing `player_links` unique constraint (`(guild_id, discord_user_id)` /
  `(guild_id, player_id)`) can serve a lookup that starts FROM a Discord user id without already
  knowing a guild id, which is exactly what `GET /api/saas/player/servers` and the stats endpoint's
  association check both do. `EXPLAIN` before this index showed a sequential scan on `player_links`
  for that exact query; with it, an index scan.
- Every kills/deaths/bounties query used here (`EXPLAIN`-checked against the live schema) resolves
  through an existing index - `idx_kills_server`, `idx_deaths_player`, `idx_bounties_server_status` -
  no new index was needed there, and none was added speculatively.
- Nothing here scans all kills/deaths globally for a page load: every query is filtered by
  `(guild_id, server_id, player_id)` or `(guild_id, server_id)` before any aggregation.

## 8. What was not built (scope)

`GET /api/saas/player/servers/{installationId}/activity` (task Part A.11, explicitly marked
optional "if scope would expand too much") was not implemented this phase, to keep this already
large phase reviewable. The player association/stats model above (section 2) is exactly what that
endpoint would reuse when built.
