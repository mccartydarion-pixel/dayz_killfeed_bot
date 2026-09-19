# Faction Hub - competitive stats, achievements and activity (Phase 5)

Backend only. Code: `internal/factionstats` (the service: figures, cache, achievements, activity),
`internal/repository/faction_hub_stats_repository.go` (grouped SQL), `faction_hub_history.go`
(membership history and activity writes), `internal/app/saas_api_faction_stats.go` (three read-only routes and
the profile block). Migration `0034_faction_stats_history`.

The rule for everything below: **a faction figure is derived from real Champion runtime data or it does not exist.**
Nothing is stored twice, nothing is estimated, and a member who cannot be attributed is reported as such instead of
guessed. The HTTP handlers hold no statistics logic; they call `factionstats.Service`.

## 1. Data-source audit

| Source | Table / column | Used | Notes |
|---|---|---|---|
| Kills | `kills` (`guild_id`, `server_id`, `killer_player_id`, `victim_player_id`, `headshot`, `longshot`, `distance`, `event_time`, `created_at`, `killer_streak_after`) | **yes** | Dedupe is `UNIQUE(guild_id, event_fingerprint)` at write time. A row is a PvP kill (suicides and environment deaths never reach it). |
| Headshots | `kills.headshot` | **yes** | The killfeed's own classification. |
| Longshots | `kills.longshot` | **yes** | The killfeed's flag (a kill at 100 m or more; `isLongshotEvent`). |
| Deaths | `deaths` (`player_id`, `death_type`, `server_id`, `event_time`) | **yes** | The project's existing definition of a death (`StatsRepository`): rows of the `deaths` table (`UNKNOWN` and `SUICIDE`; a `PVP` death type exists in code but is never written). |
| Kill streaks | derived from kills and death events | **yes, re-derived** | `kills.killer_streak_after` and `player_combat_stats.best_streak` are *guild-wide* and include kills made before joining the faction, so they cannot be attributed. The faction streak is recomputed per membership period (see 5). |
| Bounties | `bounties` (`status='CLAIMED'`, `claimed_by_player_id`, `claimed_kill_id`, `reward_points`) | **yes** | A bounty row is claimed once; a claim only counts when it was earned by a counted kill. |
| Server records | `record_events` (`kill_id`, `player_id`, `created_at`) | **yes** (activity only) | Scoped to the faction's server through the kill it references. |
| Player identities | `players`, `player_links` (`status='VERIFIED'`) | **yes** | The only attribution path. |
| Connections / playtime | `player_server_activity.total_observed_seconds` | no | Observed, not authoritative playtime; not part of this phase. |
| Hits | none | not available | Hit events are runtime-only (the HITFEED); no hit table exists, so there are no hit statistics. |

Kill, death and bounty rows are **read in place**. The Hub adds only what time-aware attribution needs: membership
periods (`hub_faction_membership_history`), public hub events (`hub_faction_activity`) and achievement unlocks.

## 2. Identity attribution

A member's figures are attributed through their **verified link**: `player_links` on the installation's guild with
`status = 'VERIFIED'`, joined on the member's Discord user id. Never by name.

