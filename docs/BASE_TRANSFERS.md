# Base transfers

A base owner can ask to hand one of their registered bases to an active
member of their faction who has a verified Discord link. The server owner
approves or declines on the anti-cheat Bases tab. Nothing changes until it's
approved.

- One waiting transfer per base. The asking player can cancel it.
- The new owner must have room: bases plus waiting base requests stay within
  the per-player limit (3).
- On approval the base's owner changes. Rent already paid stays with the
  base; paid base services (Raid Alarm and the rest) belong to players, so the
  new owner buys their own. Faction sharing, Black Box history and rent
  follow the base.
- Approval re-checks that the base is still the asker's and that they're
  still faction mates; if not, the server owner should decline.
- The server owner gets a staff notice (ADMIN_ALERTS, when routed) and a DM
  for each new request. Both players get a DM with the answer.
- Approve and decline are audited (`BASE_TRANSFER_APPROVED`,
  `BASE_TRANSFER_DECLINED`).

## API

Player (verified DayZ link):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/security-marketplace/base-transfers` | | `{bases, mates, transfers, playerId}`: their bases, faction mates they can hand one to, and transfers they asked for or would receive. |
| POST | `…/security-marketplace/base-transfers` | `{baseId, toPlayerId}` | 201. 409 when it's not their base, the mate isn't eligible or has no room, or a transfer is already waiting. |
| POST | `…/security-marketplace/base-transfers/{id}/cancel` | | Only the asking player, only while waiting. |

Server owner (UAV_MANAGE, owner only):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/base-transfers` | | Waiting first, then the newest answered. |
| POST | `…/admin/case/base-transfers/{id}/approve` | | 409 when already answered, no longer allowed, or the new owner has no room. |
| POST | `…/admin/case/base-transfers/{id}/decline` | `{reason?}` | Reason up to 300 characters, sent to both players. |

## Code

- `internal/database/base_transfer_schema.go` (migration `0099_base_transfers`)
- `internal/repository/base_transfer_repository.go`
- `internal/app/saas_api_base_transfers.go`
