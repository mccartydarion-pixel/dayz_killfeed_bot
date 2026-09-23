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

## V2.1 - variables, metadata and newlines

### Variable metadata

`GET .../embed-templates` and `GET .../embed-templates/{routeKey}` keep their `variables` name
lists and add `variableDefinitions`: per variable its `name`, `label`, `description`, `category`
(Players, Combat, Killer Stats, Victim Stats, Head To Head, Streaks, Event Story, Bounty,
Competitive, Economy, Activity, Server), `example`, `optional`, `availability`, `format` (text,
integer, decimal, distance, label, points, timestamp) and `source`. The backend owns this
dictionary (`internal/embedtemplates/variables.go`); the approved name lists are derived from it,
so names and metadata cannot drift. The examples are valid preview sample values.

Every variable comes from data already attached to the event being rendered - no lookup, no
query, no inference. `killfeedVars` and `hitfeedVars` take only the event (a test proves the
variable builders and the renderer reference no store, database or context). An optional
variable is simply absent; a field that uses it is omitted, and missing statistics are never
shown as zero.

Not added, because the runtime does not carry them without a new query:

- PvE victim statistics (`victim_kills` ...): the PvE notice carries only the name and cause.
- Hitfeed attacker statistics (`attacker_kills`, `attacker_kd`): hit encounters carry no stats.

**Hit data on kills.** `hit_zone`, `damage` and `headshot` are wired to the same event fields the
default card's headshot classification reads. The standard ADM kill line does not carry a hit
zone or damage, so they are usually absent on KILLFEED; their availability says so. Nothing is
inferred from earlier hit lines.

### Newlines

Template text supports line breaks, applied by the one canonical renderer (so preview, test
send and live events agree):

| Template text | Renders as |
|---|---|
| a real line break (Enter in a textarea) | a line break |
| `\n` | a line break |
| `\n\n` | two line breaks |
| `\\n` | the literal characters `\n` |
| `\r\n`, `\r` | a line break |

Escapes are expanded in the **template** before variables are substituted, so a value can never
become syntax: a player named `Bad\nPlayer` stays that name (sanitized, no extra line), and a
real line break inside a value becomes a space. Description, field values and footer keep line
breaks; title, author name and field names are single-line in Discord and fold breaks into a
space. Discord limits are enforced after expansion and substitution - each `\n` counts as one
rendered character.

Example field (KILLFEED), label `KILLER STATS`:

```
Kills: {{killer_kills}}\nDeaths: {{killer_deaths}}\nK/D: {{killer_kd}}\nStreak: {{killer_streak}}
```

renders

```
Kills: 9
Deaths: 0
K/D: 9.00
Streak: 1
```

### Variables per runtime-rendered route

### KILLFEED

