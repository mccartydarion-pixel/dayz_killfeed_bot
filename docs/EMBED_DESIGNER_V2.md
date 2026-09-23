# Embed Designer V2 — preview and test send (backend)

The website's Embed Designer can preview an **unsaved** draft with Champion's real production
renderer and send it as a clearly-labelled test message to the installation's actual Discord
destination. Code: `internal/app/saas_api_embed_designer.go`.

## One renderer

`embedrender.RenderEvent(cfg, routeKey, vars, at)` is the only render path. It keeps the route's
approved variables, sanitizes every value (`SanitizeValue`) and calls `Render`. It is used by:

| Caller | Where |
|---|---|
| Live events | `embedrender.Renderer.Customize` (every runtime publisher) |
| Preview | `renderEmbedDraft` |
| Test send | `sendEmbedTest` → `renderEmbedDraft` |

There is no preview-only or test-only substitution, sanitization or limit logic. Tests prove the
preview embed, the embed passed to Discord in a test send, and a live `Customize` render are
structurally identical for the same route, template, variables and timestamp.

## Preview

`POST /api/saas/organizations/{organizationID}/installations/{installationID}/embed-templates/{routeKey}/preview`

Request (unknown keys - `channelId`, `guildId`, tokens - are rejected):

```json
{ "template": { "...embedtemplates.Config..." }, "variables": { "killer": "ChampionPlayer", "distance": "127.4m" } }
```

Pipeline:

1. service auth, acting user, organization membership (any member may preview - it only renders);
2. the installation belongs to the organization (a foreign one is 404);
3. valid route key;
4. the draft passes the same `embedtemplates.Validate` a save runs (typed structure, approved
   `{{variables}}` only, no expressions, colors, `http`/`https` URLs without userinfo or control
   characters, per-section and total limits);
5. every sample variable is approved for the route (≤ 32 values, ≤ 512 characters each);
6. `RenderEvent` renders it - sample values are treated exactly like live player data (no
   `@everyone`/`@here`/user/role/channel mentions, control, bidi or zero-width characters, markdown
   structure escaped, 100-character cap);
7. the result is returned as a DTO with metrics and warnings, plus the route's destination
   preflight (when the route has a channel).

Nothing is saved, the renderer cache is not invalidated, nothing is sent, and no gameplay system is
touched.

Response:

```json
{
  "routeKey": "KILLFEED", "runtimeRendering": "ENABLED", "customRenderingSupported": true,
  "renderable": true,
  "embed": { "title": "...", "description": "...", "color": 13872695, "author": { "name": "...", "iconUrl": "..." },
             "thumbnail": { "url": "..." }, "image": { "url": "..." }, "footer": { "text": "...", "iconUrl": "..." },
             "timestamp": "2026-09-23T12:00:00Z", "fields": [ { "name": "Weapon", "value": "M4-A1", "inline": true } ] },
  "metrics": { "fieldCount": 2, "totalText": 843 },
  "warnings": ["1 field(s) are hidden because their values are missing - live events omit them the same way."],
  "destination": { "configured": true, "channelId": "...", "channelName": "🔫・combat-feed", "reachable": true,
                   "canView": true, "canSend": true, "canEmbed": true, "canReadHistory": true }
}
```

- `renderable: false` with `reason: "CUSTOM_TEMPLATE_DISABLED"` for a disabled template, or
  `"TEMPLATE_PRODUCED_NO_CONTENT"` when nothing Discord would show remains; `embed` is then null.
- A field whose value is missing is omitted, exactly as live events omit it.
- Routes without runtime custom rendering still preview, with `customRenderingSupported: false`
  and a warning.
- Errors: `EMBED_TEMPLATE_INVALID` (400) with a customer-safe message.

## Send Test

`POST .../embed-templates/{routeKey}/test` - **OWNER/ADMIN only**. Same request body.

1. Same checks and render as preview, on the **unsaved draft in the request** (the saved
   template is never read).
2. Only routes in `embedrender.SupportedRoutes()` (today KILLFEED, HITFEED, PVE_FEED,
   BOUNTY_TRACKING, CONNECTIONS, ECONOMY); others get `EMBED_CUSTOM_RENDERING_NOT_SUPPORTED`.
3. A disabled or empty draft is never sent (`EMBED_TEMPLATE_NOT_RENDERABLE`).
4. **Destination is resolved server-side** from the installation's own Channel System V2 route
   (`installation_channel_routes`, tenant-scoped). The request cannot name a channel. The channel
   must exist in the installation's own Discord guild.
5. Preflight: the bot needs View Channel, Send Messages and Embed Links there. Read Message
   History is reported but not required to post.
6. One Discord message: a notice line above the untouched embed -

   ```
   🧪 Champion Embed Test • Killfeed
   This is a design preview — not a live server event.
   ```

   with `allowed_mentions` = `{ parse: [], users: [], roles: [], replied_user: false }`, so a hostile
   sample value cannot ping anyone.

**Rollout flag.** Test send is allowed for runtime-supported routes even while
`CHAMPION_CUSTOM_EMBEDS_ENABLED` is off, so a design can be checked before rollout; the response's
`runtimeRendering` says `NOT_ENABLED` in that case. Preview is allowed regardless of the flag.

Response:

```json
{ "sent": true, "routeKey": "KILLFEED", "runtimeRendering": "ENABLED",
  "channel": { "id": "...", "name": "🔫・combat-feed" }, "messageId": "...", "sentAt": "2026-09-23T12:00:00Z" }
```

Errors (`{"error":{"code","message"}}`):

| Code | HTTP | When |
|---|---|---|
| `EMBED_TEMPLATE_INVALID` | 400 | draft, variable or body invalid (including a `channelId` key) |
| `EMBED_TEMPLATE_NOT_RENDERABLE` | 422 | disabled or empty draft |
| `EMBED_CUSTOM_RENDERING_NOT_SUPPORTED` | 422 | route has no runtime custom rendering |
| `EMBED_ROUTE_NOT_CONFIGURED` | 409 | no channel routed for this route |
| `EMBED_TEST_CHANNEL_UNAVAILABLE` | 409 | channel gone, not in this guild, or unreachable |
| `EMBED_TEST_SEND_FORBIDDEN` | 409 | missing View Channel or Send Messages |
| `EMBED_TEST_EMBED_LINKS_REQUIRED` | 409 | missing Embed Links |
| `EMBED_TEST_RATE_LIMITED` | 429 | too many test sends |
| `EMBED_TEST_SEND_FAILED` | 502 | Discord rejected the message |

**Rate limit** (process-local, the existing `saasRateLimiter`): one send per 3 seconds per actor
and installation, and 10 per minute per installation.

**Audit.** Each send writes `embed_test_sent` to `admin_audit_log` and the structured log:
organization, installation, route, acting user, destination channel, message ID and time. Never
the template, sample values or player names.

## Zero gameplay side effects

Preview and test send perform template validation, rendering, a route lookup and (test only) one
Discord send. They never touch the kill/death pipeline, economy, bounties, stats, streaks,
achievements, presence, location history or the ADM parser; `saas_api_embed_designer.go` does not
import any of those packages (enforced by a test), and an integration test asserts the gameplay
tables are unchanged after previews and a test send.

## Live publishing is unchanged

Publishers still build the Champion default card and pass it to `Customize`, which returns that
default on any problem (no template, flag off, disabled, render error). Queueing, dedupe, routing,
persistence, rate limits and ordering are untouched; the only change is that `Customize` calls the
shared `RenderEvent`.
