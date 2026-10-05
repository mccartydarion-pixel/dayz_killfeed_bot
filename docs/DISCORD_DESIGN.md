# Discord design: one look, one voice

Everything Champion posts or says in Discord follows the rules on this page. They match the
website's "glass" design: calm, sentence case, crimson as the brand accent, gold for highlights,
green for healthy, and no shouting.

The rules live in code, in `internal/presentation` (`brand.go`, `rules.go`, `format.go`) and
`internal/discord/voice.go`, and they are tested: `TestDesignRules` (in `internal/discord` and
`internal/app`) walks every card in the design preview and fails when one breaks a rule. A new
card that is added to the preview set is checked automatically.

This page supersedes the look described in `DISCORD_PRESENTATION_V2.md` and the titles and colours
listed in `AUTO_LEADERBOARD_V3.md`. Their descriptions of structure (what a card contains, how a
board is laid out, how it is refreshed) still hold.

## The rules

### 1. Colour: six colours, six meanings

Colour is chosen by what a message means, never by which feature sent it.
`presentation.Palette()` is the whole list.

| Name | Hex | Means | Examples |
| --- | --- | --- | --- |
| `Crimson` | `#D60F1D` | The brand: kills and combat, primary announcements, calls to action | kill card, hot zone, bounty placed, map vote, welcome, link panel |
| `Gold` | `#F0C65E` | Highlights: records, rewards, winners, leaderboards | long shot, bounty claimed, reward, leaderboards, season results, rank-up |
| `Green` | `#38C987` | Success and healthy: confirmed, recovered, online, joined | player joined, alert resolved, status board when every server is connected |
| `Amber` | `#E08A1E` | Warnings: needs attention soon, degraded, expiring | staff warning, rent due, points removed, status board when a server is degraded |
| `Red` | `#FF3344` | Errors and danger: failed, critical, under attack, declined | critical staff alert, log download failed, base raid alarm, request declined |
| `Neutral` | `#8D8D96` | Information: routine status, diagnostics, quiet events | death, hit, player left, diagnostics, summaries |

The older constant names (`ChampionGold`, `EventGold`, `FactionGold`, `CombatRed`, `SuccessGreen`,
`WarningAmber`, `ErrorRed`, `InfoSteel`, `Steel`, `NeutralGraphite`) still compile and each resolves
to one of the six. No builder may use a raw hex value.

### 2. Author, title and emoji

* **Author line**: `Champions® Killfeed`, or `Champions® <Product>` for a card that belongs to one
  part of the product (`presentation.BrandAuthor("Battle Pass")`). The brand appears nowhere else
  on a card. Compact feed cards (hit, PvE, connection, bounty, economy) have no author and no
  title: their first line is a bold header.
* **Title**: sentence case. No ALL CAPS, no Title Case. `☠️ Player eliminated`, not
  `☠️ PLAYER ELIMINATED`. Product names keep their capitals (Champion Points, Player Hub, Security
  Store, Ranked).
* **Emoji**: at most one, at the start of the title, and only where it says what kind of message
  this is. Never one at each end. Status dots (🟢 🔴) and medals (🥇 🥈 🥉) inside a card are content, not
  decoration.
* **The title is the event.** The server's name is not part of the title.

### 3. Fields

* Labels are sentence case: `Killer`, `Head to head`, `Final hit`, `Channel summary`.
* A short value that sits beside others is inline (`Killer | Victim | Head to head`). A list, a
  sentence or a link is a full-width field.
* A field with nothing to show is left out. There is never a placeholder ("N/A", "Unknown").
* A link goes in a field, never in the footer: Discord shows footers as plain text, so a link there
  cannot be clicked.

### 4. Footer and time

* One footer format: **`Server name · short context`** (`presentation.Footer(server, context)`).
  Either part may be missing; with neither, the card has no footer. The context is a few plain
  words: the season, `Staff only`, `Updates automatically`, or a one-line hint
  (`Turn these off with /life recap off`).
* A kill or death card's context is its season; without a season it is the slogan
  `Every kill tells a story`.
* Where a card already names the server in a `Server` field (staff alerts, build feed) or in a
  sentence (direct messages, which are read outside the server), the footer does not repeat it.