| Variable | Label | Category | Description | Source | Example | Optional | Format | Availability |
|---|---|---|---|---|---|---|---|---|
| `killer` | Killer | Players | The player who got the kill. | `Event.Killer.Name` | `WilliamAle--10` | no | text | Always present. |
| `victim` | Victim | Players | The player who was killed. | `Event.Victim.Name` | `Semillita-azul-_` | no | text | Always present. |
| `weapon` | Weapon | Combat | The weapon used. | `Event.Weapon` | `M4-A1` | yes | text | Present when the kill line names a weapon. |
| `weapon_category` | Weapon Category | Combat | The weapon's class from Champion's weapon classifier. | `presentation.WeaponCategory(Event.Weapon)` | `Assault Rifle` | yes | label | Present when the classifier recognizes the weapon. |
| `ammo` | Ammo | Combat | The ammunition type. | `Event.Ammo` | `5.56x45` | yes | text | Present when the kill line names the ammunition. |
| `distance` | Distance | Combat | Kill distance in meters. | `Event.Distance` | `11.7m` | yes | distance | Present when the kill line has a distance. |
| `range` | Range | Combat | Champion's range class for the kill. | `presentation.RangeClass(Event.Distance)` | `CLOSE QUARTERS` | yes | label | Present when the distance is known or the kill was melee. |
| `hit_zone` | Hit Zone | Combat | The body part hit. | `Event.HitZone` | `Torso` | yes | text | Only present when the kill event carries hit data. Standard ADM kill lines do not include a hit zone or damage, so this is usually absent. |
| `damage` | Damage | Combat | Damage of the confirmed hit. | `Event.Damage` | `98.4` | yes | decimal | Only present when the kill event carries hit data. Standard ADM kill lines do not include a hit zone or damage, so this is usually absent. |
| `killer_kills` | Killer Kills | Killer Stats | The killer's all-time kills after this kill. | `Event.KillerStats.Kills` | `9` | yes | integer | Present when Champion loaded the player's combat record for this kill. |
| `killer_deaths` | Killer Deaths | Killer Stats | The killer's all-time deaths. | `Event.KillerStats.Deaths` | `0` | yes | integer | Present when Champion loaded the player's combat record for this kill. |
| `killer_kd` | Killer K/D | Killer Stats | The killer's all-time K/D after the confirmed kill. | `Event.KillerStats.KD()` | `9.00` | yes | decimal | Present when Champion loaded the player's combat record for this kill. |
| `killer_streak` | Killer Streak | Killer Stats | The killer's current kill streak including this kill (same value as streak). | `Event.KillerStreak` | `1` | yes | integer | Present when the streak is known and above zero. |
| `victim_kills` | Victim Kills | Victim Stats | The victim's all-time kills. | `Event.VictimStats.Kills` | `2` | yes | integer | Present when Champion loaded the player's combat record for this kill. |
| `victim_deaths` | Victim Deaths | Victim Stats | The victim's all-time deaths after this death. | `Event.VictimStats.Deaths` | `2` | yes | integer | Present when Champion loaded the player's combat record for this kill. |
| `victim_kd` | Victim K/D | Victim Stats | The victim's all-time K/D after this death. | `Event.VictimStats.KD()` | `1.00` | yes | decimal | Present when Champion loaded the player's combat record for this kill. |
| `h2h_killer_wins` | H2H Killer Wins | Head To Head | How many times the killer has killed this victim. | `Event.Encounters.KillerWins` | `4` | yes | integer | Present when Champion loaded the head-to-head record. |
| `h2h_victim_wins` | H2H Victim Wins | Head To Head | How many times this victim has killed the killer. | `Event.Encounters.VictimWins` | `0` | yes | integer | Present when Champion loaded the head-to-head record. |
| `h2h_score` | H2H Score | Head To Head | The head-to-head record, killer first. | `Event.Encounters` | `4–0` | yes | text | Present when Champion loaded the head-to-head record. |
| `streak` | Streak | Streaks | The killer's current kill streak including this kill. | `Event.KillerStreak` | `1` | yes | integer | Present when the streak is known and above zero. |
| `ended_streak` | Ended Streak | Streaks | The victim's streak this kill ended. | `Event.EndedStreakCount` | `8` | yes | integer | Only when this kill ended a meaningful streak. |
| `killing_spree` | Killing Spree | Streaks | Set when this kill reached a killing-spree milestone. | `Event.KillingSpree` | `KILLING SPREE` | yes | label | Only on killing-spree kills. |
| `streak_ended` | Streak Ended | Streaks | Set when this kill ended the victim's streak. | `Event.StreakEnded` | `STREAK ENDED` | yes | label | Only when this kill ended a meaningful streak. |
| `headshot` | Headshot | Event Story | Set when the confirmed hit zone is the head. | `Event.HitZone == Head` | `HEADSHOT` | yes | label | Only present when the kill event carries hit data. Standard ADM kill lines do not include a hit zone or damage, so this is usually absent. |
| `special_kill` | Special Kill | Event Story | The kill's highlight line from Champion's story engine. | `BuildPresentation(Event).Hero` | `PRECISION FINISH` | yes | label | Only for non-standard kills (long range, headshot, streaks, bounty, events ...). |
| `kill_type` | Kill Type | Event Story | The kill's classification from Champion's story engine. | `BuildPresentation(Event).Title` | `HEADSHOT` | no | label | Always present. |
| `story_title` | Story Title | Event Story | The kill's title as the default card shows it, with its icon. | `BuildPresentation(Event).Icon + Title` | `🎯 HEADSHOT` | no | label | Always present. |
| `bounty_amount` | Bounty Amount | Bounty | Champion Points paid for the claimed bounty (a number - write {{bounty_amount}} pts). | `Event.BountyPoints` | `500` | yes | points | Only when this kill claimed a bounty. |
| `bounty_target` | Bounty Target | Bounty | Set when the victim had an active bounty. | `Event.BountyTarget` | `MOST WANTED` | yes | label | Only when the victim had an active bounty. |
| `bounty_claimed` | Bounty Claimed | Bounty | Set when this kill claimed a bounty. | `Event.BountyClaimed` | `BOUNTY CLAIMED` | yes | label | Only when this kill claimed a bounty. |
| `season_name` | Season | Competitive | The active season. | `Event.SeasonName` | `Season 3` | yes | text | Present while a season is active. |
| `war_badge` | War Badge | Competitive | The faction war this kill counted for. | `Event.WarBadge` | `WAR KILL` | yes | label | Only for kills in an active faction war. |
| `event_badges` | Event Badges | Competitive | Active competitive events this kill counted for, joined by •. | `Event.ActiveEventBadges` | `Double Kill • NWAF Event` | yes | text | Only when the kill counted for an active event. |
| `server_name` | Server Name | Server | The DayZ server's display name. | `game server display name` | `Champions Deathmatch` | yes | text | Present when the server has a display name. |
| `timestamp` | Timestamp | Server | When Champion published the card (UTC). | `publish time` | `2026-09-23 12:00:00 UTC` | no | timestamp | Always present. |

