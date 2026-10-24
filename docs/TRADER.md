# Discord Trader

The Trader is the Champion Shop in Discord. Players browse and buy with Champion Points; the Trader role
and the server's Owner and Administrators open and close it and hand orders over. It sells the website shop's
catalog through the website shop's own purchase, so prices, stock, purchase limits, the single
Champion Points debit, idempotency and the delivery record are exactly the website's
(`docs/SHOP.md`, `docs/SHOP_DELIVERY.md`).

Code: `internal/discord/trader.go` (commands, buttons, forms), `trader_setup.go` (channels, roles,
shared messages, refresher), `trader_views.go` (messages), `trader_offers.go` and
`trader_offer_views.go` (deals, sales, announcements, hours, order cards, deal pings),
`trader_items.go` (items), `internal/app/trader.go`, `trader_offers.go` and `trader_items.go` (backend), `internal/repository/trader_repository.go`
and `shop_discount_repository.go`, migrations `0143_trader_hubs` and `0144_trader_deals`.

## Setup

`/trader setup [role] [server]` (Administrator or Owner; the plan must include the shop) creates, or
repairs, one Trader per game server:

| Made | Who sees it | What it is |
| --- | --- | --- |
| `🛒 Trader` category | everyone | holds the rest |
| `🟢 Trader: Open` / `🔴 Trader: Closed` voice channel | everyone; nobody can join | the trader's state, by name |
| `#trader` | everyone reads; only Champion posts | the trader post: state, featured items, categories, Browse / My wallet / My orders |
| `#deals` | everyone reads; only Champion posts | deals, sales and trader news |
| `#trader-panel` | the Trader role, the Owner and Administrators | the Trader panel (below) |
| `#trader-orders` | the Trader role, the Owner and Administrators (can talk there) | a card for every new order |
| `@Trader` role | (created only when no role is given and none exists) | the people who run the trader |
| `@Deals` role | players give it to themselves with **Deal pings** | pinged by every new deal and sale |

"Administrators" are the Discord roles mapped to Administrator or Owner on the Champion website
(Server Hub › Staff), plus Discord administrators, who see every channel. Setup gives the Trader
role, those roles and the organization owner access to the two private channels; after mapping a
new staff role, run `/trader setup` again. Choosing a different `role` moves the private channels
to it (the old Trader role is taken off them unless it is also an Administrator role).

Running setup again reuses every channel and role that still exists, makes the missing ones,
re-grants access, and refreshes the post and panel. It never changes whether the trader is open. Champion needs the
Manage Channels and Manage Roles permissions; without them setup says so and changes nothing more.

## Buying

Everything after Browse is private to the player (`/trader browse` opens the same thing anywhere).

1. Categories, then an item: price, stock as a word (In stock, Low stock, Sold out), the per-player
   limit, delivery, the player's balance. Buy is disabled with the reason when it can't be bought.
2. Confirm: total and balance after. The confirm screen carries one order key, so pressing it twice
   places one order.
3. A drop-off item (`MANUAL_COORDINATE`) asks for X and Z on the server's delivery map. The shop
   checks the position against the map.
4. The receipt (with what a deal saved); the Trader role and staff get a card in `#trader-orders`
   with **Mark delivered** and **Refund**. The buyer's DM on delivery and the Received / Issue with
   order question are the shop's (`docs/SHOP_ORDER_CONFIRMATION.md`).

Rules:

- The player needs a VERIFIED gamertag link (`/link`). A Discord user who never signed in to the
  website gets a Champion account the first time they buy (as the faction recruit buttons do).
- A closed trader refuses orders in the backend, not only by greying Browse. Orders already placed
  are still delivered. The website shop is not affected by the switch.
- Trader orders are not announced in the public economy feed.
- Items delivered automatically at the buyer's logged position stay on the website shop: Discord
  can't check the position at checkout.
- Orders share the website's per-person purchase rate limit.

## Order cards

**Mark delivered** is the shop's own fulfilment and **Refund** its refund (a reason is required,
shown to staff only), exactly as on the website (Shop › Orders), which still works too. Both need
the Trader role, Administrator or Owner, work once (a second press says it was already handled),
and turn the card into its handled state with who did it. A refund of a Discord order isn't
announced in the public economy feed either. The person pressing gets a Champion account on first
use, as a buyer does, because the shop records who delivered or refunded by account. Audited as
`TRADER_ORDER_DELIVERED` and `TRADER_ORDER_REFUNDED`.