* **Time**: when a builder has a moment in time it uses Discord's own timestamps
  (`presentation.Timestamp`, `presentation.TimestampWithRelative`), so every reader sees their own
  time zone. A scheduled moment is written `<t:…:F> (<t:…:R>)`. The time a card was made goes in
  the embed timestamp (`presentation.StampEmbed`), never in the footer, where Discord does not
  render it. The one exception is a **calendar day or week in UTC** (a ranked week, a daily reset):
  it stays a written date, because a timestamp would show the day before to readers west of UTC.

### 5. Numbers, names and separators

* Thousands separators everywhere (`presentation.FormatThousands`): `12,500 pts`, `1,284 kills`.
* Distances: one decimal for a kill (`FormatDistance`: `1,287.6m`), whole metres where the source
  only knows whole metres (`FormatWholeDistance`: `86m`). No space before the `m`.
* K/D at two decimals (`FormatKD`), percentages at one (`FormatPercent`). A bare `K/D` is the
  overall one (kills per death of any kind). Where both are shown they are labelled `K/D (PvP)` and
  `K/D (overall)`, and a death count's split reads `PvP 250 • PvE 60` (`DeathSplit`). The stat strip
  of a kill or death card gains a second line, `PvP **262 D** • **4.90 K/D**` (`CompactPvPStats`),
  only for a player with at least one PvE death (docs/DEATH_COUNTS.md).
* Units are lower case: `12 kills`, `3 hits`, `2 bounties` (`presentation.Plural`).
* Every name written by a person (player, faction, server, zone, base, event) goes through
  `presentation.SafeName`: no pings, no invisible characters, capped length, markdown escaped.
* Values inside a line are separated by ` • `. Footer and author parts are separated by ` · `.
* A stored code shown to people is shown as words (`presentation.EnumLabel`: `BASE_RADAR` reads
  `Base radar`).

## Voice: how the bot answers

Every private answer to a command, button or form reads as one product
(`internal/discord/voice.go`, enforced by `TestReplyVoice`).

* Calm, plain, sentence case. Say what happened, then what to do next.
* A refusal or a failure has no emoji and never blames the person. A success may start with one ✅.
* No internal names: no error classes, no ids, no "database". A detail that helps the person
  (which permission, which channel, which command) stays.

| Situation | Sentence | Helper |
| --- | --- | --- |
| Something failed on our side | Something went wrong on our side. Try again in a moment. | `ReplyTryAgain` |
| A command that is switched off | This command is unavailable right now. | `ReplyCommandUnavailable` |
| A button on an old message | This button is no longer valid. | `ReplyButtonExpired` |
| The server is not set up | This server isn't set up yet. Run `/setup run` first. | `ReplyNotSetUp` |
| No DayZ server picked | No DayZ server is selected for this Discord yet. | `ReplyNoServerSelected` |
| Not allowed | You need the Administrator or Manage Server permission to *manage servers*. | `ReplyNeedsPermission(action)` |
| Not linked | 🔗 **Account not linked** + the next step for that command | `ReplyNotLinked(nextStep)` |
| A feature cannot be used | *Stats* are unavailable right now. Try again later. | `ReplyIsUnavailable` / `ReplyAreUnavailable` |
| Loading or saving failed | Couldn't *load your stats* right now. Try again in a moment. | `ReplyCouldNot(action)` |
| A rule was hit | Couldn't *start the season*: *a season is already active*. | `ReplyCouldNotBecause(action, reason)` |

The first three are what the interaction router answers with; they are the reference tone.

## The Embed Designer comes first

A server owner's saved template always wins over the built-in card, and nothing on this page
applies to it.

* **How priority works.** Every publisher builds its built-in card, then hands it, with the
  event's variables, to `embedrender.Renderer.Customize` (`internal/discord/embed_custom.go`).
  The saved template is posted instead of the built-in card only when all of these hold: the
  rollout switch `CHAMPION_CUSTOM_EMBEDS_ENABLED` is on, the installation has the `custom_embeds`
  feature, the route is one of the six that render at runtime, the installation selected
  **Custom Embed** for that route, and the saved template is valid and enabled. In every other
  case, and on any error, the built-in card is posted unchanged.
* **Which routes are designable.** `KILLFEED` (kill cards), `HITFEED`, `PVE_FEED`,
  `BOUNTY_TRACKING`, `ECONOMY` and `CONNECTIONS` (a single join or leave; a batch of several keeps
  the built-in list). Templates can be saved for the other routes but are never rendered.
