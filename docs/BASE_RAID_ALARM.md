# Base Raid Alarm

The Base Raid Alarm sends a base owner a Discord DM when another player starts
taking their base apart. It is **off** until the server owner turns it on, and
nothing is sold: the Security Marketplace entry `BASE_RAID_ALARM` stays
"Coming soon" and no Champion Points move.

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
