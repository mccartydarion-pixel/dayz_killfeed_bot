# Champion Shop - Delivery Engine 2.0 (Phase 2A)

Every Champion Shop purchase now has a **persistent delivery record**. A product's **delivery policy** decides whether the player only waits for a staff handover
(`MANUAL_PICKUP`) or also gives **X/Z map coordinates** that staff use to deliver (`MANUAL_COORDINATE`). Delivery is still performed **by staff**: this phase adds no automatic
spawning, no Nitrado file write, no server restart. It builds on `docs/SHOP.md` (purchase transaction, ledger, idempotency, fulfillment, refunds), which is unchanged except
where this document says otherwise.

## 1. Implemented vs. future

| Implemented (Phase 2A) | Future proposal (Phase 2B, **not built**) |
|---|---|
| `MANUAL_PICKUP` and `MANUAL_COORDINATE` policies; both have delivery type `MANUAL` | `IN_GAME_FUTURE` delivery type (still reserved and rejected) |
| One `shop_deliveries` row per purchase, written in the purchase transaction | An automatic adapter that stages items on the server |
| States `MANUAL_READY`, `FULFILLED`, `CANCELLED` | States `QUEUED`, `PREPARING`, `FILE_STAGED`, `AWAITING_RESTART`, `RESTART_OBSERVED`, `VERIFICATION_REQUIRED`, `FAILED_REVIEW` (names reserved in code, **refused by the database**) |
| Staff fulfill through the existing purchase fulfill route; a refund cancels an undelivered delivery | Automatic verification that an item actually reached the player |
| Per-installation delivery map (admin-configured), verified map bounds | Reading the map from the server (no verified source exists today) |
| `shop.DeliveryAdapter` interface with `DisabledAdapter` only (dry run explains, never executes) | A Nitrado adapter (section 11) |

`FILE_STAGED` will never mean an item spawned, and `RESTART_OBSERVED` will never mean an item was delivered.

## 2. Delivery policies

| Policy | Player sends | Staff do |
|---|---|---|
| `MANUAL_PICKUP` (default; every existing product) | no `delivery` object - one is **refused** (`400 DELIVERY_COORDINATES_NOT_ACCEPTED`) | arrange the handover |
| `MANUAL_COORDINATE` | `delivery: { x, z }` - **required** (`400 DELIVERY_COORDINATES_REQUIRED`) | deliver to that position |

OWNER/ADMIN set `deliveryPolicy` on `POST .../shop/products` and `PUT .../shop/products/{productID}` (anything else is `400`; a blank value means `MANUAL_PICKUP`). The policy, map
and coordinates are **snapshotted on the delivery record at purchase time**: changing the product's policy or the installation's map later never rewrites an existing order.

## 3. Map validation

DayZ positions are `(x, y, z)` with `y` the altitude; a ground position is `(x, z)` in metres from the south-west corner of a square terrain - the same axes the heatmaps use
(`docs/HEATMAPS.md`). **Champion never invents a `y`**; a `y` key in the request is rejected as an unknown field.

No verified source tells Champion which map a server runs (the Nitrado service list has no map field, and ADM logs do not name it), so **the map is configured per installation
by OWNER/ADMIN** (`PUT .../shop/delivery-settings`). Only maps with verified bounds are accepted (`internal/dayzmap`):

| `mapKey` (engine world name) | Map | Valid `x` and `z` |
|---|---|---|
| `chernarusplus` | Chernarus | `0..15360` inclusive |
| `enoch` | Livonia | `0..12800` inclusive |

Any other key - including `sakhal`, community maps, or display names such as `chernarus` - is `400 UNSUPPORTED_MAP`. Adding a map means adding its verified size to
`internal/dayzmap`. A coordinate purchase on an installation without a supported map is `409 DELIVERY_MAP_UNRESOLVED` ("an admin must set this server's map ..."); pickup products keep
working. Validation order for a coordinate purchase: request shape (both `x` and `z` present and finite -> else `400 DELIVERY_COORDINATES_INVALID`; a non-number is `400 INVALID_REQUEST`),
then - under the product lock, inside the purchase transaction - the policy, the installation's map, and the bounds (`400 DELIVERY_COORDINATES_OUT_OF_BOUNDS`).

