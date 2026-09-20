# Champion Economy - web API, accounts and admin controls (Phase 1: web foundation)

The website is the primary economy experience (balances, history, later Shop and Casino); Discord only launches into it and shows optional notifications. **The Go backend
remains the single authority** for balances, transactions, security, tenant isolation, idempotency and audit.

This document covers the **web layer** added on top of the economy that already exists. The ledger itself - tables, atomic credit/debit, idempotency, bounty payouts, the
`ECONOMY` Discord route - is documented in [`docs/ECONOMY_SYSTEM.md`](ECONOMY_SYSTEM.md) and is reused unchanged.

## 1. Audit result: what already existed

| Concern | Existing (audited on `main`) | Decision |
|---|---|---|
| Currency | **Champion Points** (`player_points`, `point_transactions`; `/economy`, "My Balance" panel, bounty rewards, event prizes) | **Reused.** It is the canonical, production currency; it is not renamed to "Champion Credits". |
| Balance | `player_points.balance` (spendable, `CHECK >= 0`) next to earn-only `lifetime_points` / `season_points` | Reused - no second balance |
| Ledger | `point_transactions`: signed `BIGINT amount`, `balance_after`, `created_by`, `description`, `server_id`, `UNIQUE(guild_id, player_id, reason_type, source_key)`, append-only trigger | Reused - no second ledger |
| Atomic write path | `EconomyRepository.applyLedger` (one function for every credit/debit) | Reused - all web writes go through it |
| Bounty rewards | paid **inside** the bounty claim transaction through the ledger (`BOUNTY_CLAIM`, `bounty:<id>`) | **Not changed** (already canonical, already exactly-once) |
| Event prizes | `EVENT_FIRST/SECOND/THIRD_PLACE`, `event:<id>:<place>` through the ledger | Not changed |
| Admin adjustments | `/economy credit|debit` (Discord admins) via `economy.Service.AdminCredit/AdminDebit` | Reused by the web admin endpoints |
| ECONOMY publisher | `discord.EconomyFeed` (routing model, post-commit, no fallback), custom-embed variables `player, amount, balance, transaction_type, server_name` | Reused - **no second publisher**; web adjustments raise the same event |
| Shop / casino / transfers | none (placeholders only in docs) | none built here |

Nothing in the audit required a new currency, balance table or ledger table, so **no `economy_accounts` / `economy_transactions` tables were created.** Creating them would have
produced two balances for the same points.

### The one migration: `0035_economy_ledger_no_delete`

`0030` already blocked *editing* a ledger row. `0035` also blocks *deleting* one directly (`BEFORE DELETE` trigger, raising `point_transactions is append-only`). Deletion caused by a
foreign-key cascade (deleting a guild or a player) is unaffected - it runs inside an internal trigger (`pg_trigger_depth() > 1`), so an operator can still remove a whole guild.
No table, column or index changed. A correction is always a **compensating transaction**, never an edit or a removal.

## 2. Currency

```json
{ "code": "CHAMPION_POINTS", "name": "Champion Points", "symbol": "pts" }
```

Every economy response carries this object (`economy.ChampionPoints`), so the name is never hardcoded in two places. Amounts are whole numbers (no decimals, no floating point).
If the product later renames the currency, it is one constant plus the Discord wording; the stored values do not change.

## 3. Account model (and scope)

An **account is the existing `(guild, player)` pair**: `player_points` (balance) + `point_transactions` (ledger), keyed by the verified DayZ identity (`players.id`).
The API reaches it through an installation:

```
organization -> installation -> Discord guild (balance scope) + DayZ server (write attribution)
```

* **`accountId` = `players.id`** inside the installation's guild. It is never a display name. An id from another guild is `404 ECONOMY_ACCOUNT_NOT_FOUND`.
* **Identity for players** is the acting Champion user's **VERIFIED** `player_links` row for the guild. A pending/rejected link, an unlinked user, or a matching display name gets
  `409 PLAYER_IDENTITY_REQUIRED`; nothing is created.
