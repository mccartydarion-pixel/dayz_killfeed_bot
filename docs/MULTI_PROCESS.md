# Running more than one bot process

Production runs **one** bot process (one Railway replica). This page says what happens when two
processes run against the same database anyway - a deploy overlap, a scale-up, a second service
started by mistake - and what is and is not safe.

Short version: the periodic workers that would duplicate Discord posts, DMs or Nitrado calls now
run in one process only. The log pipeline and the Discord gateway are **not** covered: a second
long-lived process still doubles the killfeed. Keep one replica; a short deploy overlap is fine.

## The leader lock

`internal/leader`. One process is the **leader**. It holds a PostgreSQL session-level advisory
lock (`pg_try_advisory_lock`, key `leader.SingletonLockKey`) on a **dedicated connection** - not
one from the pool, and not the migration lock, which is a transaction-level lock on another key.

- The process that holds the lock runs the singleton workers (below).
- A process that does not hold it tries again every **15 seconds** and takes over when the
  holder's connection ends. PostgreSQL drops a session lock when its connection ends, however it
  ends, so a crashed leader frees it without a timeout.
- The leader checks every **5 seconds** (5 s limit per check) that its session still holds the
  lock. When the check fails - the connection dropped, the database restarted - the process stops
  being leader at once: every singleton worker's context is cancelled. It then tries to take the
  lock again straight away, and after that every 15 seconds; the workers start again when it has it.
- On shutdown the lock is released first, before the feeds are flushed and the database closed, so
  a standing-by process takes over within 15 seconds.
- Start-up with one process: the lock is taken in the first few milliseconds; nothing waits for it.
- The HTTP API, the Discord commands and the log pipeline do not depend on leadership. They run in
  every process.

It costs one extra database connection per process (application name `champion-leader-lock`).

### What you see

- `GET /api/runtime/status` → `leadership`: `{enabled, leader, since, acquisitions, lastError}`
  for the process that answered (docs/runtime-status-api.md).
- Health component `singleton_leader` (admin health output): `leader since …`, `standby since …:
  another process runs the singleton workers` (healthy), or **DEGRADED** `not leader since …:
  cannot take the leader lock (…)` when the lock cannot be reached at all.
- Logs, `component=leader`: `leadership_acquired`, `leadership_lost` (warn, with the error),
  `leadership_released` (shutdown), `standby` (once, when another process holds the lock),
  `acquire_failed` (warn), `worker_stopped`.

### Switching it off

`SINGLETON_LEADER_LOCK=off` disables the election: the process always acts as the leader, exactly
as before the lock existed. Use it only with a single process, and only if the database cannot
give a session lock - a pooler in **transaction mode** (PgBouncer) hands every statement to a
different backend, so a session lock means nothing there. With such a pooler and the lock left on,
the 5-second check does not find the lock and leadership is lost and retaken in a loop
(`leadership_lost … leader lock is not held by this session`): visible, not silent. Point
`DATABASE_URL` at a direct or session-mode connection instead.

### Limits