## 4. Database (migration `0049_shop_delivery_engine`, additive)

| Table / column | Notes |
|---|---|
| `shop_products.delivery_policy` | `NOT NULL DEFAULT 'MANUAL_PICKUP'`, `CHECK IN ('MANUAL_PICKUP','MANUAL_COORDINATE')` - every existing product becomes `MANUAL_PICKUP` |
| `shop_delivery_settings` | `installation_id` PK, `organization_id` (composite FK `(installation_id, organization_id)` -> `installations`, `ON DELETE CASCADE`), `map_key` (NULL = not configured), `updated_by_user_id`, timestamps |
| `shop_deliveries` | `id`, `purchase_id` (**`UNIQUE`** - one delivery per purchase; composite FK `(purchase_id, installation_id)` -> `shop_purchases`), `organization_id` + `installation_id` (composite FK -> `installations`), `game_server_id` (`ON DELETE SET NULL`), `player_id`, `delivery_type` (`CHECK = 'MANUAL'`), `delivery_policy` (snapshot), `map_key`, `coord_x`, `coord_z` (`DOUBLE PRECISION`; no altitude column), `status` (`CHECK IN ('MANUAL_READY','FULFILLED','CANCELLED')`), `created_at`, `updated_at`, `fulfilled_at`, `fulfilled_by_user_id`, `cancelled_at`, `cancelled_by_user_id`, `cancel_reason` (`REFUNDED`, `PURCHASE_CANCELLED`, `PURCHASE_FAILED`) |

Constraints on `shop_deliveries`: a pickup has no map and no coordinates; a coordinate delivery has a map and both coordinates in `0..100000` (a range comparison that also rejects
`NaN` and `+-Infinity`); `FULFILLED` requires `fulfilled_at`; `CANCELLED` requires `cancelled_at` and a reason. Indexes: `(installation_id, status, id DESC)` (the queue),
`(installation_id, id DESC)`, `(installation_id, player_id, id DESC)`. **No Nitrado credential or server file path is stored.**

**Backfill** (`database.ShopDeliveryBackfillSQL`, run by the migration, idempotent): every existing purchase gets a `MANUAL_PICKUP` delivery derived from - never changing - its own state:

| Purchase | Delivery |
|---|---|
| `PENDING_FULFILLMENT` / `PAID` / `PENDING` | `MANUAL_READY` |
| has `fulfilled_at` (`FULFILLED`, or `REFUNDED` after being handed over) | `FULFILLED`, with the purchase's `fulfilled_at` and actor |
| `REFUNDED` / `CANCELLED` / `FAILED` without `fulfilled_at` | `CANCELLED` (`REFUNDED` / `PURCHASE_CANCELLED` / `PURCHASE_FAILED`), at the refund/cancel time |

**Rolling deploys / restarts**: a purchase written by an older instance after the migration ran (no delivery row) is healed by `database.ShopDeliveryHealSQL` - the same derivation
for one purchase - inside fulfill and refund, so it stays fulfillable/refundable. All delivery state is in PostgreSQL; there is no in-memory queue, so a process restart loses nothing.

## 5. State transitions

```
purchase created ............ delivery MANUAL_READY        (same transaction as the purchase)
POST .../fulfill ............ MANUAL_READY -> FULFILLED    (purchase PENDING_FULFILLMENT -> FULFILLED, same transaction)
POST .../refund (undelivered) MANUAL_READY -> CANCELLED    (purchase -> REFUNDED, same transaction, cancel_reason REFUNDED)
POST .../refund (delivered) . FULFILLED stays FULFILLED    (purchase FULFILLED -> REFUNDED; the historical fulfillment is kept)
```

