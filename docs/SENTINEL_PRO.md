# Sentinel Pro

Sentinel Pro is one Security Marketplace purchase (`SENTINEL_PRO`) that covers
every base service:

- Base Raid Alarm
- Perimeter Watch
- Base Black Box
- Faction Security

The server owner sets its price and length like any other offer
(`PUT …/admin/case/sentinel-pro/offer`). Paid time for it counts as paid time
for each covered service; it never renews or charges by itself.

## Rules

- **No separate switch.** It can be bought while the owner sells it and at
  least one covered service is switched on. The store lists the services it
  currently includes. A buyer gets whichever covered services are on; turning
  a service off stops it for bundle buyers too.
- **Selling only the bundle keeps the services paid.** While a Sentinel Pro
  offer is on, each covered service counts as "on sale", so only players with
  paid time (for that service or the bundle) get it — selling the bundle alone
  never makes the services free.
- **Paid time.** A player's paid-until for a service is the latest end of
  their own purchases of that service or of the bundle.
- **Expiry DM.** When a single-service purchase ends while the bundle still
  covers it, no "ended" DM is sent. When the bundle ends, one DM says so.

## API

Server owner only (`UAV_MANAGE`):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/sentinel-pro` | | `{offer, sales, activeSubscribers, includes:[{id,on}]}` |
| PUT | `…/admin/case/sentinel-pro/offer` | `{enabled, pricePoints, durationDays}` | Audited `SENTINEL_PRO_OFFER_SAVED`. |

The player store and purchase use the existing Security Marketplace routes;
the catalog entry carries `includes` while it is on sale.

Migration `0092_sentinel_pro` only widens the service checks.
