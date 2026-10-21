# Shop automatic delivery worker: design

**Status: design, with stage 0 built.** What exists in the code is described in
`SHOP_DELIVERY_WORKER.md`. Automatic delivery stays off by default (`CHAMPION_SHOP_AUTO_DELIVERY`
unset) until the owner approves each rollout stage.

The worker does by itself what the canary (attempt `champion:d1:a1`, fulfilled 2026-09-30) did by
hand: it puts a bought item into the Champion spawner file, waits for a server restart, removes it
again, and asks the buyer whether it arrived.

## What the canary proved, and what it did not

| Proved | Not proved |
| --- | --- |
| An item listed in `custom/champion_shop_delivery.json` spawns once at the next server start, at the exact position given | That a spawn can be confirmed from logs: a successful spawn writes nothing to the RPT |
| The upload, read-back and unstage work against the live Nitrado service | More than one item, or more than one order, in the file |
| Removing the entry before the following start prevents a second spawn | Item types other than `BandageDressing` |
| Only files under `custom/` reach the game host | The worker's own timing against real restarts |

Two timing facts from the canary shape the design:

- The spawner runs about 30 to 40 seconds after a boot starts.
- The bot only learns about a new boot 7.5 to 9 minutes after it starts, when the new ADM file is
  listed. So a file changed less than 10 minutes after a boot started is ambiguous: the worker
  cannot tell whether the spawner read it. The existing rule (`StagingQuietPeriod`, 10 minutes)
  already covers this.

## One order, end to end

1. **Purchase.** The buyer buys an automatic-delivery product while in game. The order records the
   buyer's own last logged position as the drop point (decision 1).
2. **Stage.** The worker adds the order to the spawner file, reads the file back and checks its
   hash. Attempt state: `FILE_STAGED`, then `AWAITING_RESTART`.
3. **Restart.** The worker never restarts the server. It waits for the next scheduled restart
   (about every 68 minutes on Champions).
4. **Boot accepted.** When the new boot's ADM file is accepted, the worker checks the boot's RPT for
   a spawner error naming the Champion file. State: `RESTART_OBSERVED`, then `UNSTAGE_REQUIRED`.
5. **Unstage.** The worker removes the order from the file at once and verifies the read-back.
   State: `VERIFICATION_REQUIRED`. This must finish before the following boot; there are about 59
   minutes to do it.
6. **Ask the buyer.** The order shows as "Delivered, please confirm" and the buyer gets the DM and
   site buttons that are already live.
   - **Received order**: the order becomes `FULFILLED`.
   - **Issue with order**: a ticket opens, the attempt goes to `FAILED_REVIEW`, and staff decide
     (re-deliver by hand, or refund).
   - **No answer in 48 hours**: the order becomes `FULFILLED`. The buyer can still report an issue.

From purchase to "please confirm" takes up to one restart interval plus about 10 minutes.

## Decisions for the owner

### 1. Where the item appears

The spawner does not snap to the ground, so every drop point needs a real altitude. The only source
of altitude is a position the server itself logged. Three options:

