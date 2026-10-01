# Hot zones

When the PvP heatmap shows a fight already happening, a short event opens there by itself: kills
inside the circle score, and the top three earn Champion Points. Staff configure it once; nobody
has to run it.

Off by default. Migration `0080_installation_feature_settings`. Code: `internal/hotzone` (the
decision), `internal/events` (the `HOT_ZONE` event type), `internal/app/hot_zones.go`.

## How one opens

Every two minutes, for each installation of the guild with hot zones enabled:

1. Kills on its server in the last `windowMinutes` are aggregated on the 500 m heatmap grid (the
   same query the heatmap uses, so a kill with no logged position is not counted).
2. The busiest cell qualifies if it has at least `minKills` kills. Ties go to the lowest cell
   coordinates, so the same data always opens the same zone.
3. A `HOT_ZONE` event is created `ACTIVE`, centred on that cell, with radius `radiusMeters`,
   ending `durationMinutes` later, carrying the configured podium points.

Step 3 is one `INSERT ... WHERE NOT EXISTS`: a server never has two hot zones open, and none opens
within `cooldownMinutes` of the last one ending.

Kills are counted per grid cell, not per neighbourhood: ten kills split five and five across a cell
boundary do not open a zone at a threshold of six.

## Scoring, ending, paying

Everything after the opening is the existing competitive-event machinery. A persisted kill scores
one point when it happened on the zone's server and the **victim's** logged position is inside the
circle (the killer's stands in only when the victim's was not logged) - so a long shot into the
zone from outside counts, and shooting out of it does not. The qualifying kill's card gets the
event badge. The event scheduler ends the zone, pays first/second/third through the economy ledger
and posts the completion card.

The opening is announced where completion cards go (the `SERVER_STATUS` route, else the legacy
status channel): where, how big, when it ends, what it pays.

## Settings

`GET .../admin/features` (`FEATURE_SETTINGS_VIEW`, Moderator) returns every opt-in feature's settings.
`PUT .../admin/features/hot-zones` (`FEATURE_SETTINGS_MANAGE`, Administrator):

| Field | Default | Range |
| --- | --- | --- |
| `enabled` | false | |
| `windowMinutes` | 60 | 15-360 |
| `minKills` | 6 | 2-500 |
| `radiusMeters` | 500 | 100-2000 |
| `durationMinutes` | 30 | 5-240 |
| `cooldownMinutes` | 120 | 0-1440 |
| `firstPoints` / `secondPoints` / `thirdPoints` | 500 / 250 / 100 | 0-1,000,000 |

Changes are written to the admin audit log (`HOT_ZONE_SETTINGS_UPDATED`).

## Reading them

`GET .../admin/hot-zones` (`FEATURE_SETTINGS_VIEW`) - the server's last 25 hot zones with their top three.

`GET /api/saas/player/servers/{installationId}/hot-zone` (player authorization) - the open zone, or
`{"hotZone": null}`:

```json
{"hotZone": {"id": 31, "name": "Hot Zone 7750 / 12750", "status": "ACTIVE", "centerX": 7750, "centerZ": 12750,
             "radiusMeters": 500, "killsObserved": 6, "startsAt": "…", "endsAt": "…",
             "standings": [{"rank": 1, "playerName": "Hunter", "kills": 2}]}}
```

Coordinates are map metres (east, north), the same axes as the heatmap.

## A note on what this reveals

A hot zone announces roughly where people are fighting. That is the point, and it is why the
feature is opt-in: a server that treats locations as secret should leave it off.
