# Auto Leaderboard V3

The persistent `AUTO_LEADERBOARD` board is **one Discord message carrying
several embeds**, sent and edited together in a single API call. One message
ID, one refresh, and no drift in message order.

Code: `internal/discord/leaderboard_panel.go` (`BuildAutoLeaderboardEmbeds`,
`LeaderboardPanel`, `hashEmbeds`), `internal/discord/leaderboard_scheduler.go`
(`LeaderboardScheduler`), `internal/discord/route_panels.go`
(`PanelContent.Embeds`, `MultiEmbedMessageAPI`), `internal/presentation/board_grid.go`
(grid cells and value formats), `internal/repository/stats_repository.go`
(queries).

## Package

| # | Embed | Scope | Source | Color |
|---|-------|-------|--------|-------|
| 0 | `📊 AUTO LEADERBOARD 📊` (header) | — | server display name(s), refresh time | Champion Gold |
| 1 | `🔫 All Time Top 15 Kills 🔫` | ALL TIME | `StatsRepository.TopByKills` | Champion Gold |
| 2 | `🥷 All Time Top 15 Killstreaks 🥷` | ALL TIME | `StatsRepository.TopByBestStreak` | Event Gold |
| 3 | `🎖️ Current Top 15 Ranks 🎖️` | CURRENT | selected public server's active `ServerSeasonRankReader` | Champion Gold |
| 4 | `💀 All Time Top 15 Deaths 💀` | ALL TIME | `StatsRepository.TopByDeaths` | Combat Red |
| 5 | `🔭 All Time Top 15 Longest Kills 🔭` | ALL TIME | `StatsRepository.TopLongestKill` | Steel |

The order is fixed. A category with nobody qualifying keeps its embed and
shows `_No qualifying players yet._`, so the other boards never move.

### Header

```
CHAMPIONS® KILLFEED
📊 AUTO LEADERBOARD 📊
**Champions Deathmatch**
Last Updated <t:UNIX:R>
Auto Refresh • Every 3 Hours
CHAMPION • AUTO-REFRESH
```

- The server name is the guild's active server display name(s)
  (`servers.display_name`), de-duplicated and joined with ` • `. If the lookup
  fails, the line is left out and the refresh still goes ahead.
- `Last Updated` is a Discord relative timestamp. The client renders "just
  now" / "14 minutes ago" / "3 hours ago", so nothing re-renders it between
  refreshes.
- The cadence comes from `LeaderboardRefreshInterval` (3 hours).
- Only the header carries the brand (author + footer). The ranking embeds
  have no author and no footer.

### Ranking grid

Each ranked player is **exactly one inline field**, so Discord lays a Top 15
out as 3 columns × 5 rows:

```
🥇 PlayerOne       🥈 PlayerTwo       🥉 PlayerThree
6,053 Kills        5,385 Kills        4,753 Kills
#4 PlayerFour      #5 PlayerFive      #6 …
4,323 Kills        3,001 Kills
```

- Ranks 1–3 are marked 🥇 🥈 🥉 and ranks 4–15 are `#N`. No rank below 3 gets
  a medal.
- The value formats are:
  - kills: `1 Kill` / `6,053 Kills`
  - streaks: `27 Kill Streak`
  - deaths: `1 Death` / `5,012 Deaths`
  - longest kills: `98.3m`, `215.0m`, `1,104.2m` (one decimal, thousands
    separators)
  - ranks: current server tier and RP, e.g. `DIAMOND • 6,053 RP`
- A value is never shown as "Value" or "Kill(s)".
- Only qualifying players are rendered. If 7 players qualify, the board shows
  7 cells and no placeholders. Anything past 15 is dropped.
- Names go through `presentation.SafeName`, which strips `@`/`#` (no
  `@everyone`, `@here`, role or channel mentions), removes control, zero-width
  and bidi characters, and escapes markdown. Names are capped at 32 runes, so
  normal PSN/Xbox tags are never cut.
- Discord applies its 6000-character limit to **all embeds of a message
  combined**. If a pathological package would exceed it, the name cap steps
  down (32 → 24 → 20 → 16). As a last resort, trailing cells of the fullest
  board are dropped. With six embeds the package is well under the
  10-embeds-per-message limit, and 15 fields is under the 25-fields-per-embed
  limit.

The interactive `/leaderboard` command is unchanged and keeps its compact
one-field-per-category style (`docs/DISCORD_PRESENTATION_V2.md`).

## Sources and tie breakers

Every board is one bounded, guild-scoped aggregate query with `LIMIT 15`.
That is 4 queries per refresh, plus 1 when a rank source is wired. There are
no per-player (N+1) queries.

