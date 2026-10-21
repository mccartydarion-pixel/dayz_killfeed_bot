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
| PUT | `…/admin/case/base-rent/rent-free` | `{baseId, free, note?}` | Make one player-requested base rent-free, or charge it again (its clock restarts). 409 for bases not registered from a request. Audited `BASE_RENT_FREE_SAVED`. GET lists them as `rentFree`. |
| POST | `…/admin/case/base-rent/gift` | `{baseId, days, note?, idempotencyKey}` | Free rent days (1-90) for one rented base; stacks like a payment, no Champion Points move. 201 new, 200 replay, 409 when the base doesn't pay rent. Audited `BASE_RENT_GIFTED`; the base owner gets a DM. |

Player (verified DayZ link):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/security-marketplace/base-rent` | | `{enabled, pricePoints, periodDays, graceDays, bases, history}`: their rented bases (and faction mates', `faction: true`) with `dueAt`, `graceUntil`, `overdue`, `paused`; `history` is the newest 20 payments and gifts on those bases with who paid (gifts show no staff name). |
| POST | `…/security-marketplace/base-rent` | `{baseId, idempotencyKey}` | 201 new, 200 replay. 409 when rent is off, the base isn't theirs or doesn't pay rent, or they don't have enough points. |

## Code

- `internal/database/base_rent_schema.go` (migration `0095_base_rent`; the
  `base_rent_due_at` / `base_rent_paused` SQL functions)
- `internal/repository/base_rent_repository.go`
- `internal/app/saas_api_base_rent.go` (API and reminder worker)
- The three match queries (raid alarm, Perimeter Watch, Black Box) skip paused
  bases; Faction Security only shares their alerts, so it stops too.

## Paying from Discord

`/mybase` shows a **Pay rent** button per rented base. It asks first (price
and days), and charges only on **Confirm and pay**, through the same payment
as the Security Store. The confirm prompt's message ID is the idempotency
key, so clicking Confirm twice charges once.

## Gifted rent

The owner can give a rented base 1-90 free rent days (a giveaway, a new
player, downtime). It's a payment row with price 0, no ledger entry and the
owner recorded (migration `0096_base_rent_gifts`, enforced by a constraint).
It stacks with paid time, counts for the base's owner, shows in the owner's
payment list marked as a gift and is not counted as rent income. Idempotency
keys starting with `gift-` are reserved for gifts.

## Faction pays rent

Any active member of the base owner's faction can pay rent on that base, from
their own Champion Points, on the Security Store page or with `/mybase` (the
base is shown as their faction mate's). The payment counts for the base like
the owner's own; the payer is recorded on the payment. The base owner gets a
DM saying who paid. Former members and other players can't pay.

## Daily staff notice for paused bases

At most once a day per server, when bases were paused for unpaid rent since
the last notice, a `BASE_RENT_PAUSED` staff notice goes to the server's
ADMIN_ALERTS route (when one is set) listing up to 10 of them with their
owners. It is information only: nothing else happens to the players. The
last send time is kept in `base_rent_digests` (migration
`0097_base_rent_digests`); the check runs with the reminders every 10
minutes.

## Rent-free bases

The owner can make a single player-requested base rent-free (a staff or
event base) without switching rent off for everyone. It pays nothing, is
never paused and gets no reminders. When it's charged again, its clock
restarts from that moment, so it isn't overdue straight away (migration
`0098_base_rent_exemptions`).

The due date is the latest of: the last paid day, when the base was
registered, when rent was switched on, and when rent resumed for that base.
So switching rent off and back on also never leaves a base overdue straight
away, even with old payments.