## Deals and sales

A **deal** takes a percentage off one item; a **sale** takes it off one category, or off every
item. Both run from now for a number of hours (1 to 720), at most 75% off, and are the shop's
price everywhere: the trader, the website shop (its product list gains `sale`) and checkout. When
several cover an item, the biggest percentage counts. The price is decided in the purchase
transaction under the product lock, so a deal that ends while a player is on the confirm screen
is not charged. Points taken off round down and an item always costs at least 1 point. The order
records the deal or sale (`shop_purchase_items.discount_id`, `discount_percent`,
`list_unit_price_points`).

From the panel: **Post deal** (pick a category, then the item, then the percentage, hours and an
optional message), **Start sale** (everything or a category), **End deals** (end one or all now).
Each is announced in `#deals` with a button to the item or the sale, and pings `@Deals`, the only
role the post may mention. When it ends, early or on time, the refresher marks the post ended.
Prices show struck through in the trader post, the item lists and the item card. Audited as
`TRADER_DEAL_START`, `TRADER_SALE_START` and `TRADER_OFFER_END`.

**Deal pings** (on the trader post and every deal post) gives or takes `@Deals`. Champion needs
Manage Roles and a role above `@Deals`, which setup creates.

## Items

**Items** on the panel is the shop's catalog for the Trader role, Administrators and the Owner:
the same items, categories and validation as the website (Shop › Products), so a change shows on
both at once and on the trader post within 2 minutes (straight away for the panel and post when
made from Discord).

- The list: pick a category, see every item including ones turned off (on/off, price, stock), pick
  one to change, or **Add item** (name, price, stock with empty meaning unlimited, category,
  description). A category is matched by name whatever its capitals, and made when there is none;
  one turned off on the website is refused, not turned back on.
- An item: **Edit** (name, price, category, limit per player, description), **Stock** (a count, or
  empty for unlimited), **Turn off / Turn on**, **Feature** (on the trader post), **Drop-off**
  (buyers give map coordinates; the card warns while the delivery map isn't set).
- Adding sets the product type to ITEM and manual delivery; the website edits everything else
  (product type, sort order, automatic delivery). Audited as `TRADER_ITEM_ADD` and
  `TRADER_ITEM_EDIT`.

## Announcements

**Announce** posts a title and message in `#deals` (no ping). Audited as `TRADER_ANNOUNCE`.

## Open and closed

The Trader role, the Owner and Administrators press Open trader / Close trader on the panel (a
Moderator without the Trader role can't). Setting up the trader stays Administrator or Owner.
Close trader asks for an optional message to players, shown on the trader post and to anyone who
tries to shop while it is closed; opening clears it.

**Opening hours** open the trader at one time and close it at another every day, in a time zone
(an IANA name such as `Europe/London`, or `UTC`); closing after midnight works. Saving them opens
or closes the trader to match at once. The refresher applies each opening and closing time once
(`hours_applied_at`), so opening or closing by hand lasts until the next one. Leaving both times
empty turns them off. Audited as `TRADER_HOURS`; a change the hours make has no person. Each
change is in the admin audit log (`TRADER_OPEN`, `TRADER_CLOSE`, `TRADER_SETUP`, after-state
`source: discord_trader`). The trader post and panel change at once. Discord lets a bot rename a
channel only twice every 10 minutes, so the voice channel can lag; `voice_name_applied` records
the last name it got, and the refresher (every 2 minutes, one process) retries until it matches.

## Panel

| Button | What it does |
| --- | --- |
| Open trader / Close trader | as above; closing asks for the optional message |
| Post deal, Start sale | private screens, then a form |
| Announce | a form; posts in `#deals` |
| Opening hours | a form with the current hours |
| End deals | the running deals and sales, to end one or all |
| Items | add and change the shop's items (above) |
| Refresh | redraws the panel |

The panel also shows orders waiting, the status channel, the last change, the closing message,
the opening hours, what's on sale and stock alerts.

## Later phases

Phase 3: daily deal, VIP discounts, bundles, gifts, sales stats, player trade board. The plan and
preview: the Trader Hub artifact.