| Board | SQL source | Order |
|-------|-----------|-------|
| Kills | `COUNT(kills)` by killer, no season filter | kills DESC, display_name ASC, player id ASC |
| Killstreaks | `player_combat_stats.best_streak` (> 0) | best_streak DESC, all-time kills DESC, display_name ASC, player id ASC |
| Deaths | `COUNT(deaths)` by player, all death types, no season filter | deaths DESC, display_name ASC, player id ASC |
| Longest | `MAX(kills.distance)` by killer, no season filter | distance DESC, display_name ASC, player id ASC |

**The streak record is all-time.** `player_combat_stats` is keyed only by
`(guild_id, player_id)`, with no season column. `best_streak` is only ever
raised, through `GREATEST()` in `StreakRepository.Increment`.
`StreakRepository.Reset` clears `current_streak` only, and no season rollover
touches the table. The board shows the record streak, not the current active
streak. The integration test `TestAutoLeaderboardV3Queries` covers this: the
record survives a reset, and kills from two seasons both count.

## Current Ranks: selected server season

The Ranks embed uses the guild's selected public game server (or its only
active game server). It reads that server's active Ranked season and orders
players by seasonal RP, then player ID for ties. The value shows tier and RP;
the embed explicitly says it belongs to the selected public server. The four
all-time boards remain guild scoped.

Before a local season is started, or while a multi-server guild has no public
server selection, the Ranks embed is omitted. An actual database error still
fails the entire refresh and preserves the last good message. Dedicated
`SERVER_RANKS` panels remain separate for each game server. This wiring was
written as part of the Ranked stack, which has since been merged
(`internal/discord/server_ranks_board.go`, `docs/RANKED_SERVER_SEASONS.md`).

## Refresh model

- The scheduled refresh runs every **3 hours** (`LeaderboardRefreshInterval`),
  plus once at startup. `/admin` leaderboard-refresh and a route change use
  the same `RefreshOnce`. A kill never refreshes the board directly.
- **Atomic.** Every category is loaded before anything is rendered. The whole
  package is then sent or edited in one Discord call. If any ranking query
  fails, the refresh fails, the last good board stays exactly as it was, the
  error is logged, and the next refresh retries.
- **Hash.** `hashEmbeds` covers the embed count, the embed order, and every
  visible part of each embed: title, description, color, author, footer,
  fields with name, value and inline flag, image and thumbnail. It excludes
  embed timestamps. The header's `Last Updated` line changes on each real
  refresh, so a real refresh edits the message on purpose, even if the
  rankings are unchanged. The board always shows its last successful refresh.

## Persistence and routing

- **Routed** (`AUTO_LEADERBOARD`): `RoutePanels.Sync` keeps exactly **one**
  `guild_route_panels` row per (guild, route key, channel), never one row per
  embed. On the first run it posts one multi-embed message. Later runs edit
  that same message. After a restart the recorded message ID is reused, so
  no duplicate is posted. When the route changes, the package is posted in
  the new channel and the old Champion-managed message is retired.
- **Legacy** (`GuildSetup.LeaderboardsChannelID`): `LeaderboardPanel` sends
  and edits the same package, also as one message. `RestoreLegacyPanels`
  posts a header-only placeholder, and the next refresh edits it into the full
  package.
- `AUTO_LEADERBOARD` and `STATS_LEADERBOARDS` can share one channel. Each keeps
  its own single recorded message, and neither deletes or replaces the other.
- **Multi-embed support** is a separate, optional `MultiEmbedMessageAPI`
  (`ChannelMessageSendEmbeds` / `ChannelMessageEditEmbeds`, implemented by
  `SessionAPI`). A panel whose `PanelContent.Embeds` is empty uses the original
  single-embed path unchanged. This covers Bounty Board, Heatmaps, Server
  Status, Player Link, Player Stats and the others. A multi-embed package is
  never split across several messages: without multi-embed support nothing is
  posted and an error is recorded.


## Obsolete board cleanup

After every successful publish the scheduler sweeps the leaderboard channel(s) (the routed `AUTO_LEADERBOARD` channels plus the legacy `GuildSetup` leaderboard channel) and deletes **only** messages that are all of:

- authored by the bot itself (not a player, not another bot),
- a plain message (not a `/leaderboard` or other interaction reply),
- a leaderboard *board* by its first embed title — the pre-V2 `SEASON LEADERBOARD` / `CHAMPION KILLFEED\nSEASON LEADERBOARD` / `🏆 CHAMPION LEADERBOARD` cards, the V2 `🏆 SEASON LEADERBOARD` card, or a duplicate `📊 AUTO LEADERBOARD 📊` header,
- not the currently recorded board message.

The Player Stats panel, Server Ranks, player messages and every other panel are never touched. The sweep reads at most 100 recent messages per channel and is skipped for a routed refresh with any channel error. It removes old single-embed boards left by earlier releases, a legacy board whose retirement delete failed, and orphaned placeholders.

Retiring the legacy board now durably clears `guilds.leaderboard_message_id` (the setup upsert COALESCEs ids, so an empty value used to keep the retired id). If the recorded legacy board was deleted in Discord, the next refresh posts a fresh board instead of failing every refresh.
