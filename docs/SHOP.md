# Champion Shop - product catalog, purchases and refunds (Phase 1)

An installation-scoped product catalog that players buy with **Champion Points**, plus admin management, manual fulfillment and refunds. The website is the Shop UI; the Go backend is
authoritative for prices, stock, purchase limits, the Points debit, idempotency and audit. **No Discord Shop UI, no transfers, no cart, no automatic delivery** exist in this phase.

## 1. Audit result

| Concern | Found on `main` | Decision |
|---|---|---|
| Shop / product / order / purchase / redeem code | **None.** Only: a `SHOP` channel-route key marked "not implemented" (`saas_api_channel_routes.go`), a `SHOP` custom-embed variable set with no publisher (`embedtemplates`), reserved `SHOP_*` names in the economy docs | New tables `shop_categories`, `shop_products`, `shop_purchases`, `shop_purchase_items` (migration `0036`) |
| Currency | Champion Points (`docs/ECONOMY.md`) | Reused. Prices are whole-point integers; no Credits/Coins/Tokens |
| Debit mechanism | `EconomyRepository` ledger path (`applyLedger`), used by admin debits and bounty payouts | **Reused, not duplicated**: a purchase writes its debit through `applyLedger` inside the purchase transaction |
| Buyer identity | economy `Accounts.Me` (VERIFIED `player_links`) | Reused exactly |
| Discord publisher | `EconomyFeed` (ECONOMY route) | Reused: purchases/refunds are announced through the existing notifier. **No `SHOP` publisher was built**; SHOP custom embeds are *not* supported |
| Images | no asset contract for products | `image_key` column reserved but not settable; an unknown JSON key such as `imageUrl` is `400`. No external image URLs |

## 2. Scope and tenancy

Catalog and purchases belong to **organization + installation** (composite foreign keys `(installation_id, organization_id)`); a purchase records the installation's `game_server_id`.
Every query repeats the organization and installation filter, so:

* products and categories of one installation never appear on another (also not on another installation of the same Discord guild);
* another tenant's product/purchase/category id is a safe `404` (`SHOP_PRODUCT_NOT_FOUND`, `PURCHASE_NOT_FOUND`, `SHOP_CATEGORY_NOT_FOUND`);
* a mismatched organization/installation pair is `404`; an organization admin of another organization gets `403 SHOP_FORBIDDEN`.

**Shared-guild balance caveat (existing economy behaviour, unchanged).** Champion Points balances are guild-wide (`docs/ECONOMY.md` section 3). Two installations that share one Discord guild
therefore debit the **same** underlying player balance: buying on the second server lowers the balance shown on the first. Catalogs and purchase history stay per installation; only the balance
is shared. Different guilds/organizations have completely separate balances. Faction membership is irrelevant: joining, leaving or changing factions never changes purchases or balances.

## 3. Data model (migration `0036_shop_foundation`, additive)

| Table | Notes |
|---|---|
| `shop_categories` | `name`, `slug` (`UNIQUE(installation_id, slug)`), `description`, `sort_order`, `is_active`. Categories are only ever disabled, never deleted |
| `shop_products` | `name`, `slug` (unique per installation, generated once from the name: `m4-a1-rifle`, `m4-a1-rifle-2`, stable after renames), `description`, `price_points` (`CHECK 1..1,000,000,000`), `product_type` (`ITEM`/`LOADOUT`/`VEHICLE`/`SERVICE`/`CUSTOM`), `delivery_type`, `image_key` (reserved), `sort_order`, `is_active`, `is_featured`, `stock_mode` (`UNLIMITED`/`FINITE`), `stock_quantity` (`CHECK`: NULL for UNLIMITED, `0..1e9` for FINITE, so it can never be negative), `purchase_limit` (`1..1000` or NULL). Composite FK `(category_id, installation_id)` so a product can only use its own installation's category |
| `shop_purchases` | `user_id` (website user), `player_id` (verified DayZ player), `status` (`CHECK`), `total_points`, `delivery_type` (snapshot), `idempotency_key`, `paid_at`, `fulfilled_at/by`, `refunded_at/by`, `refund_reason` (admin-only), `game_server_id`. **`UNIQUE (installation_id, user_id, idempotency_key)`** |
| `shop_purchase_items` | immutable snapshot: `product_id` (nullable, `ON DELETE SET NULL`), `product_name`, `unit_price_points`, `quantity`, `line_total_points` (`CHECK = unit * quantity`) |

