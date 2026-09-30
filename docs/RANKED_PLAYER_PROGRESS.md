# Player Hub Ranked progress

`GET /api/saas/player/servers/{installationID}/ranked` returns the acting user's local Ranked progress. It uses the same service authentication, verified DayZ link, and observed activity gate as `/stats`. The client never supplies a player ID or gamertag. Unobserved installations return the same 404 as unknown installations.

No active season:

```json
{"installationId":42,"status":"NOT_STARTED"}
```

Active local season:

```json
{"installationId":42,"status":"ACTIVE","progress":{"seasonId":7,"serverId":9,"startsAt":"2026-09-29T13:00:00Z","rp":100,"tier":"ROOKIE","nextTier":"BRONZE","remainingRp":200,"tierStartRp":100,"nextTierRp":300,"serverPosition":1}}
```

The progress bar uses `(rp - tierStartRp) / (nextTierRp - tierStartRp)` clamped to 0–100%. At Master, `nextTierRp` is null, `nextTier` is omitted, and `remainingRp` is zero. A player with zero RP has no `serverPosition` yet. The client must label these as **server ranks**, never global Elite ranks.

Badge mapping for the supplied seven-emblem reference, left to right: Rookie (dark metal), Bronze, Silver, Gold, Platinum (ice blue), Diamond (royal blue), Master (red and gold). Unranked uses no tier emblem. This API returns tier identifiers; website asset integration is a separate change. The image is a visual reference and is not stored in this backend repository.

Global progress and Elite Top 250 remain unavailable until platform-qualified cross-server identity and event deduplication are proven.