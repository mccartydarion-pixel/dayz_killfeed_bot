# Gifted paid time

The server owner can give a player days of any base service — Base Raid
Alarm, Perimeter Watch, Base Black Box, Faction Security or the Sentinel Pro
bundle — for free: for giveaways, event prizes or to make up for downtime.

- **No Champion Points move.** A gift is a Security Store purchase with price
  0 and no ledger entry; the owner who gave it and an optional note (up to 200
  characters) are recorded. The database refuses anything in between.
- It **stacks** like a purchase (starts when the player's current paid time
  for that service ends) and lasts 1–90 days. A bundle gift counts for every
  service the bundle covers.
- It works whether or not the service is on sale, but the player only gets
  the service while it's switched on.
- The player gets a Discord DM (if linked). Each gift is audited as
  `SECURITY_GIFT_GIVEN`; replays of the same request give nothing twice.
- Gifts are **not sales**: the sales summary counts them separately
  (`gifts`) and they're marked `gift` in the recent sales list.

## API (server owner only, `UAV_MANAGE`)

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/security-gifts` | | `{gifts}`: the 20 newest. |
| POST | `…/admin/case/security-gifts` | `{playerId, serviceId, days, note?, idempotencyKey}` | 201 new, 200 replay. The player must be on this server. |

Migration `0094_security_gifts` adds the gift columns and constraint.
