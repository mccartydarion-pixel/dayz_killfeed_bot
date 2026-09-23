# Champion Discord Presentation V2

The visual language of every **default** Champion Discord card. Presentation only:
nothing here changes parsing, persistence, dedupe, classification, routing,
rotating-feed cadence, rate limits or custom-template storage. Renderers take an
already-authoritative event or snapshot and return an embed: no database or
network calls, no allocation-heavy work (the feeds are high frequency).

Shared primitives live in `internal/presentation`:

| File | Owns |
| --- | --- |
| `brand.go` | palette, author, footer variants, `NewFeedEmbed`, `StampEmbed` |
| `format.go` | numbers, points, distance, K/D, `CompactStats`, name safety |
| `rank.go` | the one rank system: categories, value formatting, medals, ranking blocks |
| `card.go` | Discord limits, `MetricField`/`AppendFields`, `FitEmbed` |
| `story_header.go` | story titles, accents, story lines (subtitles), weapon flavor |
| `leaderboard_embed.go` | `/leaderboard` response |

The Discord renderers are `internal/discord/killfeed_embeds.go` (kills),
`deathfeed.go` (deaths/suicides), `leaderboard_panel.go` (season board + panel
hash), `bounty_feeds.go`, `competitive_embeds.go`.

## Brand hierarchy

The brand appears **once**, in the author line. The title is the event. The
footer is the slogan.

```
author   CHAMPIONS® KILLFEED
title    🎯 HEADSHOT
footer   EVERY KILL TELLS A STORY
```

Footer variants (one line, never a multi-line block, never the brand again):

| Variant | Text |
| --- | --- |
| Standard | `EVERY KILL TELLS A STORY` |
| Season (when the event carries one) | `CHAMPION • {Season} • EVERY KILL TELLS A STORY` |
| Persistent board | `CHAMPION • AUTO-REFRESH` |
| Operational panel | `CHAMPION • LIVE SERVER INTELLIGENCE` |

Time goes in Discord's native embed **timestamp** (`StampEmbed`), set only from an
authoritative absolute time. Discord does not render `<t:…>` inside footers, so it
is never put there. No new external URLs are introduced (no author icon).

## Colors

Color carries meaning. Not everything is gold.

| Token | Hex | Used for |
| --- | --- | --- |
| `ChampionGold` | `#C9A227` | standard kills, leaderboards, season cards |
| `CombatRed` | `#8B0000` | headshot, killing spree, streak ended |
| `EventGold` | `#D4AF37` | rare moments: extreme range, bounty claim |
| `Steel` (`InfoSteel`) | `#4682B4` | longshot, operational panels |
| `WarningAmber` | `#B7791F` | suicide, cautionary bounty/economy states |
| `SuccessGreen` | `#3F8F68` | bounty paid, rewards, connects |
| `NeutralGraphite` | `#2B2F33` | ordinary deaths |

## Kill card

Order answers: **what** (title) → **who** (matchup) → **how** (weapon block) →
**why notable** (story line, badges, hero metric) → **stats**.

```
CHAMPIONS® KILLFEED
☠️ PLAYER ELIMINATED

**WilliamAle--10** → **Semillita-azul-\_**

🔫 **Controlled Burst**
`M4-A1` • 11.7m • Close Quarters

KILLER                     VICTIM                     H2H
**9 K** • **0 D** •        **2 K** • **2 D** •        **4–0**
**9.00 K/D**               **1.00 K/D**               WilliamAle--10 leads
🔥 Streak **1**

FINAL HIT
Torso • 34.2 dmg

EVERY KILL TELLS A STORY                                   (timestamp)
```

Rules:

- Description: bold matchup `**Killer** → **Victim**`; for special stories a short
  italic story line; then the weapon block — the flavor headline (only when
  `WeaponStory()` knows the weapon) over a `` `WEAPON` • distance • range `` strip;
  then at most **3** secondary badges joined with ` • `.
- Weapon categories are never invented: flavor/category come from the shared
  classifier, and an unknown weapon shows just its name.