* **Variables are data.** The story engine's labels stay in capitals (`{{kill_type}}`,
  `{{story_title}}`, `{{special_kill}}`, `{{range}}`, `{{headshot}}`, `{{war_badge}}` ...) because
  owner templates read them. Only the built-in card shows them in sentence case
  (`presentation.SentenceCase`). No variable was renamed, removed or changed.
* **The proof.** `TestSavedTemplatesRenderByteForByteAsBefore` renders 68 saved-template and event
  pairs across the six routes and compares the variables and the embed, byte for byte, with
  `internal/discord/testdata/embed_designer_golden.json`, which was written by the code as it was
  before the built-in cards were restyled. Do not regenerate that file to make a failure go away.
* **Two kinds of "default".** The *Champion Default* a route falls back to (no template, a reset,
  or the owner choosing Champion Default) is the built-in card, so it has the look on this page
  automatically. The *starting template* the designer offers when an owner begins to customise is
  defined by the website (`lib/saas/embedTypes.ts`), not here. A built-in card cannot be fully
  written as a template (its title, colour and hero field follow the kill's story, and a template
  has one colour and no conditions), so the two are kept consistent as far as a template can go:
  `suggestedDesignerDefaults` in `internal/discord/designer_suggested_defaults_test.go` holds a
  starting template per route in the new look, using only existing variables, and a test proves
  each one validates, renders and passes the design rules. Copy them to the website when its
  defaults are next updated.

## Deliberate exceptions

| What | Why it is not held to the rules |
| --- | --- |
| Saved Embed Designer templates | The owner's own design. |
| The welcome card's custom title, footer, colour and images | The owner's own text and choice. The built-in welcome card and the presets follow the rules. |
| Faction recruitment cards and supporter tier colours | The faction's or the owner's own colours. |
| C.A.S.E. cards (staff alerts, watch digest, staff demo, C.A.S.E. starter cards) | Their wording is reviewed separately as a compliance contract, and the digest's title is how its delivery is reconciled. They use palette colours through the shared constants; their text is untouched. |
| Discord category and channel names (`🏆 CHAMPION • LIVE`, `🔫・combat-feed`) | They are names the bot finds its channels by, not messages. |
| The story engine's labels and the event/route codes | Data read by templates and by the website. |

## Previewing a change

```
DESIGN_PREVIEW_OUT=/tmp/design-preview/after.json \
  go test -p 1 -count=1 -run 'TestDesignPreview$' ./internal/discord/ ./internal/app/
```

writes every card of the preview set, rendered by the real builders with made-up sample data, to
one JSON file: an array of `{id, title, group, source, content, embed, embeds}` where `embed` is
the Discord API embed. Run it on two versions of the code to show a change side by side: the ids
match. `-p 1` matters, because both packages add to the same file.

## Less noise

* A board or panel is one message that is edited in place (`RoutePanels`): leaderboards, server
  ranks, server status, heatmap, bounty board, Security Store, link and stats panels, the ADM
  monitor, event scoreboards. None of them reposts.
* A run of failed log downloads is one outage and one staff message (another only if the kind of
  failure changes), then one "recovered" message (`ADMMonitorPublisher.HandleDownload`).
* Direct messages are sent once per event (`notifyOnce`, alarm cooldowns) and the shop's
  "delivered" message is edited when the buyer answers.

Not changed, and worth a decision (each needs a stored message id or a product choice):

1. **Map vote.** "Pick the next map", "Next map" and "Map changed" are three posts; the first
   keeps a "Vote here" link after voting closed. The result could replace the vote post.
2. **Double RP.** "Is coming", "is live" and "has ended" are three posts; the first two could be
   one message that is edited.
3. **Events.** When the live scoreboard is on, the final standings are posted twice: the
   scoreboard is edited to "Final" and an "Event complete" card is posted.
4. **Staff alerts.** An alert and its "Alert resolved" are two messages; the resolution could edit
   the alert (the history of what happened would then live in one place).
5. **Event scoreboard.** It is re-edited every three minutes even when nothing changed, because
   its timestamp is the time of the edit. Silent, but wasted requests.
6. **Shop progress messages.** An order can send "received", "on its way" and "delivered" as three
   direct messages; one message edited through the steps would do.
7. **One kill, several channels.** A bounty claim shows on the kill card, the bounty feed, the
   economy feed and (for an RP bounty) the ranked card. That is right when each has its own
   channel; an owner who routes them to one channel sees the same event three or four times.

