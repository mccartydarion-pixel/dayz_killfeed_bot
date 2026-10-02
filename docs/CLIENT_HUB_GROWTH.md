# Client Hub growth & staff tools

Nine Client Hub tools for owners and staff. All routes live under
`/api/saas/organizations/{org}/installations/{inst}/admin` and go through the
normal capability check, tenant scope and rate limits. Every change is written to
the admin audit log.

| Tool | Routes | Capability | Plan |
| --- | --- | --- | --- |
| Peak hours planner | `GET /planner/peak-hours` | `RETENTION_VIEW` (Administrator) | Retention |
| Event builder | `GET /events`, `GET /events/templates`, `POST /events`, `POST /events/{id}/cancel`, `POST /events/{id}/end` | `EVENTS_VIEW` (Moderator) / `EVENTS_MANAGE` (Administrator) | — |
| Wipe & season planner | `GET /seasons/planner`, `POST /seasons/planner`, `POST /seasons/planner/{id}/cancel` | `SERVER_STATS_RESET` (Owner) | Ranked reset: Ranked seasons |
| Invite tracking | `GET /invites` | `INVITES_VIEW` (Administrator) | Retention |
| Rewards automation | `GET /rewards`, `PUT /rewards/rules`, `DELETE /rewards/rules/{id}` | `REWARDS_VIEW` (Administrator) / `REWARDS_MANAGE` (Owner) | Economy |
| Player timeline | `GET /players/{id}/timeline` | `PLAYER_DIRECTORY_VIEW` (Moderator) | — |
| Staff activity | `GET /staff-activity` | `STAFF_ACTIVITY_VIEW` (Administrator) | — |
| Zone & base map | existing `/zones` API | `ZONE_VIEW` / `ZONE_MANAGE` (`UAV_MANAGE` for UAV and Base Radar) | — |
| Supporter & VIP tiers | `GET /vip`, `PUT /vip/tiers`, `DELETE /vip/tiers/{id}`, `POST /vip/members`, `POST /vip/members/{id}/revoke` | `VIP_VIEW` (Moderator) / `VIP_MANAGE` (Administrator) | Economy |

Migrations `0104`–`0109` add tables and columns only; none of them rewrites
existing rows.

## Event builder

Seven templates (Kill Frenzy, Sniper Weekend, Long Range Hunt, Headshot Hunt,
Streak Master, Weapon Challenge, Faction Showdown). Each one maps to an event type
the scorer already supports. An event can be scheduled up to 60 days ahead and run
for 15 minutes to 14 days, with prizes of up to 1,000,000 points each. Event
configs are decoded strictly, so an unknown field is rejected. When `announce` is
set (the builder's default), Champion posts an "upcoming" card and a "started" card,
each exactly once, to the `🏁・events` channel (route `EVENTS`), and the results go
there when the event ends. Channel setup creates the channel; an installation set up
before it existed keeps getting event cards in Server Status until setup runs again.
The existing event engine still handles scoring, finishing and prize payout.

## Wipe & season planner

- **Stats season rollover.** Archives the active stats season the same way
  `/season end` does (results row, then the completion card), then starts the named
  new season.
- **Ranked reset.** Runs per server. It archives the server's Ranked season and
  starts a new one with the same RP per kill and the same thresholds. The planner
  never sets or changes Ranked rules.
- **Limits.** Only one pending change of each kind per server. The scheduler claims
  each change once. If a change is interrupted while running for more than 10
  minutes, it is marked FAILED and is never run again, so a reset can't happen
  twice.

## Invite tracking

The bot now also requests the **Guild Invites** gateway intent. This intent is not
privileged. Champion keeps each guild's invite use counts. When a member joins,
Champion credits the one invite whose count went up, or a last-use invite that
disappeared. If more than one invite could match, the join stays unattributed
instead of being guessed. Reading invites requires the bot's **Manage Server**
permission. Without it, joins are recorded as "unknown invite" and the report says
why. For each invite the report shows joined, stayed, linked a game account, played
after joining, and active in the last 14 days.

## Rewards automation

There are three reward rules:

- **Ranked tier reached.** Paid once per season per tier.
- **Weekly activity.** Paid for each completed ISO week, Monday 00:00 UTC.
- **Stats season top N.** Paid for each season that ended.

The scheduler checks the rules every 10 minutes. Payouts are `SYSTEM_REWARD`
ledger rows with a `reward:...` reference, so each one is paid at most once. History
from before a rule was created is not paid. The one exception is a Ranked tier that
a player already holds in the current season. Supporter tiers multiply rewards.

## Player timeline

The timeline shows sessions (connect/disconnect), kills, deaths, other deaths,
warnings, purchases, staff point adjustments, faction joins and leaves, and name
changes. It is sorted newest first, can be filtered by kind, and pages with
`before`. It never includes map positions. Name changes are recorded by a trigger
added in `0108`, so only changes from that migration onward appear. Session rows
follow the location-event retention.

## Staff activity

Built from `admin_audit_log`. It shows each staff member's actions grouped by
category (`internal/staffactivity`), actions per day, and a filtered list paged by
id. Before/after snapshots are never returned here. They are only in the Audit Log
tab.

## Zone & base map (website only)

Live Ops → Zones shows every zone to scale on a 1 km grid of Chernarus (15,360 m)
or Livonia (12,800 m). It can also show fights from the last 7 days. Clicking the
map places or moves a zone, and saving goes through the existing zone API. A zone's
type can't be changed after it is created, because the API doesn't support it.

## Supporter & VIP tiers

A tier has:

- a killfeed badge, added to the killer's kill cards
- a display colour, used in the Client Hub
- an optional Discord role
- a reward multiplier from 1.0 to 3.0

A player can hold only one active tier. The Discord role goes to the player's
verified Discord link. Adding or removing the role requires **Manage Roles**, and the
bot's role must sit above the VIP role. If a role change fails, Champion records the
error, shows it next to the member, and does not retry. Expired memberships end on
the scheduler tick and lose the role.

## Scheduler scope

Event announcements, the season planner, VIP expiry and rewards all run on the
competitive scheduler tick, every 45 seconds. That tick runs for the bot's
configured guild.