* **Accounts are created lazily**: the first credit creates the `player_points` row. A player with no activity reads as balance `0`.
* **Balances are guild-wide** (the decision recorded in `ECONOMY_SYSTEM.md`, kept because every existing leaderboard, bounty payout and link is per guild and splitting would have
  reset production balances). Consequences, stated precisely:
  * The same person on **different guilds/organizations/installations of different communities** has fully separate balances (tested).
  * Two installations that share **one Discord guild** (two DayZ servers of one community) show **the same balance**. Each transaction records the installation's `server_id`, so a
    per-server view - or a future per-server currency - can be derived without a schema change. If the product later wants per-server balances inside one guild, that is a deliberate
    migration with a balance split plan, not something this phase does silently.
* **Factions are independent**: joining, leaving, being removed from or founding a faction never touches the balance (tested).

## 4. Ledger model

Immutable and append-only (see `ECONOMY_SYSTEM.md`): `id`, `guild_id`, `player_id`, `server_id`, `reason_type` (the *transaction type*), signed `amount`, `balance_after`, `source_key`
(the *reference / idempotency key*), `description`, `created_by`, `created_at`.

**Transaction types** (stable, persisted as-is; production rows already use them):

| Type | Direction | Written by |
|---|---|---|
| `BOUNTY_CLAIM` | CREDIT | bounty claim (inside its transaction) |
| `EVENT_FIRST_PLACE` / `EVENT_SECOND_PLACE` / `EVENT_THIRD_PLACE` | CREDIT | event finalization |
| `SYSTEM_REWARD` | CREDIT | reserved for automatic rewards (e.g. kill rewards); **no amounts are configured - nothing pays automatically** |
| `ADMIN_CREDIT` | CREDIT | admin grant (web `grant`, Discord `/economy credit`); spendable balance only |
| `ADMIN_DEBIT` | DEBIT | admin debit |

`SHOP_PURCHASE`, `SHOP_REFUND`, `CASINO_BET`, `CASINO_WIN`, `TRANSFER_IN/OUT` and `SYSTEM_ADJUSTMENT` are **reserved names only**: the service does not accept them yet. The historical
names are not renamed to the ones a greenfield design would choose (`ADMIN_GRANT`, `BOUNTY_REWARD`), because they are part of every existing row's uniqueness key.

**Direction** is explicit in the API (`CREDIT` / `DEBIT`) and stored as the sign of `amount` (one convention). The API always reports a **positive magnitude** plus `direction`.

### Guarantees

* `balance == SUM(amount)` and `balance == newest balance_after`, per account, always. Every row satisfies `balance_after = previous balance_after + amount` (credit) or `- amount` (debit).
* The balance **never goes below zero** (`CHECK (balance >= 0)` plus an atomic guard `UPDATE ... WHERE balance >= amount`). A debit that exceeds it is `409 INSUFFICIENT_FUNDS`; nothing is written.
* Ledger row and balance change in **one database transaction**; concurrent operations on an account serialize on its row lock (two simultaneous debits of 80 on a balance of 100: exactly
  one succeeds, final balance 20 - tested through the real HTTP routes).
* **Rows are never edited or deleted** (triggers `0030` + `0035`). Correction = compensating transaction. Deleting a player/guild cascades (there is no per-player "leave" delete).
* Amounts are `1 .. 10^15` at the ledger; the **web admin maximum is `1,000,000,000` per operation** (`MaxAdminAmount`) so every JSON number stays exactly representable (JS safe integer limit 2^53).

### Idempotency

