# Discord Trader

The Trader is the Champion Shop in Discord. Players browse and buy with Champion Points, and the
people with the Trader role open and close it and hand orders over. It sells the website shop's
catalog through the website shop's own purchase, so prices, stock, purchase limits, the single
Champion Points debit, idempotency and the delivery record are exactly the website's
(`docs/SHOP.md`, `docs/SHOP_DELIVERY.md`).

Code: `internal/discord/trader.go` (commands, buttons, forms), `trader_setup.go` (channels, role,
shared messages, refresher), `trader_views.go` (messages), `internal/app/trader.go` (backend),
`internal/repository/trader_repository.go`, migration `0143_trader_hubs`.

## Setup

`/trader setup [role] [server]` (Administrator or Owner; the plan must include the shop) creates, or
repairs, one Trader per game server:

| Made | Who sees it | What it is |
| --- | --- | --- |
| `🛒 Trader` category | everyone | holds the rest |
| `🟢 Trader: Open` / `🔴 Trader: Closed` voice channel | everyone; nobody can join | the trader's state, by name |
| `#trader` | everyone reads; only Champion posts | the trader post: state, featured items, categories, Browse / My wallet / My orders |
| `#deals` | everyone reads; only Champion posts | deals, sales and trader news (phase 2) |
| `#trader-panel` | the Trader role | the Trader panel: Open / Close trader, Refresh, orders waiting, stock alerts |
| `#trader-orders` | the Trader role (can talk there) | a card for every new order |
| `@Trader` role | (created only when no role is given) | who runs the trader |

Running setup again reuses every channel and the role that still exist, makes the missing ones,
and refreshes the post and panel. It never changes whether the trader is open. Champion needs the
Manage Channels and Manage Roles permissions; without them setup says so and changes nothing more.

## Buying

Everything after Browse is private to the player (`/trader browse` opens the same thing anywhere).

1. Categories, then an item: price, stock as a word (In stock, Low stock, Sold out), the per-player
   limit, delivery, the player's balance. Buy is disabled with the reason when it can't be bought.
2. Confirm: total and balance after. The confirm screen carries one order key, so pressing it twice
   places one order.
3. A drop-off item (`MANUAL_COORDINATE`) asks for X and Z on the server's delivery map. The shop
   checks the position against the map.
4. The receipt; the Trader role gets a card in `#trader-orders`. The buyer's DM on delivery and the
   Received / Issue with order question are the shop's (`docs/SHOP_ORDER_CONFIRMATION.md`).

Rules:

- The player needs a VERIFIED gamertag link (`/link`). A Discord user who never signed in to the
  website gets a Champion account the first time they buy (as the faction recruit buttons do).
- A closed trader refuses orders in the backend, not only by greying Browse. Orders already placed
  are still delivered. The website shop is not affected by the switch.
- Trader orders are not announced in the public economy feed.
- Items delivered automatically at the buyer's logged position stay on the website shop: Discord
  can't check the position at checkout.
- Orders share the website's per-person purchase rate limit.
- The trader is marked delivered or refunded on the website (Shop › Orders) in phase 1.

## Open and closed

The Trader role, Administrators and the Owner press Open trader / Close trader on the panel. Each
change is in the admin audit log (`TRADER_OPEN`, `TRADER_CLOSE`, `TRADER_SETUP`, after-state
`source: discord_trader`). The trader post and panel change at once. Discord lets a bot rename a
channel only twice every 10 minutes, so the voice channel can lag; `voice_name_applied` records
the last name it got, and the refresher (every 2 minutes, one process) retries until it matches.

## Later phases

Phase 2: Mark delivered and Refund on the order cards, deals, sales, announcements, opening hours,
opt-in @Deals pings. Phase 3: daily deal, VIP discounts, bundles, gifts, sales stats, player trade
board. The plan and preview: the Trader Hub artifact.
