# Champion Card

A 1200x630 PNG of one player's stats on one server - the Open Graph size, so a shared link unfurls
as the card itself.

Migration `0056_player_card_shares`. Code: `internal/playercard` (rendering),
`internal/repository/card_repository.go`, `internal/app/player_card.go`,
`internal/discord/card_command.go`. New dependency: `golang.org/x/image` (font rendering with the
embedded Go fonts; no font files to ship).

## What it shows

Player name, server name, Faction Hub faction and tag, active season, and eight tiles: kills,
deaths, K/D, headshots, longest kill, playtime, longest life, server rank by kills.

Every figure is scoped to the installation's own server and counted exactly as the player stats API
counts it (docs/PLAYER_API.md). A value that was never measured (no recorded life, no kills to rank)
renders as a dash. Characters the font has no glyph for render as `?`; long names shrink, then truncate.

Rendering is pure: the same card data always produces the same image.

## Routes

Player routes use the player API's authorization chain (docs/PLAYER_API.md).

| Route | Purpose |
| --- | --- |
| `GET /api/saas/player/servers/{installationId}/card` | the acting player's card as JSON, plus their active share |
| `GET /api/saas/player/servers/{installationId}/card.png` | the same card as a PNG (`Cache-Control: private, no-store`) |
| `POST /api/saas/player/servers/{installationId}/card/share` | create, or return, the player's share link |
| `DELETE /api/saas/player/servers/{installationId}/card/share` | revoke it |
| `GET /api/saas/cards/{token}` | the data behind a share link, for the website's share page (service secret, no acting user) |
| `GET /cards/{token}.png` | **public, unauthenticated** card image |

Card JSON:

```json
{
  "playerName": "Ace", "serverName": "Chernarus PvP", "factionName": null, "factionTag": null, "seasonName": "Season 3",
  "kills": 3, "deaths": 1, "kd": 3, "headshots": 1, "longestKillMeters": 80, "playtimeSeconds": 3600,
  "longestLifeSeconds": null, "rank": 2, "rankedPlayers": 2,
  "tiles": [{"label": "KILLS", "value": "3"}],
  "image": {"width": 1200, "height": 630},
  "share": {"token": "…", "imageUrl": "https://<PUBLIC_BASE_URL>/cards/<token>.png", "createdAt": "…"}
}
```

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

`/card` posts the caller's own card in the channel as an image. There is no player argument:
nobody can post someone else's stats for them. One card per user per 30 seconds.
