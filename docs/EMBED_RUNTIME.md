# Embed Designer runtime rendering (Phase 4)

Saved custom embed templates (Phase 2, `docs/SAAS_API.md`) are now consumed by the Discord
publishers - **presentation only**, behind a rollout flag, with the existing Champion
default as the fallback for everything.

> **Rollout flag:** `CHAMPION_CUSTOM_EMBEDS_ENABLED` (default **false**).
> Off: every publisher builds exactly the cards it always did, whatever templates are saved,
> and no template lookup happens. On: eligible routes render an installation's enabled
> template; on any problem the default card is published instead.

## Runtime audit and route compatibility matrix

Performed against the code BEFORE any integration; a route key alone never counts as support.
"Custom runtime rendering: yes" means the publisher's card path calls the shared renderer.

| Route | Template persistence | Runtime publisher | Output type | Custom runtime rendering | Fallback |
|---|---|---|---|---|---|
| `KILLFEED` | yes | yes (`KillfeedPublisher`, batched by the rotating feed) | single embed per kill | **yes** (one card per kill, never a duplicate) | Champion kill card |
| `HITFEED` | yes | yes (`HitfeedPublisher`) | aggregated embeds, up to 10 per message | **yes** (each aggregated card rendered independently) | Champion hit card |
| `PVE_FEED` | yes | yes (explicit suicides only today) | embeds, up to 10 per message | **yes** (presentation only) | Champion PvE card |
| `BOUNTY_TRACKING` | yes | yes (`BountyTracker`) | embeds, up to 10 per message | **yes** (placed / increased / claimed / expired / cancelled) | Champion lifecycle card |
| `ECONOMY` | yes | yes (`EconomyFeed`: bounty rewards, admin credit/debit) | embeds, up to 10 per message | **yes** | Champion economy card |
| `CONNECTIONS` | yes | yes (`ConnectionsPublisher`) | one embed per message: a single event, or a batched summary list | **single-event cards only**; a batched summary (several players / "earlier events not shown") keeps the default | Champion connection card |
| `BOUNTY` | yes | yes (`BountyBoard`) | **persistent board** (one edited message per channel, top-N rows) | **no - INCOMPATIBLE**: a multi-row board reconciled from the database is not a single-event card; left untouched | Champion board |
| `ADMIN_LOGS` | yes | yes (`ADMMonitorPublisher`) | **persistent live diagnostic panel** (edited in place) plus per-download report embeds, with a fixed diagnostic field model | **no - INCOMPATIBLE** | Champion ADM monitor |
| `LINK_GAMERTAG` | yes | yes (`RouteSyncer` panel) | **persistent interactive panel** (buttons, modals) | **no - INCOMPATIBLE** (panel, not an event card) | Champion panel |
| `STATS_LEADERBOARDS` | yes | yes (`RouteSyncer` panel) | **persistent interactive panel** (stable message id, ephemeral replies) | **no - INCOMPATIBLE** | Champion panel |
| `AUTO_LEADERBOARD` | yes | yes (`LeaderboardScheduler`) | **persistent ranked-rows message** edited on a schedule | **no - INCOMPATIBLE** (rows cannot be expressed by the single-event model) | Champion leaderboard |
| `ADMIN_ALERTS` | yes | yes (`AdminAlertPublisher`) | one embed per alert / resolution | **no - NOT YET ENABLED** (severity colors and the alert/resolution pairing are fixed; templates stay stored) | Champion admin alert |
| `BUILD_FEED` | yes | yes (`BuildFeedPublisher`) | up to 5 cards per message, or one burst summary | **no - NOT YET ENABLED** (the summary card is not a single-event card) | Champion build activity |
| `SHOP` | yes | **no** (not implemented) | - | no | n/a |
| `HEATMAPS` | yes | yes (`HeatmapBoard`) | **persistent aggregate summary** (edited in place on an interval) | **no - INCOMPATIBLE** (a multi-zone aggregate, not a single-event card) | Champion PvP heatmap summary |

