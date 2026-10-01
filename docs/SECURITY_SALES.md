# Security Store sales summary

One read-only, owner-only view of the Security Store on a server:

`GET …/admin/case/security-sales?days=7|30|90` (default 30, `UAV_MANAGE`)

For each sellable base service (Base Raid Alarm, Perimeter Watch, Base Black
Box, Faction Security, Sentinel Pro) it returns:

- `sales` and `pointsEarned` in the period, and `buyers` (distinct players);
- `activePlayers`: players with paid time right now;
- `onSale`, the current `pricePoints`/`durationDays`, and `switchedOn`
  (for Sentinel Pro: at least one covered service is on).

Plus `totalSales`, `totalPoints`, `playersWithAny` (players with any paid time
now) and the 20 newest sales across all services.

Champion Points spent here are the existing `SECURITY_PURCHASE` ledger entries;
this view never moves points.