- A leader cut off from the database notices within about 10 seconds (check interval plus the
  check's limit). If PostgreSQL has already dropped its session and another process took the lock
  in that window, both run the singleton workers for those seconds.
- While **no** process can reach the lock, no singleton worker runs anywhere. That is the intended
  failure mode (they all need the database), and it shows as `singleton_leader` DEGRADED.

## Every background worker

"Gated" = runs only in the leader. "Safe" = already correct with two processes, for the reason
given; it runs in every process. "Not safe" = still assumes one process.

| Worker | Where | With two processes |
| --- | --- | --- |
| Server name sync (Nitrado name read, ~20 min) | `server_name_sync.go` | **Gated.** Had no claim: every process repeated every Nitrado read. |
| Competitive scheduler (45 s): completion-announcement recovery, event scheduler and finalization, event announcements, season planner, perk store, RP boost and ranked bonus announcements, feature upgrades (priority rewards, win-back, season rewards, rent reminders, daily play reward, shop order updates, security digest, event scoreboards, perk reminders, spotlight), territory / challenges / battle pass, VIP expiry, rewards, hot zones, bounty expiry | `runCompetitiveSchedulers` | **Gated** as one loop. Several steps claim their rows (completion announcements, season planner, bounty sweep) but many post or pay from a plain "due → act → mark" pass. |
| Auto leaderboard refresh | `discord.LeaderboardScheduler` | **Gated.** Two processes raced to create/edit the same message. |
| Server ranks boards (one per server) | `discord.ServerRanksBoard` | **Gated.** Same. |
| Bounty board, server status board, heatmap board | `discord.BountyBoard`, `ServerStatusBoard`, `HeatmapBoard` | **Gated.** Same. |
| Route syncer (guild-level routed panels) | `discord.RouteSyncer` | **Gated.** Same. |
| Security Store panel refresh (10 min) | `security_store_panel.go` | **Gated.** Two processes would both post a panel that has no message yet. |
| Security service expiry DMs (5 min) | `security_expiry_worker.go` | **Gated.** The DM is sent before the row is marked. |
| Base rent reminders and the paused-bases staff digest (10 min) | `saas_api_base_rent.go` | **Gated.** Same. |
| Verified-role reconciler | `runRoleReconciler` | **Gated.** Role assignment is idempotent, but both processes made every Discord call. |
| Owner-ops monitor (2 min) | `owner_ops_worker.go` | **Gated.** Its actions and notices were already claimed with conditional `UPDATE`s, but each process judged incidents from its own workers, so two could open and resolve the same incident against each other. |
| Online players voice counter - renaming the channel | `online_counter.go` | **Gated** (the rename only). Every process still evaluates the count, because its own API and health output answer from it; a new leader publishes at once. |
| Map rotation worker (1 min) | `map_rotation_worker.go` | **Safe.** One pass per installation runs under a lease (`map_rotation_settings.lease_owner/lease_until`, 5 min, longer than the 4 min pass). Vote transitions, the switch and the notices all happen inside that lease; one `PENDING` switch per installation is also a unique index. |
| Shop automatic delivery worker | `shop_delivery_worker.go` | **Safe.** Per-installation lease (`FOR UPDATE SKIP LOCKED`). |
| Shop order desk (buyer DMs, ticket channels) | `shop_order_discord.go` | **Safe.** Rows claimed with `FOR UPDATE SKIP LOCKED` and a 5 min lease. |
| Shop confirmation sweeper | `saas_api_shop_confirmation.go` | **Safe.** Each row closes once. |
| C.A.S.E. staff alerts | `case_staff_alert_worker.go` | **Safe.** Unique incident key on insert, leased delivery (`FOR UPDATE SKIP LOCKED`). |
| C.A.S.E. digest delivery | `case_digest_delivery_worker.go` | **Safe.** Outbox rows claimed with `FOR UPDATE SKIP LOCKED` and a claim version. |
| Faction Hub achievements: pending evaluation, daily reconcile | `factionstats` | **Safe.** Unlock rows are unique; nothing is announced. |
| Faction logo orphan sweep, Base Black Box prune, location / live sync / data retention | several | **Safe.** Idempotent deletes; a second process only repeats the query. |
| Shop/economy/bounty feeds, staff alerts publisher, life recap, zone and bounty DMs | `discord.EconomyFeed`, `BountyTracker`, `AdminAlertPublisher`, `LifeRecapNotifier`, … | **Safe, and must not be gated.** They send what *this* process's own requests and events queued; gating them would lose a follower's messages. |
| Health refresh, Discord presence, feature-flag / owner-access caches | `refreshHealth`, `PresenceManager` | **Safe.** Per-process state only. |
| **The log pipeline**: one worker per server under `servers.WorkerManager` - the ADM engine, persistence and location queues, killfeed / death / hit / build / connections / PvE feeds, ADM monitor message, base raid alarm, perimeter watch, black box recorder, ranked award reconcile, and the Live Sync supervisor (RPT, script, crash, restart.log) | `runServerWorker` | **Not safe - left alone.** See below. |
| **Discord gateway handlers**: slash commands, buttons and modals, member-join welcome, invite tracking | `App.Run` | **Not safe - left alone.** See below. |

### Still not safe with two processes

**The log pipeline.** Every process starts a worker per active server, and each worker has its own
supervisor (`WorkerManager`: restart with backoff, a restart gate on the server row, an explicit
connect / disconnect / repair API). Its in-memory state - the player tracker, the diagnostics, the
queues - is what that process's HTTP API and health output answer from, and connect / repair
requests arrive on whichever process the request reaches. Gating it to the leader would leave the
other process's API with no worker to report on or to restart, so it is not gated. With two
processes:

- stored data stays correct: kills, deaths, locations and live sync records are inserted with
  deterministic identities and `ON CONFLICT DO NOTHING`, and checkpoints only move forward;
- **Discord output is doubled**: each process posts its own killfeed, death, hit, build and
  connection cards and its own alarm DMs, and both edit the ADM monitor message;
- Nitrado is read twice.

**Discord gateway events.** Each process opens its own gateway session with the same bot token and
receives every event. An interaction is answered by whichever process acknowledges first (the
other's reply fails with "already acknowledged"), but a member-join welcome and invite tracking
run in both.

**Triggered refreshes.** A change made through a process that is not the leader (a saved channel
route, a new bounty) only marks that process's board as due. The leader's board picks it up on its
own schedule (seconds to minutes, depending on the board), not at once.

**In-memory caches** have no cross-process invalidation (docs/PERFORMANCE.md).

So: two processes for the length of a deploy overlap cost a few doubled kill cards at most. Two
processes for good are not supported until the log pipeline has an owner per server.