Only the six routes marked **yes** are ever rendered; a template saved for any other route is
stored but never used, and the API reports that route's `runtimeRendering` as `NOT_ENABLED`
(`ENABLED` only when the flag is on **and** the route is one of the six).

## Embed Designer preview and test send

The website's preview and test send (`docs/EMBED_DESIGNER_V2.md`) render with
`embedrender.RenderEvent` - the exact sanitize-and-render function `Customize` below uses - so a
preview is what an event would post. They never change the cache or any publisher.

Template text may contain line breaks - real ones or the `\n` syntax (`\\n` is a literal) -
expanded by the renderer before substitution; see `docs/EMBED_DESIGNER_V2.md` "Newlines". The full
per-route variable tables (source, example, availability) are in the same document.

## Architecture

```
publisher (unchanged routing, aggregation, rate limits, persistence order)
   │  builds its EXISTING default embed
   │  builds the event's variables (map[string]string, authoritative values only)
   ▼
embedrender.Renderer.Customize(guild, server, route, vars, at, default)
   │  cache (installation, route) -> template          [30s TTL, invalidated on save/reset]
   │  RenderEvent: sanitize + filter variables to the route's approved set,
   │  then pure Render(template, vars) -> discordgo.MessageEmbed
   │  (the Embed Designer preview/test call the same RenderEvent)
   ▼
custom embed   OR   the SAME default embed (never mutated)
```

* `internal/embedrender` - one shared package; publishers contain **no template logic**, only a
  variables function and one `Customize` call (`internal/discord/embed_custom.go`). There is
  no database access in any publisher: the renderer reads through
  `EmbedTemplateRepository.ResolveTemplate`.
* **Effective template.** Resolved `(guild, server) -> installation -> route template`, with the
  same tenant-consistency joins as channel-route resolution (`ChannelRouteRepository.ResolveChannel`),
  lowest installation id first. It is never keyed by guild alone: two DayZ servers of one guild
  have independent templates (tested against real PostgreSQL).
* **No custom template, template disabled, flag off, no installation:** the default embed,
  byte-for-byte what the publisher built (asserted with `DeepEqual` against the existing builders).
* `Render` itself is pure: no network, no image fetch, no Nitrado call, no database.

## Cache

`(guild, server, route)` -> `{installation id, template | none | malformed | error}`, at most 2048
entries, TTL 30s (`embedrender.DefaultTTL`), single-flight (a cold-cache burst is one lookup).
"No template" is cached too, so a guild that never customizes costs one lookup per 30s per route.
A failed lookup is cached for 5s (a database outage is not hit once per event).

* **Invalidation.** `PUT`/`DELETE .../embed-templates/{routeKey}` call `Renderer.Invalidate(installation,
  route)` in the same process, so the very next event sees the change (tested with a 1-hour TTL, so
  only invalidation can explain it).
* Out-of-process changes (another instance, a direct database edit) are visible within the TTL.

## Fallback (a template can never cost an event its card)

The default is published when: the lookup fails or times out (1s); the stored template is malformed
or no longer validates (re-validated when cached); the render fails or produces nothing Discord would
accept; anything panics. Each is counted and logged safely (below). Database, lookup and render
failures are isolated from persistence: the renderer runs after the event was persisted and only ever
changes the embed object.

## Variables

Only the route's approved set (`embedtemplates`, listed by `GET .../embed-templates`) can be used, and
only values the event actually carries are provided; a value that is unknown is **absent** (never
`N/A`, never a fabricated value). Values are sanitized once, by the renderer (below).