Indexes: catalog `(installation_id, is_active, is_featured DESC, sort_order, id)` and `(installation_id, category_id)`; purchases `(installation_id, id DESC)`, `(installation_id, player_id, id DESC)`,
`(installation_id, status, id DESC)`; items by purchase and by product. Points are **not** stored in the shop: the ledger (`point_transactions`) holds the debit and the refund.

## 4. Products

* **Types** `ITEM`, `LOADOUT`, `VEHICLE`, `SERVICE`, `CUSTOM` are labels of what an admin will hand over.
* **Delivery is separate from payment.** Only **`MANUAL`** exists: a purchase is paid immediately and waits for an admin to fulfill it. `DISCORD_ROLE` and `IN_GAME_FUTURE` are reserved names that the API
  rejects (`400`, "reserved and not implemented yet") - nothing here grants roles or spawns items, and it does not pretend to.
* **Delivery policy and delivery records (Delivery Engine 2.0, `docs/SHOP_DELIVERY.md`).** Each product is `MANUAL_PICKUP` (default) or `MANUAL_COORDINATE` (the buyer sends X/Z map
  coordinates); every purchase gets one persistent delivery record in the purchase transaction, and fulfill/refund move it atomically.
* **Stock**: `UNLIMITED`, or `FINITE` with a required `stockQuantity`. Players see only `stockState` (`UNLIMITED`, `IN_STOCK`, `LOW_STOCK` for 1-5, `OUT_OF_STOCK`); the exact quantity is admin-only.
* **Purchase limit** (optional, per player and product): counts **units** in purchases with status `PENDING_FULFILLMENT`, `PAID` or `FULFILLED`. A **refunded, cancelled or failed purchase does not count**,
  so a refund gives the limit back.
* **Disable, never delete.** `isActive=false` hides a product from players and blocks purchases (`SHOP_PRODUCT_DISABLED`); there is no delete endpoint, so history always stays valid. A disabled *category* hides its
  products from the player catalog and blocks buying them. Purchase items snapshot name and price, so editing/disabling a product never rewrites history.
* Validation: name 1-80, description <= 1000 (plain text, control characters stripped), price whole 1..1e9, sort order +-10000, quantity limits above. The tenant, slug and product type are not editable; unknown JSON keys are `400`.

## 5. The purchase transaction

`POST .../shop/purchases` `{ productId, quantity, idempotencyKey }` (quantity 1..100; key 8-64 chars of `A-Za-z0-9._:-`, required). One database transaction:

