# Champion Points economy (Phase 1)

Phase 1 turns the existing Champion Points tables into a durable economy
foundation: one append-only ledger, one materialized spendable balance, atomic
credit/debit, idempotent payouts, admin adjustments, and an `ECONOMY` lifecycle
feed. **No casino and no shop exist yet** - nothing can be bought.

## What already existed (audited) and what this reuses

Points were already stored in two tables, used by three earn paths. There is no
second currency: the economy is built on them.

| Existing | Role today | Economy Phase 1 |
|---|---|---|
| `player_points` (PK `guild_id, player_id`) | `lifetime_points`, `season_points` - leaderboard scores | keeps both **unchanged in meaning**; gains `balance` |
| `point_transactions` (`UNIQUE(guild_id, player_id, reason_type, source_key)`) | append-only-by-convention award log, the idempotency key for payouts | becomes **the ledger** (signed BIGINT amount, `balance_after`, audit columns, real append-only trigger) |
| `PointsRepository.Award` | generic earned credit | now a thin wrapper over the economy credit |
| `bounties` claim (`BOUNTY_CLAIM`, `bounty:<id>`) | paid inside the claim transaction | same transaction, same key, now through the ledger |
| `FinalizeEvent` (`EVENT_FIRST/SECOND/THIRD_PLACE`, `event:<id>:<place>`) | event prizes | same keys, now through the ledger |

Transaction types and idempotency keys that already exist in production rows are
**not renamed** (`BOUNTY_CLAIM`, `EVENT_*`): they are part of every existing row's
uniqueness key, so a replay of an already-paid bounty or event is still a no-op.

## Two numbers: score vs. balance

| Column | Meaning | Earned credit | Admin credit | Debit | Season reset |
|---|---|---|---|---|---|
| `lifetime_points` | leaderboard score | + | - | - | kept |
| `season_points` | seasonal leaderboard score | + | - | - | **reset to 0** |
| `balance` | **spendable** points | + | + | - | **kept** |

* "Earned" = a bounty reward, an event prize or a `SYSTEM_REWARD`. It raises all
  three numbers.
* An admin credit adds spendable points only, so an admin cannot manufacture
  leaderboard rank. A debit removes spendable points only - spending never lowers a
  player's rank.
* `balance` never goes negative (a `CHECK`), and a season rollover never touches it.

## Schema - migration `0030_economy_ledger` (additive)

* `point_transactions.amount` -> `BIGINT` (signed: debits are negative);
  `+ balance_after BIGINT`, `+ description TEXT`, `+ created_by TEXT`,
  `+ server_id BIGINT REFERENCES game_servers(id) ON DELETE SET NULL`.
* `player_points.balance BIGINT NOT NULL DEFAULT 0` with
  `CHECK (balance >= 0)` (`player_points_balance_nonneg`).
* One-time backfill (exported as `database.EconomyBackfillSQL`, idempotent, tested
  directly): `balance := lifetime_points`, and legacy ledger rows get a running
  `balance_after`. Nothing existing loses information.
* `idx_point_transactions_history (guild_id, player_id, id DESC)` for history.
* Append-only trigger: a ledger row's amount, type, reference, description,
  creator and timestamp can never be updated (an already-set `balance_after` is
  also frozen). Legacy `balance_after` may be filled in once; the
  `ON DELETE SET NULL` foreign keys may null `season_id`/`server_id`; deleting a
  guild or player still cascades.
* Historical migrations are untouched.

## Atomic operations

All balance changes go through one function (`applyLedger`,
`internal/repository/economy_repository.go`); every earn path and both admin
operations use it.

* **Credit.** `INSERT ... ON CONFLICT DO UPDATE` on `player_points` (one atomic
  statement; the row lock serialises concurrent operations on the player), then the
  ledger row with the resulting `balance_after`.
