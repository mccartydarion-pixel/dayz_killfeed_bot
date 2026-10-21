# Base Raid Alarm

The Base Raid Alarm sends a base owner a Discord DM when another player starts
taking their base apart. It is **off** until the server owner turns it on. The
owner can also sell it to players for Champion Points through the Security
Marketplace (`BASE_RAID_ALARM`); see "Selling it to players" below.

## How it decides

1. The server's ADM log writes a line when a player **dismantles** part of a
   structure (the server needs `adminLogBuildActions` on). The line includes
   the player and their position.
2. If that position is inside a base registered on the dashboard's anti-cheat
   **Bases** tab (draft or reviewed, not withdrawn), it may be a raid.
3. It is **not** a raid when the player is:
   - the base owner,
   - in the same faction as the owner, or
   - on the base's friend list (an active player or faction grant).
4. Otherwise the owner gets a DM, if their Discord account is linked and
   verified. Each base sends at most one alarm per cooldown (default 10
   minutes, owner can pick 1–60).

Every alarm is logged in `base_raid_alerts` with how it was delivered:
`SENT`, `OWNER_NOT_LINKED` or `FAILED` (for example, the owner's DMs are closed).

## What it can't see

- Walking up to or standing in a base (use a Base Radar zone for that).
- Explosive or weapon damage: DayZ doesn't write those to the ADM build log.
- Servers that don't log build actions.

## API (server owner only, `UAV_MANAGE`)

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/organizations/{organizationID}/installations/{installationID}/admin/case/raid-alarm` | | `{settings:{enabled,cooldownSeconds,updatedAt}, recent:[…10 latest alarms]}` |
| PUT | `…/organizations/{organizationID}/installations/{installationID}/admin/case/raid-alarm` | `{enabled, cooldownSeconds?}` | Cooldown 60–3600 s; omitted keeps the saved value. Audited as `BASE_RAID_ALARM_SAVED`. |

## Code

- `internal/database/base_raid_alarm_schema.go` (migration `0072_base_raid_alarm`)
- `internal/repository/base_raid_alarm_repository.go`: the match rules, cooldown and log
- `internal/discord/base_raid_alarm.go`: the build-line consumer and DM
- `internal/app/saas_api_base_raid_alarm.go`: the owner switch

## Selling it to players (Security Marketplace)

The owner can sell the alarm for **Champion Points** (the existing wallet, no new currency):

- **Owner:** `PUT …/admin/case/raid-alarm/offer` `{enabled, pricePoints (1–1,000,000,000), durationDays (1–90)}`.
  Owner only, audited `BASE_RAID_ALARM_OFFER_SAVED`. `GET …/admin/case/raid-alarm` also returns
  `offer`, the 10 latest `sales` and `activeSubscribers`.
- **Player:** `POST /api/saas/organizations/{org}/installations/{inst}/security-marketplace/purchases`
  `{serviceId: "BASE_RAID_ALARM", idempotencyKey}`. Requires a verified DayZ link, the alarm switch
  **on** and the offer **on**. One transaction does a `SECURITY_PURCHASE` debit on the point ledger and
  records the paid time. Replaying the same key returns the original purchase and charges nothing.
  Not enough points means nothing is charged. Buying again while active adds the days on the end.
- **Catalog:** `GET …/security-marketplace/catalog` shows the alarm as `AVAILABLE` with `pricePoints`,
  `durationDays` and the player's `activeUntil` while it is on sale. The other sellable services (Perimeter Watch,
  Base Black Box, Faction Security, Sentinel Pro) are listed the same way once their owner switch and offer are on;
  the rest of the catalog stays unavailable.

**Who gets alarms:** while the offer is **on**, only base owners with paid time. With the offer **off**,
the alarm is free for every registered base again (paid players keep it too). Turning the alarm switch
**off** stops it for everyone, including players who paid, so the dashboard warns about that.

**When paid time ends** it just stops, and the player gets one DM ("Your Base Raid Alarm … has ended").
It never renews or charges by itself. Migration `0083_security_service_sales` adds
`security_service_offers` and `security_service_purchases` (additive).
