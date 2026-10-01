# Perimeter Watch

Perimeter Watch sends a base owner a Discord DM when another player comes
near their base. It is **off** until the server owner turns it on. The owner
can also sell it to players for Champion Points through the Security
Marketplace (`PERIMETER_MONITORING`), the same way as the Base Raid Alarm.

## How it decides

1. The ADM log reports player positions (position lines and events that carry
   one). Each position goes through the same location queue as the killfeed.
2. If the position is within a registered base's radius **plus a margin**
   (default 100 m, owner can pick 25–300 m), the player is near that base.
3. It is **not** an alert when the player is the base owner, in the owner's
   faction, or on the base's friend list (active player or faction grant).
4. While a `PERIMETER_MONITORING` offer is on sale, only owners with paid time
   get alerts. With selling off, it is free for every base. Turning the
   feature switch off stops it for everyone, payers included.
5. One alert per visitor per base per cooldown (default 30 minutes, 5–120
   minutes), and at most one alert per base every 2 minutes.

Every alert is logged in `perimeter_watch_alerts` with how it was delivered:
`SENT`, `OWNER_NOT_LINKED` or `FAILED`.

## What it can't see

Positions only exist when the log writes one, so a player who passes quickly
between position lines can be missed. It never acts on the visitor: no ban,
kick or score — it only tells the owner.

## API (server owner only, `UAV_MANAGE`)

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/perimeter-watch` | | settings, 10 latest alerts, offer, recent sales, active subscribers |
| PUT | `…/admin/case/perimeter-watch` | `{enabled, marginMeters?, cooldownSeconds?}` | Omitted values keep the saved ones. Audited as `PERIMETER_WATCH_SAVED`. |
| PUT | `…/admin/case/perimeter-watch/offer` | `{enabled, pricePoints, durationDays}` | Same rules as the raid alarm offer. |

## Code

- `internal/database/perimeter_watch_schema.go` (migration `0085_perimeter_watch`)
- `internal/repository/perimeter_watch_repository.go`
- `internal/discord/perimeter_watch.go`
- `internal/app/saas_api_perimeter_watch.go`
- `internal/killfeed/location_queue.go` (`LocationObserver`)