There is no separate "cancel delivery" operation: cancelling a paid order means refunding it, so Points are always returned. `FULFILLED` and `CANCELLED` are final.
`ShopRepository.ReconcileShop` additionally reports `MISSING_DELIVERY` and `DELIVERY_STATE` (the delivery disagrees with its purchase) - read-only, never repaired.

## 6. Purchase payload

`POST .../shop/purchases`:

```json
{ "productId": 101, "quantity": 1, "idempotencyKey": "shop-unique-request", "delivery": { "x": 7500.25, "z": 8300.50 } }
```

`delivery` is required for `MANUAL_COORDINATE` products and refused for `MANUAL_PICKUP` products; omitting it keeps the Phase 1 request exactly as before. `x`/`z` are JSON numbers.

**Atomicity.** One transaction commits the purchase row, the Champion Points debit (`applyLedger`), the stock decrement, the item snapshot **and the delivery row** - or none of
them: a delivery validation error or a delivery insert failure leaves no purchase, no debit, no stock change and no delivery (`TestShopDeliveryPersistenceFailureRollsBackThePurchase`
forces an insert failure with a trigger). Lock order is unchanged: product row -> purchase row -> balance row; the delivery row is written after the item snapshot.

**Idempotency.** The delivery instructions are part of the request identity. Same key + same product, quantity **and coordinates** (or no coordinates for a pickup) -> `200`, the existing
purchase and its delivery, `duplicate: true`. Same key with a different product, quantity, coordinates, or with/without coordinates -> `409 DUPLICATE_PURCHASE`, and the stored order is
unchanged. A replay is answered from the stored order even if the product's policy or the map changed since.

## 7. Response DTOs

```ts
interface ShopDelivery {                   // player-safe; embedded in ShopPurchase.delivery and returned by GET /me/deliveries/{id}
  id: number; purchaseId: number;
  deliveryType: "MANUAL";
  policy: "MANUAL_PICKUP" | "MANUAL_COORDINATE";
  status: "MANUAL_READY" | "FULFILLED" | "CANCELLED";          // open set: Phase 2B adds states
  map: ShopDeliveryMap | null;                                  // the map snapshotted on the order (null for pickup)
  coordinates: { x: number; z: number } | null;                 // null for pickup; never a y
  createdAt: string; updatedAt: string; fulfilledAt: string | null; cancelledAt: string | null;
}
interface ShopDeliveryMap { key: string; name: string; minX: number; maxX: number; minZ: number; maxZ: number }

interface AdminShopDelivery extends ShopDelivery {   // admin queue/detail; embedded in AdminShopPurchase.delivery
  player: { accountId: number; gamertag: string };
  gameServerId: number | null;
  purchaseStatus?: string;                           // queue/detail only
  items?: ShopPurchaseItem[];                        // queue/detail only: product snapshot (name, unit price) and quantity
  fulfilledByDiscordUserId: string | null; cancelledByDiscordUserId: string | null;
  cancelReason: "REFUNDED" | "PURCHASE_CANCELLED" | "PURCHASE_FAILED" | null;
}

// ShopPurchase (docs/SHOP.md) gains:  delivery: ShopDelivery | null     (null only for a not-yet-healed rolling-deploy purchase)
// AdminShopPurchase gains:           delivery: AdminShopDelivery | null
// ShopProduct / AdminShopProduct gain: deliveryPolicy: "MANUAL_PICKUP" | "MANUAL_COORDINATE"

interface ShopDeliverySettings { map: ShopDeliveryMap | null; coordinateDeliveryAvailable: boolean; automaticDelivery: false }
interface AdminShopDeliverySettings extends ShopDeliverySettings {
  mapKey: string | null; supportedMaps: ShopDeliveryMap[]; updatedAt: string | null; updatedByDiscordUserId: string | null;
}
```

## 8. API routes

Base `/api/saas/organizations/{organizationID}/installations/{installationID}/shop`; the standard SaaS chain (service auth, acting user, tenant scope) and error envelope, exactly as
`docs/SHOP.md` section 7. **player** = synced user (`/me/*` additionally a VERIFIED DayZ link, `409 PLAYER_IDENTITY_REQUIRED`); **admin** = organization OWNER/ADMIN (`403 SHOP_FORBIDDEN`).