1. **idempotency pre-check** - a committed purchase with this `(installation, user, key)` is returned as-is (even if the product has since sold out or been disabled);
2. lock the **product row** (`FOR UPDATE`): every buyer of a product serializes here, so stock, price and limit are decided against locked, committed state;
3. re-check the key under the lock (a request that waited finds the winner's purchase);
4. validate: active (and category active), stock, purchase limit, `unit price x quantity` in overflow-safe integer math (`<= 10^15`);
5. insert the purchase (status `PENDING_FULFILLMENT`, `paid_at`) - the unique key is the backstop against a concurrent duplicate;
6. **debit Champion Points through the economy ledger path** (`applyLedger`): type `SHOP_PURCHASE`, reference `purchase:<id>`, atomic `balance >= amount` guard, `created_by` = the buyer;
7. decrement stock, insert the item snapshot, commit.

Any failure - including `INSUFFICIENT_FUNDS` - rolls **everything** back: no purchase row, no ledger row, no stock change. The unique `(guild, player, SHOP_PURCHASE, purchase:<id>)` ledger key also makes a double
debit impossible at the database level. Lock order is always product/purchase row before the balance row (purchase, refund, fulfill, admin debit), so the paths cannot deadlock.

**Idempotency**: same key + same product/quantity -> `200` with the original purchase and `duplicate: true` (nothing charged, nothing announced). Same key + different data -> `409 DUPLICATE_PURCHASE`.
Keys are per user. Only a *committed* purchase consumes a key: a purchase refused for insufficient funds can be retried with the same key after topping up.

**Response** (`201` new, `200` replay): `{ currency, purchase, remainingBalance, duplicate }` - `remainingBalance` is the authoritative balance right after the debit (for a replay, the current balance),
so the website can update the wallet without another call.

## 6. Statuses, fulfillment and refunds

```
PENDING_FULFILLMENT --fulfill--> FULFILLED
PENDING_FULFILLMENT --refund---> REFUNDED
FULFILLED           --refund---> REFUNDED
(reserved for later phases: PENDING, PAID, CANCELLED, FAILED)
```

Anything else is `409 INVALID_PURCHASE_STATUS`; the transition table is `shop.CanTransition` (unit-tested) and the SQL conditions enforce it.

* **Fulfill** `POST .../purchases/{id}/fulfill` (OWNER/ADMIN): `PENDING_FULFILLMENT -> FULFILLED`, records `fulfilled_at` and the admin. Concurrent fulfills: exactly one succeeds.
* **Refund** `POST .../purchases/{id}/refund` `{ "reason": "..." }` (OWNER/ADMIN, reason required, max 200): under the purchase row lock it (a) returns finite stock **only if the purchase was not yet fulfilled**
  (a handed-over product is not restocked), (b) writes a **compensating `SHOP_REFUND` credit** (`+total`, reference `purchase:<id>`, not an earned score) - the original debit is never edited or deleted - and (c) sets `REFUNDED`.
  A purchase is refunded **once**: a second or concurrent refund is `409 INVALID_PURCHASE_STATUS`, and the ledger's unique `(SHOP_REFUND, purchase:<id>)` row is a second guard. The reason is stored for admins and **never shown to the player**.

## 7. API

Base: `/api/saas/organizations/{organizationID}/installations/{installationID}/shop`. Standard chain (service auth, acting user, tenant scope). **Player** routes need a synced user (the catalog needs no DayZ link;
purchases and history need a VERIFIED link -> `409 PLAYER_IDENTITY_REQUIRED`). **Admin** routes need an organization **OWNER/ADMIN** (`403 SHOP_FORBIDDEN` otherwise - faction roles grant nothing).

| Route | Who | Purpose |
|---|---|---|
| `GET /categories` | player | active categories with active product counts |
| `GET /products?category=&q=&limit=&cursor=` | player | active catalog; `category` = slug; `q` = name/description, case-insensitive, `%`/`_` literal (<= 50 chars) |
| `GET /products/{productID}` | player | one active product (disabled = `404 SHOP_PRODUCT_NOT_FOUND`) |
| `POST /purchases` | player | buy (section 5) |
| `GET /me/purchases?status=&productId=&limit=&cursor=` | player | own purchases, newest first |
| `GET /me/purchases/{purchaseID}` | player | own purchase; anyone else's is `404 PURCHASE_NOT_FOUND` |
| `GET /admin/categories`, `POST /categories`, `PUT /categories/{categoryID}` | admin | manage categories (create / edit / `isActive`) |
| `GET /admin/products`, `GET /admin/products/{productID}` | admin | all products incl. inactive, stock quantity |
| `POST /products`, `PUT /products/{productID}` | admin | create / partial update (`categoryId` and `purchaseLimit` accept `null` to clear) |
| `GET /admin/purchases?status=&playerId=&productId=&limit=&cursor=` | admin | every purchase of the installation; `playerId` = the economy `accountId` |
| `GET /admin/purchases/{purchaseID}` | admin | one purchase with player, reason and actors |
| `POST /purchases/{purchaseID}/fulfill`, `POST /purchases/{purchaseID}/refund` | admin | section 6 |

* **Catalog order** (deterministic, no personalization): featured first, `sortOrder`, name (case-insensitive), id. Category name/slug come from the same joined query (no N+1); purchase history loads all items with one extra query.
* **Pagination**: keyset `limit` default 25 / max 100 (larger clamped; `0`, negatives, non-numbers `400`), opaque `cursor` = the previous `nextCursor`. A malformed query string (e.g. `;`) is `400`.
* **Errors** (standard envelope): `SHOP_PRODUCT_NOT_FOUND` 404, `SHOP_PRODUCT_DISABLED` 409, `SHOP_CATEGORY_NOT_FOUND` 404, `OUT_OF_STOCK` 409, `PURCHASE_LIMIT_REACHED` 409, `INVALID_QUANTITY` 400, `INSUFFICIENT_FUNDS` 409,
  `DUPLICATE_PURCHASE` 409, `PURCHASE_NOT_FOUND` 404, `INVALID_PURCHASE_STATUS` 409, `SHOP_FORBIDDEN` 403, `PLAYER_IDENTITY_REQUIRED` 409, `INVALID_REQUEST` 400, `RATE_LIMITED` 429; installation problems: `CONFLICT` 409 (suspended, or no DayZ server).
* **Rate limits** (per acting user): purchase 10/min; every admin mutation (create/update product or category, fulfill, refund) 30/min. Catalog and history reads are not limited.
* **Audit** (ids only, never names, descriptions or reasons): `shop_product_created`, `shop_product_updated`, `shop_product_disabled` (an update that turns `isActive` off), `shop_purchase_fulfilled`,
  `shop_purchase_refunded`, plus `shop_purchase_created`, `shop_category_created/updated`.

## 8. Economy integration

* Ledger types **`SHOP_PURCHASE`** (debit) and **`SHOP_REFUND`** (credit) appear in the player's economy history (`GET .../economy/me/transactions`, filterable with `type=SHOP_PURCHASE|SHOP_REFUND`, `referenceType: "SHOP"`,
  generated descriptions "Shop purchase" / "Shop refund"). The economy `Credit`/`Debit` service methods still refuse these types: only the shop's own transaction writes them.