- `KILLER` / `VICTIM` / `H2H` are **inline** so they sit side by side on desktop; each
  reads on its own when mobile stacks them. Each appears only when its data exists.
- `FINAL HIT` (inline) shows the hit zone plus damage when present. It is omitted for
  a head hit, which is already the title or a badge.
- Ammo stays available to templates (`{{ammo}}`) but is not on the default card —
  lowest priority, not worth the height.
- `LOCATION` appears only with the explicit coordinates option.
- Nothing absent is shown: no `N/A`, `Unknown Ammo`, `No Damage`, spacer fields or
  ASCII dividers.
- Target: **2–6 fields**.

### Special stories

The story is selected by the shared engine (`presentation.SelectPrimary`) from the
persisted classification — renderers never recompute it. The primary story is the
title and is **never repeated as a badge**.

| Story | Title | Accent | Story line | Hero field |
| --- | --- | --- | --- | --- |
| Standard | ☠️ PLAYER ELIMINATED | Champion Gold | — | — |
| Headshot | 🎯 HEADSHOT | Combat Red | _Precision finish_ | — |
| Long range | 🎯 LONGSHOT | Steel | _Long-range elimination_ | `DISTANCE` **173.8m** |
| Extreme range | 👑 EXTREME RANGE | Event Gold | _Extreme-range elimination_ | `DISTANCE` **247.3m** |
| Bounty claim | 💰 BOUNTY CLAIMED | Event Gold | _Bounty secured_ | `REWARD` **12,500 pts** |
| Killing spree | 🔥 KILLING SPREE | Combat Red | _Streak extended_ | `STREAK` **5** |
| Streak ended | 💀 STREAK ENDED | Combat Red | Ended a **8-kill streak** (persisted count only) | — |
| Melee | 🥊 CLOSE QUARTERS | Faction Gold | _Close-quarters finish_ | — |

The hero metric is shown once: a longshot's distance leaves the weapon strip, a
spree's streak leaves the KILLER field. Story lines are static, competitive, and
never claim a fact the data does not prove.

Badges (secondary, confirmed data only, max 3, priority order): Most Wanted,
Headshot, Extreme Range / Long Shot, Close Range (headshot ≤15m), Killing Spree,
Streak Ended, stored event badges, war badge.

## Death card

```
CHAMPIONS® KILLFEED
☠️ PLAYER DEATH            (💀 SUICIDE, Warning Amber)

**Ceiyxe**

PLAYER STATS
**1 K** • **50 D** • **0.02 K/D**
```

- `CAUSE` only for a parser-proven non-player cause (infected / animal /
  environment). Suicide is already the title, so it has no cause field; its item
  shows as `WEAPON`.
- `FINAL HIT` when a hit zone exists. Cause/weapon/final hit are inline.
- No empty "DEATH DETAILS" block. Target: **0–3 fields**.

## Stats

One compact line everywhere: `**9 K** • **0 D** • **9.00 K/D**`. K/D is always two
decimals, from `CombatRecord.KD()` (its zero-death semantics are unchanged).
Numbers use separators (`125,000`); Champion Points are `12,500 pts` / `1 pt` —
never "credits", "coins" or "Value".

## Leaderboards

A scoreboard, not a stack of cards: **one field per category**, rows inside it.

```
🏆 SEASON LEADERBOARD
Competitive Rankings

⚔️ TOP KILLERS
🥇 IIIIIIIIIIII-I • **20 Kills**
🥈 Its\_H14METIYO • **16 Kills**
🥉 zTonii99 • **11 Kills**
`#4` WilliamAle--10 • **9 Kills**
…

🎯 LONGEST KILLS         📈 BEST K/D          🏆 CHAMPION POINTS
🔥 LIVE EVENTS           🎯 MOST WANTED       (bullets of stored strings)

CHAMPION • AUTO-REFRESH
```

- Order: Top Killers (10), Longest Kills (5), Best K/D (5), Champion Points (5),
  Live Events, Most Wanted. Limits come from `LeaderboardConfig`; K/D eligibility
  stays in the query.
- Rank format `marker name • **value**`: 🥇🥈🥉 for 1–3, `` `#N` `` from 4. No
  whitespace-aligned columns (they break on mobile).
