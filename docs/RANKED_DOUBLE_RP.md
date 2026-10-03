# Double RP

Staff can open a **double RP** window on a server: every ranked kill that happens inside it earns
twice the active season's RP per kill. The cooldown on repeat kills of the same player still
applies, so a doubled kill that would have been a cooldown still earns nothing.

## Rules

- A window lasts 1 to 72 hours. It starts now, or at a set time up to 14 days ahead.
- Two windows on one server may not overlap.
- It needs an active ranked season: double RP doubles ranked kills, so without a season it does
  nothing.
- Staff can stop a live window early, or cancel a scheduled one.
- **What counts is when the kill happened**, not when the bot processed it. Nitrado shows the
  game's logs in steps of minutes (`docs/NITRADO_POLLING.md`), so a kill near the end of a window
  can reach the bot after the window closed and is still doubled.
- Each award records the multiplier it was earned under (`ranked_awards.multiplier`, 1 = none).

## Announcements

Posted to the server's `EVENTS` channel, or its `SERVER_RANKS` channel when no events channel is
routed:

| Card | When |
| --- | --- |
| "2× RP is coming" | When a window is scheduled more than a minute ahead |
| "2× RP IS LIVE" | When it starts |
| "2× RP has ended", with the three players who earned the most RP in it | 10 minutes after it ends, so kills that reach the bot late are counted |

A window cancelled before it starts posts nothing more. A card that cannot be posted (no channel)
is not retried later.

The Player Hub shows a live or upcoming window beside the player's rank (`boost` on
`GET /api/saas/player/servers/{installationID}/ranked`).

## Access

- Plan feature `ranked_seasons`.
- `EVENTS_VIEW` (Moderator) reads; `EVENTS_MANAGE` (Administrator) starts and stops.

```
GET  .../admin/ranked/boosts                   the season, the windows, limits
POST .../admin/ranked/boosts                   {"hours":2,"startsAt":null}
POST .../admin/ranked/boosts/{boostID}/stop
```
