# Ranked bonuses

Optional extras on top of a server's ranked season, each switched on separately by staff in
**Client Hub → Growth → Events → Ranked bonuses** (EVENTS_MANAGE to change, EVENTS_VIEW to see;
plan feature `ranked_seasons`). Everything is off until turned on.

## Bonus RP on a kill

A bonus is decided together with the kill's award (`repository.RecordServerKill`, under the
killer's and the pair's locks) and from when the kill happened, so a kill that reaches the bot late
is judged as of its own time. Amounts are a percentage (10-500%) of the season's RP per kill.
Bonuses are added after double RP and are not multiplied by it. A kill that earns nothing
(repeat-kill cooldown, out of order) earns no bonus.

| Bonus | When | Default | Limit |
|---|---|---|---|
| Bounty | The victim was #1 on the server, or on a kill streak of at least N (3-20, default 5) | 100% | The #1 bounty pays once an hour per victim |
| Underdog | The victim's tier was higher than the killer's | 50% | Once an hour per killer and victim |
| Revenge | The victim had killed the killer within the window (5-120 min, default 30), and that was the last kill between them | 25% | Once an hour per killer and victim |
| First kill of the day | The killer's first bonus-earning ranked kill of the UTC day | 50% | Once a day |

A **streak** is the ranked kills a player made since their last death on the server (killed by a
player or by anything else).

Each award stores `bonus_rp` and `bonuses` (`[{kind, rp, detail}]`); `amount` includes the bonus.

## Discord

Cards go where double RP cards go: the server's EVENTS channel, else SERVER_RANKS. The competitive
scheduler (45 s) posts them.

- **Bounty on X**: when a player becomes wanted (once per stretch of being wanted; `ranked_wanted`).
- **Bounty claimed**: after a kill that carried a bounty (awards from the last day not yet stamped).
- **Rank-ups** (cards and/or DMs, separately): when a player reaches Bronze or higher. Rookie is not
  announced. Turning rank-ups on records everyone's current tier first, so nothing old is announced.
  At most 10 per server per pass. DMs go to players with a verified Discord link.
- **Weekly recap**: Monday 12:00 to Tuesday 12:00 UTC, for the week (Monday to Sunday, UTC) that
  ended: ranked kills, top 3 climbers, biggest bounty, longest kill streak, revenge kills. Posted
  once per week (`ranked_weekly_recaps`); a week with no ranked kills posts nothing.

## Player Hub

`GET /api/saas/player/servers/{installationID}/ranked` adds `bonuses` (the ones that are on, with
their RP and rule) and `wanted` (who has a bounty now).

## Admin API

Under `.../installations/{installationID}/admin`:

- `GET /ranked/bonuses`: `{settings, season, wanted}`
- `PUT /ranked/bonuses`: the full settings object; answers with what was saved.