* **ECONOMY Discord route** (optional notification, unchanged publisher): after the commit, `SHOP_PURCHASE` / `SHOP_REFUND` events are announced (`🛒 SHOP PURCHASE: PlayerA bought Care Package for 750 pts`, `↩️ SHOP REFUND`), showing only the
  public product name - never the admin or the reason. No `ECONOMY` route = nothing sent. Replays, refusals and reads never notify. Custom embed templates keep the same ECONOMY variables (`transaction_type` = "Shop purchase"/"Shop refund");
  **SHOP custom embeds are not implemented.**
* **Reconciliation** (read-only, `shop.Service.Reconcile` / `ShopRepository.ReconcileShop`): every paid/delivered/refunded purchase has exactly its `SHOP_PURCHASE` debit of `-total`; every `REFUNDED` purchase has its `SHOP_REFUND`
  credit of `+total`; no refund credit exists for a non-refunded purchase; no `SHOP_PURCHASE` ledger row is orphaned; item snapshots sum to the total. It reports `MISSING_DEBIT`, `MISSING_REFUND`, `UNEXPECTED_REFUND`, `ORPHAN_DEBIT`, `ITEM_TOTAL`
  and never repairs anything (and a read never mutates).

## 9. Performance

PostgreSQL 16, one installation with **3,000 products in 20 categories and 60,000 purchases** (200 players): first catalog page 3 ms, page of 100 4 ms, cursor page 14 ms, category filter 11 ms, name/description search 4 ms, categories with counts 2 ms,
own purchase history 2 ms (deep page 3 ms), admin filtered purchase list 3 ms (HTTP round trip included). No new index beyond section 3 was needed. `TestShopCatalogAndHistoryScaleWithVolume` guards it (`PERF_PRODUCTS`, `PERF_PURCHASES`).

## 10. Website handoff: exact DTOs