The kill pipeline's delivery (immediate and rotating feed, the feed journal, burst handling) is
not touched by any of this.

## Inventory

Every message the bot posts, edits or answers with, grouped by kind. "Layer" says whether the
builder uses `internal/presentation`. "Changed" says whether its look or wording changed when
these rules were applied. C = crimson, G = gold, Gr = green, A = amber, R = red, N = neutral.

### Feed cards (one per event, in a feed channel)

| # | Message | Built in | Layer | Colour | Title or header | Fields | Footer | Time | Changed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | Kill card | `discord/killfeed_embeds.go` `BuildKillEmbedWithOptions` | yes | C; G for long shot, extreme range, spree, bounty, record | `☠️ Player eliminated`, `🎯 Headshot` ... | hero (`Distance`/`Streak`/`Reward`), `Killer` `Victim` `Head to head` inline, `Final hit`, `Location` | season, else slogan | embed timestamp = kill time | yes |
| 2 | Death card | `discord/deathfeed.go` `BuildDeathEmbed` | yes | N | `☠️ Player death`, `💀 Suicide` | `Cause` `Weapon` `Final hit` inline, `Player stats` | season, else slogan | embed timestamp | yes |
| 3 | Hit card | `discord/hitfeed.go` `buildHitEmbed` | yes | N | header `🎯 **Hit**  A  ➜  B` | none (three lines) | none | none | yes |
| 4 | PvE death card | `discord/pvefeed.go` `buildPveEmbed` | yes | N | header `💀 **Suicide**`, `☣️ **PvE death**` | none | none | none | yes |
| 5 | Connection card | `discord/connections.go` `buildConnectionsEmbed` | yes | Gr joined, N left | header `🟢 **Connected**`, `🔴 **Disconnected**` | none | none | none | yes |
| 6 | Connection list (several at once) | same | yes | N | header `🔌 **Server connections**` | none | none | none | yes |
| 7 | Build activity card | `discord/build_feed.go` `BuildActivityEmbed` | yes | N | `🏗️ Build activity` | `Player` `Action` `Object` `On`/`From` `Tool` `Location` `Server` inline | `Staff only` | none | yes |
| 8 | Build activity summary | same, `BuildActivitySummaryEmbed` | yes | N | `🏗️ Build activity` | `Server` | `Staff only` | none | yes |
| 9 | Bounty feed card (placed, increased, claimed, expired, cancelled) | `discord/bounty_feeds.go` `buildBountyEventEmbed` | yes | C placed/increased, G claimed, N expired/cancelled | header `🎯 **Bounty placed**` ... | none | none | none | yes |
| 10 | Economy card (bounty reward, reward, staff credit, staff debit, shop purchase, refund) | `discord/economy_feed.go` `buildEconomyEmbed` | yes | G reward, Gr granted or refunded, A removed, N purchase | header `💰 **Bounty reward**` ... | none | none | none | yes |
| 11 | A saved owner template on any of the six runtime routes | `embedrender.RenderEvent` | n/a | the owner's | the owner's | the owner's | the owner's | the owner's | **no (by contract)** |

### Boards and panels (one message, edited in place)

