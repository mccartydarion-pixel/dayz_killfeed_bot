# Feature upgrades

Upgrades layered on existing features. Most are **automations**: switches per server in
**Client Hub → Growth → Automations** (`GET/PUT .../admin/upgrades`, FEATURE_SETTINGS_VIEW to see,
FEATURE_SETTINGS_MANAGE to change), all off until switched on. The switches live in one JSON
document per server (`upgrade_settings`); `switchedOn` records when each went on, so an automation
can leave alone what happened before. The competitive scheduler runs every automation's pass
(`runUpgrades`, internal/app/upgrades.go); slow passes are throttled per server.

Every direct message an automation sends is claimed first in `upgrade_notices (kind, server,
player, ref)`, so it goes out at most once, even across restarts. DMs go to the player's
VERIFIED Discord link; closed DMs are not retried.

| # | Upgrade | Switch | Where |
|---|---|---|---|
| 1 | Priority queue rewards: top N ranked players and supporter tier holders go on the Nitrado priority list, and come off when they no longer qualify. Only names Champion added (`priority_auto_grants`) are ever removed, never one a perk purchase pays for. Every 10 minutes. | `priorityRankedTop` (0-50), `priorityVip` | upgrade_priority.go |
| 2 | Win-back DM to linked players away `winbackDays` (3-30) days, with double RP and their RP to the next rank. Once per time away, never twice in 14 days, 20 per server an hour. | `winbackEnabled` | upgrade_dms.go |
| 3 | Raid alarms (owner and faction) link to the Black Box; the Player Hub Black Box shows the faction's bases too. | always | base_raid_alarm.go, `TeamHistory` |
| 4 | Season-end rewards: champ credits for a ranked season's top 3 when it ends (reset by staff or the planner), a "Season champions" card and a DM. Seasons ended within 7 days, once each (`reward:ranked-season:<id>:<place>`). | `seasonRewards` [3] | upgrade_season.go |
| 5 | Live event scoreboard: one message in the events channel, edited every 3 minutes while an event runs (route panel `EVENT_LIVE:<id>`), then final standings once. Hot zones excluded. | `eventScoreboard` | upgrade_scoreboard.go |
| 7 | Ranked tags on kill cards: "+250 RP ⚡💀" (double RP, bounty, underdog, revenge, first kill). Embed designer variable `ranked_rp`. | `killfeedRankedTags` | upgrade_killfeed.go |
| 9 | Heatmap time of day: `fromHour`/`toHour` (0-23, may wrap) and `tz` on the heatmap API. Hot zone forecast: the player hot zone API adds `forecast` (busiest two hours, UTC, last 28 days, needs 20+ kills). | always | heatmap/service.go, upgrade_forecast.go |
| 10 | Life recaps for every linked player who never turned them off; the recap adds the player's personal best. | `lifeStoryDms` | life_repository.go, discord/lives.go |
| 11 | Nemesis, favourite victim and weapon mastery (Bronze 25, Silver 100, Gold 250, Master 500 kills): `GET /api/saas/player/servers/{id}/rivals`. | always | upgrade_rivals.go |
| 13 | Auto leaderboard: "This week's top kills" board (Monday 00:00 UTC) and ▲/▼/🆕 movement since the day before on the kills and longest-kill boards (`leaderboard_positions`). | always | discord/leaderboard_* |
| 14 | Bounty placers hear by DM when their bounty is claimed or runs out. Bounty cards never name who placed a bounty. | `bountyDms` | upgrade_bounties.go |
| 15 | New event templates: Power Hour, Headshot Hour, Sniper Sunday, Streak Sprint, Weekend Marathon. | always | events/templates.go |
| 19 | Rent: a reminder three days ahead to the owner and their faction, and the one-day reminder to the faction too (the owner already gets it). | `rentReminders` | upgrade_rent.go |
| 21 | Daily play reward: credits for each UTC day a player is seen (real connections), plus `dailyLoginStreakBonus` per day in a row (up to 7). Once a day (`reward:daily:<server>:<day>`). | `dailyLoginCredits` | upgrade_daily.go |
| 23 | Shop order updates by DM: received, waiting for the restart (automatic delivery), held for staff, refunded. Only orders placed after the switch went on. The shop's own "delivered" DM is unchanged. | `shopProgressDms` | upgrade_shop.go |
| 24 | Reminders three days before a donation subscription renews or ends, or a supporter tier runs out; gift notes (`giftMessage` on a perk purchase, up to 200 characters) go to the recipient by DM. | `perkReminderDms` (gift notes always) | upgrade_perks.go |
| 26 | Weekly security digest to the staff alerts channel (Mondays 12:00-36:00 UTC); player appeals: `GET/POST /api/saas/player/servers/{id}/appeals`, staff `GET .../admin/appeals`, `POST .../admin/appeals/{id}/decide` (BANLIST_MANAGE). One open appeal per player; the answer is DMed. | `caseWeeklyDigest`, `caseAppeals` | upgrade_security.go |
| 27 | Zone alert player: `GET/PUT .../admin/zones/{id}/alert-player` (ZONE_MANAGE). That player gets the zone's intrusion, UAV and radar alerts by DM, once per cooldown, even without an alert channel. | per zone | upgrade_zones.go |
| 28 | Server of the Week on the public network (most active listed server; active players ×3 + kills; not twice running): `GET /api/saas/network/spotlight`, and a card in the winner's events channel. | always | upgrade_spotlight.go |
| 29 | A post in the features channel (`/features`) when player-facing automations are switched on. | `featureAnnouncements` | upgrade_features.go |

Migration `0118_feature_upgrades` adds every table above; it is additive.
