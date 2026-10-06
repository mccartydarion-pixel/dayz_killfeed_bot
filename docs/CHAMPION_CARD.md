# Champion Card

A 1200x630 PNG of one player's stats on one server - the Open Graph size, so a shared link unfurls
as the card itself - and the same card animated, as a GIF of it filling in (see "Animated card").

Migration `0079_player_card_shares`. Code: `internal/playercard` (rendering: `playercard.go` the
card data, tiles and layout, `animation.go` the sequence and the GIF, `quantize.go` the GIF palette
and dither, `draw.go` the canvas helpers, `assets.go` the embedded assets),
`internal/repository/card_repository.go`, `internal/app/player_card.go`,
`internal/discord/card_command.go`. Dependency: `golang.org/x/image` (OpenType rendering, the
`vector` rasterizer for anti-aliased shapes, `draw` for scaling the emblem); the GIF is written
with the standard library's `image/gif`.

## What it shows

Player name, server name, Faction Hub faction and tag, active stats season, the player's Ranked
Points standing (docs/RANKED_SERVER_SEASONS.md, docs/RANKED_PLAYER_PROGRESS.md), and eight tiles:
kills, deaths, K/D, headshots, longest kill, playtime, longest life, server rank by kills. The
deaths tile is every death and the K/D tile is the overall K/D; under each figure a small caption
gives the split (`PVP 160 / PVE 51`, `PVP 8.03`): deaths caused by another player, every other
death, and kills per PvP death (docs/DEATH_COUNTS.md). The server rank tile's caption says what it
counts (`of 123 · by kills`), since the position by RP is on the rank row. In the JSON the caption
is the tile's `note`, and the numbers are `pvpDeaths`, `pveDeaths` and `pvpKd`.

Every figure is scoped to the installation's own server and counted exactly as the player stats API
counts it (docs/PLAYER_API.md). A value that was never measured (no recorded life, no kills to rank)
renders as a dash. Long names shrink, then truncate.

### Layout

- A gold accent bar down the left edge; the `CHAMPIONS KILLFEED` eyebrow top left and the server
  name top right (cut with an ellipsis when long), over a hairline.
- The identity band: the Ranked tier emblem (180 px) with a soft glow in the tier's own colour
  (none for Unranked), the player name (80 px, shrinking to 48 px for long names), then the
  faction and season (`[NEX] Nex · Season 1`, whichever exist).
- The rank row, when the server has an active Ranked season: a pill with the tier name in the
  tier's colours, the RP (`1,240 RP`), the position on the server (`#5 in Ranked`, omitted until
  the player has RP), and on the right a progress bar through the current tier with its caption
  (`260 RP to Platinum`; at Master a full gold bar and `Top tier`). A player with no RP yet shows
  `UNRANKED`, `0 RP` and an empty bar. Without a season the row reads `Ranked season not started`
  under the Unranked emblem, with no bar.
- The stat grid, four tiles by two, in this order: KILLS, DEATHS (`PVP n / PVE n`), K/D
  (`PVP x.xx`), HEADSHOTS, LONGEST KILL, PLAYTIME, LONGEST LIFE, SERVER RANK (`of 123 · by kills`).
  Kills and the server rank are in gold.
- The footer: `EVERY KILL TELLS A STORY` and the website's host (`CHAMPION_SITE_BASE_URL`), or
  the date the card was generated when no site address is configured.

### Fonts and emblems

The card is set in the website's own faces, embedded in the binary from
`internal/playercard/assets/fonts`: Sora 800 (the name and every figure) and Manrope 700/500
(labels and captions), as static Latin-subset instances of the variable fonts, SIL Open Font
License 1.1 (`assets/fonts/LICENSE.txt`). A string that needs a glyph the subset lacks (a Cyrillic
gamertag) is drawn whole with the Go font of the matching weight (Go Bold or Go Regular), so it
still reads as a name; a character no face has renders as `?`. The tier emblems are the website's
`/ranks/<tier>.png` at 256 px (`assets/ranks`), scaled with a Catmull-Rom kernel. The assets add
about 670 KB to the binary (about 750 KB with the rasterizer and scaler code).

Rendering is pure: the same card data always produces the same image.

## Animated card

`playercard.RenderAnimation` is the card filling in: 1200x630, 25 fps, 2.6 s of motion (65
frames) and then the finished card held for 4 s, looping forever. The still and the animation
share one layout: every element has a window in which it eases (cubic ease-out) from its start
state to its place, and the still is the frame where every window has passed - the last frame of
the GIF, before its colours are reduced, is `Render`'s image pixel for pixel.

