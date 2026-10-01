# Shop automatic delivery worker: what was built (stage 0)

The design and its four owner decisions are in `SHOP_DELIVERY_WORKER_DESIGN.md`. This document is
what exists in the code after stage 0, how it is switched, and what a person does when it stops.

**Stage 0 writes nothing to any game server.** Every switch is off by default, and stage 0 was
tested only against simulated servers and disposable databases.

## The switches

All four must be on for an order to be delivered automatically. Any one of them off stops it.

| # | Switch | Who sets it | Where |
| --- | --- | --- | --- |
| 1 | Worker lock | Operator, on the server | `CHAMPION_SHOP_AUTO_DELIVERY` = `enabled`, and the installation id in `CHAMPION_SHOP_AUTO_DELIVERY_INSTALLATION_IDS` |
| 2 | Installation switch | Organization owner or admin | `PUT …/shop/admin/auto-delivery {"enabled": true}` |
| 3 | Not paused | The worker pauses; a person resumes | `POST …/shop/admin/auto-delivery/resume` |
| 4 | Product switch | Organization owner or admin | `PUT …/shop/admin/products/{id}/auto-delivery {"autoDelivery": true, "className": "…"}` |

The worker lock:

- Only the exact words `report` and `enabled` start a worker. `true`, `1`, `yes` or `ENABLED` do not.
- `report` runs every pass read-only: it inspects the server every 5 minutes and logs which boot it
  sees and which orders it would stage. It writes nothing to the server and nothing to the Shop
  tables. It does hold the installation's lease.
- `enabled` delivers, with a pass every minute.
- `CHAMPION_SHOP_AUTO_DELIVERY_MAX_STAGED` is the number of orders in the file at once: 1 unless set
  to 2 to 5.
- The canary lock is separate. Neither lock opens the other.
- Startup logs `component=shop_delivery_worker mode=… installations=… max_staged=…`.

## What a pass does

One pass works one installation, under a database lease so two bot processes never work the same
server. In order:

1. **Buyer answers** (database only). For each order whose item was removed from the file:
   - once the server's own log shows the boot finished without a spawner error, the buyer's
     confirmation is opened and the existing DM and site buttons take over;
   - "Received order", or the 48-hour deadline, fulfils the order;
   - "Issue with order" sends the attempt to review (the buyer's ticket is already open);
   - a spawner error naming the Champion file, or no log evidence within 30 minutes, sends it to
     review and opens a ticket.
2. **Stop if a write is unresolved.** An earlier write with an uncertain outcome pauses the
   installation.
3. **Inspect the server**, only if there is something to do: the configuration, the Champion file,
   the boot list and the status, in one snapshot.
4. **Check the server is what the worker expects.** The configuration and mission folder must be
   the ones accepted when delivery was last resumed, and the file must hold exactly the orders the
   ledger says are staged. Otherwise the installation is paused.
5. **Finish an interrupted write.** A write that was journaled but never recorded is completed from
   the file's read-back: landed, not landed, or neither (paused).
6. **Apply the restart rules** to each staged order, then at most **one write**:
   - remove orders whose restart has happened (and those sent to review), or
   - stage new orders, if none needed removing.

Restart rules for a staged order:

| What the boot list shows | Result |
| --- | --- |
| No boot newer than the one it was staged in | Wait |
| One newer boot, started at least 3 minutes after staging | Remove it, then ask the buyer |
| One newer boot that started before the staging, or less than 3 minutes after it | Remove it, review: whether it spawned is unknown |
| Two or more newer boots | Remove it, review: it may have spawned twice |
| A new boot appears while the removal is being written | Review: it may have spawned twice |

Staging needs all of these: the server is running, the server clock offset is known, and the current
boot is at least 10 minutes old (a newer boot's log file can take 9 minutes to be listed).

## What the worker can and cannot write

- One file only: `custom/champion_shop_delivery.json`. It never creates the file or its folder
  (that is the owner-approved Gate C) and never edits `cfggameplay.json` (Gate D).
- One write request per pass, never repeated. The outcome is decided by reading the file back.
- Every write is journaled in `shop_delivery_writes` before it is sent. The journal row is closed
  only after the ledger reflects the write.
- `TestWriteCapabilityIsIsolated` enforces that the worker is the only package besides the
  operator's tool that may import the write code, that it uses only the two artifact primitives,
  and that only `internal/app/shop_delivery_worker.go` starts it. **This guard was widened for the
  worker**: before stage 0 it allowed the operator's tool alone.

## Fulfilment: the one ledger rule that changed

Migration `0091_shop_delivery_attempt_buyer_fulfilment` adds `fulfilment_mode` to attempts.

- `OBSERVED` (default, the manual canary path): unchanged. Fulfilment needs a recorded sighting,
  pickup, second boot and no-respawn check.
- `BUYER` (worker attempts): fulfilled only with the buyer's answer, `RECEIVED` or
  `AUTO_COMPLETED`. A database trigger checks that the buyer's confirmation holds exactly that
  answer, so no code path can fulfil a worker attempt the buyer did not answer.

The mode is fixed when the attempt is created. While the buyer is being asked, the order cannot be
refunded or fulfilled by hand, exactly as for any attempt that may have put an item on the server.

## Where the item appears

The buyer's own last logged position (owner decision 1).

- `GET …/shop/me/delivery-position?productId=N` returns `automatic`, and the buyer's newest logged
  position in the server's current boot: `x`, `z`, its age, and whether it is fresh (at most 20
  minutes old). The altitude is never sent to or accepted from the client.
- A purchase of an automatic product is accepted only with exactly those coordinates. Otherwise it
  is refused with `DELIVERY_POSITION_REQUIRED` and nothing is written.
- The worker takes the altitude from the same log line (`player_location_events`).
- When any switch is off, the same product is bought the old way: typed coordinates, manual queue.

## When it pauses

A pause stops delivery for that installation and logs `event=paused` with the reason. New orders
wait in the manual queue. Nothing is retried and nothing is refunded.

| Reason | What to check |
| --- | --- |
| The Champion file is missing, or `cfggameplay.json` no longer references it | Gates C and D on the server |
| `cfggameplay.json` or the mission folder changed | Whether the change was intended; resuming accepts the current configuration |
| The file holds content Champion did not write, or does not match the ledger | Who edited the file; restore the expected content with the operator's tool |
| A write had an uncertain outcome | The file's actual content, with the operator's tool |
| The configuration changed while the file was being written | As above |

Resuming (`POST …/admin/auto-delivery/resume`) is a person saying they checked. It clears the pause,
forgets the accepted configuration and marks an unresolved write as resolved by that person.

## When an order goes to review

The attempt is `FAILED_REVIEW`, its item has been removed from the file, and a ticket is open (the
buyer's own, or a `SYSTEM` ticket with the reason). Staff check in game and record the result:

`POST …/shop/admin/delivery-attempts/{attemptID}/review {"outcome": "NOT_SPAWNED" | "SPAWNED", "observer": "…", "detail": "…"}`

- `NOT_SPAWNED` makes the refund possible.
- `SPAWNED` makes the manual fulfilment possible.
- Neither moves points or creates a new attempt. The worker never stages that order again.

`GET …/shop/admin/purchases/{purchaseID}/delivery-attempts` lists an order's attempts.

## Tests

| Suite | What it covers |
| --- | --- |
| `internal/shop/missionwrite/artifact_test.go` | The worker's reads and its single write through the real Nitrado client against the stand-in server: stale snapshots, refused, dropped, partial and unverifiable transfers, changes during the write |
| `internal/shop/deliveryworker/worker_test.go` | The pass engine against a simulated server, ledger and buyer: the full cycle, every restart rule, every write outcome, every pause, recovery after stopping mid-write, batching, report-only |
| `internal/repository/shop_auto_delivery_integration_test.go` | PostgreSQL: the buyer-fulfilment rule, switches, pause and resume, the lease, the journal, deliverable-order selection, boot log, system tickets |
| `internal/app/saas_api_shop_auto_delivery_integration_test.go` | The routes: switches, product setting, the position gate on purchase, staff review |
| `internal/app/saas_api_shop_worker_e2e_integration_test.go` | One order from purchase to fulfilment and one to review and refund, with the real worker, ledger and routes and a simulated server |
| `internal/config/config_test.go` | The lock opens only for the exact words, and never through the canary lock |

## Not done in stage 0

- The website: owner switch, product setting, the position step at purchase, staff review form.
- Predicting restart times, to avoid staging shortly before one. Today such an order goes to review.
- Any contact with a real server. That starts at stage 1 (report only), which needs its own approval.