| # | Message | Built in | Layer | Colour | Title | Fields | Footer | Time | Changed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 12 | Bounty board | `discord/bounty_feeds.go` `buildBountyBoardEmbed` | yes | G | header `🎯 **Active bounties**` | none (rank lines) | `Updates automatically` | none | yes |
| 13 | Auto leaderboard (header + up to six category embeds) | `discord/leaderboard_panel.go` | yes | G | `📊 Auto leaderboard`, `🔫 All-time top 15 kills` ... | one inline cell per player | `Updates automatically` | `Last updated <t:R>` | yes |
| 14 | Server ranks board | `discord/server_ranks.go` | yes | G | `🎖️ Server ranks` | one inline cell per player | none | `Last updated <t:R>` | yes |
| 15 | Server ranks: no season yet | `discord/server_ranks_board.go` | yes | N | `🎖️ Server ranks` | none | `Updates automatically` | none | yes |
| 16 | Server status board | `discord/server_status_board.go` | yes | Gr all connected, A one degraded, N waiting | `📡 Server status` | one field per server | `Updates automatically` | `updated <t:R>` | yes |
| 17 | PvP heatmap board | `discord/heatmap_board.go` | yes | C | `🗺️ PvP heatmap update` | `Window` `Type` `Resolution` inline, `Activity`, `Hot zones` | `Updates automatically` | embed timestamp | yes |
| 18 | Security Store panel | `discord/security_store_panel.go` | yes | C | `🛡️ Security Store` | one field per offer | `Server · Updates automatically` | embed timestamp | yes |
| 19 | Link your account panel (+ button) | `discord/public_panels.go` | yes | C | `Link your PlayStation account` | none | none | none | yes |
| 20 | Player stats panel (+ four buttons) | `discord/leaderboard_panel.go` | yes | C | `Player stats` | none | none | none | yes |
| 21 | Online players panel (legacy) | `discord/panels.go` `OnlinePlayersEmbed` | yes | Gr online, N offline | `Live players` | none | `Live server status` | embed timestamp | yes |
| 22 | Server status panel (legacy) | `discord/panels.go` `ServerStatusPanel` | yes | Gr healthy, A not | `Server status` | `Nitrado` `ADM log` `Killfeed` `Online players` inline | `Live server status` | embed timestamp | yes |
| 23 | `/server` status card | `discord/embeds.go` | yes | N | `Server status` | status fields inline | none | none | yes |
| 24 | ADM monitor (staff, live) | `discord/adm_monitor.go` `BuildADMMonitorEmbed` | yes | Gr / A / R by health | `ADM monitor` | diagnostics inline | `ADM monitor · refreshes every 5 minutes` | `<t:R>` values | yes |
| 25 | Event scoreboard (live, final) | `app/upgrade_scoreboard.go` | yes | C live, G final | `📋 Live: <event>`, `🏁 Final: <event>` | `Standings` | none | embed timestamp | yes |
| 26 | Features guide (13 cards) | `discord/features_command.go` | yes | C intro and combat, G rewards, N the rest | `🏆 What's new on Champion` ... | `How it works`, `Where` | hint on the first card | none | yes |
| 27 | Starter card of a new feed channel (nine) | `app/saas_channel_layout.go` | yes | N | `🔫 Combat feed` ... | none | none | none | yes |
| 28 | C.A.S.E. starter cards (three) | same | yes | N | unchanged | none | none | none | no (exception) |
| 29 | Faction recruitment card (+ buttons) | `app/faction_recruit.go` | no | the faction's | `[TAG] Name` | faction details | `Champion Factions · site` | embed timestamp | no (exception) |

### Announcements (posted once)

