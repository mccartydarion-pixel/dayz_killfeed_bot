# Shop order confirmation and support tickets

After a Shop order is delivered, the buyer is asked one question with two answers:

- **Received order** closes the order from the buyer's side.
- **Issue with order** opens a support ticket for staff.

If the buyer does not answer within **48 hours**, the order is completed automatically. The buyer can
still report an issue after that: silence is not treated as consent.

This document covers the bot API and database (piece 1) and the Discord side (piece 2). The website
pages and the automatic delivery worker are separate changes.

## What this change does not do

- It does not change the purchase or delivery status machines, their constraints, or the
  delivery-attempt ledger. The confirmation is a separate record next to the order.
- It moves no Champion Points. Resolving a ticket as `REFUNDED` only records the decision; the refund
  itself is still the Shop's refund action, with its own guards.
- It contacts no game server.
- It posts nothing in public channels. The only Discord messages are the buyer's DM and the private
  ticket channel.
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

## Discord

Migration `0084_shop_order_confirmation_discord` adds the delivery state of the DM to
`shop_order_confirmations`, the channel retry state to `shop_order_tickets`, and
`shop_ticket_discord_setup` (per Discord server: Owner role, Staff role, Tickets category).

### The delivered-order DM

Every 30 seconds the bot looks for delivered orders still awaiting their buyer and sends each buyer
one DM: what was delivered, the deadline, and three buttons: **Received order**, **Issue with order**
and a link to the order on the website.

- The DM goes to the Discord account with a `VERIFIED` link to the order's player in that server.
- A buyer with no verified account, with DMs closed, or who blocked the bot gets no DM. The notice
  ends as `UNAVAILABLE` and the buyer answers on the website. Nothing is posted in a public channel.
- A Discord outage is retried, up to 5 attempts, then the notice ends as `UNAVAILABLE`.
- Work is leased (`FOR UPDATE SKIP LOCKED` plus a 5-minute claim), so several bot instances do not
  send the same DM at once. A crash between sending and recording can produce a second DM; the
  buttons are safe to press twice because every answer is a compare-and-set.

### The buttons

A button id only names a purchase. On every press the bot looks the order up for the pressing account
(`VERIFIED` link to the order's player, in the order's own server) and then calls the same service the
website uses, so a forged or forwarded id reaches nothing and the plan gate applies as on the API.

- **Received order** records the answer and replaces the DM with the result.
- **Issue with order** opens a form with one required field, "What went wrong?" (up to 1000
  characters). Submitting it opens the ticket and replaces the DM with the ticket number.
- A press on an order that was already answered (on the website, by the deadline, or refunded) shows
  where the order stands and removes the buttons that no longer apply.

### Ticket channels, Owner and Staff

Every open ticket, whether it was opened from Discord or from the website, gets a private channel
named `ticket-<ticket>-order-<purchase>`. It is created right after the ticket is opened, and a sweep
every minute retries the ones that failed (up to 5 attempts each).

The first ticket on a server makes the bot set up three things, which it then reuses:

| What | Rule |
| --- | --- |
| **Owner** role | The stored role if it still exists, otherwise an existing role named "Owner", otherwise a new one. The bot gives it to the Discord server owner. |
| **Staff** role | Same rule with "Staff". The server owner decides who else gets it. |
| **Tickets** category | The stored category if it still exists, otherwise an existing category named "Tickets", otherwise a new private one. |

Roles the bot creates carry no permissions of their own; they only name who can see tickets. A role
managed by an integration is never adopted.

A ticket channel is hidden from everyone and visible to exactly: the buyer, the Staff role, the Owner
role and the bot. Each channel carries these permissions itself, so its privacy does not depend on
the category. The opening message pings the buyer and the two roles and shows the buyer's
description and the order contents; the buyer's text cannot ping anyone.

When staff resolve the ticket, the bot posts the resolution in the channel. It does not delete or
lock the channel.

### What the bot needs on the Discord server

- **Manage Roles**, to create Owner and Staff and to give Owner to the server owner. The bot's own
  role must sit above the Owner role for the assignment to work; if it does not, the ticket still
  opens and the bot logs `owner_role_not_assigned`.
- **Manage Channels**, to create the Tickets category and the ticket channels.

Without them the ticket is still open and visible to staff on the website; only the channel is
missing. The bot logs `ticket_channel_failed` with the action to take.

## Tests

- `internal/shop/confirmation_test.go`: validation, buyer scoping, staff paths, sweep batching.
- `internal/repository/shop_confirmation_integration_test.go` (PostgreSQL): the trigger on fulfilment
  and refund, tenant and buyer isolation, one open ticket per order, the deadline, an issue after
  auto-completion, concurrent answers, and the table constraints.
- `internal/discord/shop_order_test.go`: button ids, the DM and its states, the issue form, role and
  category setup (create, reuse, adopt, failures), channel privacy and mentions.
- `internal/app/shop_order_discord_test.go`: notice outcomes (sent, no account, DMs closed, outage),
  roles created once, one channel per ticket, missing permissions.
- `internal/repository/shop_confirmation_discord_integration_test.go` (PostgreSQL): the notice and
  ticket leases, retries running out, the verified-link lookup behind every button, the per-server
  setup.