* **Linked** members are attributed (`identity: "LINKED"`, `statsEligible: true`, `gamertag` = the linked player's name).
* **Unlinked** members (no link, or a `PENDING`/unverified one) get `identity: "UNLINKED"`, `statsEligible: false`, all figures zero and
  `gamertag: null`. They are still listed (they are members) so the roster is complete and the UI can say why.
* A player with the same in-game name as a member is **not** that member. A same-named player in another guild is not either
  (`player_links` is per guild).
* The link is resolved **live** at read time. When a pending link is verified, the kills that player made *while the user was a
  faction member* are attributed from then on. `hub_faction_membership_history.player_identity_id` is only an informational snapshot at join
  time; attribution never reads it.
* `linkedMemberCount` in the summary says how many current members are attributable.

## 3. Membership timing

`hub_faction_members` holds only the current state (a leave or removal deletes the row), so it cannot answer "was this player in the
faction when the kill happened". `hub_faction_membership_history` records one row per membership **period**
`[joined_at, left_at)`; `left_at IS NULL` means the member is still in.

* Written inside the same transaction as the membership change: joining (creating a faction, accepting an application) opens a period
  at the member row's own `joined_at`; leaving and removal close it (`left_at = clock_timestamp()`). Rows are never deleted.
* At most one open period per user per installation (partial unique index), matching the one-faction-per-installation rule.
* **Counting rule**: an event counts only if `joined_at <= at < left_at` (`at = COALESCE(event_time, created_at)`). Half-open: a kill exactly
  at `left_at` is not counted. A kill before joining, after leaving, or during a gap between two periods is not counted; every period of
  a rejoin counts.
* **Backfill**: migration `0034` inserts a period for every current member using their real `joined_at`. Members who left the faction
  *before* this migration have no recorded period and cannot be reconstructed, so their earlier kills are not credited. The summary's
  `trackingSince` (the faction's first recorded period) says from when the figures cover.
* A faction that is deleted takes its history with it (cascade); a deleted user's periods go with the user.

## 4. What is counted

All figures are computed for the **faction's server** (`hub_factions.game_server_id`) on the installation's guild, in one grouped statement
(no per-member queries). `kills`/`deaths` rows of another server of the same guild, of another guild, or with no `server_id` (legacy) are never
counted.

* **kills** - kills whose killer is a member's verified player at a time inside a period. **A kill of a fellow member of the same faction
  (a team kill) is not a counted kill** (and never counts toward headshots, longshots, streaks or bounties).
* **deaths** - `deaths` rows of a member's player inside a period (a teammate's team-kill death is a death).
* **kdRatio** - `kills / max(deaths, 1)` rounded to two decimals: the project's existing convention (`StatsRepository`). With zero deaths the
  ratio is the kill count itself: no division by zero, no infinity, never NaN.
* **headshots / longshots** - counted kills whose `headshot` / `longshot` flag is set.
* **bountiesClaimed / bountyValueClaimed** - claimed `bounties` rows whose claim kill is a counted kill of that member; stacked bounties claimed by one
  kill are each counted. Unclaimed, expired and other-server bounties are not.
* **Dedupe is inherited.** A replayed kill/death fails the `(guild, event_fingerprint)` unique key when persisted, so it never exists twice; a bounty is
  claimed once (`claimed_kill_id` is one row). Recomputing is idempotent.

## 5. Streaks

`currentKillStreak` and `bestKillStreak` are recomputed from the counted events, **per membership period**: a streak is a run of counted kills between two
death events - a `deaths` row, or being the victim of a PvP kill - inside one period. A streak never carries across a leave/rejoin, and a death outside a
period does not count as the faction's. At the same instant a kill sorts before a death.

* Member `bestKillStreak` = the longest run in any of their periods; faction `bestKillStreak` = the best single member's (streaks are not summed).
* `currentKillStreak` (faction) = the largest ongoing run among **current** members; a former member's run never counts as current.

## 6. Member contributions and ranking

`memberContributions` lists every current member and every former member with counted events, ordered **kills DESC, deaths ASC, member id ASC (former
members last), user id**. There is no skill rating or hidden score. Rows carry `status` (`ACTIVE` | `FORMER`), `identity` (`LINKED` | `UNLINKED`),
`statsEligible`, the role and `memberId` (both `null` for a former member) and the same figures as the summary.

## 7. Achievements

System-defined only (code catalog `factionstats.Definitions`), never user-editable. Only the unlock records are stored
(`hub_faction_achievement_unlocks`, `UNIQUE(faction_id, achievement_key)`). Every condition is provable from the data above:

| Key | Name | Condition | Progress |
|---|---|---|---|
| `FIRST_BLOOD` | First Blood | 1 counted kill | kills |
| `KILLS_100` | Centurions | 100 counted kills | kills |
| `KILLS_500` | Warband | 500 counted kills | kills |
| `KILLS_1000` | Legion | 1,000 counted kills | kills |
| `HEADHUNTERS` | Headhunters | 25 headshot kills | headshots |
| `LONG_RANGE` | Long Range | 10 longshot kills | longshots |
| `BOUNTY_HUNTERS` | Bounty Hunters | 5 claimed bounties | bounties |
| `KILLING_MACHINE` | Killing Machine | a member reaches a 10-kill streak while in the faction (see 5) | best streak |
| `FULL_SQUAD` | Full Squad | 5 members at once (stays unlocked when members leave) | members |
| `VETERAN_FACTION` | Veteran Faction | 30 days since the faction was created | days |

Every achievement has a measurable value, so `progress`/`target`/`unit` are exposed for all of them (progress capped at the target; an unlocked one shows full
progress). The catalog order and keys are stable.

* **Idempotent and race-safe.** `InsertUnlock` is `INSERT ... ON CONFLICT (faction_id, achievement_key) DO NOTHING`: concurrent evaluators unlock exactly once and only
  the winner reports the key (tested with concurrent evaluators against PostgreSQL). `unlocked_at` is the moment the condition was **really met** when the data says so
  (the n-th counted kill/headshot/longshot/bounty; `created_at + 30 days`), otherwise the evaluation time.
* **Evaluation is event-driven, not a recalculation per kill.** The killfeed calls `NotifyCombat` after a kill, death or bounty claim is durably persisted (nil-safe, never
  blocking). It (a) invalidates cached figures of that guild+server and (b) for a kill queues the killer's player. A worker drains the queue every 5 seconds, resolves
  the affected factions (current members by verified link on that server) and evaluates each **once per batch**, however many kills arrived. Evaluation is the same grouped
  query (about 20 ms even with a 300,000-kill guild history).
* **Read-time safety net.** `GET .../achievements` evaluates first if the figures already prove an unrecorded achievement, so it is never stale.
* **Historical backfill / reconciliation.** `ReconcileAll` evaluates every faction: it unlocks whatever existing data already proves (with the real historical
  `unlocked_at`) and the time-based `VETERAN_FACTION`. It runs once a minute after start and then daily, is idempotent, and is **silent**: unlocking sends nothing to
  Discord (no retroactive announcements).

## 8. Activity

`GET .../activity` is the faction's **public** feed, newest first. It merges five sources at read time (each limited before the merge, so a page never scans a
faction's history); combat events are not copied anywhere.

| Type | Source | `details` |
|---|---|---|
| `FACTION_CREATED`, `MEMBER_JOINED`, `MEMBER_LEFT` | `hub_faction_activity` | none (a removal reads as `MEMBER_LEFT`: moderation stays private) |
| `MEMBER_PROMOTED`, `MEMBER_DEMOTED` | `hub_faction_activity` | `role` |
| `LEADERSHIP_TRANSFERRED` | `hub_faction_activity` | `previousLeader` (display name); `member` is the new leader |
| `FACTION_UPDATED`, `FACTION_LOGO_CHANGED` | `hub_faction_activity` | none (says *that*, never *what*) |
| `KILL` / `HEADSHOT` / `LONGSHOT` | counted kills | `victim`, `weapon`, `distanceMeters`, `headshot`, `longshot` (one event per kill; type priority HEADSHOT > LONGSHOT > KILL) |
| `BOUNTY_CLAIMED` | claimed bounties | `target`, `rewardPoints`, `bounties` (one event per claiming kill, stacked bounties combined) |
| `SERVER_RECORD` | `record_events` | `record` (the record type) |
| `ACHIEVEMENT_UNLOCKED` | unlock rows | `key`, `name` (no `member`) |

* **Privacy.** Events carry the member's display name, Discord id/avatar (already public on member lists) and linked gamertag - and no internal numeric id,
  application message, reviewer, moderation detail or free text. `hub_faction_activity.detail` is constrained to a role key.
* **Not the audit log.** The `slog` audit events (`faction_*`, ids only) stay internal and separate; nothing in the public feed is read from logs.
* **Pagination.** `limit` (default 20, max 100; `0` or non-numeric is 400) and an opaque `cursor` (the previous page's `nextCursor`; invalid is 400). Keyset order is
  `(occurredAt, source, id)` all descending, so events at the same instant never repeat or vanish across pages (tested).
* Event `id` is opaque (`h12`, `k345`, ...).

## 9. Cache

`GetFactionStats` results are cached for **45 seconds** per `(organization, installation, faction)` (max 2,048 entries) with single-flight (a cold burst is one query).
The key includes all three ids, so an entry can never answer another tenant. It is invalidated by: any persisted kill, death or bounty claim on the faction's guild+server
(a counter bump per guild+server; an event that lands *during* a computation invalidates the entry it produced), membership and role changes through the API (`Invalidate`),
and achievement unlocks. Activity is not cached (indexed keyset queries).

## 10. Performance

One grouped statement per faction, driven by the faction's few linked players through the existing `idx_kills_killer`, `idx_kills_victim`, `idx_deaths_player` and bounty
indexes; no per-member queries. Measured on PostgreSQL 16 with a guild history of 300,000 kills, 150,000 deaths and 60,000 claimed bounties (other players, same server) and a
10-member faction: `ComputeStats` 22 ms, activity page 19 ms, achievement evaluation 22 ms (20,000 kills: 13 ms, 14 ms, 13 ms). **No new index was needed**; the plans do not require one at this
scale. Membership history and activity have their own faction-keyed indexes. `TestStatsQueriesScaleWithTheFactionNotTheGuild` guards the shape (`PERF_KILLS` scales it up).

## 11. API

All routes are under `/api/saas/organizations/{organizationID}/installations/{installationID}/factions/{factionID}`, read-only, and open to **any synced Champion user** (no
faction or organization membership needed - they back the public faction page). Organization + installation + faction are scoped on every read; another tenant's ids are `404`;
nothing aggregates across installations. Errors use the standard envelope.

| Route | Returns |
|---|---|
| `GET .../stats` | `FactionStats` |
| `GET .../activity?limit=&cursor=` | `ActivityPage` |
| `GET .../achievements` | `{ items: FactionAchievement[], unlockedCount, total }` |
| `GET .../factions/{factionID}` (existing) | the profile now includes `stats: FactionStatsSummary | null` |

### Website handoff: exact DTOs

```ts
interface FactionStatsSummary {
  kills: number; deaths: number;
  kdRatio: number;                 // kills / max(deaths,1), 2 decimals; deaths=0 -> equals kills
  headshots: number; longshots: number;
  currentKillStreak: number;       // largest ongoing streak among CURRENT members
  bestKillStreak: number;          // best single-member streak (per membership period)
  bountiesClaimed: number; bountyValueClaimed: number;
  memberCount: number;
  linkedMemberCount: number;       // members whose figures are attributable
  achievementsUnlocked: number;
  trackingSince: string | null;    // RFC 3339: the first recorded membership period
}

interface FactionStats {           // GET .../stats
  summary: FactionStatsSummary;
  memberContributions: FactionMemberContribution[];   // kills DESC, deaths ASC, member id
  updatedAt: string;               // RFC 3339, when the figures were computed (<= 45 s old)
}

interface FactionMemberContribution {
  memberId: number | null;         // current membership id (use for manage actions); null for a former member
  discordUserId: string; displayName: string; avatar?: string;
  gamertag: string | null;         // the verified linked gamertag
  role: "LEADER" | "OFFICER" | "MEMBER" | null;       // null for a former member
  status: "ACTIVE" | "FORMER";
  identity: "LINKED" | "UNLINKED"; statsEligible: boolean;   // UNLINKED => all figures 0
  joinedAt: string;                // start of the current (or latest) membership period
  kills: number; deaths: number; kdRatio: number; headshots: number; longshots: number;
  currentKillStreak: number; bestKillStreak: number;
  bountiesClaimed: number; bountyValueClaimed: number;
}

interface FactionAchievement {     // GET .../achievements -> items[] (stable order)
  key: "FIRST_BLOOD" | "KILLS_100" | "KILLS_500" | "KILLS_1000" | "HEADHUNTERS" | "LONG_RANGE"
     | "BOUNTY_HUNTERS" | "KILLING_MACHINE" | "FULL_SQUAD" | "VETERAN_FACTION";
  name: string; description: string;
  unlocked: boolean; unlockedAt: string | null;
  progress: number; target: number; unit: string;   // progress <= target; "kills"|"headshots"|"longshots"|"bounties"|"members"|"days"
}

interface FactionActivityEvent {   // GET .../activity -> items[]
  id: string;                      // opaque, stable event key
  type: "FACTION_CREATED" | "MEMBER_JOINED" | "MEMBER_LEFT" | "MEMBER_PROMOTED" | "MEMBER_DEMOTED"
      | "LEADERSHIP_TRANSFERRED" | "FACTION_UPDATED" | "FACTION_LOGO_CHANGED"
      | "KILL" | "HEADSHOT" | "LONGSHOT" | "BOUNTY_CLAIMED" | "SERVER_RECORD" | "ACHIEVEMENT_UNLOCKED";
  occurredAt: string;
  member: { discordUserId: string; displayName: string; avatar?: string; gamertag: string | null } | null;
  details?: Record<string, unknown>;   // per type, see section 8
}
interface ActivityPage { items: FactionActivityEvent[]; nextCursor: string | null; limit: number }
```

Treat `type` as an open set (ignore unknown types) so new event kinds do not break the UI.

## 12. Faction ranking readiness

`FactionStatsSummary` is deliberately flat and comparable - `kills`, `kdRatio`, `headshots`, `longshots`, `bountiesClaimed`, `achievementsUnlocked` - so a faction leaderboard
can rank on any of them per installation without another model. No overall skill score exists or is implied. **Phase 6 delivers that leaderboard**
(`GET .../factions/leaderboard`, `docs/FACTION_LEADERBOARDS.md`): it uses the very same SQL definition (`hubStatsCTE`, with the faction filter switched off) and the same
attribution rules as this document, so a leaderboard value always equals the faction's `summary` value; a parity test compares every faction's `stats` on both surfaces.

## 13. Limits to know

* Members who left before migration `0034` are not attributable (no history). Figures cover from `trackingSince`.
* `deaths` follows the project's existing definition (rows of the `deaths` table); a server whose ADM does not log deaths shows fewer deaths and a higher K/D.
* `event_time` (the ADM timestamp) is used when present, else the persistence time. Clock skew of a few seconds around `joined_at`/`left_at` can move a boundary kill.
* Team kills are excluded by design; there is no per-faction "friendly fire" statistic.
* Playtime and hit statistics are not available (see 1).
