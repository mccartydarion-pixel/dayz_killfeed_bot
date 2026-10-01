# Base rent

The server owner can charge rent, in Champion Points, for bases registered
from **players' requests**. Bases the owner registers by hand stay free.

## How it works

- **One price for the server:** the owner sets the rent (1–1,000,000,000
  points) and the period (1–30 days) on the Bases tab and switches rent on.
- **Players pay ahead.** In the Security Store a player pays one period at a
  time for each of their rented bases; it's a `BASE_RENT` debit on the existing
  ledger (shown as "Base rent" in their wallet). **Nothing is ever taken
  automatically.** Paying again adds the next period on the end.
- **Grace, then paused.** When paid time runs out there's a 3-day grace
  period. After that the base is **paused**: its Base Raid Alarm, Perimeter
  Watch, Base Black Box recording and Faction Security sharing stop until rent
  is paid. The base itself is kept. Paying resumes it immediately (the new
  period starts when paid; the paused days aren't charged).
- **When rent starts:** for a new base, when it's approved; for bases that
  existed already, when the owner switches rent on. Switching rent off stops
  it for everyone; switching it back on restarts every base's clock, so
  nobody is paused on the spot.
- **Reminders:** one DM a day before rent is due and one when a base is
  paused (each sent once per due date). The approval DM for a new base
  explains the rent terms. `/mybase` shows each base's rent status.

## API

Server owner only (`UAV_MANAGE`):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/base-rent` | | `{settings, bases, payments, graceDays}`: every rented base (soonest due first, with `paused`) and the newest payments. |
| PUT | `…/admin/case/base-rent` | `{enabled, pricePoints, periodDays}` | Audited `BASE_RENT_SAVED`. |

Player (verified DayZ link):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/security-marketplace/base-rent` | | `{enabled, pricePoints, periodDays, graceDays, bases}` (their rented bases with `dueAt`, `graceUntil`, `overdue`, `paused`). |
| POST | `…/security-marketplace/base-rent` | `{baseId, idempotencyKey}` | 201 new, 200 replay. 409 when rent is off, the base isn't theirs or doesn't pay rent, or they don't have enough points. |

## Code

- `internal/database/base_rent_schema.go` (migration `0095_base_rent`; the
  `base_rent_due_at` / `base_rent_paused` SQL functions)
- `internal/repository/base_rent_repository.go`
- `internal/app/saas_api_base_rent.go` (API and reminder worker)
- The three match queries (raid alarm, Perimeter Watch, Black Box) skip paused
  bases; Faction Security only shares their alerts, so it stops too.
