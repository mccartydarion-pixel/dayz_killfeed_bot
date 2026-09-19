# Bounty system (Phase 1)

Server-scoped bounties with an atomic, persistence-ordered claim, a persistent
public board (`BOUNTY` route) and a lifecycle feed (`BOUNTY_TRACKING` route).

Phase 1 deliberately has **no economy**: no purchasing, casino, shop or cash. A
bounty's *amount* is what the claimant is awarded in **Champion Points** (the
existing `point_transactions` / `player_points` ledger). Nothing is deducted from
whoever places a bounty.

## What already existed (audited) and what this reuses

A bounty system was already built in Phase 4.5; Phase 1 extends it instead of
duplicating it.

| Piece | Where | Status |
|---|---|---|
| `bounties` table (ACTIVE/CLAIMED/EXPIRED/CANCELLED, ADMIN/AUTOMATIC, `reward_points`, `starts_at`/`expires_at`, claimant + kill) | migration `0007` | reused; extended by `0029` |
| Points ledger (`point_transactions` with a unique `source_key`, `player_points`) | migration `0007` | reused for awards |
| Automatic **streak bounties** (`RewardForStreak`: 10/15/20/25 kills -> 500/750/1000/1500 pts) | `internal/bounties` | reused, rule unchanged |
| Claim on a persisted kill (`ProcessPersistedKill`), already excluding self-kills and same-faction kills | `internal/app/app.go` | reused; now server-aware and stacking |
| Kill card: `BountyClaimed` story ("BOUNTY CLAIMED" header, "💰 BOUNTY" badge) and `BountyTarget` ("wanted") | `killfeed.Event`, `presentation` | reused unchanged |
| `/bounty create/list/status/cancel` | `discord/competitive_commands.go` | reused; now goes through the service |
| Expiry sweeper (every competitive-scheduler tick) | `internal/app/app.go` | reused; now reports each expiry once |
| "MOST WANTED" section of the live-panels message | `discord/panels` | reused; now shows names, not player ids |

What did **not** exist: server scope (bounties were guild-wide), stacking (a unique
index allowed one active bounty per target), a placement service with tenant
validation, a public board channel, and a lifecycle feed.

## State model

```
              place / streak tier
                     |
                     v
   +------------- ACTIVE -------------+
   |        |          |              |
 claim   expire     cancel        (increase: stays ACTIVE,
   |        |          |             amount only goes up)
   v        v          v
CLAIMED  EXPIRED   CANCELLED
```

Every transition is a single status-guarded `UPDATE ... WHERE status='ACTIVE'
... RETURNING`, so a bounty leaves ACTIVE at most once and exactly one of two
racing transitions (claim vs cancel vs expire) wins.

Fields: `id, guild_id, server_id (nullable), target_player_id, created_by_type
(ADMIN | AUTOMATIC), created_by_discord_user_id, status, reward_points (the amount),
reason, starts_at, expires_at (nullable), claimed_by_player_id, claimed_kill_id,
claimed_at, created_at`. The target's display name is read from `players`.

### Migration `0029_bounties_server_scope` (additive)

* `server_id BIGINT NULL REFERENCES game_servers(id) ON DELETE CASCADE`. **NULL keeps
  every pre-existing bounty guild-wide** - claimable on any server of the guild
  exactly as before. A set `server_id` scopes the bounty to that server.
* Drops the historical blanket `uq_active_bounty_target` and adds
  `uq_active_automatic_bounty (guild_id, target_player_id) WHERE status='ACTIVE' AND
  created_by_type='AUTOMATIC'`: manual bounties may stack, the streak bounty keeps
  its "at most one active per target" rule (also what makes concurrent kills unable
  to create two).
* Indexes: `(guild_id, server_id, status)`, `target_player_id`, `created_at`,
  `claimed_at`. Historical migrations are untouched.

## Server scope

Bounties are server-scoped. A bounty is claimable by a kill iff it is in the same
guild **and** (`server_id IS NULL` **or** `server_id` = the server the kill happened
on). A bounty for server A can never be claimed by a kill on server B; a kill with
an unknown server (0) can only claim guild-wide bounties.

## Placement (application service)

`bounties.Service.Place(PlaceRequest{GuildID, ServerID, OrganizationID,
TargetPlayerID, Amount, PlacedBy, Reason, ExpiresAt})`:

