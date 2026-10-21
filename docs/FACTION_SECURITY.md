# Faction Security

Faction Security sends the Base Raid Alarm and Perimeter Watch messages for a
base to the base owner's faction too, not just the owner. It is **off** until
the server owner turns it on, and the owner can sell it to players for
Champion Points through the Security Marketplace (`FACTION_SECURITY`).

## How it decides

1. A Base Raid Alarm or Perimeter Watch alert fires for a base (the usual
   rules and cooldowns apply; a cooling-down alert is not shared either).
2. The base owner's **active** faction mates with a **verified** Discord link
   get the same message, worded for the faction ("Someone is breaking into
   your faction's base …"). The owner is never sent a second copy.
3. The base owner chooses who: **everyone** in the faction (default) or **only
   leaders** (faction owner, leaders and officers). They change it on the
   Security Store page.
4. At most 15 faction members are messaged per alert.
5. While it is on sale, only base owners with paid time share. With selling
   off it is free for every base. Turning it off stops sharing for everyone.
6. If the base owner hasn't linked Discord, their faction still gets the alert.

Each shared alert is logged in `faction_security_shares` with how many members
were messaged and how many couldn't be (for example, closed DMs).

It never acts on anyone; it only passes on alerts the owner already gets.

## API

Server owner only (`UAV_MANAGE`):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/faction-security` | | settings, 10 latest shares, offer, recent sales, active subscribers |
| PUT | `…/admin/case/faction-security` | `{enabled}` | Audited `FACTION_SECURITY_SAVED`. |
| PUT | `…/admin/case/faction-security/offer` | `{enabled, pricePoints, durationDays}` | Same rules as the other offers. |

Player (verified DayZ link):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/security-marketplace/faction-security` | | `{available, reason?, recipients, activeUntil?, faction:{inFaction, factionName, linkedMembers, linkedLeaders}}`. `reason` is `OFF` or `NOT_PAID`. |
| PUT | `…/security-marketplace/faction-security` | `{recipients: "ALL" \| "LEADERS"}` | Refused (409) while Faction Security is off. |

## Code

- `internal/database/faction_security_schema.go` (migration `0090_faction_security`)
- `internal/repository/faction_security_repository.go`
- `internal/discord/faction_security.go` (sharing, used by the raid alarm and Perimeter Watch publishers)
- `internal/app/saas_api_faction_security.go`
