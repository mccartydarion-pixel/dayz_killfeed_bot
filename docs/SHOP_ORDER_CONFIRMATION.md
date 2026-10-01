# Shop order confirmation and support tickets

After a Shop order is delivered, the buyer is asked one question with two answers:

- **Received order** closes the order from the buyer's side.
- **Issue with order** opens a support ticket for staff.

If the buyer does not answer within **48 hours**, the order is completed automatically. The buyer can
still report an issue after that: silence is not treated as consent.

This document covers the bot API and database (piece 1). The Discord buttons and ticket channels, the
website pages and the automatic delivery worker are separate changes and are not part of this one.

## What this change does not do

- It does not change the purchase or delivery status machines, their constraints, or the
  delivery-attempt ledger. The confirmation is a separate record next to the order.
- It moves no Champion Points. Resolving a ticket as `REFUNDED` only records the decision; the refund
  itself is still the Shop's refund action, with its own guards.
- It sends nothing to Discord and contacts no game server.
- It does not enable automatic delivery.

## Data (migration `0087_shop_order_confirmations`)

Additive and forward-only. No existing row is read or rewritten.

### `shop_order_confirmations`

One row per purchase. A database trigger on `shop_purchases` opens it at the moment the purchase
becomes `FULFILLED`, so every fulfilment path (manual, canary, and later the automatic worker) gets a
confirmation without its own code. Orders that were already `FULFILLED` before the migration get no
row: they predate the feature and the API reports them as having nothing to confirm.

| State | Meaning | Reached from |
| --- | --- | --- |
| `AWAITING_BUYER` | Delivered, waiting for the buyer | the purchase becoming `FULFILLED` |
| `RECEIVED` | The buyer confirmed | `AWAITING_BUYER` |
| `ISSUE_REPORTED` | The buyer reported an issue; a ticket exists | `AWAITING_BUYER`, `AUTO_COMPLETED` |
| `AUTO_COMPLETED` | The deadline passed with no answer | `AWAITING_BUYER` |
| `VOID` | The purchase was refunded while still awaiting | `AWAITING_BUYER` |

`response_source` records who moved it: `SITE`, `DISCORD`, `AUTO` (the deadline) or `SYSTEM` (refund).
A refund after the buyer answered leaves the answer on record.

Every transition is a compare-and-set on the current state. Of two concurrent answers, or an answer
racing the deadline, exactly one wins; the other gets `INVALID_CONFIRMATION_STATE`.

### `shop_order_tickets`

One ticket per reported issue, at most one `OPEN` ticket per purchase (unique partial index). Staff
resolve it as `COMPLETED`, `REFUNDED` or `OTHER` (a note is required for `OTHER`).
`discord_channel_id` is empty until the Discord side creates the ticket channel; it is set once.

## API

Base: `/api/saas/organizations/{organizationID}/installations/{installationID}/shop`. Every route runs
the Shop's own chain: service authentication, acting user, tenant scope and the plan feature gate.

### Buyer

The buyer can only see and answer for their own purchases. Another player's purchase, or another
tenant's, is reported as not found.

| Method and path | Body | Result |
| --- | --- | --- |
| `GET /me/purchases/{purchaseID}/confirmation` | | `{confirmation}` |
| `POST /me/purchases/{purchaseID}/confirm-received` | | `{confirmation}` |
| `POST /me/purchases/{purchaseID}/report-issue` | `{"reason": "..."}` (1-1000 characters) | `201 {confirmation, ticket}` |

`confirmation` carries `state`, `deliveredAt`, `deadlineAt`, `respondedAt`, `responseSource`, and two
flags the buttons render from: `canConfirmReceived` and `canReportIssue`.

### Staff (organization OWNER or ADMIN)

| Method and path | Body | Result |
| --- | --- | --- |
| `GET /admin/purchases/{purchaseID}/confirmation` | | `{confirmation}` |
| `GET /admin/tickets?status=OPEN\|RESOLVED&before=<id>&limit=` | | `{items, nextBefore}`, newest first |
| `GET /admin/tickets/{ticketID}` | | `{ticket}` |
| `POST /admin/tickets/{ticketID}/resolve` | `{"resolution": "COMPLETED\|REFUNDED\|OTHER", "note": "..."}` | `{ticket}` |

### Error codes

| Code | HTTP | When |
| --- | --- | --- |
| `ORDER_CONFIRMATION_NOT_FOUND` | 404 | Not delivered yet, delivered before this feature, or not the caller's purchase |
| `INVALID_CONFIRMATION_STATE` | 409 | Already answered, refunded, or a second issue report |
| `SHOP_TICKET_NOT_FOUND` | 404 | No such ticket in this installation |
| `SHOP_TICKET_ALREADY_RESOLVED` | 409 | A second resolve |
| `INVALID_REQUEST` | 400 | Missing or over-long reason, unknown resolution, unknown status filter |

The buyer answer routes share the purchase rate limiter; the resolve route shares the Shop admin one.

## Deadline sweep

The bot auto-completes overdue confirmations once at start and then every 5 minutes. Rows are taken
with `FOR UPDATE SKIP LOCKED` and closed with a compare-and-set, so several bot instances can run the
sweep at once and each row is closed exactly once. The 48-hour window is the default of the
`deadline_at` column; `repository.ShopConfirmationWindow` mirrors it.

## Tests

- `internal/shop/confirmation_test.go`: validation, buyer scoping, staff paths, sweep batching.
- `internal/repository/shop_confirmation_integration_test.go` (PostgreSQL): the trigger on fulfilment
  and refund, tenant and buyer isolation, one open ticket per order, the deadline, an issue after
  auto-completion, concurrent answers, and the table constraints.
