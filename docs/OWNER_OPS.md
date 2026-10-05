# Owner Hub operations

What the platform owner uses to run Champion as a business: configuration checks, a fleet wall,
customer health, the onboarding funnel, revenue, broadcasts, a read-only "view as customer"
session, a daily briefing, and a monitor that can heal broken workers by itself.

Code: `internal/ownerops` (the decisions, pure functions), `internal/app/admin_api_ownerops.go`
(routes), `internal/app/owner_ops_worker.go` (the monitor), `internal/repository/platform_ops_*.go`
(storage and cross-tenant facts). Migration `0103_owner_ops`.

Every route is under `/api/admin` and goes through `adminRoute` (docs/ADMIN_API.md): service
secret, acting user, platform owner or platform staff. Staff can read every page here and change
nothing; every write is owner only, needs a `reason` and is recorded in `platform_audit_log`.

## What is on, and what is off

Watching is always on: the monitor reads state and keeps the incident list. It changes nothing
else.

Everything that **acts** on a customer's installation or **sends a message** is off until the
owner switches it on (`PUT /api/admin/automation`):

| Switch | Default | Effect when on |
| --- | --- | --- |
| `alertsEnabled` | off | DM the platform admins when an incident opens or resolves |
| `selfHealEnabled` | off | Restart a missing or stalled worker automatically |
| `customerNoticesEnabled` | off | DM an organization's owner when only they can fix it, and when it is fixed |
| `briefingEnabled` | off | DM the platform admins the daily briefing at `briefingHourUtc` (default 13) |

A stored settings row this code cannot read counts as everything off.

## Read models

All `GET`, all computed on request from stored and in-memory state. No live Nitrado or Discord
call. No response carries a Stripe id, a credential or a raw log line.

`GET /api/admin/config-check` - `switches` (plan gating, live sync, custom embeds, C.A.S.E.,
Stripe checkout, the automation switches, the feature-flag environment switches), `checks`, and
`liveInstallations` (each organization's set-up installations with their server and whether a
worker is running). Each check is `{id, severity: OK|INFO|WARN|FAIL, title, detail, items}`:

| Check | Fails or warns when |
| --- | --- |
| `catalog_configured` | no plan catalog, or a public plan has no price |
| `subscription_price_in_catalog` | a Stripe-billed subscription is on a price the catalog does not list (its plan then comes from stale checkout metadata) |
| `subscription_plan_matches_price` | a subscription's stored plan is not the plan its price belongs to |
| `subscription_plan_known` | a plan set without Stripe is not a catalog plan |
| `plan_gating_reach` | info: who is on Survivor while gating is on |
| `expired_trials` | info: trials past their end date |
| `live_installations_have_worker` | a set-up installation has no running worker |
| `one_installation_per_server` | two non-suspended installations point at one game server |
| `live_installations_per_organization` | info: an organization runs several live installations |

The price id is read to resolve the plan and is never returned.

`GET /api/admin/fleet` - every installation: feed state, worker, ADM source state, last poll,
last kill, kills in 24 hours, players online, active players this week and last, Nitrado link
status, open incident kinds. Sorted worst first.

| Feed state | Meaning |
| --- | --- |
| `NO_WORKER` | set up, but nothing is reading its logs |
| `STALLED` | the worker runs but completes no poll cycle |
| `DEGRADED` | Nitrado errors, or the log is behind |
| `LIVE` / `QUIET` | healthy; `QUIET` means the log has simply not changed |
| `SETTING_UP` | setup not finished; no feed is expected |
| `SUSPENDED` | suspended by the owner |

`GET /api/admin/customer-health` - every organization scored 0-100, worst first, with the
`factors` that took points off (feed down, players fell, owner away, trial ending or expired,
past due, set to cancel, no plan, open incidents). Below 50 is `AT_RISK`, below 75 `WATCH`. An
organization is judged by its best installation, so spare installations do not drag it down. A
server that had fewer than five active players last week is too small for the player trend to
count.

`GET /api/admin/funnel?days=1..365` (default 30) - how far the organizations created in the
window got: organization created, Discord connected, Nitrado connected, server selected, setup
complete, first kill. A later step implies the earlier ones. `stuck` lists every organization
(any age) that has not reached its first kill and has not moved for a day, most recently active
first, with the step it needs next and the plan it picked at checkout (`intendedPlan`).

`GET /api/admin/revenue` - `mrrCents` from the catalog price of each active or past-due Stripe
subscription (a yearly price counts a twelfth), by plan, plus C.A.S.E. add-ons; `counts`
(paying, trial, expired trials, past due, set to cancel, canceled in 30 days, owner grants,
`unpriced` = active Stripe subscriptions whose price is not in the catalog); trial conversion;
and the payments actually recorded per month for 12 months (`billing_transactions`). MRR is what
the catalog says should be billed; the monthly series is what was collected.

`GET /api/admin/briefing` - today's briefing as text and as numbers, whether or not sending is
switched on.

## The monitor and self-healing

`runOwnerOps` ticks every two minutes. It concludes nothing during the first three minutes after
a start, so a deploy never looks like an outage and never closes an open incident.

For every set-up installation with an active server it derives the incidents its state implies:

| Kind | Opens when | Healed by |
| --- | --- | --- |
| `WORKER_DOWN` | no log worker is running | restarting the worker |
| `FEED_STALLED` | the worker completes no poll cycle, **or the feed is silent** (below) | restarting the worker |
| `NITRADO_ACCESS` | Nitrado keeps refusing the saved token (authentication or permission) | the customer reconnecting Nitrado |
| `DISCORD_ACCESS` | the bot is no longer in the Discord server | the customer adding it again |