| Route | Who | Returns |
|---|---|---|
| `POST /purchases` | player | as before, with optional `delivery`; `purchase.delivery` in the response |
| `GET /me/purchases`, `GET /me/purchases/{purchaseID}` | player | purchases now carry `delivery` (own purchases only) |
| `GET /me/deliveries/{deliveryID}` | player | `{ delivery: ShopDelivery }`; another player's (or another installation's) is `404 DELIVERY_NOT_FOUND` |
| `GET /delivery-settings` | player | `{ settings: ShopDeliverySettings }` - the map and bounds for coordinate input |
| `GET /admin/delivery-settings` | admin | `{ settings: AdminShopDeliverySettings }` |
| `PUT /delivery-settings` | admin | body `{ "mapKey": "chernarusplus" \| "enoch" \| null }` (null or `""` clears) -> `{ settings: AdminShopDeliverySettings }` |
| `GET /admin/deliveries?status=&policy=&playerId=&limit=&cursor=` | admin | `{ currency, items: AdminShopDelivery[], nextCursor, limit }`, newest first. `status` = `MANUAL_READY`/`FULFILLED`/`CANCELLED`; `policy` = `MANUAL_PICKUP`/`MANUAL_COORDINATE`; `playerId` = economy `accountId`; `limit` default 25, max 100; `cursor` = the previous `nextCursor` |
| `GET /admin/deliveries/{deliveryID}` | admin | `{ delivery: AdminShopDelivery }` |
| `GET /admin/purchases`, `GET /admin/purchases/{purchaseID}` | admin | purchases now carry `delivery` (admin view) |
| `POST /purchases/{purchaseID}/fulfill` | admin | **reused**: fulfills the purchase **and** its delivery atomically |
| `POST /purchases/{purchaseID}/refund` | admin | **reused**: cancels an undelivered delivery atomically; keeps a delivered one |
| `POST /products`, `PUT /products/{productID}` | admin | accept `deliveryPolicy` |

Rate limits: `PUT /delivery-settings` shares the 30/min admin mutation budget; reads are not limited. Audit: `shop_delivery_settings_updated` (map key only);
`shop_purchase_created` and `shop_product_updated` log the delivery policy. Coordinates are never logged.

## 9. Error codes

| Code | HTTP | When |
|---|---|---|
| `DELIVERY_COORDINATES_REQUIRED` | 400 | a `MANUAL_COORDINATE` product bought without `delivery` |
| `DELIVERY_COORDINATES_INVALID` | 400 | `delivery` without both `x` and `z`, or a non-finite value |
| `DELIVERY_COORDINATES_OUT_OF_BOUNDS` | 400 | the position is outside the installation's map |
| `DELIVERY_COORDINATES_NOT_ACCEPTED` | 400 | `delivery` sent for a `MANUAL_PICKUP` product |
| `DELIVERY_MAP_UNRESOLVED` | 409 | coordinate purchase, but the installation has no supported map configured |
| `UNSUPPORTED_MAP` | 400 | `PUT /delivery-settings` with a map that has no verified bounds |
| `DELIVERY_NOT_FOUND` | 404 | unknown delivery, another installation's, or (player) another player's |
| `INVALID_REQUEST` | 400 | malformed body (a non-number coordinate, an unknown key such as `y`), bad queue filter/cursor/limit |

Existing Shop codes are unchanged (`INVALID_PURCHASE_STATUS` for fulfilling a refunded/cancelled order, `DUPLICATE_PURCHASE`, `INSUFFICIENT_FUNDS`, ...).

## 10. Fulfillment and refund guarantees

* **Fulfill** locks the purchase row, requires `PENDING_FULFILLMENT` and a `MANUAL_READY` delivery, and moves both to `FULFILLED` with the same actor and time in one transaction.
  Concurrent fulfills: exactly one succeeds (`TestShopDeliveryFulfillmentAndRefunds`, 10 concurrent). Saving coordinates never fulfills anything.
