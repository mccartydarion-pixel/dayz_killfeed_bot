# Progression: challenges, battle pass, territory

Three systems that give players a reason to log in every day and fight over the map. Staff set them
up per server in **Client Hub → Growth → Progression**; players follow them in the **Player Hub**.
All three are **off** until an owner switches them on. Rewards are Champion Points (the guild-wide
balance, ledger type `SYSTEM_REWARD`, so they also count as earned points on the leaderboards).

The pure rules live in `internal/progression` (challenge catalog and draw, weapon classes, territory
zones and the capture rule, battle pass levels and the default track). Storage is migration
`0120_progression`. The competitive scheduler runs `runProgression` every 45 seconds: territory
first (challenges count its kills), then challenges (the battle pass counts their XP), then the
battle pass. Each pass runs at most once a minute (territory) or every two minutes per server.

## Daily and weekly challenges

- Each server's UTC day and Monday-start UTC week get a set of challenges drawn once and stored
  (`challenge_sets`), so the draw never changes during a period. The draw is seeded by server and
  date: every server gets its own, and the same day always draws the same.
- Kinds: kills, headshot kills, long-range kills (200 m daily, 300 m weekly), kills with a weapon
  class (sniper, assault rifle, SMG, shotgun, pistol; at most one weapon challenge a period), minutes
  played, minutes survived in one life, kills inside a hot zone (only when hot zones are on) and
  kills inside a territory (only when territory is on).
- Progress is measured from what the bot already records: `kills` (PvP only, and the same victim
  counts at most once an hour for the same killer), `player_daily_activity`, `player_lives` plus the
  life in progress, `event_kills` of hot zone events and `territory_kills`. Late kills still count:
  each period is checked for 30 minutes after it ends.
- Settings (`GET/PUT .../admin/challenges`): on/off, 1-5 daily and 0-4 weekly challenges, points
  for each (0-1,000,000; defaults 50 and 250), and a Discord card with the day's and week's set.
- A completion is a row (`challenge_completions`); the points are credited after, with the
  reference `challenge:<set>:<index>`, and `paid_at` is stamped only once the credit went through,
  so a failed payment is retried and never doubled. A running battle pass season adds its daily or
  weekly challenge XP at the same time.
- Player route: `GET /api/saas/player/servers/{id}/challenges` (today's and this week's set with the
  player's progress, the points and the XP each pays).

## Battle pass

- One open season per server (`battle_pass_seasons`): name, start and end (at most a year), 5-100
  levels, 100-100,000 XP a level, a premium price (0 = no premium track), the XP sources and the
  reward track (a JSON list stored on the season).
- XP (`battle_pass_xp`, one row per piece, so nothing counts twice): a PvP kill (default 100 XP; the
  same victim once an hour; at most 20 kills a UTC day by default), each full hour played on a UTC
  day (default 150 XP, at most 6 hours a day) and each daily (300) or weekly (1,000) challenge.
  Kills and playtime are read back 48 hours on every pass.
- Rewards: on each level, one reward on the free track and one on the premium track - Champion
  Points, a title or an emoji badge. When a season is created without a track, the default track
  (`progression.DefaultRewards`) is used. Rewards are granted as soon as a level is reached
  (`battle_pass_grants`); points are credited with the reference `bp:<season>:<level>:<track>`.
- Premium: `POST /api/saas/player/servers/{id}/battle-pass/premium`. The price is debited from the
  player (`PASS_PURCHASE`) and credited to the owner's linked character (`PASS_SALE`), the way the
  perk store pays. Premium rewards of levels already reached are granted at once. One purchase per
  player and season; buying is open until the season ends.
- Titles and badges (`player_cosmetics`) stay with the player after the season. The player picks
  which to show with `PUT /api/saas/player/servers/{id}/cosmetics` (only ones they own); the battle
  pass leaderboard shows them.
- Discord: a card when a season starts and one with the top three when it ends.
- Staff routes: `GET/POST .../admin/battle-pass`, `PUT .../admin/battle-pass/{seasonID}` (a season
  that has started keeps its start; granted rewards stay granted), `POST .../{seasonID}/end`.
  Player route: `GET /api/saas/player/servers/{id}/battle-pass`.

## Territory

- Zones are the map's named places worth fighting over (cities, airfields, military bases and a few
  big towns): 20 on Chernarus, 12 on Livonia, from the website's place labels. An unset map uses
  Chernarus, as the live map does.
- Each pass places the last three hours of PvP kills in zones (the killer's position, else the
  victim's; the nearest centre where circles overlap) with both players' hub factions at that
  moment (`territory_kills`; a member counts only while their DayZ link is verified).
- Points: kills by a faction's members on players outside their faction, inside the zone, over the
  rolling window (default 7 days); the same killer and victim count once an hour.
- Capture rule (`progression.DecideHolder`): a challenger takes a zone with at least the minimum
  kills (default 3) and strictly more points than the holder; a tie never changes hands, and a tie
  between two challengers takes nothing. A holder with no points in the whole window loses the zone.
  Holds are rows in `territory_holds`; each change can post a Discord card.
- Income: each held zone pays its holder once a UTC day (default 100 points), split evenly between
  the faction's verified members who played on the server in the last 7 days. The reference is
  `territory:<server>:<day>:<zone>`; `territory_payouts` records each zone paid.
- Routes: `GET/PUT .../admin/territory` (settings, zones with holders and standings, recent
  captures), `GET /api/saas/player/servers/{id}/territory` (zones, holders, the race and the
  player's faction's points). The public live map adds `territory` (zones and holders, never the
  scores) when territory is on.
