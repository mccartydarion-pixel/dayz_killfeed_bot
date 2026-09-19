# Embed Designer runtime rendering (Phase 4)

Saved custom embed templates (Phase 2, `docs/SAAS_API.md`) are now consumed by the Discord
publishers - **presentation only**, behind a rollout flag, with the existing Champion
default as the fallback for everything.

> **Rollout flag:** `CHAMPION_CUSTOM_EMBEDS_ENABLED` (default **false**).
> Off: every publisher builds exactly the cards it always did, whatever templates are saved,
> and no template lookup happens. On: eligible routes render an installation's enabled
> template; on any problem the default card is published instead.

## Architecture

```
publisher (unchanged routing, aggregation, rate limits, persistence order)
   │  builds its EXISTING default embed
   │  builds the event's variables (map[string]string, authoritative values only)
   ▼
embedrender.Renderer.Customize(guild, server, route, vars, at, default)
   │  cache (installation, route) -> template          [30s TTL, invalidated on save/reset]
   │  sanitize + filter variables to the route's approved set
   │  pure Render(template, vars)  -> discordgo.MessageEmbed
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
| `KILLFEED` | `killer` `victim` `weapon` `distance` (`86.4m`) `ammo` `streak` (when > 0) `server_name` `timestamp` |
| `HITFEED` | `attacker` (= `killer`) `victim` `weapon` `ammo` `distance` `hit_zone` `damage` (only when **every** hit in the encounter carried one) `hits` `server_name` |
| `PVE_FEED` | `victim` `cause` (the parser-proven cause: today only `suicide`) `server_name` `timestamp` |
| `BOUNTY_TRACKING` | `status` (`placed` `increased` `claimed` `expired` `cancelled`) `target` (= `victim`) `amount`; claims only: `hunter` (= `killer`) `total` `count` `weapon` `distance`; `server_name` |
| `CONNECTIONS` | `player` `event` (`joined`/`left`) `event_type` (`connected`/`disconnected`) `session` (disconnects with an observed session >= 1 minute) `server_name` `timestamp` |
| `ECONOMY` | `player` `amount` `transaction_type` (`Bounty reward`, `Admin credit`, `Admin debit`, ...) `balance` (rewards only) `server_name` |

`timestamp` is the publish time in UTC (`2006-01-02 15:04:05 UTC`); the embed's own timestamp
(template option) is the same instant. `server_name` is the game server's display name. There are no
special-kill variables because the designer schema has none. `ammo` is passed as the event carries
it (HITFEED strips the `Bullet_` prefix like the default card).

## Optional data

* A **field** whose label or value references an absent variable is **omitted entirely**.
* An absent variable in the **title, description, footer or author** renders as nothing and the
  gap is closed; if the whole section ends up empty it is omitted. (Put optional values in fields.)
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
`templateRenderError`, plus `enabled`. Logs (`component=embedrender event=embed_template_fallback`)
carry `installation_id`, `route_key` and `fallback_reason` only, at most once a minute per
(installation, route, reason).

## Flood protection and ordering are untouched

HITFEED keeps its aggregation window, backlog bounds, 1 message per 2s tick and 10 cards per
message; each aggregated card is rendered independently (a test compares message/card counts with and
without a customizer). PVE_FEED keeps its ownership/claim logic (an unproven cause is never claimed,
custom template or not) and its "N earlier deaths were not shown" note. Kill persistence, dedupe,
replay and bounty-claim ordering happen before the embed is built and are not involved. The rotating
killfeed batching, per-server queues and Discord failure handling are unchanged.

## Route compatibility matrix

| Route | Templates persisted | Runtime publisher exists | Runtime custom rendering | Default fallback |
|---|---|---|---|---|
| `KILLFEED` | yes | yes | **yes** (one card per kill, no duplicate) | yes |
| `HITFEED` | yes | yes | **yes** (per aggregated card) | yes |
| `PVE_FEED` | yes | yes (explicit suicides only today) | **yes** (presentation only) | yes |
| `BOUNTY_TRACKING` | yes | yes | **yes** (placed/increased/claimed/expired/cancelled) | yes |
| `ECONOMY` | yes | yes (bounty rewards, admin credit/debit) | **yes** | yes |
| `CONNECTIONS` | yes | yes | **single-event cards only**; a batched summary (several players / "earlier events not shown") keeps the default | yes |
| `BOUNTY` | yes | yes (persistent board) | **no - incompatible**: a multi-row board reconciled from the database is not a single-event card; left on its default | n/a |
| `ADMIN_LOGS` | yes | yes (ADM monitor) | **no - incompatible**: a live, edited-in-place diagnostic status panel plus download reports with a fixed field model; left on its default | n/a |
| `ADMIN_ALERTS` | yes | no | no (no publisher) | n/a |
| `BUILD_FEED` | yes | no | no (no publisher) | n/a |
| `CASINO` | yes | no | no (no publisher) | n/a |
| `SHOP` | yes | no | no (no publisher) | n/a |
| `HEATMAPS` | yes | no | no | n/a |
| `LINK_GAMERTAG`, `STATS_LEADERBOARDS`, `AUTO_LEADERBOARD` | yes | yes (persistent interactive panels) | no (panels, not event cards) | n/a |

`GET .../embed-templates` reports `runtimeRoutes` (the rows marked yes) and a per-route/global
`runtimeRendering` of `ENABLED` only when the flag is on **and** the route is one of those.

## Rollout

1. Deploy with the flag unset (default): nothing changes.
2. Set `CHAMPION_CUSTOM_EMBEDS_ENABLED=true` and restart. Watch `embedRender` on
   `/api/admin/health` (`templateFallbackRender`/`templateRenderError` should stay near zero).
3. Roll back by unsetting the flag: all publishers return to the default cards immediately on restart.