* **Debit.** `UPDATE player_points SET balance = balance - $n WHERE ... AND
  balance >= $n RETURNING balance`. The guard is evaluated against the committed value
  after any lock wait, so racing debits can never spend the same points twice; zero
  rows -> `ErrInsufficientFunds`, and nothing is written. The `CHECK` is a second
  line of defence.
* **Ledger and balance share a transaction** - either both change or neither.
  `SUM(amount)` for a player always equals `balance`, and the newest
  `balance_after` equals it too (asserted after every concurrency test).
* **Bounds.** Amounts are `1 .. 10^15` (`MaxLedgerAmount`); the balance is a
  BIGINT, and an operation that would overflow it fails and changes nothing (no
  wrap-around). Anything larger than the cap is rejected before touching the
  database.

### Idempotency

`(guild_id, player_id, type, reference)` is the key (the existing unique index). A
call with a reference that was already applied returns the original ledger entry
(`Duplicate = true`), moves no balance, writes no row and fires no event. The
reference - not the amount - identifies a transaction. Two requests racing on the
same new reference: exactly one applies; the loser's balance change is undone in a
savepoint (so the caller's surrounding transaction survives) and it returns the
winner's entry. An empty reference means "always a new transaction" (admin
adjustments).

### Bounty integration

`ClaimForKill` already claimed bounties and paid them in **one** transaction. It now
pays through the ledger with `CreditTx`: for each claimed bounty one `BOUNTY_CLAIM`
credit, reference `bounty:<id>`, attributed to the kill's server, `created_by =
SYSTEM`. The whole claim - status changes, every payout, every balance update -
commits or rolls back together, so a crash can never claim a bounty without paying it
or pay without claiming. A replayed claim finds the bounty already `CLAIMED` and pays
nothing; a direct re-award of a paid bounty hits the reference and is a duplicate,
including for rows written before this migration.

## Scope decision

**Guild-wide, as it was.** A balance is per `(guild, player)`; it is not split per
server. Reasons: `player_points`/`point_transactions` were already guild-wide, the
leaderboards read them that way, a player's linked identity is per guild, and
splitting would have silently reset every existing balance. Transactions still
record *where* they happened (`server_id`, nullable) so per-server views or a
future per-server currency can be derived without a schema change. Guild-wide
events (admin adjustments) are announced on every server's `ECONOMY` route; a
server-attributed event (a bounty reward) goes to that server's route only.

## Service API (`internal/economy`)

| Method | Notes |
|---|---|
| `Credit` / `Debit` | credit types `ADMIN_CREDIT`, `SYSTEM_REWARD` (an earned credit); debit type `ADMIN_DEBIT`; validated type, amount, player |
| `AdminCredit` / `AdminDebit` | require an actor (Discord user id) - audit; amount `1..MaxAmount` |
| `Balance(guild, player)` | `0` for a valid player with no transactions; `ErrPlayerNotFound` for a player not in that guild |
| `History(guild, player, limit, cursor)` | newest first; default 10, **hard cap 50**; keyset cursor (`NextCursor != 0` = older entries remain) |

Tenant checks: a request may carry `OrganizationID`; the named server must exist,
belong to the request's guild (`ErrServerNotInGuild`) and to that organization
(`ErrForbiddenTenant`); an organization-scoped call must name a server
(`ErrServerRequired`). Players are always looked up inside the requested guild.

## Commands and panel

`/economy` (registered like the other slash commands):

| Subcommand | Who | Behaviour |
|---|---|---|
| `balance [player]` | anyone (`player` admins only) | your own balance via your **verified** link; ephemeral |
| `history [player]` | anyone (`player` admins only) | last 10 transactions with type, signed amount, reason, resulting balance; ephemeral |
| `credit player amount [reason]` | Administrator / Manage Server | add points; audit-logged |
| `debit player amount [reason]` | Administrator / Manage Server | remove points; insufficient balance is refused with the balance shown |

* A player is resolved through the existing **linked identity**; only a
  `VERIFIED` link counts (a pending or rejected link carries a player id but proves
  nothing). Unlinked callers get the link instructions.
* The player-stats panel gains two buttons, **My Balance** and **Recent
  Transactions**, which reuse the same renderers (ephemeral).
* Replies never show internal ids.

## Audit trail

Each admin adjustment stores who (`created_by` = the acting Discord user id), the
target (`player_id`), the amount, the reason (`description`, sanitised, 200 chars),
the type and the time. The audit trail is stored in the ledger and is **not
published**: the `ECONOMY` card never names the admin and never shows the reason.

## `ECONOMY` route (runtime routed)

`EconomyFeed` (`internal/discord/economy_feed.go`) is an event feed on the shared
routing model (`routing.Resolver`, key `(guild row, server, ECONOMY)`), same shape as
HITFEED / PVE_FEED / BOUNTY_TRACKING: a bounded queue (200), one goroutine, a 2s tick,
at most 20 events per tick (60 on the final flush), up to 10 cards per message.

```
💰 BOUNTY REWARD          ➕ ADMIN CREDIT             ➖ ADMIN DEBIT
PlayerA earned 125,000 pts   PlayerA received 50,000 pts   PlayerA lost 25,000 pts
Balance: 340,000 pts
```

* **Only after commit.** The economy service and the bounty claim hand the feed an
  event *after* their transaction committed; the feed can only describe committed
  state and never blocks or fails the operation. A panicking notifier is recovered.
* **No fallback.** No `ECONOMY` route -> nothing is sent, never to `KILLFEED` or any
  other channel; the economy works fully in the database. A route lookup error is a
  no-op with a throttled warning.
* **Failure isolation.** A Discord failure is logged and dropped - never retried,
  so it cannot repeat a transaction; the transaction stays committed.
* **Route changes** take effect for the next event (the resolver is invalidated on
  save). Only rewards show the resulting balance on the public card.
* Names are sanitised, and messages never ping.

## Failure behaviour

| Failure | Result |
|---|---|
| Discord down / send fails | transaction committed; card dropped |
| No route / lookup error | transaction committed; nothing sent |
| Insufficient funds | debit refused, nothing written, no card |
| Same reference replayed or raced | applied once |
| Process crash mid-claim | the claim + payout transaction never committed; the kill is replayed and claims once |
| Overflowing credit | refused, nothing written |

## Tests

* Unit: `internal/economy`, `internal/discord` (embeds, command dispatch, admin
  gating, privacy), `internal/bounties` (one economy event per bounty; replays
  silent; notifier failure isolated).
* Real PostgreSQL (`-tags=integration`): `internal/repository/economy_ledger_integration_test.go`
  (initial balance, credit/debit, insufficient funds, 40 racing credits with no lost
  update, 30 racing debits with no double spend, mixed racing operations reconciling
  with the ledger, same-reference race, legacy bounty key, one ledger transaction per
  bounty, replayed claim, event prizes, backfill, append-only trigger, season reset,
  overflow, tenant scoping) and `internal/app/economy_integration_test.go` (the whole
  flow through the real routes and resolver: bounty reward card, stacked bounties,
  admin fan-out without leaking the audit trail, no-route, Discord failure after
  commit, route change/removal, cross-organization isolation).
* Mutation-checked: removing the debit guard, committing instead of rolling back a
  lost same-reference race, dropping the balance increment, counting admin credits
  as earned, losing the debit sign and marking replays as new each fail the suite.

## Known limitations / not built

* No purchases: no shop, casino or transfers between players.
* Bounty amounts themselves are still 32-bit (`bounties.reward_points`); the ledger
  and balance are 64-bit.
* Balances are guild-wide; there is no per-server balance.
* A dropped `ECONOMY` card is not retried; the ledger (`/economy history`) is the
  source of truth.
* Event scoring (`EventRepository.ScoreKill`) uses an `ON CONFLICT` target that does
  not match its partial unique index on real PostgreSQL. This is a pre-existing bug
  unrelated to the economy (it was found while testing prize payouts; prizes are
  tested by seeding standings directly) and is tracked separately.
