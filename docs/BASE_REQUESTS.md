# Base registration requests

Players can ask for their own base to be registered, so they can use the base
services (Base Raid Alarm, Perimeter Watch, Base Black Box, Faction Security).
Nothing is registered until the server owner approves.

## How it works

1. The player goes to their base in game, then opens the Security Store and
   clicks **Register my base here**, with a name, a size (10–150 m around the
   centre) and an optional note.
2. The centre is the **last position the server log reported for them** — the
   player never types coordinates. It must be from the last 30 minutes.
3. A player has at most one waiting request, and at most 3 bases and requests
   together on a server.
4. When a request arrives, a staff notice is posted to the **ADMIN_ALERTS**
   channel (if it's set up) and the organization owner gets a Discord DM
   with a link to the Bases tab. Nothing is approved automatically.
5. The server owner sees waiting requests on the anti-cheat **Bases** tab, with
   any other player's base the circle overlaps. They can:
   - **Approve**, choosing the map (pre-filled with the map most of the
     server's bases use) and adjusting the name or size. This registers the
     base as a draft, exactly like one the owner adds by hand.
   - **Decline**, with an optional reason.
6. The player gets a Discord DM either way (if their account is linked) and
   can cancel a waiting request.

## API

Player (verified DayZ link):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/security-marketplace/base-requests` | | `{position:{x,z,observedAt,fresh}\|null, bases, requests, maxBases}` |
| POST | `…/security-marketplace/base-requests` | `{name, radius, note?}` | 409 with no position in the last 30 minutes, a waiting request, or at the limit. Coordinates can't be sent. |
| POST | `…/security-marketplace/base-requests/{requestID}/cancel` | | Own waiting request only. |

Server owner only (`UAV_MANAGE`):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/base-requests` | | `{requests, mapKeyHint}`; waiting first, with `overlaps`. |
| POST | `…/admin/case/base-requests/{requestID}/approve` | `{mapKey, name?, radius?}` | Registers the base. Audited `CASE_BASE_REQUEST_APPROVED`. |
| POST | `…/admin/case/base-requests/{requestID}/decline` | `{reason?}` | Audited `CASE_BASE_REQUEST_DECLINED`. |

## Code

- `internal/database/case_base_request_schema.go` (migration `0091_case_base_requests`)
- `internal/repository/case_base_request_repository.go`
- `internal/app/saas_api_case_base_requests.go`
- `internal/discord/base_request.go` (decision DM)