| # | Message | Built in | Layer | Colour | Title | Fields | Footer | Time | Changed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 30 | Hot zone opened | `discord/hot_zone_embeds.go` | yes | C | `🔥 Hot zone open` | `Where` `Ends` inline, `Prizes`, `How it scores` | `Server · Hot zone` | `Ends <t:R>` | yes |
| 31 | Event upcoming / started | `discord/event_announcement_embeds.go` | yes | G | `📅 Upcoming event`, `🏁 Event started` | `How to win`, `Starts` `Ends` inline, `Prizes` | none | `<t:F> (<t:R>)` | yes |
| 32 | Event complete | `discord/competitive_embeds.go` | yes | G | `👑 Event complete` | `🏆 Final standings` | season | none | yes |
| 33 | Season complete | same | yes | G | `🏆 Season complete` | four records inline | season | none | yes |
| 34 | Faction war complete | same | yes | G | `⚔️ Faction war complete` | both sides and records inline | season | none | yes |
| 35 | Season change scheduled / done | `discord/season_plan_embeds.go` | yes | G | `🗓️ Stats season ending`, `🎖️ New ranked season` ... | `New season`, `When` | `Server · Ranked` (ranked only) | `<t:F> (<t:R>)` | yes |
| 36 | Map vote open (+ optional @everyone line) | `app/map_rotation_worker.go` | yes | C | `🗺️ Pick the next map` | `Maps`, `Voting closes` `Who can vote` inline | none | `<t:t> (<t:R>)`, embed timestamp | yes |
| 37 | Map vote result | same | yes | Gr | `🗺️ Next map: <map>` | `Votes` | `Server · Map vote` | embed timestamp | yes |
| 38 | Map changed | same | yes | N | `🗺️ Map changed to <map>` | none | none | embed timestamp | yes |
| 39 | Daily / weekly challenges | `app/challenges.go` | yes | C | `📋 Today's challenges` | none | server | none | yes |
| 40 | Battle pass season starts | `app/battle_pass.go` | yes | G | `🎟️ New battle pass season` | none | none | `until <t:D>` | yes |
| 41 | Battle pass season ends | same | yes | G | `🏁 <season> has ended` | none | server | none | yes |
| 42 | Ranked: bounty on a player | `app/ranked_bonuses.go` | yes | C | `💀 Bounty on <player>` | none | none | none | yes |
| 43 | Ranked: bounty claimed | same | yes | C | `🎯 Bounty claimed` | none | none | embed timestamp | yes |
| 44 | Ranked: rank-up | same | yes | G | `🏅 <player> reached <tier>` | none | server | none | yes |
| 45 | Ranked: week in review | same | yes | N | `📊 Ranked week in review` | `Top climbers`, three inline records | server | written dates (a UTC week) | yes |
| 46 | Double RP coming / live / ended | `app/rp_boosts.go` | yes | G | `⚡ 2× RP is live` ... | `Starts` `Ends`, leaders, Player Hub link | none | `<t:F> (<t:R>)` | yes |
| 47 | Territory captured / unclaimed | `app/territory.go` | yes | C captured, N unclaimed | `🏴 <zone> captured` | none | none | none | yes |
| 48 | Season champions (ranked) | `app/upgrade_season.go` | yes | G | `🏆 Season champions` | `Final standings` | none | `<t:D>` | yes |
| 49 | Supporter shout-out | `app/perk_store.go` | yes | G | `💎 New supporter`, `🎁 Perk gifted` | perks, top supporters, Player Hub link | none | embed timestamp | yes |
| 50 | New features switched on | `app/upgrade_features.go` | yes | C | `✨ New on <server>` | none | none | none | yes |
| 51 | Server of the week | `app/upgrade_spotlight.go` | yes | G | `🌟 Server of the week` | network link | none | none | yes |
| 52 | Welcome card (built-in and presets) | `discord/welcome.go` | yes | C | `Welcome to Champions` | `Get started`, `Next step` | none | none | yes |
| 53 | Welcome card (the owner's own text, colour, images) | same | yes | the owner's | the owner's | built-in | the owner's | none | no (exception) |
| 54 | Zone alert in the owner's zone channel | `app/intrusion_publisher.go` | yes | A intrusion, R ban violation, N detection, Gr exit | `Zone intrusion` ... | `Zone type` `Player` inline | none | embed timestamp | yes |
| 55 | Platform broadcast | `app/owner_ops_worker.go` | no | none (plain text) | `**Prefix: title**` | none | none | none | no |

### Direct messages

| # | Message | Built in | Layer | Colour | Title | Fields | Footer | Time | Changed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 56 | Death recap | `discord/lives.go` `BuildLifeRecapEmbed` | yes | N | `🪦 Life recap` | `Survived` `Kills` `Longest kill` `Tracked distance` inline | how to turn it off | embed timestamp | yes |
| 57 | Base raid alarm | `discord/base_raid_alarm.go` | yes | R | `🚨 Someone is breaking into <base>` | `Player` `Server` `When` inline, `What happened` | alarm limit hint | `<t:R>`, embed timestamp | yes |
| 58 | Perimeter watch | `discord/perimeter_watch.go` | yes | A | `👀 Someone is near <base>` | `Player` `Server` `Seen` inline, `Distance` | limit hint | `<t:R>` | yes |
| 59 | Base request approved / declined | `discord/base_request.go` | yes | Gr / R | `✅ <base> is registered` ... | `Reason` | next step (declined) | embed timestamp | yes |
| 60 | New base request (to the owner) | same | yes | N | `📍 New base request on <server>` | request details | where to answer | embed timestamp | yes |
| 61 | Gift of base security | same | yes | Gr | `🎁 You've been given 30 days of ...` | `Active until`, `Note`, `Security Store` | none | `<t:f>` | yes |
| 62 | Base rent due / paused | same | yes | A / R | `🏠 Rent for <base> is due <t:R>` ... | `Pay in the Security Store` | none | `<t:R>` | yes |
| 63 | Free rent given | same | yes | Gr | `🎁 30 days of free rent for <base>` | details | none | embed timestamp | yes |
| 64 | Rent paid for you | same | yes | Gr | `🏠 <player> paid rent for <base>` | details | none | embed timestamp | yes |
| 65 | Base transfer request (to the owner) | same | yes | N | `🔁 Base transfer request on <server>` | details | where to answer | embed timestamp | yes |
| 66 | Base transfer decision | same | yes | Gr / R | `🔁 <base> is now yours` ... | `Reason` | none | embed timestamp | yes |
| 67 | Alert shared with a faction mate | `discord/faction_security.go` | yes | the original's | reworded original | the original's | `Shared with you by Faction Security` | the original's | no |
| 68 | Shop order delivered (+ buttons) | `discord/shop_order.go` | yes | A | `📦 Your order was delivered` | `Total` `Answer by` inline | what happens without an answer | `<t:f>` | yes |
| 69 | Shop order answered (edits #68) | same | yes | Gr received, R issue, N done | `Order #1042` | none | none | none | yes |
| 70 | Shop ticket opened (in the ticket channel) | same | yes | R | `🎫 Ticket #7 · order #1042` | `What went wrong`, `Order`, `Reported from` `Opened` inline, `Staff` | none | `<t:f>` | yes |
| 71 | Shop ticket closed | same | yes | Gr | `✅ Ticket #7 closed` | `Note from staff` | none | none | yes |
| 72 | Shop progress (received, on its way, needs a hand, refunded) | `app/upgrade_shop.go` | yes | Gr | `📦 Order received` ... | none | none | none | yes |
| 73 | Rank-up | `app/ranked_bonuses.go` `buildRankUpDM` | yes | G | `🏅 You reached <tier>` | Player Hub link | none | none | yes |
| 74 | Supporter tier received | `app/vip.go` | yes | G, or the owner's tier colour | `💎 You received the <tier> tier` | `What you get`, `Yours until`, Player Hub link | none | `<t:f>` | yes |
| 75 | Supporter tier ending, perk gifted | `app/upgrade_perks.go` | yes | G | `⏰ Your <tier> tier ends soon`, `🎁 You got a gift` | none | none | none | yes |
| 76 | We miss you | `app/upgrade_dms.go` | yes | C | `👋 We miss you on <server>` | Player Hub link | none | none | yes |
| 77 | Your bounty was claimed / ran out | `app/upgrade_bounties.go` | yes | C | `🎯 Your bounty was claimed` ... | none | none | none | yes |
| 78 | Ranked season finish | `app/upgrade_season.go` | yes | G | `🥇 You finished #1 in the ranked season` | none | none | none | yes |
| 79 | Appeal accepted / not accepted | `app/upgrade_security.go` | yes | Gr / R | `✅ Your appeal was accepted` ... | none | none | none | yes |
| 80 | Zone alert to the zone's alert player | `app/upgrade_zones.go` | yes | as #54 | as #54 | as #54 | none | embed timestamp | yes |
| 81 | Verification complete (plain text) | `discord/verified_role.go` | no | none | `✅ **Verification complete**` | none | none | none | yes |
| 82 | C.A.S.E. staff alert | `discord/case_core8_alert.go` | partly | palette | unchanged | unchanged | unchanged | embed timestamp | no (exception) |

### Staff messages

| # | Message | Built in | Layer | Colour | Title | Fields | Footer | Time | Changed |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 83 | Staff alert (warning, critical, resolved, notice): log stale, Nitrado failure, zone, UAV and base radar intrusions, zone ban, base request, base transfer, bases paused for rent, security week, new appeal, map not changed | `discord/admin_alerts.go` `BuildAdminAlertEmbed` | yes | A / R / Gr / N | `🚨 Admin alert`, `✅ Alert resolved`, `📍 Staff notice` | `Severity` `Server` and the alert's own, inline | `Staff only` | embed timestamp | yes |
| 84 | Log download report (failed, recovered, rotation) | `discord/adm_monitor.go` `BuildADMDownloadEmbed` | yes | R / Gr / A | `🚨 ADM download failed` ... | diagnostics inline | `ADM monitor` | `<t:R>` | yes |
| 85 | Setup report (`/setup run`, `/setup repair`, partial) | `discord/setup_report.go` | yes | Gr done, A needs attention | `🏆 Discord setup`, `🔧 Discord repair`, `⚠️ Setup incomplete` | `Channel summary`, `Systems verified`, `Needs attention`, `Not available yet`, `Legacy channels`, `Result` | `Discord setup` | none | yes |
| 86 | C.A.S.E. watch digest | `discord/admin_alerts.go` `BuildCaseWatchDigestEmbed` | yes | N | unchanged | unchanged | unchanged | embed timestamp | no (exception) |
| 87 | C.A.S.E. staff demo | `discord/case_staff_demo.go` | yes | A | unchanged | unchanged | unchanged | fixed | no (exception) |

### Command, button and form answers (private)

| # | Message | Built in | Layer | Style | Changed |
| --- | --- | --- | --- | --- | --- |
| 88 | `/stats` profile | `discord/stats_commands.go` | text | bold header, sentence case | yes |
| 89 | `/leaderboard` card and its "unavailable" card | `presentation/leaderboard_embed.go` | yes | G / R, `🏆 Player leaderboard` | yes |
| 90 | `/link`, `/unlink` and the link form (pending, account, confirm, nine failure reasons) | `discord/link_commands.go`, `public_panels.go` | text | bold header + next step | yes |
| 91 | `/server connect, services, select, disconnect, repair, status` | `discord/server_commands.go` | text | voice helpers | yes |
| 92 | `/setup run, repair, reset, verified-role` | `discord/setup_commands.go` | text + #85 | voice helpers | yes |
| 93 | `/faction war` (challenge, accept, decline, cancel, status, history) | `discord/war_commands.go` | text | voice helpers | yes |
| 94 | `/faction info, rivalry, leaderboard` | same | text | bold headers, sentence case | yes |
| 95 | `/event list, info, leaderboard, create, start, end` | `discord/competitive_commands.go` | text | voice helpers | yes |
| 96 | `/bounty list, info, create, cancel` | same | text | voice helpers | yes |
| 97 | `/economy balance, history, credit, debit` | `discord/economy_commands.go` | text | voice helpers | yes |
| 98 | `/points` | `discord/points_commands.go` | text | voice helpers | yes |
| 99 | `/season start, end, status, history` | `discord/season_commands.go` | text | voice helpers | yes |
| 100 | `/welcome` settings, status, preview and test | `discord/welcome_commands.go` | text | voice helpers, failure reasons as sentences | yes |
| 101 | `/life me, top, recap` (two cards and text) | `discord/lives.go` | yes | N profile, G boards | yes |
| 102 | `/card` | `discord/card_command.go` | text + image | voice helpers | yes |
| 103 | `/mybase`, `/registerbase`, pay-rent buttons | `discord/base_commands.go` | yes | N, `📍 Your bases` | yes |
| 104 | `/features` | `discord/features_command.go` | text | voice helpers | yes |
| 105 | `/admin` status, diagnostics, source scan (four cards and text) | `discord/admin_commands.go` | yes | N, `Pipeline diagnostics` ... | yes |
| 106 | `/matchup`, `/weapon` | `discord/analytics_commands.go` | text | voice helpers | yes |
| 107 | Panel buttons: my stats, search player, my balance, recent transactions | `discord/public_panels.go` | text | voice helpers; button labels in sentence case | yes |
| 108 | Shop order buttons and issue form | `app/shop_order_discord.go` | text | shared sentences | yes |
| 109 | Faction recruitment Join / Apply buttons | `app/faction_recruit.go` | text | shared sentences | yes |
| 110 | The router's three fallback answers | `discord/voice.go` | text | the reference tone | no |
| 111 | Embed Designer test send | `discord/embed_test_message.go` | n/a | the owner's template | no |

111 message families: 100 changed, 11 not (2 are the owner's own templates and are unchanged by
the Embed Designer contract, 6 are deliberate exceptions, 3 already followed the rules or are
plain text with nothing to restyle).

## What changed for people who already use the bot

* **Nothing was removed.** Every fact a card showed is still on it, in the same place or a
  clearer one. What went away is decoration: the `CHAMPION •` prefix in footers, the upper-case
  slogan on cards that have something better to say there, and the second copy of an emoji at the
  end of board titles.
* **Moved, not removed:** a server's name moved from the title to the footer on challenges,
  battle pass and rank-up cards, and from a `Server` field to the footer on the map vote result
  and the ranked season card. A Player Hub link moved from the footer (where it could not be
  clicked) to a field on the double RP and supporter cards.
* **Reworded, same meaning:** "X is unavailable until the database is connected" now reads
  "X is unavailable right now. Try again later." The welcome test no longer shows an error class;
  it shows the same reason as a sentence. `Run /setup first` names the real command,
  `/setup run`.
* **Boards and panels** are edited once to the new look the first time they refresh. Cards that
  were already posted stay as they were.
