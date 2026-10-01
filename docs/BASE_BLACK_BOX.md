# Base Black Box

The Base Black Box keeps a history, for each registered base, of players who
came near it or took parts off it. Base owners look back on it on the website
(Security Store page). It is **off** until the server owner turns it on, and the
owner can sell it to players for Champion Points through the Security
Marketplace (`BASE_BLACK_BOX`), like the Base Raid Alarm and Perimeter Watch.

It only records what the server log already reports. It never messages anyone
and never acts on a player: no ban, kick or score.

## What it records

- **Visits:** a logged player position within the base circle **plus 100 m**.
  Sightings of the same player within 10 minutes are one entry, with how many
  times they were seen and the closest distance.
- **Dismantles:** an ADM "dismantled" build line inside the base circle, with
  the part. Repeated dismantles by the same player within 10 minutes are one
  entry.
- The base owner, the owner's faction and anyone on the base's friend list are
  never recorded.
- While the owner sells it, only bases whose owner has paid time are recorded,
  and only payers can see their history. With selling off it is free for every
  base. Turning it off stops recording for everyone.
- History is deleted after the owner's retention (3–30 days, default 14).

## What it can't see

Positions only exist when the log writes one, so a quick pass can be missed.
Dismantles need build logging (`adminLogBuildActions`). Explosive damage isn't
logged.

## API

Server owner only (`UAV_MANAGE`):

| Method | Path | Body | Notes |
| --- | --- | --- | --- |
| GET | `…/admin/case/black-box` | | settings, 20 latest entries, offer, recent sales, active subscribers |
| PUT | `…/admin/case/black-box` | `{enabled, retentionDays?}` | Omitted retention keeps the saved value. Audited `BASE_BLACK_BOX_SAVED`. |
| PUT | `…/admin/case/black-box/offer` | `{enabled, pricePoints, durationDays}` | Same rules as the other offers. |

Player (verified DayZ link):

| Method | Path | Notes |
| --- | --- | --- |
| GET | `…/security-marketplace/black-box` | `{available, reason?, retentionDays, activeUntil?, bases, events}`. `reason` is `OFF` or `NOT_PAID`. Only the signed-in player's own bases. |

## Code

- `internal/database/base_black_box_schema.go` (migration `0088_base_black_box`)
- `internal/repository/base_black_box_repository.go`
- `internal/app/base_black_box_recorder.go` (recorder, location fan-out, hourly clean-up)
- `internal/app/saas_api_base_black_box.go`
