# Perk store

The Player Hub's **Donate** tab and the Client Hub's control hub. A server owner sells perks to
players for **Champion Points**; the points a player pays go to the owner's own balance.

No real money is involved, and the perks do not affect gameplay.

## What an offer grants

An offer has a price and grants one or more of:

| Perk | How it is delivered |
|---|---|
| Supporter tier | The tiers under Growth, Supporters (`vip_members`): killfeed badge, Discord role |
| Priority queue | The player's in-game name is put on the Nitrado priority list (`internal/nitrado/priority.go`) |
| Custom perk | Free text; staff hand it out and mark it done in the control hub |

An offer is either **one-time** (lasts `durationDays`, 0 = for good) or **monthly** (30 days,
charged again each month). It can be limited in time (`availableFrom` / `availableUntil`) and in
quantity (`stockLimit`), and can be giftable.

## Money

Every charge runs through the point ledger in the same transaction as the purchase row
(`internal/repository/perk_store_repository.go`).

| Ledger type | Who | What |
|---|---|---|
| `PERK_PURCHASE` | buyer, debit | a purchase or a monthly renewal |
| `PERK_SALE` | owner, credit | the same amount, received |
| `PERK_REFUND` | buyer, credit | a refunded payment |
| `PERK_SALE_REFUND` | owner, debit | the same amount, given back |

The owner is the organization owner's verified in-game character in that Discord server. With no
linked character nobody receives the points; the control hub says so.

A refund gives back the latest payment only, closes the purchase and takes the perks back. It is
refused when the owner no longer has the points.

## Lifecycle

1. **Purchase**: price, stock, gift rules and "already holds it" are checked and the buyer is
   charged, atomically. An idempotency key makes a repeated request charge once.
2. **Perks** are handed out right after. A failure (Nitrado down, the player holds another tier)
   is kept on the purchase for staff and retried by the scheduler, up to 5 times.
3. **Scheduler** (`runPerkStore`, every 45 seconds per guild):
   - monthly purchases that came due are charged again; one the buyer cannot pay, or whose offer
     was removed or switched off, ends;
   - purchases whose time ran out end;
   - pending perk hand-outs and removals are retried.
4. **Ending** takes the perks back. A tier is ended only when the purchase was closed early; one
   that ran out has a membership that ran out with it. A priority entry is removed only when the
   store put it there and no other active purchase pays for it, so a name staff added by hand
   stays.

A player holds an offer at most once at a time, and at most one supporter tier.

## Discord

Route `DONATION_PERKS`, channel `💎・donations-perks` in the Champion layout. The channel is made only
for a server whose store is open: opening the store creates it, and a layout run for a closed
store skips it (the route reports `DISABLED`). Each purchase posts a
shout-out with the perks and the month's top three supporters. The owner can switch shout-outs off.

## Access

- Plan: `perk_store` (not on Survivor when plan gating is enforced).
- Client Hub: `PERKS_VIEW` (Administrator) sees offers and orders and marks custom perks done;
  `PERKS_MANAGE` (Owner) changes offers and settings, refunds and ends purchases.
- Player Hub: any signed-in user can see the store; buying and gifting need a verified player link.

## API

Client Hub, under `/api/saas/organizations/{org}/installations/{inst}/admin`:

```
GET    /perks                                   PERKS_VIEW
PUT    /perks/settings                          PERKS_MANAGE   {"enabled":bool,"shoutouts":bool}
PUT    /perks/offers                            PERKS_MANAGE   an offer; id 0 creates
DELETE /perks/offers/{offerID}                  PERKS_MANAGE   takes it off sale for good
POST   /perks/purchases/{purchaseID}/refund     PERKS_MANAGE
POST   /perks/purchases/{purchaseID}/end        PERKS_MANAGE
POST   /perks/purchases/{purchaseID}/custom-done  PERKS_VIEW
POST   /perks/purchases/{purchaseID}/retry      PERKS_VIEW     hand out / take back perks again
```

Player Hub, under `/api/saas/organizations/{org}/installations/{inst}/perks`:

```
GET    ""                                       the store, my perks, my balance, the board
GET    /recipients?q=                           players to gift to
POST   /purchases                               {"offerId":1,"recipientPlayerId":0,"idempotencyKey":"..."}
POST   /purchases/{purchaseID}/auto-renew       {"enabled":bool}   the buyer only
```

## Not verified live

The Nitrado priority calls have not been exercised against a live service. The store reads the
list before every write and never writes when the read fails.