* `Amount` must be `1 .. MaxInt32` (the column is a 32-bit integer);
* the target must exist **in that guild**;
* a named server must belong to that guild; when `OrganizationID` is given the
  server must not be claimed by a *different* organization, and an
  organization-scoped caller **must** name a server (so it can never create a
  guild-wide bounty over servers it does not own);
* `ExpiresAt` (optional, default none) must be in the future.

Also `Increase` (raise an active bounty; must really be higher), `Cancel`, and
`Sweep` (expiry). No payment is taken. The Discord `/bounty create` command calls
`Place` (guild-wide, as it always did).

### System bounties

Only the existing streak rule creates them: reaching a streak tier puts a
**guild-wide AUTOMATIC** bounty on the killer, and a higher tier raises it (never
lowers it). No new automatic rules were invented. One deliberate change: a manual
bounty on a player no longer blocks their streak bounty (they coexist and stack),
because the old blanket uniqueness rule is what stacking replaces.

## Claim logic and ordering

```
ADM line -> parser -> PLAYER_KILL -> ADM dedupe
   -> PersistenceQueue: durable, non-duplicate kills INSERT   (persist first)
   -> ProcessPersistedKill  (only after a successful insert)
        -> BountyRepository.ClaimForKill  (one transaction)   (atomic claim, commit)
        -> lifecycle notification + board reconcile           (publish last)
   -> KillfeedPublisher.PublishKill  (the normal kill card, with the bounty flags)
```

* A bounty is **never** claimed from a raw ADM line: only after the kill is
  durably persisted. A replayed kill fails the durable-insert uniqueness
  (`ErrDuplicate`), so `ProcessPersistedKill` never runs for it - and even if it
  did, the claim is status-guarded and matches nothing the second time.
* **Only proven PvP kills claim.** Suicides, environment and ambiguous deaths are
  `deaths`, not `kills`, and never reach the claim. Self-kills (`killer == victim`),
  unresolved players and same-faction (team) kills do not claim
  (`bounties.AllowTeamKillClaims = false`, preserved).
* **Atomicity.** `ClaimForKill` is one statement,

  ```sql
  UPDATE bounties SET status='CLAIMED', claimed_by_player_id=$3, claimed_kill_id=$4, claimed_at=$5
  WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE'
    AND starts_at<=$5 AND (expires_at IS NULL OR $5<expires_at)
    AND (server_id IS NULL OR server_id=NULLIF($6,0))
  RETURNING ...
  ```

  followed, in the **same transaction**, by the point awards. Two workers or
  retries racing on one victim serialise on the row locks; the loser re-evaluates
  the `WHERE` against the committed rows and matches nothing. Awards are also
  idempotent through the unique `point_transactions` source key
  (`bounty:<id>`). Integration tests run 24 concurrent claimants at one bounty and
  16 at a 5-bounty stack: every bounty is claimed exactly once and paid exactly
  once.

### Stacking policy

Several active bounties may sit on one target. **One kill claims every eligible
bounty on the victim** - all guild-wide ones plus all scoped to that kill's
server - in one transaction, and reports `count` and `total`. Bounties for other
servers stay ACTIVE. The public board and the `/bounty` views show the per-target
**sum** ("Target - 126,000 pts (2 bounties)"). (This replaces the earlier
one-active-bounty-per-target index; see the migration above.)

### Interaction with the KILLFEED and "bounty kill"

The kill card is never suppressed or duplicated. There is still exactly one
definition of a bounty kill: `ProcessPersistedKill` sets `ev.BountyClaimed` and
`ev.BountyPoints` (now the **stacked total**) from the durable claim, which drives
the existing "BOUNTY CLAIMED" story style and badge. `ev.BountyTarget` ("wanted"
badge on a killer with an active bounty) is now server-aware.

## Routing

Both routes use the shared `internal/routing` resolver on the `(guild row, server)`
identity - never by guild alone - and have **no fallback** to any other channel.