### HITFEED

| Variable | Label | Category | Description | Source | Example | Optional | Format | Availability |
|---|---|---|---|---|---|---|---|---|
| `attacker` | Attacker | Players | The player who landed the hits. | `hit encounter attacker` | `WilliamAle--10` | no | text | Always present. |
| `killer` | Killer (alias) | Players | Same as attacker (kept for older templates). | `hit encounter attacker` | `WilliamAle--10` | no | text | Always present. |
| `victim` | Victim | Players | The player who was hit. | `hit encounter victim` | `Semillita-azul-_` | no | text | Always present. |
| `weapon` | Weapon | Combat | The weapon used. | `hit encounter weapon` | `M4-A1` | yes | text | Present when the hit lines name a weapon. |
| `ammo` | Ammo | Combat | The ammunition type. | `hit encounter ammo` | `5.56x45` | yes | text | Present when the hit lines name the ammunition. |
| `distance` | Distance | Combat | Distance of the hits in meters. | `hit encounter distance` | `87m` | yes | distance | Present when the hit lines have a distance. |
| `hit_zone` | Hit Zone | Combat | The body part hit last. | `hit encounter zone` | `Torso` | yes | text | Present when the hit lines name a zone. |
| `damage` | Damage | Combat | Total damage across the grouped hits. | `hit encounter damage sum` | `142` | yes | integer | Only when every grouped hit reported its damage (a partial sum is never shown). |
| `hits` | Hits | Combat | How many hits this card groups: consecutive hits by the same attacker on the same victim with the same weapon. | `hit encounter count` | `3` | no | integer | Always present. |
| `server_name` | Server Name | Server | The DayZ server's display name. | `game server display name` | `Champions Deathmatch` | yes | text | Present when the server has a display name. |
| `timestamp` | Timestamp | Server | When Champion published the card (UTC). | `publish time` | `2026-09-23 12:00:00 UTC` | no | timestamp | Always present. |

### PVE_FEED

| Variable | Label | Category | Description | Source | Example | Optional | Format | Availability |
|---|---|---|---|---|---|---|---|---|
| `victim` | Victim | Players | The player who died. | `PvE notice name` | `Semillita-azul-_` | no | text | Always present. |
| `cause` | Cause | Combat | The proven non-PvP cause. | `PvE notice cause` | `suicide` | yes | label | Present when the ADM line proves a cause (today: suicides). |
| `server_name` | Server Name | Server | The DayZ server's display name. | `game server display name` | `Champions Deathmatch` | yes | text | Present when the server has a display name. |
| `timestamp` | Timestamp | Server | When Champion published the card (UTC). | `publish time` | `2026-09-23 12:00:00 UTC` | no | timestamp | Always present. |

