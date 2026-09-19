# Faction Hub - faction leaderboards (Phase 6)

One read-only endpoint ranks the factions of an installation by **one explicit metric** at a time. It adds no table, no migration and no new statistic: every figure is the
one the faction profile already shows (`docs/FACTION_STATS.md`), computed by the *same SQL definition*, so the leaderboard and a faction's page cannot disagree.

**There is no overall, hidden or weighted score.** Nothing like `overallRating`, `powerScore`, `combatScore` or `skillScore` exists in the response or the code. A
leaderboard ranks exactly the metric it names, with a documented tie-break.

## 1. Endpoint

```
GET /api/saas/organizations/{organizationID}/installations/{installationID}/factions/leaderboard
    ?metric=KILLS|DEATHS|KD|HEADSHOTS|LONGSHOTS|BEST_STREAK|BOUNTIES_CLAIMED|BOUNTY_VALUE|ACHIEVEMENTS   (default KILLS)
    &limit=1..100        (default 25, larger values are clamped to 100; 0, negatives and non-numbers are 400)
    &cursor=<opaque>     (the previous page's nextCursor; bound to its metric)
    &q=<text>            (optional; case-insensitive substring of the faction name or tag)
```

* Open to **any synced Champion user** (no faction or organization membership), like the faction profile. `401` without a synced acting user.
* Scoped by **organization + installation** (an installation is one Discord guild + one DayZ server). A mismatched pair is `404`; nothing aggregates across
  installations, servers or organizations - not in the query, not in the cache.
* `metric` is matched case-insensitively against the closed list above; **anything else is `400`** (it is never mapped to a default, never interpolated into SQL). A
  malformed query string (for example a `;` separator, which Go would silently drop) is also `400`, so a bad metric can never fall back to `KILLS`.
* The literal `/leaderboard` route wins over `/{factionID}`.
* Only public metadata is returned (name, tag, slug, logo, flag, armband, colors, member count, figures). No user ids, Discord ids, gamertags, descriptions or requirements.

## 2. Metrics, direction and value types