* **`BOUNTY` - the public board** (`discord.BountyBoard`). One persistent message
  per routed channel, recorded durably in `guild_route_panels` (the same table the
  link/stats/leaderboard panels use), so restarts and route changes edit or move
  the *same* board instead of posting a new one. Content per channel = top 10
  targets by combined active amount for the servers routed to that channel (plus
  guild-wide bounties); a server's own channel shows only its own bounties, a
  channel shared by several servers shows the union. Names only - no internal ids.
  ```
  🎯 ACTIVE BOUNTIES

  1. PlayerA — 250,000 pts
  2. PlayerB — 126,000 pts (2 bounties)
  ```
  Reconciled on startup, on every lifecycle event, on any in-process route write,
  and every 30s (which also picks up expiries and out-of-process route changes).
  An edit that would change nothing is skipped. Route removed -> the board is
  retired; a failed route lookup leaves it alone; a deleted board message is
  re-created once.
* **`BOUNTY_TRACKING` - the lifecycle feed** (`discord.BountyTracker`), separate
  from the board:
  ```
  🎯 BOUNTY PLACED        📈 BOUNTY INCREASED     💰 BOUNTY CLAIMED        ⌛ BOUNTY EXPIRED     🚫 BOUNTY CANCELLED
  Target: PlayerA         Target: PlayerA         Hunter: PlayerB          Target: PlayerA       Target: PlayerA
  Value: 100,000 pts      Value: 150,000 pts      Target: PlayerA          Value: 100,000 pts    Value: 100,000 pts
                                                  Value: 100,000 pts
                                                  Weapon: M4-A1   <- only if the kill has one
                                                  Distance: 86m   <- only if the kill has one
  ```
  Automatic ones add `Source: kill streak`; a stacked claim adds `Bounties: N`.
  Event routing: a server-scoped bounty's events go to **that server's** route; a
  claim (or a streak placement) goes to the server the **kill** happened on; an
  event with no server (guild-wide) goes to **every** server's route, deduplicated
  by channel. Events carry no internal ids. Delivery is one bounded queue (200,
  oldest dropped) and one goroutine, at most 20 events per 2s tick in messages of
  up to 10 cards; names are sanitised and `AllowedMentions` is empty.

Amounts are shown as Champion Points (`pts`), never as a currency.

## Failure behaviour

Bounty correctness never depends on Discord or on any route being configured.

| Situation | Behaviour |
|---|---|
| `BOUNTY` route absent | database works; no board; nothing posted |
| `BOUNTY_TRACKING` route absent | database works; lifecycle events are no-ops |
| Route lookup error | no-op + throttled `channel_route_fallback` warning; an existing board is left alone |
| Discord send/edit fails after a claim | the claim stays committed; the failed card is logged and dropped (never retried, so nothing can be re-claimed); the board catches up on its next reconcile |
| Panicking notifier / sender | recovered; the committed change is unaffected |
| Process restart | the board is reconstructed from the database and the recorded message; lifecycle cards for events still queued in memory are lost (they are announcements, not state) |
| Replayed kill | durable-insert duplicate -> no claim, no card, no second kill card |

Order is always: **persist kill -> atomic claim -> commit -> notify (Discord)**.

## Expiry

`expires_at` is nullable; Phase 1 creates none by default (the admin command sets
one). The sweeper is the existing single scheduler tick: one
`UPDATE ... WHERE status='ACTIVE' AND expires_at<=now RETURNING` transitions every
due bounty and yields exactly the rows it changed (concurrent sweepers get disjoint
sets), each reported once as `EXPIRED`. There is no goroutine per bounty, and
the board's SQL also filters by time, so an expired bounty disappears from the
board even before the sweeper runs.

## Future economy integration

The seams are deliberately narrow so a Phase 2 can add money without redesign:

* **Placement** is one method (`Service.Place`); charging the placer (and
  refunding on cancel/expire) belongs there, inside the same transaction as the
  insert.
* **Payout** is the award loop at the end of `ClaimForKill`. As of the economy
  Phase 1 it pays each bounty through the economy ledger (`CreditTx`, reference
  `bounty:<id>`, same transaction and idempotency key - see `docs/ECONOMY_SYSTEM.md`),
  and the claimed bounties carry their ledger entry so an `ECONOMY` card can follow.
* The amount is already one integer per bounty; a currency would add a
  `currency` column rather than reinterpret `reward_points`.

## Known limitations

* Amounts are 32-bit integers.
* `server_id` uses `ON DELETE CASCADE`: deleting a server deletes its bounty rows
  (including claimed history) - servers are deactivated, not deleted, in normal use.
* The runtime is still a single configured Discord guild, so the board and feed
  serve that guild's servers.
* Lifecycle cards for events queued at the moment of a crash are not replayed.