### BOUNTY_TRACKING

| Variable | Label | Category | Description | Source | Example | Optional | Format | Availability |
|---|---|---|---|---|---|---|---|---|
| `target` | Target | Players | The player the bounty is on. | `bounty event target` | `Semillita-azul-_` | no | text | Always present. |
| `victim` | Victim (alias) | Players | Same as target (kept for older templates). | `bounty event target` | `Semillita-azul-_` | no | text | Always present. |
| `hunter` | Hunter | Players | The player who claimed the bounty. | `bounty event hunter` | `WilliamAle--10` | yes | text | Only on CLAIMED events. |
| `killer` | Killer (alias) | Players | Same as hunter (kept for older templates). | `bounty event hunter` | `WilliamAle--10` | yes | text | Only on CLAIMED events. |
| `amount` | Amount | Bounty | Champion Points on the bounty (a number - write {{amount}} pts). | `bounty event amount` | `500` | no | points | Always present. |
| `total` | Total Paid | Bounty | Champion Points paid out on the claim (a number). | `bounty event amount` | `500` | yes | points | Only on CLAIMED events. |
| `count` | Bounties Claimed | Bounty | How many bounties the claim paid out. | `bounty event count` | `2` | yes | integer | Only on CLAIMED events. |
| `weapon` | Weapon | Combat | The weapon of the claiming kill. | `bounty event weapon` | `M4-A1` | yes | text | Only on CLAIMED events that name a weapon. |
| `distance` | Distance | Combat | Distance of the claiming kill. | `bounty event distance` | `87m` | yes | distance | Only on CLAIMED events with a known distance. |
| `status` | Status | Bounty | What happened to the bounty. | `bounty event kind` | `claimed` | no | label | Always present. |
| `server_name` | Server Name | Server | The DayZ server's display name. | `game server display name` | `Champions Deathmatch` | yes | text | Present when the server has a display name. |

### ECONOMY

| Variable | Label | Category | Description | Source | Example | Optional | Format | Availability |
|---|---|---|---|---|---|---|---|---|
| `player` | Player | Players | The player the transaction belongs to. | `economy event player` | `WilliamAle--10` | no | text | Always present. |
| `amount` | Amount | Economy | Champion Points moved (a number - write {{amount}} pts). | `economy event amount` | `250` | no | points | Always present. |
| `balance` | Balance | Economy | The player's Champion Points balance afterwards (a number). | `economy event balance` | `1,250` | yes | points | Only for rewards (bounty payouts, system rewards). |
| `transaction_type` | Transaction Type | Economy | What kind of transaction it was. | `economy event type` | `Bounty reward` | no | label | Always present. |
| `server_name` | Server Name | Server | The DayZ server's display name. | `game server display name` | `Champions Deathmatch` | yes | text | Present when the server has a display name. |

### CONNECTIONS

| Variable | Label | Category | Description | Source | Example | Optional | Format | Availability |
|---|---|---|---|---|---|---|---|---|
| `player` | Player | Players | The player who joined or left. | `connection notice name` | `WilliamAle--10` | no | text | Always present. |
| `event` | Event | Activity | joined or left. | `connection notice kind` | `joined` | no | label | Always present. |
| `event_type` | Event Type | Activity | connected or disconnected. | `connection notice kind` | `connected` | no | label | Always present. |
| `session` | Session Length | Activity | How long the player was online. | `connection notice session` | `1h 12m` | yes | text | Only on disconnects when the session length is known. |
| `server_name` | Server Name | Server | The DayZ server's display name. | `game server display name` | `Champions Deathmatch` | yes | text | Present when the server has a display name. |
| `timestamp` | Timestamp | Server | When Champion published the card (UTC). | `publish time` | `2026-09-23 12:00:00 UTC` | no | timestamp | Always present. |


## Live publishing is unchanged

Publishers still build the Champion default card and pass it to `Customize`, which returns that
default on any problem (no template, flag off, disabled, render error). Queueing, dedupe, routing,
persistence, rate limits and ordering are untouched; the only change is that `Customize` calls the
shared `RenderEvent`.