The sequence, in seconds: the background and the gold bar are there from the start; the header
fades in (0.00-0.30); the emblem lands, shrinking from 1.25x and fading in, while its glow
overshoots to 0.55 (0.10-0.60), settles to its resting 0.30 (0.60-1.10) and breathes once before
the end (1.90-2.50); the name and sub line fade in rising 18 px (0.20-0.60); the rank row, the bar
and its caption fade in rising 12 px (0.45-0.80); the tiles fade in rising 14 px, one every 70 ms
from 0.40; the footer fades in (0.60-0.90). Every figure counts up over 0.60-1.80: the tiles from
zero (ratios included), the RP from the tier's start with the bar filling and the caption's
remaining RP counting down (at Master the bar simply fills), the server rank down from the field
(`#123` to `#3`); a dash stays a dash and the position (`#5 in Ranked`) does not count. Each step
is formatted by the still's own functions. A soft light sweep crosses the card from left to right
(2.00-2.60), over everything but the gold bar.

The GIF: one global palette per animation (a median cut over three sample frames: the still, the
emblem's landing and the middle of the sweep), 255 colours and a transparent index; pixels map to
it through a 64x64x64 lookup table with an 8x8 Bayer ordered dither, so a region that does not
change between frames maps to the same bytes; every frame after the first carries only the
rectangle that changed, with the unchanged pixels inside it transparent (disposal "none"), and a
frame that changes nothing lengthens the one before instead. Frames are 40 ms, the hold 4000 ms,
loop count 0. The Gold sample is about 1.75 MB and renders in about a second on two cores
(frames render in parallel; the result does not depend on the order).

## Routes

Player routes use the player API's authorization chain (docs/PLAYER_API.md).

| Route | Purpose |
| --- | --- |
| `GET /api/saas/player/servers/{installationId}/card` | the acting player's card as JSON, plus their active share |
| `GET /api/saas/player/servers/{installationId}/card.png` | the same card as a PNG (`Cache-Control: private, no-store`) |
| `GET /api/saas/player/servers/{installationId}/card.gif` | the same card animated, as a GIF (`Cache-Control: private, no-store`) |
| `POST /api/saas/player/servers/{installationId}/card/share` | create, or return, the player's share link |
| `DELETE /api/saas/player/servers/{installationId}/card/share` | revoke it |
| `GET /api/saas/cards/{token}` | the data behind a share link, for the website's share page (service secret, no acting user) |
| `GET /cards/{token}.png` | **public, unauthenticated** card image (PNG only: link previews need a still) |

Card JSON:

```json
{
  "playerName": "Ace", "serverName": "Chernarus PvP", "factionName": null, "factionTag": null, "seasonName": "Season 3",
  "kills": 3, "deaths": 1, "kd": 3, "pvpDeaths": 1, "pveDeaths": 0, "pvpKd": 3, "headshots": 1, "longestKillMeters": 80, "playtimeSeconds": 3600,
  "longestLifeSeconds": null, "rank": 2, "rankedPlayers": 2,
  "ranked": {"status": "ACTIVE", "tier": "GOLD", "rp": 1240, "position": 5, "nextTier": "PLATINUM", "remainingRp": 260, "tierStartRp": 1000, "nextTierRp": 1500},
  "tiles": [{"label": "KILLS", "value": "3"}, {"label": "DEATHS", "value": "1", "note": "PVP 1 / PVE 0"}, {"label": "SERVER RANK", "value": "#2", "note": "of 2 · by kills"}],
  "image": {"width": 1200, "height": 630},
  "share": {"token": "…", "imageUrl": "https://<PUBLIC_BASE_URL>/cards/<token>.png", "createdAt": "…"}
}
```

`ranked` is `{"status": "NOT_STARTED"}` while the server has no active Ranked season. While one is
active, `position` is null until the player has RP, and the next-tier fields (`nextTier`,
`remainingRp`, `nextTierRp`) are absent at Master. A failure to read the standing is logged and the
card comes without it; the card never fails over its rank row.

`tiles` is exactly what the image shows, in order, so a page that renders its own card never
disagrees with the picture.

## Sharing

A share is the player's own decision and theirs to undo. The token is 128 random bits and is the
only address of the public image; a player has at most one active share per installation, so
sharing twice returns the same link. Revoking takes effect immediately (the cached render is
dropped) and a later share is a new link.

The public image is rendered at most once per five minutes per token (in-memory, 512 entries,
oldest evicted) and served with `Cache-Control: public, max-age=300`. Unknown tokens are a plain
404 and are never cached.

For the website's share page: fetch `GET /api/saas/cards/{token}` server-side and set
`og:image` to `share.imageUrl`.

## Discord

`/card` posts the caller's own card in the channel, animated (`champion-card.gif`). The reply is
deferred first (the GIF takes a few seconds to render and upload); if the animation cannot be
rendered the still is posted instead (`champion-card.png`, logged as `card_gif_failed`). There is
no player argument: nobody can post someone else's stats for them. One card per user per 30
seconds.