The key is `(guild, player, type, reference)` - unique in the database, so "same key in the same installation" cannot credit or debit twice, even under a race (the loser is rolled back
in a savepoint and returns the winner's entry). System writers use natural references (`bounty:<id>`, `event:<id>:<place>`); a future kill reward would use `kill:<id>`, a purchase
`order:<id>`, a bet `bet:<id>`.

Admin endpoints accept an optional **`idempotencyKey`** (8-64 chars of `A-Za-z0-9._:-`, stored as reference `admin:<key>`):

* first call applies; a retry returns the **original transaction** with `duplicate: true` (HTTP 200, no second row, no notification);
* the same key with a **different amount** is `409 DUPLICATE_TRANSACTION`;
* the key is scoped to the account and the operation type (a grant and a debit may reuse a string);
* without a key every call is a new transaction.

## 5. API

All routes: `/api/saas/organizations/{organizationID}/installations/{installationID}/economy/...`. Standard chain: service auth -> acting user (`X-Champion-Acting-User`) ->
organization + installation scope (`404` for another tenant's ids, `401` without a synced user).

| Route | Who | Returns |
|---|---|---|
| `GET .../economy/me` | any synced user with a **verified** DayZ link | `EconomyBalanceResponse` |
| `GET .../economy/me/transactions?limit=&cursor=&type=` | same | `EconomyTransactionList` (newest first) |
| `GET .../economy/accounts?q=&limit=` | org **OWNER / ADMIN** | `AdminEconomyAccountList` (lookup) |
| `GET .../economy/accounts/{accountID}` | org OWNER / ADMIN | `{ currency, account: AdminEconomyAccount }` |
| `GET .../economy/accounts/{accountID}/transactions?limit=&cursor=&type=` | org OWNER / ADMIN | `AdminEconomyTransactionList` |
| `POST .../economy/accounts/{accountID}/grant` | org OWNER / ADMIN | `EconomyAdjustmentResponse` |
| `POST .../economy/accounts/{accountID}/debit` | org OWNER / ADMIN | `EconomyAdjustmentResponse` |

* **Players see only their own account.** `/me` derives the account from the acting user; there is no parameter that selects another player (an `accountId` query parameter is ignored).
* **Admin rights are organization rights only.** A faction role (even LEADER), an organization MEMBER or a player gets `403 ECONOMY_FORBIDDEN` on every admin route. A platform admin uses the
  separate admin API; the economy admin routes do not consult it.
* **Pagination** is keyset over the ledger id: `limit` default 25, max 100 (larger is clamped; `0`, negatives and non-numbers are `400`), `cursor` = the previous `nextCursor` (opaque),
  `type` = one of `BOUNTY_CLAIM`, `ADMIN_CREDIT`, `ADMIN_DEBIT`, `SYSTEM_REWARD`, `EVENT_PRIZE` (all three event prizes). Reads use `idx_point_transactions_history (guild_id, player_id, id DESC)`
  and never scan the whole ledger. The admin lookup returns at most 25 rows (no cursor) for `q` of 2-50 characters; `%`/`_` in `q` are literal.
* **Errors** use the standard envelope. Economy codes: `PLAYER_IDENTITY_REQUIRED` 409, `ECONOMY_ACCOUNT_NOT_FOUND` 404, `INSUFFICIENT_FUNDS` 409, `INVALID_AMOUNT` 400 (not a whole number 1..1,000,000,000,
  incl. negatives, decimals, exponents, overflow), `DUPLICATE_TRANSACTION` 409 (idempotency key reused with another amount), `ECONOMY_FORBIDDEN` 403; plus `INVALID_REQUEST` 400 (missing/blank reason,
  reason/keys/limit/cursor/type/q problems, unknown JSON keys), `NOT_FOUND` 404, `CONFLICT` 409 (installation suspended or without a DayZ server), `RATE_LIMITED` 429.
* **Rate limits** (per acting user): admin grant/debit 30 per minute; history/transactions 120 per minute. Balance reads (`/me`) and lookups are not limited.

### Admin adjustments

`POST .../accounts/{accountID}/grant|debit` with `{ "amount": 500, "reason": "Event prize correction", "idempotencyKey": "optional-8-64-chars" }`.

* `amount`: JSON whole number `1 .. 1,000,000,000`.
* `reason`: **required**, max 200 characters (control characters are dropped, the rest is truncated). It is stored on the ledger row and shown to admins only; **players never see it** - their
  history text is generated from the type ("Admin credit").
* A grant is `ADMIN_CREDIT`: it adds **spendable balance only** and never raises leaderboard scores (an admin cannot manufacture rank). A debit removes spendable balance only.
* The installation must not be `SUSPENDED` and must have a DayZ server (the write is attributed to it and tenant-checked against the organization).
* **Audit**: every success logs `economy_admin_grant` / `economy_admin_debit` with `organization_id`, `installation_id`, `acting_user_id`, `account_id`, `amount`, `transaction_id`, `duplicate`
  - never the reason, names or credentials. The ledger row itself records who (`created_by` = Discord user id), what and when.

## 6. Reconciliation

`economy.Accounts.Reconcile(ctx, scope, limit)` (`EconomyRepository.Reconcile`) is a **read-only** internal utility (no HTTP route). It reports, per guild:

* `BALANCE` - `player_points.balance` differs from `SUM(amount)` or from the newest `balance_after`;
* `CHAIN` - a row whose `balance_after` is not the previous `balance_after` plus its amount.

It never changes anything and a normal read never "fixes" a drift. A repair is a deliberate operator action (a compensating transaction). Measured: 200,000 ledger rows reconcile in ~130 ms.

## 7. Integration with existing systems

* **Bounties** are unchanged: the claim already pays each bounty through the ledger inside its own transaction with reference `bounty:<id>` (exactly once, replay-safe). The bounty suites
  run unchanged and pass.
* **Kill rewards**: not enabled. `SYSTEM_REWARD` exists for a future configured reward; no amount is hardcoded anywhere.
* **ECONOMY Discord route** (`discord.EconomyFeed`): the web admin endpoints call the same `economy.Service`, which hands committed transactions to the existing feed *after commit*. Discord is an
  **optional notification channel**: no `ECONOMY` route = nothing is sent (never a fallback); a Discord failure or a panicking notifier cannot undo or repeat a transaction; replays, refusals and
  balance reads never notify. Cards never show the admin or the reason. The website remains the primary UI. Custom embed templates keep their variables
  (`player`, `amount`, `balance`, `transaction_type`, `server_name`); nothing was added.
* **Transfers, daily rewards**: intentionally not built (fraud/abuse surface without product rules).

## 8. Shop and Casino readiness (not built)

The write primitive a purchase needs already exists and is what admin debits use: `EconomyRepository.DebitTx(ctx, tx, params)` debits **inside the caller's transaction** with a reference. A Shop
order can therefore create its order row and debit the account atomically (`type SHOP_PURCHASE`, reference `order:<id>`, exactly once by the unique key; a refund is a credit with
`refund:<order id>`). A Casino bet is a debit with `bet:<id>` and a win a credit with `win:<id>` - one idempotent reference each. Both only need their type added to the service's allowed set.

## 9. Security review

* **IDOR / cross-tenant**: every read/write resolves organization + installation first; the guild comes from the installation, and an account id is looked up **inside that guild**. Org A's admin cannot
  reach org B's installation (403/404), nor use org B's account id on org A's installation (404) - all tested, with mutation checks (removing the role gate or the account scope fails the suite).
* **Acting-user spoofing**: the acting user is accepted only behind service auth; `/me` has no player parameter; admin routes re-check the organization role on every call.
* **Amounts**: whole numbers only, `1..1e9`; negatives, decimals, exponents, strings and int64 overflow are rejected before any database access; the ledger re-validates and PostgreSQL raises on overflow.
* **Duplicates / races**: unique ledger key + row-lock serialization + savepoint (tested with 20 identical concurrent requests: one application).
* **Privilege escalation**: faction/organization-member roles grant nothing; admin credits never touch leaderboard scores.
* **Injection**: the lookup uses parameterized `ILIKE` with escaped wildcards; the type filter is a closed list; descriptions shown to players are generated, never free text; the reason is stored as text and only
  shown to admins.

## 10. Website handoff: exact DTOs

```ts
interface EconomyCurrency { code: "CHAMPION_POINTS"; name: "Champion Points"; symbol: string }   // symbol "pts"

interface EconomyAccount {
  accountId: number;              // the DayZ player id in this installation's guild; use it in admin URLs
  gamertag: string;               // the verified DayZ identity, never a free-typed name
  balance: number;                // whole Champion Points, >= 0
  updatedAt: string | null;       // RFC 3339, last balance change; null = no economy activity yet
}

interface EconomyBalanceResponse {            // GET .../economy/me
  currency: EconomyCurrency;
  account: EconomyAccount;
  installationId: number;
  gameServerId: number | null;
}

interface EconomyTransaction {                // player view
  id: number;                     // ledger id, strictly decreasing down the list
  type: "BOUNTY_CLAIM" | "ADMIN_CREDIT" | "ADMIN_DEBIT" | "SYSTEM_REWARD"
      | "EVENT_FIRST_PLACE" | "EVENT_SECOND_PLACE" | "EVENT_THIRD_PLACE";   // open set: ignore unknown values
  direction: "CREDIT" | "DEBIT";
  amount: number;                 // positive magnitude
  balanceAfter: number;
  description: string;            // generated: "Bounty reward" | "Admin credit" | "Admin debit" | "Reward" | "Event prize" | "Adjustment"
  referenceType: "BOUNTY" | "EVENT" | null;
  createdAt: string;              // RFC 3339
}

interface EconomyTransactionList {            // GET .../economy/me/transactions
  currency: EconomyCurrency;
  items: EconomyTransaction[];    // newest first
  nextCursor: string | null;      // pass back as ?cursor= (same type filter); null = last page
  limit: number;                  // effective page size (default 25, max 100)
}

interface AdminEconomyAccount extends EconomyAccount {   // GET .../economy/accounts, .../accounts/{id}
  linked: boolean;                // has a VERIFIED Champion identity
  discordUserId: string | null;
  displayName: string | null;     // the linked Champion user's display name
  avatar: string | null;
}
interface AdminEconomyAccountList { currency: EconomyCurrency; items: AdminEconomyAccount[]; limit: number }   // <= 25, best match first

interface AdminEconomyTransaction extends EconomyTransaction {   // GET .../accounts/{id}/transactions
  reason: string | null;                  // the admin's reason (admins only)
  actorDiscordUserId: string | null;      // null for system rows
  isSystem: boolean;
  referenceId: string | null;             // e.g. "bounty:42", "admin:<idempotencyKey>"
  gameServerId: number | null;
}
interface AdminEconomyTransactionList { currency: EconomyCurrency; items: AdminEconomyTransaction[]; nextCursor: string | null; limit: number }

// POST .../accounts/{accountID}/grant | debit
interface AdjustEconomyRequest { amount: number; reason: string; idempotencyKey?: string }
interface EconomyAdjustmentResponse {
  currency: EconomyCurrency;
  account: AdminEconomyAccount;           // fresh, with the new balance
  transaction: AdminEconomyTransaction;   // the created (or, for a replay, the original) transaction
  duplicate: boolean;                     // true = an idempotent replay: nothing changed
}
```

UI notes: show the `PLAYER_IDENTITY_REQUIRED` state as "Link your DayZ account" (it is a normal state, not a failure); format amounts with the currency symbol; send an `idempotencyKey`
(a fresh UUID per form submission) on admin actions so a double click cannot apply twice; treat `type` as an open set.

## 11. Tests

* `internal/economy/accounts_test.go` (unit): currency, scoping, identity, search validation, cursors, filters, limits and privacy, adjustment validation order, idempotency, notification rules.
* `internal/app/saas_api_economy_integration_test.go` (real routes + PostgreSQL): balances and history (60 transactions, pagination, filters, privacy), identity required, authentication and admin
  authorization (faction leader / member / other organization), admin lookup, adjustment validation and ledger integrity, idempotency (incl. 20 concurrent identical requests), concurrent debits
  (two of 80 on 100), tenant and server isolation (two organizations, two installations of one guild), faction independence, ECONOMY notifier behaviour, reconciliation, the delete guard and
  cascades, rate limits, and a 200,000-row volume test.
* Existing suites for the ledger, bounty claims, the ECONOMY feed and custom embeds run unchanged (`internal/repository`, `internal/bounties`, `internal/discord`, `internal/app`).

## 12. Limits / not built

* No Shop, Casino, player transfers, daily rewards or automatic kill rewards; no website in this repository.
* Balances are guild-wide (see 3); a per-server currency is a future, deliberate migration.
* No repair tool for a reconciliation finding (detect only).
* A dropped `ECONOMY` Discord card is not retried; the ledger is the source of truth.