A problem must last four minutes before it becomes an incident. There is at most one open
incident per installation and kind. When the state clears, the incident resolves with how it
recovered ("recovered on its own", "recovered after 2 automatic restart(s)").

With `selfHealEnabled`, a worker is restarted at most three times per incident, at least ten
minutes apart. Each restart is claimed with one conditional `UPDATE`, so two instances never
both act (and the monitor itself runs only in the process holding the leader lock,
docs/MULTI_PROCESS.md), and is audited as `incident.self_heal` by actor `system`.

With `alertsEnabled`, the platform admins are DMed once when an incident opens and once when it
resolves. With `customerNoticesEnabled`, the organization's owner is DMed once for the kinds only
they can fix, with a link to their dashboard, and once more when it is fixed. Nobody is told
something resolved unless they were told it broke.

### Silent feeds

A worker can run, poll and still deliver nothing. Those cases are raised as `FEED_STALLED` too,
so the platform owner hears about a dead feed before the customer asks. The incident's detail
says which case it is. Code: `ownerops.SilentFeed`, fed by the feed watch
(`internal/app/feed_watch.go`), which samples every worker each 30 seconds and keeps its clocks
in memory only.

| Case | Opens when |
| --- | --- |
| Players online, log silent | at least one player has been online without a gap for **20 minutes** and not one new log line was read in those 20 minutes, on a server that has been seen writing player lists (such a server writes one every five minutes, so silence cannot be a quiet evening). On a server never seen writing a player list the wait is **60 minutes**, because a lone idle player really can produce no line; the detail then says the server may only be idle with its player list log off |
| Log unreadable | every read of the server log has failed for **15 minutes** with a Nitrado error that is not a refused token (a refused token is `NITRADO_ACCESS`) |

It never opens:

- for a suspended installation, one whose setup is unfinished, or a deactivated server (nothing
  is expected of them);
- for an empty server, or while the player count is unknown;
- while Nitrado says the game server is stopped;
- during the first minutes after a bot restart: the monitor concludes nothing for three minutes,
  and every clock above starts again when the process starts, so the earliest a silent feed can
  open after a deploy is 15 minutes (unreadable), 20 minutes (silent, once a player list has been
  read since the restart) or 60 minutes (silent from the very start), plus the grace period.

Like every incident it then needs the four-minute grace period, opens once per installation,
resolves by itself when a log line arrives (or the players leave, or the reads work again), and
follows the same switches: `alertsEnabled` DMs the admins once on opening and once on resolving,
`selfHealEnabled` restarts the worker (at most three times, ten minutes apart). No customer is
messaged for it.

The player count is the online counter's reading (Nitrado's live count) for the server it
watches, else the worker's own count once a player list or a server restart proved it
(docs/ONLINE_COUNTER_AND_LINK_CHECK.md). One failed count read neither starts nor resets the
20/60 minutes.

Known limit: on a server with the player list log switched off, one player idling for an hour
with nobody connecting, fighting or building opens the incident although nothing is broken. The
same setting leaves that customer's live map empty; their own status page tells them how to
switch it on (docs/SERVER_STATUS.md, `positionLogging`).

### Deploy self-check alert

Not an incident (it belongs to the bot process, not to an installation), but it uses the same
admin DMs and the same `alertsEnabled` switch: if a newly started bot process is not healthy five
minutes after it started, the platform admins get one DM saying what is wrong, and one more if it
recovers within the hour. See docs/DEPLOY.md.

`GET /api/admin/incidents?status=OPEN|RESOLVED`, `POST /api/admin/incidents/{id}/resolve` closes
one by hand (if the problem is still there a new incident opens after the grace period).

## Broadcasts

`POST /api/admin/broadcasts` `{reason, title, body, severity: INFO|MAINTENANCE|INCIDENT,
audiencePlan, postToDiscord, days}` publishes a notice. `audiencePlan` empty means everyone;
`days` 0 means until ended by hand (max 30). `POST /api/admin/broadcasts/{id}/end` takes it down.

Customers read theirs with `GET /api/saas/organizations/{organizationId}/broadcasts` (any
member); the website shows them as a banner in the Client Hub.

With `postToDiscord`, a copy goes to each set-up installation's staff alerts channel
(`ADMIN_ALERTS` route) in the audience, with every mention disabled. An installation that routed
no such channel is recorded as `NO_CHANNEL` and skipped - a notice never lands in a public
channel. `platform_broadcast_deliveries` makes each copy at-most-once.

## View as customer

`POST /api/admin/organizations/{organizationId}/view-as` `{reason}` returns the organization
owner's Discord id and is audited as `organization.viewed_as`. The website then makes its
`/api/saas` reads as that user for 30 minutes and sends `X-Champion-Impersonator: <admin id>` on
every such request.

The backend enforces the read-only rule itself (`requireSaaSServiceAuth`): a request carrying
that header must name a platform admin and must be `GET` or `HEAD`; anything else is `403`. The
website also refuses to send a write in such a session, so a mistake on either side is caught
by the other. No credential is involved and nothing is written on the customer's behalf.

## Daily briefing

With `briefingEnabled`, the first tick at or after `briefingHourUtc` builds the briefing and DMs
it to the platform admins. `platform_briefings` has one row per day, which is what makes it
once a day across instances.