```ts
interface ShopCategory { id: number; name: string; slug: string; description: string; sortOrder: number; productCount: number }   // GET /categories -> { currency, items }

interface ShopProduct {                      // GET /products -> items[], GET /products/{id} -> { currency, product }
  id: number; name: string; slug: string; description: string;
  pricePoints: number;                       // whole Champion Points
  category: { id: number; name: string; slug: string } | null;
  productType: "ITEM" | "LOADOUT" | "VEHICLE" | "SERVICE" | "CUSTOM";
  deliveryType: "MANUAL";                    // open set for later phases
  isFeatured: boolean;
  stockState: "UNLIMITED" | "IN_STOCK" | "LOW_STOCK" | "OUT_OF_STOCK";
  purchaseLimit: number | null;              // per player (units)
}
interface ShopProductList { currency: EconomyCurrency; items: ShopProduct[]; nextCursor: string | null; limit: number }

interface AdminShopProduct extends ShopProduct {   // admin routes
  isActive: boolean; stockMode: "UNLIMITED" | "FINITE"; stockQuantity: number | null; sortOrder: number; createdAt: string; updatedAt: string;
}
// POST /products body (deliveryType default MANUAL, stockMode default UNLIMITED, isActive default true):
interface CreateShopProductRequest { name: string; description?: string; categoryId?: number | null; pricePoints: number; productType: string;
  deliveryType?: "MANUAL"; isActive?: boolean; isFeatured?: boolean; sortOrder?: number; stockMode?: "UNLIMITED" | "FINITE"; stockQuantity?: number | null; purchaseLimit?: number | null }
// PUT /products/{id} body: any subset of the above except productType; categoryId / purchaseLimit accept null to clear. Response: { product: AdminShopProduct }.
interface AdminShopCategory extends ShopCategory { isActive: boolean; createdAt: string; updatedAt: string }   // POST/PUT body: { name?, description?, sortOrder?, isActive? }

interface ShopPurchaseRequest { productId: number; quantity: number /* 1..100 */; idempotencyKey: string /* 8-64 chars; reuse it to retry safely */ }

interface ShopPurchaseItem { productId: number | null; productName: string; unitPricePoints: number; quantity: number; lineTotalPoints: number }   // snapshot at purchase time
interface ShopPurchase {
  id: number;
  status: "PENDING_FULFILLMENT" | "FULFILLED" | "REFUNDED" | "PENDING" | "PAID" | "CANCELLED" | "FAILED";   // the last four are reserved
  totalPoints: number; deliveryType: string; items: ShopPurchaseItem[];
  createdAt: string; paidAt: string | null; fulfilledAt: string | null; refundedAt: string | null;
}
interface ShopPurchaseResponse { currency: EconomyCurrency; purchase: ShopPurchase; remainingBalance: number; duplicate: boolean }   // 201 new, 200 replay
interface ShopPurchaseList { currency: EconomyCurrency; items: ShopPurchase[]; nextCursor: string | null; limit: number }               // GET /me/purchases

interface AdminShopPurchase extends ShopPurchase {   // GET /admin/purchases (list of AdminShopPurchase), detail, fulfill; refund -> { currency, purchase, remainingBalance, duplicate:false }
  player: { accountId: number; gamertag: string };
  gameServerId: number | null;
  refundReason: string | null;               // admin only
  fulfilledByDiscordUserId: string | null; refundedByDiscordUserId: string | null;
}
// POST /purchases/{id}/refund body: { reason: string }   POST /purchases/{id}/fulfill: no body
```

UI notes: show `stockState` (never an exact count) and disable Buy for `OUT_OF_STOCK`; generate a fresh `idempotencyKey` per confirm click and reuse it on a network retry; update the wallet from `remainingBalance`;
`PLAYER_IDENTITY_REQUIRED` means "link your DayZ account"; treat `type`/`status`/`deliveryType` as open sets; show a purchase as "Waiting for delivery" while `PENDING_FULFILLMENT`.

## 11. Tests

* `internal/shop/shop_test.go` (unit): transition table, product validation, absent/null/value JSON, stock states, purchase validation order and identity, notifier rules, refund reason, catalog/purchase list clamping and cursors.
* `internal/app/saas_api_shop_integration_test.go` (real routes + PostgreSQL): catalog and admin management (slugs, order, filters, search, disabled products/categories, validation, no deletion, authorization), the atomic purchase and its ledger row,
  insufficient funds leaving nothing behind, validation and identity, idempotency (incl. 20 concurrent identical requests), concurrent purchases (balance 1000 vs price 750; the last item; 3 units for 10 buyers; purchases racing admin debits), purchase limits
  (incl. concurrency and refund), history/detail/admin list, fulfillment (incl. concurrent), refunds (compensating credit, restock rules, once, concurrent), tenant isolation and the shared-guild balance, faction independence, rate limits, audit lines, reconciliation, and the volume test.
* Economy suites (ledger, bounties, ECONOMY feed and custom embeds, economy API) run unchanged.

## 12. Not built

Cart / multi-item orders, unpaid checkout sessions (payment is immediate), automatic delivery (role or in-game - Delivery Engine 2.0 is staff delivery only, see `docs/SHOP_DELIVERY.md`),
Discord Shop UI or SHOP embeds, product images, coupons/discounts, player-to-player transfers.