- Values are formatted per category by `FormatLeaderboardValue`: `20 Kills`,
  `98.3m`, `4.25 K/D`, `12,500 pts`, `8 Kills`. An unknown category shows the cleaned
  raw value — never "Value".
- Rank names are capped at 32 runes (PSN IDs ≤16, Xbox ≤15+suffix, Steam ≤32).
- Rows are dropped whole to stay within the 1024-char field limit.
- Empty categories are omitted; an all-empty board is a single
  `_No qualifying data yet._` line.
- No "kill leader" spotlight: it would only repeat the 🥇 row.
- `/leaderboard` (`BuildPlayerLeaderboardEmbed`) uses the same rank system — one
  field — so there is one leaderboard style.
- Target: **3–6 fields**.

### Persistent-panel hash

`hashEmbed` (used by the legacy panel and `RoutePanels`) fingerprints title,
description, URL, color, author, footer, thumbnail/image URLs, and every field's
name, value and inline flag, length-prefixed. The timestamp is excluded (volatile).
So an unchanged board is never re-edited, and a ranking change inside a field is
never skipped. (Before V2 the hash ignored fields and dereferenced a nil footer.)

## Other feeds

Hitfeed, PvE, connections and economy cards were already compact
description-only cards and are unchanged. Bounty lifecycle cards drop the generic
`Value:` label (`Reward **25,000 pts**`, `**Hunter** eliminated **Target**`); the
bounty board uses the shared rank rows. The season-complete card is four inline
record fields and omits holder names it does not know.

## Name safety

Player-controlled text inside markdown goes through `SafeName`: `@` and `#`
removed (no `@everyone`/`@here`/role/channel mentions), control, zero-width and
bidi characters dropped, whitespace collapsed, length capped, then markdown
escaped (so `Semillita-azul-_` renders literally and `**x**` is not bold). Valid
gamer tags are not altered. Publishers also send with `AllowedMentions` parsing
nothing.

## Emoji and markdown

- Emoji are semantic: one per title, per heading, per badge. No emoji walls.
- Bold is for names and the key number; inline code for weapons and `#N` ranks.
- Discord embed limits are enforced on every card by `FitEmbed`: title 256,
  description 4096, 25 fields, field name 256 / value 1024, footer 2048,
  total 6000 (lowest-priority trailing fields are dropped first).

## Custom templates

The KILLFEED custom-template system (`CHAMPION_CUSTOM_EMBEDS_ENABLED`, Embed
Designer) is untouched and every variable keeps its name and value (including
`{{special_kill}}`, fed from the unchanged story Hero strings). V2 only changes the
default card, which is also the fallback:

| Situation | Card |
| --- | --- |
| Valid enabled template | custom card |
| No template / template disabled / flag off | V2 default |
| Malformed template / store failure / render failure | V2 default |

## Previews and tests

`internal/discord/presentation_fixtures_test.go` has deterministic fixtures for
every card (standard, headshot, longshot, extreme range, bounty, killing spree,
streak ended, death, suicide, season leaderboard, player leaderboard). Print them:

```bash
go test ./internal/discord -run TestPresentationFixturePreview -v
```

Structural tests (`killfeed_embeds_test.go`, `leaderboard_v2_test.go`,
`internal/presentation/*_test.go`) pin author, title, color, fields, inline flags,
footer, limits, rank lines, category values, mention safety, hash behavior and
custom-template fallback.

### Measured field counts (fixtures, before → after)

| Card | Before | After |
| --- | --- | --- |
| Standard kill | 6 (incl. 2 spacer fields) | 4 |
| Headshot | 6 | 3 |
| Longshot | 7 | 5 |
| Extreme range | 7 | 5 |
| Bounty claim | 6 | 5 |
| Killing spree | 6 | 5 |
| Streak ended | 6 | 4 |
| Death | 2 | 1 |
| Suicide | 4 | 2 |
| Season leaderboard (10/5/5) | 20 | 3 |
| Player leaderboard (10) | 10 | 1 |