| `metric` | Ranked value | Type | Direction |
|---|---|---|---|
| `KILLS` | counted kills | integer | DESC |
| `DEATHS` | counted deaths | integer | **ASC** (fewer is better) |
| `KD` | `kills / max(deaths,1)`, two decimals (the profile's `kdRatio`) | **decimal** | DESC |
| `HEADSHOTS` | headshot kills | integer | DESC |
| `LONGSHOTS` | longshot kills | integer | DESC |
| `BEST_STREAK` | best single-member kill streak | integer | DESC |
| `BOUNTIES_CLAIMED` | claimed bounties | integer | DESC |
| `BOUNTY_VALUE` | total points of claimed bounties | integer | DESC |
| `ACHIEVEMENTS` | system-unlocked achievements | integer | DESC |

`value` is a JSON integer for every metric except `KD`, which is a JSON number that may have decimals (`3.75`, and `12` when a faction has 12 kills and no deaths - the
project's K/D convention). The response repeats `direction` (`"DESC"` / `"ASC"`).

**Why `DEATHS` is ascending.** It is a competitive board ("who dies least"), so rank 1 is the faction with the fewest deaths. To stop an empty faction from being "the
best at deaths", factions with **no tracked activity** (no counted kill, death or bounty) sort *after* every faction that has some, and are still listed.

Every entry also carries the full `stats` block (all metrics), so the UI can show several columns without extra requests.

## 3. Ranking, ties and ranks

Ranks are **ordinal** (1, 2, 3, ...): tied factions get consecutive ranks in the deterministic order below, never a shared rank. A rank is the faction's position on the
*whole* installation leaderboard and does **not** change with `q` or paging. Ties are broken by a fixed, fully deterministic chain that always ends in the faction id, so
the same data always yields the same order and cursors never skip or repeat a faction:

| Metric | Order |
|---|---|
| `KILLS` | kills DESC, deaths ASC, faction id ASC |
| `DEATHS` | (no tracked activity last), deaths ASC, kills DESC, faction id ASC |
| `KD` | K/D as displayed (2 decimals) DESC, kills DESC, deaths ASC, faction id ASC |
| every other metric | that metric DESC, kills DESC, deaths ASC, faction id ASC |

(Faction id ASC is creation order, so an older faction wins a complete tie.)

## 4. Tracking coverage and zero values

Every faction of the installation is listed, including ones with all-zero figures. `hasTrackedActivity` is `false` when the faction has no counted kill, death or bounty
(a brand-new faction, or one whose members have no verified DayZ link yet); the UI can show "No tracked activity" instead of a wall of zeros.

`trackingSince` (RFC 3339, `null` when there is no membership history) is the start of the faction's first recorded membership period. **Figures cover events from that
moment on**: members who left before membership history existed (migration `0034`) cannot be reconstructed. Show it next to the board ("Tracked since ...").

## 5. What is counted (inherited, not re-implemented)

The leaderboard reuses the Phase 5 attribution exactly (`docs/FACTION_STATS.md` sections 2-5), because it is the same statement with the faction filter switched off:

* **Verified identity only**: a `player_links` row with `status = 'VERIFIED'`; a name match or a pending link never counts.
* **Membership periods**: a kill/death/bounty counts only while its player was a member of *that* faction (former members keep the kills made while a member; kills before joining
  or after leaving do not count).
* **The faction's own server**: the installation's DayZ server; another server of the same Discord guild does not contribute.
* **Team kills excluded**; kill and bounty de-duplication is inherited (a bounty counts once per counted kill).
* **Achievements** = the number of rows in `hub_faction_achievement_unlocks` (authoritative system unlocks, unique per faction + achievement). Nothing is manually awarded.

## 6. Cache and invalidation

The whole installation table (every faction, every metric) is computed by **one grouped statement** (no per-faction queries) and cached **45 seconds** per
`(organization, installation)` - per-metric orderings are memoized inside that entry, so the effective key is organization + installation + metric and **an entry is never
shared across installations**. Concurrent cold reads share one computation (single-flight); search, cursor and limit are applied to the cached ranking, never to the database.
The cache holds at most 2,048 entries.

It is invalidated (the next read recomputes) by:

* any persisted **kill, death or bounty claim** on the installation's guild + server (the killfeed's `NotifyCombat` bumps a counter; an event that lands *during* a computation
  leaves that computation stale instead of being masked by it);
* **membership changes** (join, leave, removal, role change, leadership transfer), **faction creation**, **faction edits** (name, tag, flag, armband, colors) and **logo changes**;
* an **achievement unlock** (the unlock invalidates the installation's board).

## 7. Performance

PostgreSQL 16, no new index (the plan reuses `idx_kills_killer`, `idx_kills_victim`, `idx_deaths_player` and the membership-history indexes):

| Data | Statement (uncached, all metrics) | Cached page |
|---|---|---|
| 120 factions x 5 members, 20,000 kills, 10,000 deaths, 4,000 claimed bounties | 47 ms | < 1 ms |
| 120 factions x 5 members, 300,000 kills, 150,000 deaths, 60,000 claimed bounties | 746 ms | < 1 ms |

A single faction profile computes in 5 - 18 ms on the same data. `TestLeaderboardScalesWithManyFactions` guards the shape (`PERF_KILLS` scales the history); the exact
totals are asserted there (every member kill and death is counted once, none twice).

## 8. Website handoff: exact DTOs

```ts
type FactionLeaderboardMetric =
  | "KILLS" | "DEATHS" | "KD" | "HEADSHOTS" | "LONGSHOTS" | "BEST_STREAK" | "BOUNTIES_CLAIMED" | "BOUNTY_VALUE" | "ACHIEVEMENTS";

interface FactionLeaderboardResponse {   // GET .../factions/leaderboard
  metric: FactionLeaderboardMetric;      // echoes the (normalized) request
  direction: "DESC" | "ASC";             // ASC only for DEATHS
  items: FactionLeaderboardEntry[];      // at most `limit`, in rank order
  nextCursor: string | null;             // pass back as ?cursor= (with the same metric) for the next page
  limit: number;                         // the effective page size (1..100)
  total: number;                         // factions matching `q` (all factions when there is no q)
  updatedAt: string;                     // RFC 3339, when the ranking was computed (<= 45 s old)
}

interface FactionLeaderboardEntry {
  rank: number;                          // ordinal position on the whole installation board (1 = first); unaffected by q and paging
  factionId: number;                     // link to GET .../factions/{factionId}
  name: string;
  tag: string;
  slug: string;
  logo: FactionLogo | null;              // the uploaded logo, exactly as on the profile; null = show the Champion default
  flagKey: string | null;                // approved DayZ flag key
  armbandKey: string | null;             // approved armband key
  primaryColor: string | null;           // #RRGGBB
  secondaryColor: string | null;
  memberCount: number;
  value: number;                         // the ranked metric: an integer, except KD (decimal, 2 places)
  trackingSince: string | null;          // RFC 3339; figures cover events from then on
  hasTrackedActivity: boolean;           // false => show "No tracked activity"
  stats: {                               // every metric, named like the faction profile's summary
    kills: number; deaths: number; kdRatio: number; headshots: number; longshots: number;
    bestKillStreak: number; bountiesClaimed: number; bountyValueClaimed: number; achievementsUnlocked: number;
  };
}
```

Errors use the standard envelope: `400 INVALID_REQUEST` (unknown metric, bad limit, bad or foreign-metric cursor, over-long `q`, malformed query string), `401`, `404` (mismatched
organization/installation).

Recommended UI: a metric selector (tabs or a dropdown) that re-requests with `?metric=`, a search box bound to `q`, "Load more" using `nextCursor`, the rank badge from `rank`,
and the faction row linking to the profile. Use `direction` to label the board ("Fewest deaths" for `ASC`).

## 9. Not included (by design)

* **No seasons, daily or weekly filters yet.** The response has no season fields; when seasons arrive they will add an optional request parameter and a `season` object,
  leaving this contract intact. All-time figures (from `trackingSince`) are what the board shows today.
* No overall/weighted score, no hidden rating, no cross-installation or global board.
* No materialized table: the board is a query plus a short cache, so it can never drift from the source rows.

## 10. Tests

* `internal/factionstats/leaderboard_test.go` - metric vocabulary, ranking/ties, pagination and rank stability, cursor rejection, cache hit / expiry / invalidation,
  tenant isolation of the cache, single-flight and an event landing during a computation, and a `-race` test of concurrent reads during updates (fake store).
* `internal/factionstats/leaderboard_integration_test.go` (real PostgreSQL): **parity with every faction profile** (former members, unlinked members, team kills, gaps,
  second membership periods), every metric's ranking, deterministic ties, pagination and search over 37 tied factions, isolation across installations / the same guild's
  two servers / organizations, identity and membership timing, cache invalidation on real kill / death / bounty / membership / creation / unlock events, and the 120-faction scale test.
* `internal/app/saas_api_faction_leaderboard_integration_test.go` (real routes over HTTP): shape and value types, public access, the parameter contract, tenant isolation
  (also warm), authentication, privacy, and invalidation through the real handlers.