* **Refund** reuses the compensating `SHOP_REFUND` ledger credit (docs/SHOP.md section 6) and, in the same transaction under the purchase lock, cancels a `MANUAL_READY` delivery
  (`cancel_reason = REFUNDED`); a refunded pending delivery can never be fulfilled afterwards (`409 INVALID_PURCHASE_STATUS`). A delivered order keeps its `FULFILLED` delivery and
  `fulfilledAt`. Duplicate/concurrent refunds: exactly one. Fulfill racing refund: whichever wins, purchase and delivery agree (checked by reconciliation).

## 11. Nitrado investigation (what exists today)

**Phase 2B research** (official Nitrado upload flow, the DayZ object-spawner mechanism, the non-executing plan/dry-run prototype, duplicate prevention and the test-server experiment) is in `docs/SHOP_DELIVERY_PHASE2B.md`.

What `internal/nitrado` actually implements for connected console DayZ services:

| Capability | Status | Endpoint |
|---|---|---|
| Service list / metadata | implemented (read) | `GET /services` - `details.game`, `folder_short`; **no map/mission field** |
| File listing | implemented (read) | `GET /services/{id}/gameservers/file_server/list?dir=` (used for ADM/RPT logs) |
| File download | implemented (read) | `GET /services/{id}/gameservers/file_server/download?file=` -> signed URL |
| Restart / stop | implemented (Client Admin, typed confirmation) | `POST /services/{id}/gameservers/restart`, `.../stop` (docs/CLIENT_ADMIN.md) |
| File upload / write | **not implemented** | no upload call exists; the codebase has zero Nitrado file-write capability |
| In-game spawn | **none** | no API spawns an item on console DayZ; any automatic delivery would have to edit mission files and wait for a restart |

Requirements for a Phase 2B automatic adapter (not built):

1. A verified, documented Nitrado file-upload endpoint, exercised against a disposable test server first - never a customer's live mission.
2. A verified mission-file format for staged spawns (for example a spawn list read at server start), with the map resolved from the server rather than configured by hand.
3. Explicit customer opt-in per installation; no automatic restart - restarts stay a separate, confirmed admin action.
4. New delivery states (section 1) through a new migration, with `FILE_STAGED`/`RESTART_OBSERVED` never reported as "delivered"; a verification step (for example ADM evidence) before `FULFILLED`.
5. The adapter receives a `shop.DeliveryPlan` (ids, map, x/z, item snapshot) and never a stored credential.

Phase 2A ships `shop.DeliveryAdapter` with **`DisabledAdapter` only**: `Enabled()` is false, `Deliver` returns `ErrAutomaticDeliveryDisabled`, and `DryRun` returns planned steps
all prefixed `NOT EXECUTED:` plus the blockers above. No code path calls an adapter, no configuration can enable one, and the database refuses every automatic state.

## 12. Tests

* `internal/dayzmap` (unit): verified maps only, inclusive bounds, NaN/Infinity rejected.
* `internal/shop/delivery_test.go` (unit): coordinate shape validation before any store call, policy validation, queue filter/cursor validation, player scoping, verified-map
  settings, the disabled adapter.
* `internal/app/saas_api_shop_delivery_integration_test.go` (real routes + PostgreSQL): pickup backward compatibility; map configuration and unsupported maps; coordinate shape and
  bounds; snapshot survives product/map edits; rollback of Points/stock/purchase/delivery on a forced delivery insert failure; idempotent replay, changed-payload keys and a 20-way burst;
  concurrent purchases of finite stock; concurrent fulfillment; refund cancellation; delivered-then-refunded history; fulfill-vs-refund races; the queue's filters, pagination, tenant
  isolation and authorization; player privacy; backfill, heal, restart safety; the database refusing reserved states.
* The existing Shop, Economy and Billing suites run unchanged; every Shop test's `allIntegrity` now also checks purchase/delivery agreement.
