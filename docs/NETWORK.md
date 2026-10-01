# Cross-server network

A public directory of the Champion servers that opted in, and leaderboards across them.

Off by default, per installation. Code: `internal/repository/network_repository.go`,
`internal/app/network.go`. Setting: `installation_feature_settings.network_listed`.

## Opting in

`PUT .../admin/features/network` - capability `NETWORK_MANAGE` (**Owner**), because it publishes
the server outside its own community:

```json
{"listed": true, "description": "Hardcore PvP, weekly wipes", "discordInviteUrl": "https://discord.gg/deadzone"}
```

`description` is at most 280 characters.

`discordInviteUrl` is optional: the owner's own invite to the server's Discord, shown as a
"Join Discord" link on the public listing. Only a Discord invite is accepted
(`discord.gg/<code>`, `discord.com/invite/<code>`, with or without `https://`); it is stored as
`https://discord.gg/<code>`, and anything else is a 400. The bot never creates an invite itself -
the owner decides which one to publish, and should use one that does not expire. Sending an empty
string removes the link. It is only ever returned for a listed installation. The change is audited (`NETWORK_SETTINGS_UPDATED`) and
takes effect at once.

Every network query starts from the set of listed installations. Nothing about an installation
that has not opted in - not its name, not one kill - can appear in any result. Unlisting removes
the server from the directory and its kills from every cross-server total immediately.

## Identity across servers

Across servers a player is the pair **(platform, DayZ id)**. The ADM id is the same for one account
on every server of a platform, so an account's kills on several listed servers add up, shown under
the name it was most recently seen with. PlayStation, Xbox and PC accounts never merge.

Worth confirming against production data before relying on it: that one account really carries the
same ADM id on two of your servers. If it does not, nothing breaks - the account simply appears
once per server instead of once.

## Routes

All three take the service secret and **no acting user**: the website renders them for anyone.
Responses are cached in-process for one minute.

`GET /api/saas/network/servers?platform=` - the directory, busiest first:

```json
{"items": [{"installationId": 12, "name": "Chernarus PvP", "platform": "PLAYSTATION",
            "description": "Hardcore PvP, weekly wipes", "discordGuildName": "Deadzone",
            "discordInviteUrl": "https://discord.gg/deadzone",
            "playersOnline": 14, "peak24h": 31, "activePlayers7d": 212, "kills7d": 1840,
            "totalKills": 51233, "trackedPlayers": 3120, "lastActivityAt": "…"}]}
```

`playersOnline` counts players connected and observed in the last five minutes.

`GET /api/saas/network/servers/{installationId}` - one listed server plus its own `topKillers`,
`longestKills` and `longestLives` (ten each). An unlisted installation is a plain 404.

`GET /api/saas/network/leaderboard?board=KILLS|LONGEST_KILL|LONGEST_LIFE&platform=&days=0..365&limit=1..100`

| Board | `value` | Extra fields |
| --- | --- | --- |
| `KILLS` | kills across listed servers (self-kills excluded) | `servers`: how many listed servers they were made on |
| `LONGEST_KILL` | metres, one row per kill | `serverName`, `weapon`, `at` |
| `LONGEST_LIFE` | seconds of observed playtime, one row per life (docs/LIVES.md) | `serverName`, `at` |

`days=0` (the default) is all time.

## What is not here

- **No integrity or anti-cheat badge.** C.A.S.E. observes; it does not judge. A badge would claim
  more than the data supports, and the longest-kill board in particular will show whatever the
  servers logged.
- No per-player cross-server profile page and no server search beyond the platform filter.