| Route | Variables provided |
|---|---|
| `KILLFEED` | `killer` `victim` `weapon` `distance` (`86.4m`) `ammo` `streak` (when > 0) `special_kill` (the default card's own hero label, only for a special kill) `bounty_amount` (only when the kill claimed a bounty) `server_name` `timestamp` |
| `HITFEED` | `attacker` (= `killer`) `victim` `weapon` `ammo` `distance` `hit_zone` `damage` (only when **every** hit in the encounter carried one) `hits` `server_name` |
| `PVE_FEED` | `victim` `cause` (the parser-proven cause: today only `suicide`) `server_name` `timestamp` |
| `BOUNTY_TRACKING` | `status` (`placed` `increased` `claimed` `expired` `cancelled`) `target` (= `victim`) `amount`; claims only: `hunter` (= `killer`) `total` `count` `weapon` `distance`; `server_name` |
| `CONNECTIONS` | `player` `event` (`joined`/`left`) `event_type` (`connected`/`disconnected`) `session` (disconnects with an observed session >= 1 minute) `server_name` `timestamp` |
| `ECONOMY` | `player` `amount` `transaction_type` (`Bounty reward`, `Admin credit`, `Admin debit`, ...) `balance` (rewards only) `server_name` |

`timestamp` is the runtime publish time in UTC (`2006-01-02 15:04:05 UTC`; the ADM log carries only a
time of day, so no richer event time exists); the embed's own timestamp (template option) is the same
instant, produced by the renderer - a template cannot inject an arbitrary timestamp string. `server_name` is the game server's display name. There are no
special-kill variables because the designer schema has none. `ammo` is passed as the event carries
it (HITFEED strips the `Bullet_` prefix like the default card).

## Optional data

* A **field** whose label or value references an absent variable is **omitted entirely**.
* A **line of the description or footer** that references an absent variable is **omitted
  entirely**: `Hit: {{hit_zone}} | Damage: {{damage}}` disappears when the event has no hit data
  instead of rendering empty labels, while every other line is kept. Blank spacer lines are kept.
* An absent variable in the **title or author** renders as nothing and the gap is closed; if the
  whole section ends up empty it is omitted.
* Preview, test send and live rendering use the same `Render`; the preview additionally reports
  `omittedLines`, `omittedFields` and `absentVariables` (`RenderEventWithReport`).

### Hit data on kills (`hit_zone`, `damage`)

A DayZ ADM kill line carries no hit zone or damage. DayZ writes the lethal hit on the line
immediately before it (every Champions kill observed on 2026-09-24):

```
09:33:17 | Player "V" (DEAD) (id=…)[HP: 0] hit by Player "K" (id=…) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 74.9605 meters
09:33:17 | Player "V" (DEAD) (id=…) killed by Player "K" (id=…) with M4-A1 from 74.9605 meters
```

`killfeed.Engine` attaches that hit to the kill (`Event.FinalHit`) only when every piece of
evidence agrees: the hit is lethal (`(DEAD)`); it is the line immediately preceding the kill in
the same canonical ADM file (the same boot); the victim and killer ids match; the second matches;
and the weapon and distance match. Anything else - an earlier non-lethal hit, another line in
between, a different player, second, weapon or distance, another boot's file, a hit already used,
or a hit before the resume point - leaves the kill without hit data, and lines or fields using
`hit_zone`/`damage` are omitted. `damage` is that lethal hit's damage, never a total.
`Event.FinalHit` is presentation only: `Event.HitZone`/`Damage` (headshot statistics, the default
card, the durable kill fingerprint) are unchanged, and the Hitfeed is untouched.
* A template that renders to nothing at all is not sent - the default is used (counted as a fallback).

## Rendered Discord limits and truncation

The template text was validated at save time; runtime output is enforced again because names and
weapons lengthen it:

| Part | Limit |
|---|---|
| each variable value | 100 characters, then `…` (before substitution) |
| title / description | 256 / 4096 |
| fields | 25; name 256, value 1024; a field never ends up empty |
| footer / author name | 2048 / 256 |
| combined text | 6000 |

Truncation is deterministic and ends with `…` without leaving a dangling `\`. Over the combined
limit, the text is trimmed in a fixed order that keeps the structure: description, then field values
from the last field back, then footer, author name, title; only if that is not enough are trailing
fields dropped. A Discord rejection caused by an oversized embed cannot occur.

## Mention and markdown safety

* Every variable value passes `embedrender.SanitizeValue`: control characters, line breaks, zero-width
  and bidi-override characters become spaces; **`@` and `#` are removed** (the same policy as the
  existing card sanitizers, so `@everyone`, `@here`, `<@id>`, `<#id>` cannot form); markdown and
  mention syntax (`\ * _ ~ | ` > < [ ]`) is escaped, so a name cannot break bold/quote/link/field
  structure; result capped at 100 characters. Names with nothing usable become `Unknown`, like the
  default cards.
* Substitution is single-pass: a name containing `{{victim}}` stays literal text (no template injection).
* Administrator-authored template text keeps its own markdown (`**bold**`, `> quote`, `` `code` ``);
  only variable values are escaped, including inside admin markup.
* **`AllowedMentions` is unchanged**: every send site still passes `Parse: []` (parse nothing), so even
  an administrator writing `@everyone` in a template cannot ping. Nothing in this phase touches it.
* Only variables the route approves are accepted from a publisher; extras are dropped.
* URLs (author/footer icon, thumbnail, image) were validated at save time and are re-checked at
  render (http/https, host, no credentials/whitespace); a bad one drops just that element.

## Metrics and logging

Cumulative counters (no player names, no template contents), exposed on `GET /api/admin/health`
under `embedRender`: `templateCustomRender`, `templateDefaultRender`, `templateFallbackRender`,
`templateRenderError`, plus `enabled`, and the same four broken down **by route key only**
(`byRoute.<ROUTE>.customEmbedRenderTotal` / `defaultEmbedRenderTotal` / `embedFallbackTotal` /
`embedRenderErrorTotal`). They are never labelled by installation, guild or player, so cardinality is
bounded by the number of routes. Logs (`component=embedrender event=embed_template_fallback`)
carry `installation_id`, `route_key` and `fallback_reason` only, at most once a minute per
(installation, route, reason).

## Flood protection and ordering are untouched

HITFEED keeps its aggregation window, backlog bounds, 1 message per 2s tick and 10 cards per
message; each aggregated card is rendered independently (a test compares message/card counts with and
without a customizer). PVE_FEED keeps its ownership/claim logic (an unproven cause is never claimed,
custom template or not) and its "N earlier deaths were not shown" note. Kill persistence, dedupe,
replay and bounty-claim ordering happen before the embed is built and are not involved. The rotating
killfeed batching, per-server queues and Discord failure handling are unchanged.

## Activation (per installation and route)

Each installation chooses, per route, **Champion Default** or **Custom Embed**
(`installation_embed_activation`, migration 0053; no row = Champion Default). A custom template is
rendered only when all four hold:

1. the operator rollout switch `CHAMPION_CUSTOM_EMBEDS_ENABLED` is on;
2. the route supports runtime rendering (`embedrender.SupportedRoutes()`);
3. the installation selected Custom Embed for the route;
4. the saved template is valid and enabled.

`ResolveTemplate` returns a template only for a Custom Embed selection, so the renderer keeps the
default card otherwise. Saving a template never changes the selection; resetting (DELETE) also
returns the route to Champion Default. Activation of an unsupported route (422) or without a saved,
valid, enabled template (409) is refused with the reason; selecting Custom Embed while the rollout
switch is off is allowed and reported as `runtime: BLOCKED, blockedReason: GLOBAL_DISABLED`. Every
template response carries `activation`: `mode`, `templateSaved`, `templateEnabled`, `templateValid`,
`routeSupported`, `globalEnabled`, `runtime` (`ACTIVE` / `DEFAULT` / `BLOCKED`), `blockedReason`,
`canActivate` and `activationUnavailable`. Migration 0053 carries over the previous intent - a
saved, enabled template on a runtime route was meant to render once the switch was on - as an
explicit Custom Embed selection.

## Rollout

1. Deploy with the flag unset (default): nothing changes.
2. Set `CHAMPION_CUSTOM_EMBEDS_ENABLED=true` and restart. Watch `embedRender` on
   `/api/admin/health` (`templateFallbackRender`/`templateRenderError` should stay near zero).
3. Roll back by unsetting the flag: all publishers return to the default cards immediately on restart.