| Option | How it works | Trade-off |
| --- | --- | --- |
| **A. Deliver to the buyer's position (recommended)** | The server logs every online player's position every 5 minutes. The order uses the buyer's latest logged position in the current boot, at most 20 minutes old. The site shows it before they pay: "Your item will appear at X, Z after the next restart." | The buyer must be in game when buying and come back to that spot. Works inside bases. |
| B. Owner drop points | The owner registers fixed pickup points by standing on them. The buyer picks one. | Predictable, but public spots invite camping and theft. |
| C. Buyer types X and Z (today's manual flow) | Not possible automatically. | There is no altitude for an arbitrary map coordinate. |

A needs one new thing: the bot keeps each linked player's latest position for the current boot. It
is shown only to that player and to staff, and dropped when the boot ends.

### 2. What "delivered" means without anyone watching

A clean log is not proof that an item exists. The design keeps that rule: the worker never claims
the item was received.

**Recommended:** after a restart with no spawner error and a verified unstage, the order is
"Delivered, please confirm". It only becomes `FULFILLED` when the buyer says so or the 48 hours
pass. While it waits, a refund stays blocked exactly as today, because the item may be on the
server.

This is the one rule change in the delivery ledger: today `FULFILLED` requires an in-game sighting,
a pickup, a second boot and a no-respawn check recorded by a person. The worker path replaces those
with the buyer's own answer (or the deadline) plus the worker's verified unstage. The manual canary
path keeps its stricter rules.

### 3. Which products

**Recommended:** automatic delivery is off for every product by default. The owner switches it on
per product and enters the DayZ class name. Limits for the first version:

- one item type per order, 1 to 10 units (the existing `MaxUnitsPerOrder`);
- plain items only: no vehicles, no items with attachments or contents, until each kind has had its
  own supervised test;
- at most 50 objects in the file at once (the existing `MaxStagedObjects`).

### 4. What happens when something is unclear

**Recommended:** the worker stops and a person decides. It never retries a delivery, never refunds,
and never edits anything but its own file. Any of these pauses automatic delivery for that server,
alerts staff, and leaves new orders waiting in the manual queue:

| Situation | Worker action |
| --- | --- |
| The read-back after a write does not match | Stop. Attempt to `FAILED_REVIEW`. No second write. |
| A boot started within 10 minutes of staging | Unstage, `FAILED_REVIEW`. |
| Two boots started before the unstage was verified | `FAILED_REVIEW`: the item may have spawned twice. |
| The RPT names the Champion file in a spawner error | Unstage, `FAILED_REVIEW`: nothing spawned. Staff can refund. |
| `cfggameplay.json` changed, or no longer references the Champion file | Stop before writing. |
| The Champion file holds anything the ledger does not expect | Stop before writing. |
| Nitrado is unreachable | Wait and try the same step again later. A step that only reads is safe to repeat; a write is never repeated without a read-back first. |

Each `FAILED_REVIEW` opens the same kind of staff ticket as "Issue with order", so it lands in the
ticket list that already exists.

## How it is built

### Reused as it is

- **Ledger** (`shop_delivery_attempts`, events, evidence): every state change is a compare-and-set
  with an actor. The worker's actor is `worker:<instance>`.
- **Restart rules** (`canary.DecideRestart`): which boot counts, the quiet period, the two-boot rule.
- **Nitrado write path** (`internal/nitrado/file_write.go`): one token, one transfer, https
  `*.nitrado.net` only, outcome decided by read-back.
- **Boot identity**: the newest ADM file across both mounts.
- **Buyer confirmation and tickets**: migrations 0087 and 0088, live since 2026-10-01.

### New

| Part | What it does |
| --- | --- |
| Product settings | `auto_delivery` flag and `class_name` per product (owner only). |
| Installation setting | Owner switch for automatic delivery, plus a `paused` state with its reason. |
| Player position store | Latest logged position per linked player for the current boot. |
| Batch file builder | Builds the spawner file for the exact set of staged attempts, in a fixed order, so its hash is reproducible. Generalizes today's single-attempt builder. |
| Worker loop | One per bot process, one server at a time. Each pass does at most one write per server. Work is leased in the database so two bot instances never write the same file. |
| Write journal | The manual tool keeps its journal on the operator's PC. The worker records the same facts (plan id, hash before, hash after, outcome) in the database before and after each write. |
| Ledger change | The worker fulfilment path from decision 2, as a new migration with its own tests. |
| Purchase flow | The site shows the drop position and the expected restart before payment, and "Delivered, please confirm" afterwards. |

### Switches

Automatic delivery runs only when all of these are on. Any one of them off stops it:

1. A server variable, exact value required, listing installation ids (same pattern as the canary lock).
2. The owner's switch for the installation.
3. The product's automatic-delivery flag.
4. The installation is not paused.

The canary lock stays separate and closed.

### What the worker never does

- Restart or stop the server.
- Edit `cfggameplay.json` or any file other than `custom/champion_shop_delivery.json`.
- Stage an order again after any attempt that might have spawned.
- Refund, or mark an order fulfilled without the buyer's answer or the deadline.
- Print or store a token, signed URL or password.

## Rollout

Each stage needs its own approval. Nothing moves to the next stage by itself.

| Stage | What runs | Production writes |
| --- | --- | --- |
| 0. Build | Code and tests against the local Nitrado fixture and a disposable database. All switches off. | None |
| 1. Shadow | Deployed with the worker in "report only" mode: it logs what it would stage and when, and its restart detection is compared with real restarts for a few days. | None |
| 2. Supervised single order | One 1-point order on Champions with the owner in game, the worker doing every step. Batch size limited to one order. | The Champion file only |
| 3. Limited | On for Champions, for the products the owner allows, one order per restart. | The Champion file only |
| 4. Batched | Several orders per restart, after a supervised test with two orders in one file. | The Champion file only |

## Open risks

- **Theft.** The item lies on the ground from the restart until the buyer collects it. Anyone who
  finds it can take it. Option A reduces this (the buyer chose the spot); it does not remove it.
- **Wait time.** Up to about 68 minutes on Champions before the item appears. The site must say so
  before payment.
- **Unscheduled restarts.** A crash or a manual restart shortly after a scheduled one can start two
  boots before the unstage is verified. The worker detects this and sends the order to review; it
  cannot prevent it.
- **Other item types.** Quantity, condition and stacking for spawned items are only verified for one
  bandage. Each new kind needs a supervised order first.
- **Nitrado has no conditional write.** An owner edit to the Champion file between the worker's
  last read and its write is only caught by the read-back afterwards. The file is Champion-owned and
  nothing else should edit it.
