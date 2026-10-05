# Champion Performance (Phase 1: runtime pipeline tuning)

This document is the result of a full, code-grounded audit of the live event pipeline (Nitrado ADM
polling -> parse -> persist -> route -> Discord) plus the SaaS/website-facing API, done in response
to reports that logs feel slow, Discord channels feel delayed, and events sometimes feel out of
order. It records what was actually measured, what was actually changed (and why), and - just as
important - what was audited and left alone because the evidence didn't support changing it. No
number in this document is fabricated; every measured value states exactly how it was measured and
its limitations.

## 1. The pipeline

```
Nitrado API -> ADM discovery/download -> parse -> dedupe -> persist -> route resolve -> publisher queue -> Discord API -> message visible
```

Concurrency model (internal/servers/workermanager.go, internal/app/server_worker.go `runServerWorker`): **one
goroutine per game server**, each with its own `killfeed.Engine` (own tracker, own deduplicator, own
`PersistenceQueue`) and its own Discord publishers. There is no shared poll loop and no shared
work queue across servers - this was already true before this phase and is the structural reason one
slow/misbehaving server cannot block another (see section 8 "cross-server parallelism").

## 2. ADM polling and download

- **Cadence**: `NITRADO_POLL_INTERVAL` (default `killfeed.ADMRemoteMetadataPollInterval` = 10s) while
  actively polling a selected log; `5s,10s,20s,30s` (capped) exponential backoff while in discovery
  after failures (`internal/killfeed/engine.go`). A background rescan for a newer log happens on the
  same 10s cadence, not more often.
- **Discovery cost**: a full directory walk (`ListLogs`) runs only during discovery/rediscovery, not
  on every polling tick - once a log is selected, the engine switches to a cheap single-file `Stat`
  per tick and a scoped (not recursive) directory listing every `rescanInterval` (10s). Detailed
  candidate metadata logging is already capped to the newest 5 candidates per discovery pass.
- **Download**: `Client.ReadLog` downloads the **entire** ADM file's current bytes on every poll that
  detected a size/mtime change - there is no HTTP `Range` request anywhere in `internal/nitrado`.
  Parsing itself is already correctly incremental (only the byte range after the last checkpoint
  offset is parsed - `tracker.DrainCompleteLinesWithOffsets`), so the waste is specifically in the
  network transfer, not in CPU/parse time.
  - **Addressed in Champion Performance Phase 1.5** (`docs/NITRADO_DELTA_READS.md`): a partial-read
    path using Nitrado's own `file_server/seek` and signed-URL offset/count mechanisms (HTTP `Range`
    only as a third fallback, never assumed supported) now fetches just the bytes after the last
    checkpoint offset, gated behind `NITRADO_DELTA_READ_MODE` (default `off` - this phase shipped the
    capability with the full-download path still authoritative; enabling it is a separate rollout
    decision). See that document for the full design, correctness guarantees, and rollout plan.
  - Existing checkpoint/rotation/truncation/alt-probe recovery logic (`adm_alt_probe.go`,
    `canonical_source.go`) is untouched.
- **Duplicate/derived requests audited**: `readRemoteFile` already does two calls to fetch one file's
  content (get a signed download token, then fetch it) - this is Nitrado's own API shape for a signed
  URL, not a Champion-side redundancy, and was left alone (no cache of a signed URL is safe: they are
  short-lived by design). Candidate deduplication (`canonicalADMID`) already collapses `ftproot`/
  `noftp` mount aliases into one logical candidate before ranking, specifically to avoid double-counting
  the same physical file - confirmed correct, not a bug.

## 3. Persistence

Kill/death/connect/disconnect events are persisted one at a time via a single bounded channel + one
consumer goroutine per server (`killfeed.PersistenceQueue`, capacity 500, `persistEnqueueTimeout` =
30s). This was already correctly serialized per-server (never cross-server) before this phase.
Duplicate protection is a database `UNIQUE(guild_id, event_fingerprint)` constraint on `kills` and
`deaths`, caught via a Postgres `23505` error rather than `ON CONFLICT DO NOTHING` - functionally
correct (each insert is its own implicit transaction, so a duplicate never aborts an unrelated write),
just a small avoidable per-duplicate cost; left unchanged this phase as a documented, low-value,
non-correctness finding rather than a required fix.

**Fingerprint building** (`internal/killfeed/dedupe.go` `joinParts`) was changed from repeated string
concatenation (`out += p` in a loop, reallocating on every part) to `strings.Join` - a mechanical,
behavior-preserving allocation reduction on the per-event hot path. Covered by the existing dedupe
test suite (`TestDeduplicatorDropsRepeatedKill` et al., all still pass).

## 4. Ordering guarantees

**Audited, not redesigned.** Every runtime publisher (killfeed/deathfeed via `RotatingFeed`, hitfeed,
connections, bounty tracking, economy feed) already follows the same single-writer pattern: `Enqueue`
only appends to an in-memory slice/map under a mutex; **all actual Discord I/O happens exclusively on
that publisher's own one `Run(ctx)` goroutine**, which drains its queue in the order items were
enqueued. This means:

- **Within one server**, message order to Discord is guaranteed by construction - there is no second
  goroutine that could ever race a send. No new sequencer, "per-installation stream," or event-id
  ordering scheme was needed or added.
- **Across servers**, each server's publishers are independent instances with independent `Run`
  goroutines - one server's slow Discord/database never blocks another's queue from draining. This
  was already true structurally (`internal/servers/workermanager.go`) and is now covered by a
  regression test (section 8).

New regression tests (`internal/discord/rotating_feed_burst_test.go`) lock this in:
`TestRotatingFeedOrderedBurstSingleServer` (10/50/100-event bursts delivered in exact enqueue order),
`TestCrossServerParallelismSlowServerDoesNotBlockOthers` (a deliberately-hung fake Discord sink for
server A never delays server B's delivery), `TestRotatingFeedRunGoroutineIsIndependentPerInstance`.

Authoritative ordering source: enqueue order (a Go slice append under a mutex), which is itself driven
by ADM parse order, which is itself driven by the durable byte-offset checkpoint - never `time.Now()`
wall-clock ordering. No fragile timestamp-based reordering was found or introduced.

## 5. Discord delivery, rate limits and retries

- **Rate limiting**: Champion relies entirely on discordgo's own built-in bucket-based rate limiter
  (used transparently by every `ChannelMessageSendComplex`/`ChannelMessageEditEmbed` call) - audited,
  confirmed sufficient, and **no second/conflicting rate limiter was added**, per this phase's own
  instruction not to build one without evidence discordgo's isn't enough. No evidence of duplicate or
  reordered messages caused by rate limiting was found.
- **Retries**: outside of one existing, narrowly-scoped 429/5xx retry in `VoiceChannelCounter`
  (online-count channel renaming), publishers do not retry a failed Discord send - a failure is logged
  and the item is dropped from that flush (the underlying event is already durably persisted, so no
  *data* is lost, only a Discord notification of it). Building a generalized retry/backoff framework
  across every publisher was **not** undertaken this phase: it is a real, bounded, multi-file change
  with its own ordering-preservation requirements (section 10 of the task), and there is no evidence
  from this audit that transient Discord failures are actually causing user-visible harm today (no
  reports of missing killfeed messages, only *delay/order* complaints, which trace to sections 2/6
  below instead). Flagged as a legitimate follow-up, not implemented blind.
- **Persistent panels** (leaderboards, admin logs status panel, link panels) already **edit** an
  existing message rather than delete+recreate, confirmed by direct code read
  (`ChannelMessageEditEmbed` call sites in `panels.go`, `leaderboard_scheduler.go`, `adm_monitor.go`) -
  no change needed. `RotatingFeed` (killfeed/deathfeed) intentionally deletes+reposts each cycle - a
  different, deliberate UI pattern (a rotating window of the last N items), not the persistent-panel
  case section 29 was concerned about.

## 6. Admin log noise (fixed this phase)

**Root cause found and fixed**: `ADMMonitorPublisher.HandleDownload` (`internal/discord/adm_monitor.go`)
posted a **brand-new** standalone Discord message for every `"success"`/`"success_no_new_events"`
download report - i.e. roughly once per poll cycle (as often as every ~10s) during active gameplay,
to the admin-logs channel. It also unconditionally set `forceRefresh = true` on every call, which
caused the *separate* persistent status panel (`Update()`) to bypass its own 5-minute refresh
throttle on every single poll too - compounding the noise.

**Fix**: a download report now only posts (and only forces the status panel to refresh early) for a
genuine state change - `failure`, `recovered` (a `failure -> success` transition), `checkpoint_failed`,
`rotation`, or `truncated`. A routine successful download is silent; the persistent status panel
(which already shows current file/offset/health, refreshed at least every 5 minutes) is the correct
place to see routine activity on demand. Regression tests:
`TestADMRoutineDownloadDoesNotPostAMessage`, `TestADMFailureAndRecoveryStillPostMessages`,
`TestADMRoutineDownloadDoesNotForceStatusPanelRefresh` (`internal/discord/route_adm_setup_test.go`).

## 7. Log levels

Audited every `slog` call site the task flagged as a candidate for being too chatty. Most held up on
closer reading as legitimate, infrequent, meaningful state-change logs (once per log-source selection,
once per rotation, once per discovery pass with an already-capped candidate count) rather than
per-tick/per-line noise, and were **left at Info** rather than downgraded on a first-pass assumption.
One genuine per-attempt diagnostic was found and downgraded: `adm_alt_probe.go`'s `"alt_probe"` event
fires on every alternate-candidate probe attempt while the engine is in its stale-metadata fallback
path (which can repeat every `alternativeProbeInterval` while stuck there) and carries no business
meaning of its own - the actual outcome (a source switch) is already logged at Info separately via the
`"rotation"` event. Moved to Debug.

Secret-logging audit (Nitrado token, Discord token, Stripe keys, `WEBSITE_API_SECRET`,
`CREDENTIAL_ENCRYPTION_KEY`): clean. No log or print call anywhere in `internal/` interpolates a
credential value; the Nitrado token is only ever placed in an HTTP `Authorization` header, never
logged. The admin API additionally scans its own outgoing JSON for any configured secret and withholds
the whole response rather than risk a partial leak (`internal/app/admin_api.go`,
`TestAdminResponseWithheldWhenItContainsAConfiguredSecret`) - audited, unchanged, confirmed still
passing. The new slow-query tracer (section 9) was specifically designed to log only static SQL text
and never argument values, for the same reason (`TestQueryTracerNeverLogsArgumentValues`).

Existing log throttling (`RouteBinding.logFallback`: error-class logs at most once/minute, state-change
logs only on actual transitions) is a good pattern already in place; no new repetitive-error path was
found elsewhere in this audit that needed the same treatment applied to it.

## 8. Route resolution (internal/routing, internal/discord/route_binding.go)

### What routeLookupTimeout actually bounds

`routing.Resolver.Resolve`'s cache-hit path never touches its `context.Context` at all - a hit is a
map read under a lock, unconditionally. `route_binding.go`'s 2-second timeout therefore only ever
bounded a cache **miss** (first lookup for a (guild, server, routeKey), or after the 30s TTL expires),
which calls `ChannelRouteRepository.ResolveChannel` - a single joined Postgres query.

**Measured** (local Postgres 16, loopback connection, warm pool, single seeded row matching the join;
`EXPLAIN ANALYZE` against the exact production query):

```
EXPLAIN ANALYZE: Execution Time: 0.068 ms (fully index-covered - every join step is an Index Scan,
                  no sequential scan; the join's own uniqueness comes from an existing
                  UNIQUE(installation_id, route_key) constraint, so no new index was needed)
End-to-end round trip (Go client, n=500): p50≈0, p95=575.5µs, p99=648.8µs, max=1.166ms
```

This does not include Railway's real network latency between the app and its Postgres instance
(not measured in this environment - no access to that network), but gives three-orders-of-magnitude
of headroom between "normal" and the old 2-second bound.

**Change**: `routeLookupTimeout` default reduced to **500ms** (from 2s), overridable via
`ROUTE_LOOKUP_TIMEOUT`. Rationale: on a genuine database problem, the resolver has no better answer
to wait for - `RouteBinding.ChannelID()` already falls back to the documented legacy channel on any
error or timeout (never fatal, never blocks the caller past the bound). Waiting the full 2 seconds
before falling back, when the measured normal case is sub-millisecond, only makes a live publisher sit
idle for longer during exactly the failure mode this timeout exists to bound. 500ms leaves generous
margin over the measured p99 while cutting the worst-case publisher stall 4x.
`TestChannelIDBoundedByConfiguredTimeout` proves a hanging resolver returns within the configured
bound, not the old fixed 2s; `TestEnvRouteLookupTimeoutDefaultsAndOverrides` covers the env parsing
(including a floor that rejects a pathologically small override, which would defeat the resolver by
treating every miss as an instant failure).

### Cache concurrency

`routing.Resolver`'s cache was a plain `sync.Mutex`, so even a **hit** (the overwhelmingly common case)
took a full exclusive lock shared across every guild/server/route-key in the process - a real
contention point under concurrent multi-server event volume, even though the critical section itself
is tiny (a map read, no I/O). Changed to `sync.RWMutex`: a hit now takes only a read lock; only a
miss's cache write takes the exclusive lock. Benchmarked
(`internal/routing/resolver_bench_test.go`, `go test -bench . -benchmem -cpu 4`):

```
BEFORE (sync.Mutex):    BenchmarkResolverCacheHitConcurrent-4            62.96 ns/op   0 B/op   0 allocs/op
                        BenchmarkResolverCacheHitConcurrentManyKeys-4    61.85 ns/op   0 B/op   0 allocs/op
AFTER  (sync.RWMutex):  BenchmarkResolverCacheHitConcurrent-4            42.69 ns/op   0 B/op   0 allocs/op
                        BenchmarkResolverCacheHitConcurrentManyKeys-4    43.31 ns/op   0 B/op   0 allocs/op
```

~32% faster under 4-way concurrent load, zero additional allocations either way - both numbers are
already far under the <5ms cache-hit target from before this change; the RWMutex change is about
concurrent scalability across many simultaneous callers (many servers resolving routes at once), not
single-call latency, which was never the bottleneck. `go test -race ./internal/routing/...` passes.

### Cache invalidation

Audited, unchanged, confirmed correct: an in-process route save calls `resolver.InvalidateAll()`
synchronously (`internal/app/saas_api_channels.go`'s `completeChannelsStep`), so the *same* process's very next
lookup sees the new route immediately - proven by the existing `TestResolverInvalidateAllIsImmediate`.
A **different** process (another Railway replica) has no cross-process invalidation channel and relies
purely on the 30s `DefaultTTL` (matched by `RouteSyncer`'s own 30s resync interval) - this was already
an explicit, documented design choice (`resolver.go`'s own doc comments), not an oversight, and is
restated here rather than changed: building real cross-process invalidation (pub/sub, a shared
version counter) is a materially larger change than this phase's scope justifies without evidence
multi-replica staleness is an actual user-visible problem today.

### adm_monitor.go's activeChannel()

Re-evaluates the route on every call, as documented - but a call is just one resolver `Resolve`
(a cache hit is the map-read cost measured above), so this was confirmed already cheap and was **not**
changed; adding a second cache in front of the resolver's own cache would only risk the two caches
disagreeing after an invalidation, which is exactly the kind of duplicate-inconsistent-cache the task
warned against building.

## 9. Database

### Slow-query instrumentation (new)

There was **no query-timing instrumentation anywhere in this codebase** before this phase - confirmed
by a full audit of `internal/database` and `internal/repository` (zero `duration`/`elapsed`/`EXPLAIN`
usage near any `slog` call). `internal/database.Connect` now installs a `pgx.QueryTracer`
(`internal/database/database.go`) that wraps every `Query`/`QueryRow`/`Exec` call transparently -
**no call site in any repository changed**. A query at or above `SLOW_QUERY_THRESHOLD_MS` (default
250ms, per the task's suggested starting point) logs at WARN with its static SQL text (whitespace-
collapsed, capped at 300 characters) and duration; everything else logs at DEBUG, never INFO (so this
adds no log volume at the default level). **Query argument values are never logged** - the tracer
only ever captures SQL text and timing, so no parameter value can leak through this path regardless of
what table it belongs to (`TestQueryTracerNeverLogsArgumentValues`). Cumulative counters (total
queries, slow-query count, average duration) are exposed via `DB.QueryStats()` and surfaced in the
internal performance snapshot (section 11).

### Connection pool

`pgxpool` settings (`MaxConns=10`, `MinConns=1`, `MaxConnLifetime=30m`, `MaxConnIdleTime=5m`) were
previously hardcoded with no `HealthCheckPeriod` set at all (silently defaulting to pgx's own 1-minute
default). All five are now configurable via `DATABASE_MAX_CONNS` / `DATABASE_MIN_CONNS` /
`DATABASE_MAX_CONN_LIFETIME` / `DATABASE_MAX_CONN_IDLE_TIME` / `DATABASE_HEALTH_CHECK_PERIOD` - **every
default is exactly the previous hardcoded/implicit value**, so an operator who sets none of them sees
no behavior change. `DB.ExtendedPoolStats()` exposes `pgxpool.Stat()`'s fuller shape (acquire count,
empty-acquire count, cumulative acquire wait duration) - the specific signal that indicates the pool
itself is undersized (a pool can look "full of idle connections" while every acquire still waits, if
`MaxConns` is simply too low for peak concurrency), which the pre-existing `PoolStats()` (total/idle
only) could not show. Phase 1 left the default at 10 pending evidence; the query-hygiene pass
(section 17) raised the defaults to `MaxConns=25`, `MinConns=2` - the env overrides are unchanged.

### Claim/worker contention

Audited: **no shared "claim a batch of rows" pattern exists anywhere in this codebase** (no
`SELECT ... FOR UPDATE SKIP LOCKED`, no outbox/job table). This is not a gap that needed fixing for
today's architecture - persistence is already serialized per-server through `PersistenceQueue`'s own
single-consumer channel, so there is no multi-worker contention over the same rows to eliminate. This
would only become relevant if a future phase introduces a shared multi-instance worker pool competing
over one queue table, which does not exist today.

### Query plan / index audit

The one hot query benchmarked in depth (route resolution, section 8) is fully index-covered with no
sequential scan, confirmed by `EXPLAIN ANALYZE` against the live schema - no new index was added
because none was shown to be missing. Existing indexes on `kills`/`deaths` (by guild+killer/victim,
by guild+season, by guild+server+time, plus the fingerprint uniqueness index) were reviewed and found
to already cover the query shapes used by the runtime and the SaaS/admin read APIs; no speculative
index was added anywhere.

## 10. Intentional aggregation windows (not lag - documented, not changed)

- **Hitfeed** (`internal/discord/hitfeed.go`): an encounter closes and ships once `hitfeedWindow`
  (5s) has elapsed since its first hit, checked every `hitfeedTick` (2s) - so a hit card can take up to
  ~7s to post in the worst case. **This is deliberate aggregation** (grouping a flurry of hits between
  the same two players into one card), not processing lag; it was not changed silently, per this
  phase's own instruction. Both constants are named, not magic numbers, but are not yet
  environment-configurable - left as-is (no evidence a specific different value is wanted; changing UX
  behavior is out of this phase's scope per "no user-facing feature changes").
- **Connections** (`internal/discord/connections.go`): batches up to `connectionsLinesPerMessage` (20)
  connect/disconnect lines into one embed, flushed every `connectionsTick` (2s) - also deliberate
  batching, also left unchanged.

## 11. Observability snapshot

`GET /api/admin/health` (platform-admin only, already gated by `requirePlatformAdmin` - never a
customer-facing surface) now includes a `performance` object:

```json
"performance": {
  "database": {
    "poolTotalConns": 3, "poolIdleConns": 2, "poolMaxConns": 25, "poolAcquiredConns": 1,
    "poolAcquireCount": 1042, "poolEmptyAcquireCount": 0, "poolAcquireDurationMs": 12,
    "queryTotal": 8841, "querySlow": 0, "queryAvgDurationMs": 0.31
  },
  "routing": { "cacheHits": 512, "cacheMisses": 9, "cacheHitRate": 0.9826, "cacheEntries": 9 },
  "tableSizes": [ { "table": "kills", "totalBytes": 734003200, "tableBytes": 402653184, "indexBytes": 331350016, "rowsEstimate": 1830211 } ]
}
```

`tableSizes` (section 17) is the 15 largest tables on disk, biggest first, from `pg_class` only:
`totalBytes` = `tableBytes` (heap + TOAST) + `indexBytes`; `rowsEstimate` is the planner's estimate
from the last ANALYZE (`null` when there has been none), never a `COUNT(*)`. No table is scanned
and no row content is read.

Counts are cumulative since process start (not a rate) - an operator derives a rate from two reads a
known interval apart. Deliberately **not** added to `GET /api/runtime/status` (the documented,
website-facing contract in `docs/runtime-status-api.md`) - extending a customer-facing contract
without a compatibility review was out of scope for this phase; the existing platform-admin API was
the correct "internal/admin mechanism" the task asked for.

## 12. Health

Audited `internal/health` (`model.go`, `queue.go`, `workers.go`) and `internal/app/health_refresh.go`'s `refreshHealth`:
overall status already correctly accounts for persistence-queue drops/high-water (`Degraded`/
`Unhealthy` via `health.EvaluateQueue`) and ADM staleness (`ADMHealth.Evaluate`), not merely "is the
poll loop running." It does **not** currently factor in recent Discord *send* failures (only Discord
gateway connectivity). This is a real gap but redesigning the health contract - which is customer-
visible - without a compatibility review is explicitly out of scope for this phase; noted here as a
flagged follow-up, not changed.

## 13. Website/SaaS endpoint latency (section 37)

Full HTTP-level latency (JSON marshaling, Go HTTP server overhead, the website's own network hop) was
**not** benchmarked in this phase - doing so meaningfully would require the full HTTP integration
harness under synthetic load, which risks producing numbers from an all-but-empty local test database
that don't generalize to production data volumes, and this phase prioritized correctness over
fabricating a table that looks precise but isn't defensible. What **was** measured directly (same
methodology as section 8: local Postgres, loopback, warm connection, `n=300`) is the repository call
each of the highest-traffic endpoints is built on:

```
SubscriptionRepository.GetForOrganization   (backs GET .../billing/subscription)          p95=597µs  p99=651µs  max=1.01ms
OrganizationRepository.VerifyMembership     (the auth check on every org-scoped route)     p95=585µs  p99=666µs  max=774µs
UserRepository.GetByDiscordID               (backs users/sync's re-sync path)              p95=591µs  p99=639µs  max=668µs
```

All three are single indexed-lookup queries; none showed a missing index or an N+1 pattern in this
audit. This does not include Railway's network latency between the app and Postgres, or between the
website and this backend - those legs were not measurable from this environment. The clear signal from
this and section 8's route-resolution measurement: for every endpoint audited, the database round trip
itself is sub-millisecond and well-indexed; if a website consumer perceives slowness on these routes,
the cause is more likely elsewhere in the request path (network hop, JSON payload size, website-side
rendering) than a backend query cost, and profiling that would require instrumentation on the website
side, out of this backend-only phase's reach.

## 14. What changed vs. what was measured-and-left-alone

**Changed:**
- `internal/discord/adm_monitor.go`: routine ADM downloads no longer spam a standalone admin-log
  message or force the status panel to bypass its own refresh throttle (section 6).
- `internal/routing/resolver.go`: `sync.Mutex` -> `sync.RWMutex` for the cache-hit path; hit/miss
  counters added (section 8).
- `internal/discord/route_binding.go`: `routeLookupTimeout` default 2s -> 500ms, now configurable via
  `ROUTE_LOOKUP_TIMEOUT` (section 8).
- `internal/database/database.go`: pool size/lifetime/health-check now configurable (defaults
  unchanged); a `pgx.QueryTracer` adds slow-query logging and cumulative query stats with zero
  repository call-site changes (section 9).
- `internal/app/admin_api.go`: `GET /api/admin/health` gains a `performance` snapshot (section 11).
- `internal/killfeed/adm_alt_probe.go`: the per-probe-attempt log moved from Info to Debug (section 7).
- `internal/killfeed/dedupe.go`: fingerprint joining uses `strings.Join` instead of repeated `+=`
  (section 3).
- New regression/benchmark tests: `internal/discord/rotating_feed_burst_test.go`,
  `internal/discord/route_binding_test.go`, `internal/routing/resolver_bench_test.go`,
  `internal/database/database_test.go`, plus additions to
  `internal/discord/route_adm_setup_test.go` and `internal/app/admin_api_test.go`.

**Measured and deliberately left alone** (evidence didn't support a change, or the change was judged
too risky to make blind - see each section for the specific reasoning): ADM full-file re-download every
poll (section 2 - the largest remaining lever, needs live Nitrado `Range` verification first);
insert-and-catch-duplicate vs. `ON CONFLICT DO NOTHING` (section 3 - correctness-neutral, low value);
Discord retry/backoff framework (section 5 - no evidence of need, discordgo's own limiter already
relied on); most "chatty" log call sites (section 7 - closer reading showed most are legitimate,
infrequent, meaningful events, not per-tick noise); route-cache cross-process invalidation (section 8 -
already an explicit design choice, not an oversight); connection pool default size (section 9 - made
tunable, not guessed at); hitfeed/connections aggregation windows (section 10 - intentional UX, not lag);
customer-facing health contract (section 12 - would need a compatibility review this phase didn't do).

## 15. Load/burst test coverage

`internal/discord/rotating_feed_burst_test.go`:

- `TestRotatingFeedOrderedBurstSingleServer` - 10/50/100-event bursts for one server, asserts the fake
  Discord sink receives them in **exact** enqueue order.
- `TestCrossServerParallelismSlowServerDoesNotBlockOthers` - server A's Discord sink hangs
  indefinitely; server B (a second, independent `RotatingFeed`) still delivers all 5 of its messages,
  in order, while A is still blocked.
- `TestRotatingFeedRunGoroutineIsIndependentPerInstance` - one feed's `Run()` goroutine never touches
  another feed's state.

All three, plus the full `internal/discord` and `internal/routing` packages, pass under `go test -race`.
Multi-server (10 installations) and 429-response scenarios from the task's fuller list were not built
as separate synthetic harnesses this phase, since the structural property they'd test (independent
per-server goroutines; discordgo's own rate-limit handling) was already confirmed by direct code
reading rather than needing a new simulation to discover it - the two tests above exercise that
property directly rather than re-deriving it from a bigger, slower scenario.

## 16. Restart safety

See section 22: what happens to kills across a restart, a crash and a deploy overlap, with the
tests that prove it and the cases that can still double or drop.

## 17. Slash commands, buttons and forms

Discord shows "The application did not respond" if the bot does not answer an interaction within
3 s.

- **Registration.** Every `Register*` function queues its commands in a `discord.CommandBatch`.
  After every handler is installed, `Run` sends the whole set in one
  `ApplicationCommandBulkOverwrite` request. Creating the ~21 commands one at a time used to hit
  Discord's limit of about 5 creates per 20 s. Every deploy then waited ~80 s, and the commands
  registered last (`/setup`, `/link`, every button and form) were unanswered for that long. The
  overwrite also removes guild commands the bot no longer registers. An empty batch sends nothing.
  Log line: `slash commands registered count=… duration_ms=…`.
- **Timing log.** `internal/discord/interaction_timing.go` times every interaction where its HTTP
  requests leave the bot, so no handler has to opt in. It writes two log lines:
  - `interaction answered interaction="/server select" answer_ms=… gateway_ms=…` when the bot first
    answers. This line is a warning at 2 s or more.
  - `interaction completed … total_ms=…` when a deferred reply is filled in.

  Interaction tokens are only used as map keys and are never logged. Numbers in button custom IDs
  are masked.
- **Reply first.** Slow handlers call `deferEphemeral` first, which shows a private "thinking…".
  `respondEphemeral`, `respondEphemeralEmbed` and `respondLeaderboardEmbed` then edit that reply
  instead of answering twice. Deferred handlers:
  - `/server services`, `/server select` and `/server repair` (they call Nitrado);
  - `/admin leaderboard-refresh` and `/admin link-, presence- and pipeline-diagnostics`;
  - `/mybase`, `/registerbase` and the Pay rent button;
  - `/life`;
  - `/welcome status` and `/welcome test`;
  - `/link` and the link panel form.
- **Other handler changes.**
  - `/server` autocomplete reuses each guild's Nitrado service list for a minute.
  - `/admin` builds only the diagnostics section its subcommand shows (`admin.Service.StatusWith`).
  - The faction recruit Join button answers before it edits the recruit card.
- **Worker context.** `/server select` and `/server repair` start the killfeed worker with
  `context.WithoutCancel`. Before this, the worker inherited the command's 20 s timeout and stopped
  when the command returned.

## 18. Query hygiene and data growth

A second, code-grounded pass over the read paths and the tables that only grow. Migration
`0112_query_hygiene_indexes` (`internal/database/query_hygiene_schema.go`), additive only.

### Queries

- **`TopByKD`** (`internal/repository/stats_repository.go`) ran three correlated `COUNT(*)`
  subqueries per player row - the kills count twice - before the `LIMIT`. It now joins two
  pre-aggregated derived tables (kills and deaths grouped by player once, the shape
  `TopByBestStreak` already used). Results, ordering and tie breaks are unchanged; a player with
  no kills row still ranks when `minKills` allows it, and a kill without a killer counts for
  nobody. `TestTopByKDMatchesLegacySemantics` runs the old query text next to the new method on
  seeded ties, zero-kill players and every `minKills`/`limit` combination and requires identical
  rows.
- **Case-insensitive name lookups** (`LOWER(p.display_name) = LOWER($n)` in the stats, player,
  link and analytics repositories) could not use `idx_players_guild_name`. New expression index
  `idx_players_guild_lower_name ON players (guild_id, LOWER(display_name))`, plus
  `idx_kills_guild_lower_weapon ON kills (guild_id, LOWER(weapon_display))` for the weapon
  statistics lookup, the one other `LOWER(column) = LOWER($n)` filter in `internal/repository`
  (`factions.tag` already had a `LOWER` unique index). `TestQueryHygieneIndexesServeTheirQueries`
  proves with `EXPLAIN` that the planner picks them for the predicates as the repositories write
  them.
- **Per-server kill time window** (`COALESCE(k.event_time, k.created_at) >= $from AND < $to` in
  the fight replay, heatmaps, kills-by-hour and network boards) was not sargable. New expression
  index `idx_kills_server_event_window ON kills (guild_id, server_id, (COALESCE(event_time,
  created_at)))`. A `created_at` range with a margin was rejected: `event_time` is `NULL` for every
  ADM-sourced kill today (`Event.Timestamp` is never set - docs/CHAMPION_LIVE_SYNC.md A2/A8),
  and where a row does carry it, it is the log line's own clock, which after an outage, a restart
  or a backfill read sits hours or days before `created_at`, so no fixed margin is correct. The
  expression index serves the predicate exactly as written for `NULL` and non-`NULL` rows alike.
- **N+1 lists.** `ListFactionsForModeration` issued one `QueryRow` per faction for its leader;
  it now reads every listed faction's leader in one `DISTINCT ON (faction_id) ... WHERE faction_id
  = ANY($1)` query. `ListOwnerEvents` issued one query per ACTIVE/ENDED event for its top three;
  it now ranks every event's scores in one `ROW_NUMBER() OVER (PARTITION BY event_id ORDER BY
  score DESC, kills DESC, id)` query and keeps rows ranked 1-3. Output is identical
  (`TestListFactionsForModerationLeadersFromOneQuery`, `TestListOwnerEventsTopThreePerEventFromOneQuery`).
- **Pool size.** `DefaultMaxConns` 10 -> 25, `DefaultMinConns` 1 -> 2 (`internal/database`). One
  process runs a worker per game server, the HTTP API, the Discord handlers and the hourly sweeps.
  `DATABASE_MAX_CONNS` / `DATABASE_MIN_CONNS` still override.

### Data growth

Production grows about 0.24 GB/week with one server attached. The append-only tables, from the
`INSERT INTO` sites in `internal/repository`, `internal/livesync` and `internal/killfeed`:

| Table | Written per | Read for | Retention |
| --- | --- | --- | --- |
| `case_evidence_events` | every ADM hit/kill/connect/build line (wide rows, positions) | C.A.S.E. cases, shadow review, admissibility | **none - kept** (evidence) |
| `player_location_events` | every ADM line with a position | heatmaps, fights, lives, retention backfill | 30 days (`CHAMPION_LOCATION_RETENTION_DAYS`, pre-existing) |
| `live_sync_records` | every RPT/script/crash/restart line | diagnostics API: newest records, last 6 hours | noise 3 days; rest **14 days** (`CHAMPION_RETENTION_DAYS_LIVE_SYNC_RECORDS`; was 30) |
| `kills`, `deaths` | every kill / death | everything | none - kept |
| `combat_anomaly_flags` | every PvP kill | the same pair's last 10 minutes only | **14 days** (`CHAMPION_RETENTION_DAYS_COMBAT_ANOMALY_FLAGS`, new) |
| `base_black_box_events` | base-area movement | the owner's black box | per-server setting (pre-existing) |
| `platform_audit_log`, `admin_audit_log` | staff / owner actions | audits | none - kept |
| `player_daily_activity`, `server_hourly_activity` | one row per player-day / server-hour | retention dashboard | none - small, kept |
| `nitrado_tail_trust` | one row per Nitrado service, updated in place | tail-read trust | n/a |
| `shop_delivery_attempt_evidence`, `shop_delivery_attempt_events` | per delivery attempt | delivery evidence | none - low volume, kept |

So the unbounded drivers are `case_evidence_events` (by far the widest rows, one per hit line) and
`kills`/`deaths` plus their indexes; the pruned tables reach a steady state after their window.
`case_evidence_events` feeds cases and was deliberately not pruned here; any window for it is a
C.A.S.E. policy decision (docs/CASE_PHASE2B.md: "retention is not silently applied").

**Data retention worker** (`internal/app/data_retention_worker.go`,
`internal/repository/data_retention_repository.go`): once at start-up and then hourly, for each
table in its compile-time list, `DELETE ... WHERE id IN (SELECT id ... WHERE <time> < cutoff ORDER BY
<time> LIMIT 5000)` until a batch deletes nothing (at most 200 batches per sweep), logging
`component=retention event=rows_deleted table=... count=... retention_days=...`. The default
window is 14 days; `CHAMPION_RETENTION_DAYS_<TABLE>` (table name upper-cased) overrides it and
fails closed to the default on anything unparsable or non-positive. Only plain lower-case
identifiers from the list are ever interpolated (`TestDataRetentionPrunesOnlyOldRowsInBatches`).
The live-sync sweep (`internal/app/live_sync.go`) keeps its category-aware query and reads the same
`CHAMPION_RETENTION_DAYS_LIVE_SYNC_RECORDS`; `0111` adds `idx_live_sync_records_detected` and
`idx_combat_anomaly_flags_created` so neither sweep scans its table.

**Table sizes** in `GET /api/admin/health` -> `performance.tableSizes` (section 11, docs/ADMIN_API.md)
show where the bytes are, from the catalog, so the Owner Hub can watch the trend.

## 19. Kill-to-Discord latency (the baseline)

Every kill and death card is timed from the game writing its log line to Discord accepting the
post. The measurement only reads clocks the bot already had: it sends nothing, delays nothing and
stores no player name or message text.

### The stages of one card

```
game wrote the line -> bot read it -> queued for Discord -> Discord accepted the post
```

| Stage | From | To | What sits in it |
| --- | --- | --- | --- |
| `gameLogToBotReadMs` | The line's own time | The bot finished downloading the bytes holding the line | Nitrado exposing the log (about 5 minutes per step, docs/NITRADO_POLLING.md) plus the bot's poll interval |
| `botReadToQueuedMs` | Download finished | The event is in the database and its card is in the feed's queue | Earlier lines of the same download, parsing, the database work per event (section 23) |
| `queuedToDiscordAcceptedMs` | Card queued | Discord answered the post with success | The feed's own wait (up to a whole 10-minute cycle in `rotating` mode, section 23), Discord's rate limit, the request |
| `botReadToDiscordAcceptedMs` | Download finished | Discord accepted | Everything the bot controls |
| `gameLogToDiscordAcceptedMs` | The line's own time | Discord accepted | The whole trip |

**The line's own time.** An ADM line carries only the server's local time of day. The date comes
from the ADM file's name, and the conversion to UTC uses the server's UTC offset that Live Sync
learned from `restart.log` (`live_sync_server_clock`, re-read by each worker every 5 minutes).

- Offset not learned yet: the two `gameLog…` stages are simply missing for that card, and it is
  counted in `cardsWithoutServerClock`. No offset is ever assumed.
- A converted time more than 30 seconds after the read, or more than 24 hours before it, is a
  wrong clock (for example an offset learned before a daylight-saving change). The card is counted
  in `cardsWithImplausibleServerClock` and gets no `gameLog…` stages.
- An offset that is wrong by an hour in the other direction cannot be told from a real delay. The
  offset is learned again at every game-server restart.
- Line times have one-second resolution, so `gameLogToBotReadMs` is exact to about a second.

A card restored from the feed journal after a restart keeps only its parse time; that stands in
for the read time, so its stages include the time the bot was down. A PvE-feed card that reaches
the death feed without an event has only `queuedToDiscordAcceptedMs`.

### Where to read it

- **`GET /api/admin/nitrado-usage` → `feedLatency[]`** (platform admins): every server and feed
  (`KILLFEED`, `DEATH_FEED`).
- **`GET /api/runtime/status` → `feedLatency[]`**: the same entries for the server the request is
  about. Absent until this process has delivered a card for that server.
- **Logs:** `component=killfeed event=feed_latency`, one line per server and feed at most every
  5 minutes, and only when cards were delivered since the last line.

One entry:

```json
{
  "serverId": 3, "feed": "KILLFEED",
  "deliveredSinceStart": 412,
  "cardsWithoutServerClock": 0, "cardsWithImplausibleServerClock": 0,
  "lastDeliveredAt": "2026-10-05T18:07:41Z",
  "lastHour": {
    "cards": 37,
    "gameLogToBotReadMs":         {"samples": 37, "p50Ms": 151000, "p90Ms": 289000, "p99Ms": 301000, "maxMs": 301000},
    "botReadToQueuedMs":          {"samples": 37, "p50Ms": 900,    "p90Ms": 2100,   "p99Ms": 2600,   "maxMs": 2600},
    "queuedToDiscordAcceptedMs":  {"samples": 37, "p50Ms": 4200,   "p90Ms": 9800,   "p99Ms": 12000,  "maxMs": 12000},
    "botReadToDiscordAcceptedMs": {"samples": 37, "p50Ms": 5300,   "p90Ms": 11500,  "p99Ms": 14100,  "maxMs": 14100},
    "gameLogToDiscordAcceptedMs": {"samples": 37, "p50Ms": 157000, "p90Ms": 297000, "p99Ms": 312000, "maxMs": 312000}
  },
  "last24Hours": { "cards": 412, "…": "the same five stages" }
}
```

(The numbers above only show the shape. They are not measurements.)

| Field | Meaning |
| --- | --- |
| `deliveredSinceStart` | Cards recorded since this process started. |
| `lastHour`, `last24Hours` | Cards whose post Discord accepted in that window. `cards` is their count. |
| `samples` | How many of those cards have this stage. Less than `cards` for a `gameLog…` stage when the server clock was unknown. |
| `p50Ms`, `p90Ms`, `p99Ms`, `maxMs` | Milliseconds. Half, 90% and 99% of the cards were at or below the value; `maxMs` is the slowest. |

The log line carries the last hour: `cards`, `cards_24h`, `game_log_to_discord_{samples,p50,p90,p99,max}_ms`,
`game_log_to_bot_read_{p50,p90}_ms`, `bot_read_to_queued_{p50,p90}_ms`, `queued_to_discord_{p50,p90}_ms`,
`bot_read_to_discord_{p50,p90,p99,max}_ms` and `cards_without_server_clock`.

Limits: the numbers live in memory (a restart starts again from zero) and each server feed keeps
its newest 4,096 cards, so a feed with more than that in 24 hours reports "last 24 hours" over the
newest 4,096. `internal/killfeed/feed_latency.go`; the feed records a card in
`RotatingFeed.send` only after Discord confirmed it.

The earlier figures stay where they were: `timing[]` (how often Nitrado writes each log and how
soon the bot notices) and `delivery[]` (average/maximum queue wait per route) in the same
`nitrado-usage` response.

## 20. Poll rate

The engine already had two rates (`Engine.pollingInterval`, docs/NITRADO_POLLING.md). What it does
now, first match wins:

| Situation | Poll every |
| --- | --- |
| The last Nitrado call failed with a 429, a 5xx, a timeout or a network error | the base interval, doubling per further failure, at most 60 s. A full wait, however long the failed attempt took. Ends with the first success. **New.** |
| The token's budget is low (under 20% left) | `max(3 × base, 30s)` (unchanged) |
| The server is active and the budget is known with at least half left | the fast interval, `NITRADO_POLL_INTERVAL_FAST`, default 3 s |
| Otherwise | the base interval, `NITRADO_POLL_INTERVAL`, default 10 s |

"Active" was: the log changed in the last 5 minutes. It is now: the log changed in the last
5 minutes, **or players are online and the log changed in the last 15 minutes**. DayZ writes the
player list every 5 minutes while anyone is online, so with players on and nothing else happening
the log changes about once per 5 minutes, and the old rule dropped back to the base rate right
when the next write was due. A server with nobody online and no log change for 5 minutes polls at
the base rate; one whose log has not changed for 8 minutes keeps the existing stale-source
handling (`staleProbeAfter`, `staleGiveUpAfter`), which is unchanged. The thresholds are the
constants `busyWindow`, `playersBusyWindow` and `pollFailureBackoffMax` in
`internal/killfeed/engine.go`; no new environment variable.

Two more changes:

- While the engine is searching for a log (discovery), it also never retries faster than the
  failure backoff during a 429/5xx streak. A missing file (a rotation) is not such a failure and
  keeps the short discovery backoff.
- A worker's first cycle starts 1 second after the worker starts (`firstPollDelay`), not a whole
  poll interval later.

**Requests to Nitrado per server**, from the code (one poll of the selected log is one directory
listing; the listings a tick makes are shared for 750 ms):

| | Listings | When the log changed | When it has not changed for 2 minutes |
| --- | --- | --- | --- |
| Base rate (10 s) | 6 per minute, up to 12 when the ADM is listed under both mounts | +1 token request and +1 download from the file host per change (twice while a tail read is being verified) | +1 token request and +1 download per minute (the stale probe) |
| Fast rate (3 s) | 20 per minute, up to 26 with both mounts | same | same |
| Low budget (30 s) | 2 per minute, up to 8 | same | same |
| After failures | 6 per minute falling to 1 per minute | | |

Live Sync's watchers (RPT, script, crash and restart logs) add about 21 requests per minute per
server on the same token whatever the ADM rate (docs/CHAMPION_LIVE_SYNC.md 7.2).

Nitrado's limit is whatever its `X-RateLimit-Limit` header says for the token; the bot does not
assume a number. The fast rate is only used once those headers have been seen and at least half
the budget is left, and it stops below that. `GET /api/admin/nitrado-usage` → `tokens[]` shows the
limit, what is left and the requests of the last hour by operation.

What it buys: Nitrado exposes the ADM in steps of about 5 minutes (measured 2026-10-02,
docs/NITRADO_POLLING.md). Polling at 3 s instead of 10 s shortens the wait after such a step by at
most 7 seconds. It cannot shorten the step.

Rate limiting in the client (`internal/nitrado/client.go`): a 429 is retried up to three times
inside the call, waiting `Retry-After` (capped at 60 s); the signed download does the same. What
was missing is the engine slowing down after the call had failed: it went on at the fast rate.

Tests: `TestPollingIntervalAdaptsToActivityAndBudget`, `TestPollingIntervalStaysFastWhilePlayersAreOnline`,
`TestPollingBacksOffImmediatelyOnNitradoFailures`, `TestStartRunsFirstCycleWithoutWaitingAWholeInterval`
(`internal/killfeed/poll_rate_test.go`).

## 21. Reading only what is new

### What is on by default

With `NITRADO_DELTA_READ_MODE` unset, the bot already reads only the new bytes once that is proven
for the service (verified tail reads, `internal/nitrado/tail_trust.go`, docs/NITRADO_POLLING.md):

1. It downloads the whole file as before and also reads the same bytes through Nitrado's `seek`,
   and compares the two byte for byte.
2. After three matches it reads through `seek` only. The first byte of every read must equal the
   last byte already read, and every 20th read is compared with a full download again.
3. One mismatch turns it off for that service until the bot restarts.

### What `NITRADO_DELTA_READ_MODE` does

It is the older switch. When it is set, it takes priority over the verified tail reads, and it does
not compare anything with a full download.

| Value | What the bot tries for each read after the first | If the server does not honour it |
| --- | --- | --- |
| unset / `off` / anything unknown | Nothing from this switch. Verified tail reads as above. | - |
| `seek` | `file_server/seek` for exactly the new bytes | Whole file |
| `offset_query` | The download URL with `offset`/`count` | Whole file |
| `range` | The download URL with a `Range` header | Whole file |
| `auto` | `seek`, then `offset_query`, then `range`; a method that worked is tried first next time | Whole file |

A method that fails three times in a row is not tried again for 10 minutes.

**When the server ignores the offset**, the bot detects it and downloads the whole file in the
same poll, with nothing consumed from the refused answer:

- `offset_query`: the answer is used only if a `Content-Range` header states the requested offset.
  Nitrado sends the whole file with a plain 200 and no such header (found 2026-09-24).
- `range`: only a `206` whose `Content-Range` starts at the requested offset is used. A `200` is
  the whole file and is refused (what Nitrado does, 2026-10-02).
- `seek`: **this was not detected before this change.** A `seek` answer was taken as starting at
  the requested offset whatever came back. The bot now asks `seek` for exactly the bytes the
  listing says exist and refuses an answer longer than that. A server that ignores the offset
  returns the file from its first byte, which is always longer than what is left after the offset.
  Asking for exactly those bytes also matters on Nitrado: its `seek` answered HTTP 500 for a
  request reaching past the end of the file, so the old fixed 256 KiB request could not work there.

Not detected in these modes: a server that ignores the offset but honours the length, returning
the right number of bytes from the wrong place. The default (verified tail reads) catches that,
because it compares bytes. This is one reason to leave the switch unset.

Tests, with the staging Nitrado fixture (`internal/nitrado/nitradofixture`), whose download URL
ignores `offset`/`count` like Nitrado's: `TestDeltaModesAgainstNitradoFixture`
(`internal/killfeed/delta_fixture_test.go`) runs the production engine and client in every mode
against the fixture as it is, against a download host that also ignores `Range`, and against a
`seek` that ignores or honours its offset. In every case each kill is published once, in log
order, the checkpoint ends exactly at the end of the file, and no byte of a refused answer is
counted as received. `TestReadDeltaRefusesSeekThatIgnoresTheOffset` and
`TestReadDeltaNeverAsksPastTargetSize` (`internal/nitrado/partial_read_test.go`) cover the client.

### The two documents

`docs/NITRADO_DELTA_READS.md` says none of the three methods was tried against a live service.
`docs/CHAMPION_LIVE_SYNC.md` A9 says Nitrado ignores `offset`/`count`. Both were true when written.
The later production finding in `docs/NITRADO_POLLING.md` (2026-10-02, one service) is the current
state: the download URL ignores `offset`/`count` and `Range`; `seek` works when asked for bytes
inside the file.

### The one-time check, and what to set

Nothing here could reach Nitrado, so this is for the owner or an operator to run once.

1. **From the running bot (no tool needed).** Search the production logs for
   `event=tail_read_trusted` with the server's `service_id`. If it is there, `seek` matched full
   downloads three times on that service. `event=tail_read_disabled` means a mismatch was seen.
   After trust, `component=adm event=download_complete` lines carry `download_mode=SEEK_SUPPORTED`
   and a `downloaded_bytes` equal to the new bytes only.
2. **With the operator tool** (read-only: it lists and downloads, prints no token and no download
   address):

   ```
   NITRADO_TOKEN=<the server's token> go run ./cmd/nitrado-delta-probe -service <Nitrado service id>
   ```

   It downloads the newest ADM in full, then reads the file's second half through each method and
   compares the bytes. Expected on Nitrado today: `seek SUPPORTED, byte-for-byte validated`,
   `offset_query UNSUPPORTED`, `range UNSUPPORTED`. The tool used to ask `seek` for 256 KiB whatever
   the file held, which Nitrado answers with HTTP 500 near the end of a file, so it could report a
   working `seek` as unsupported; it now asks for exactly the segment it compares.

**What to set afterwards: leave `NITRADO_DELTA_READ_MODE` unset**, whatever the tool prints.

- `seek` validated: the default already uses it, with the byte comparison the switch does not have.
- `seek` not validated: no method works, and the switch could only add failed requests.
- A `BYTE MISMATCH`: do not set the switch, and report the output.

Setting `auto` on Nitrado today would first try `seek` (which works) and would behave like the
default without its checks. Setting `offset_query` or `range` would download the whole file twice
for each change (once refused, once for real) until the method is paused.

## 22. Nothing lost or doubled across a restart

### How it works

- **Where the bot is in the log** is one row per server (`adm_checkpoints`): file and byte offset
  after the last fully handled line. It is written after each download's lines are handled.
- **A kill is stored before it is posted.** `kills` and `deaths` have
  `UNIQUE (guild_id, event_fingerprint)`. The card is handed to the Discord feed only by the
  process whose insert succeeded; a process that gets "already there" posts nothing
  (`PersistenceQueue.persistOne`, `KillRepository.InsertKillReturning`).
- **The fingerprint** is the event type, the line's time of day, the player ids, the weapon and the
  distance (`internal/killfeed/dedupe.go`).
- **The Discord feed** (`RotatingFeed`): in `immediate` mode every card is written to the feed
  journal (`discord_feed_cards`) and carries a Discord nonce, so a card queued or half-sent before
  a crash is posted once by the next process. In `rotating` mode cards wait in memory for the next
  10-minute cycle and are posted at a clean shutdown.

`GET /api/runtime/status` → `build.killfeedDeliveryMode` says which mode a deployment runs.

### Proven by tests

`internal/killfeed/restart_safety_test.go` runs the production engine, Nitrado client and
persistence queue against the staging Nitrado fixture; each bot process is a fresh engine, and
only the database survives. `restart_safety_integration_test.go` runs the same three scenarios on
PostgreSQL with the real `kills`/`deaths`/`adm_checkpoints` tables and one connection pool per
process.

| Scenario | Result |
| --- | --- |
| Kills and a death land while no bot is running (`TestRestartKillsWhileDownArePostedInOrder`) | The next process posts exactly those, in log order, and nothing the previous process posted. |
| The process dies after storing 3 of the 6 kills of one download, before the checkpoint moved (`TestRestartMidBatchPostsEachKillOnce`) | The next process reads all 6 again, posts only the last 3, in order. |
| Two processes read the same log at the same time for several downloads, as during a deploy (`TestOverlappingProcessesPostEachKillOnce`) | Across both, every kill is posted exactly once; each process posts in log order; the survivor carries on alone. |

So `docs/MULTI_PROCESS.md` was too pessimistic about kills: during an overlap kill and death cards
are not doubled. Already covered before: a card queued or sent just before a crash in `immediate`
mode (`TestJournalCrashReplaysQueuedCards`, `TestJournalCrashAfterCreateDoesNotDuplicate`,
`TestJournalSurvivesSIGKILL`), and the `rotating` flush at shutdown
(`TestRotatingFeedFlushesOnShutdown`).

### Fixed here

At shutdown the persistence queue's consumer stops with the worker's context. If the engine was in
the middle of a download's lines, it handed the next event to a queue nobody was reading and
waited the whole persistence timeout (30 s) for an answer, holding the worker's shutdown that
long. `EnqueueAndWait` now returns at once when the consumer has stopped
(`TestEnqueueAndWaitReturnsAtOnceWhenQueueHasStopped`, `TestEnqueueAndWaitReturnsWhenQueueStopsWhileWaiting`).
Nothing was lost by the wait (the event is read again by the next process); it made the old
process linger, which lengthens a deploy's overlap.

### What can still double or drop

Not fixed: each needs a change to the pipeline, not a local one.

| What | When | Effect | Proposed fix |
| --- | --- | --- | --- |
| **Hit, connection and build cards double** | Two processes overlap (about 15 s per deploy), or a process restarts in the middle of a download | Those feeds are not stored first; each process posts what it reads. Kill and death cards are not affected. | Give each server's log pipeline one owner at a time (a per-server lease like the map rotation worker's), or key these cards by file and byte offset in a small "posted" table. |
| **Cards waiting in `rotating` mode are lost** | The process is killed without a clean shutdown (crash, out of memory, forced stop) | Up to 10 minutes of kill and death cards never reach Discord. The kills are stored, so the next process sees them as already handled. | Run `immediate` mode (journaled), or journal the rotating batch too. |
| **One card is lost at shutdown** | A kill's insert commits in the instant the shutdown signal arrives: the feed's final flush can run before that card is queued (in `immediate` mode, before it is journaled). Also when the database commits the insert but the cancelled request reports an error. | That one kill is stored and never posted. A window of milliseconds per deploy, only while a kill is being handled. | Stop the feeds after the engine and the persistence queue have stopped (they share one context today), with a bounded wait so a slow engine cannot hold back the flush. |
| **Kills written while the bot was down are skipped** | The bot is down across a game-server restart. The checkpoint names the old log file; the new process finds a different newest file, does not match it and starts at that file's end (`cold_start_baseline_required`, `TestColdStartMismatchedCheckpointBaselinesCurrentADM`). The same happens if the same file is listed only under the other mount (`ftproot` instead of `noftp`) when the new process starts. | The tail of the old file and everything in the new file up to that moment is never read: no rows, no cards. A normal deploy is not affected (the old process keeps reading until the new one is up). | On a mismatch with an older boot, drain the checkpoint's file from its offset, then read the new file from byte 0, bounded by age; match mounts by the canonical file name. |
| **A death is dropped as a duplicate** | The same player dies at the same second of the day on two different days. The fingerprint has no date (`Event.Timestamp` is never set) and no file. | The second death is not stored and not posted. For kills the distance to four decimals is part of the fingerprint, so a collision needs the same killer, victim, weapon, distance and second. | Add the ADM file's date to the fingerprint for new rows. Needs a migration plan: a changed fingerprint for a line already stored would post it again. |
| **Old cards stay in the channel in `rotating` mode** | Every restart | The cards of the final flush belong to nobody: the next process does not know their ids and never deletes them. Not a double or a drop, but the channel shows more than one batch. | Journal the rotating batch's message ids (the journal already does this for `immediate`). |

## 23. Other delays on the kill's path

Found by reading the path; evidence is the code named. Only the first two were changed.

| Delay | Where | Size | Status |
| --- | --- | --- | --- |
| A worker's first cycle waited a whole poll interval | `Engine.Start` | 10 s after every restart and every newly connected server | **Fixed**: 1 s (`firstPollDelay`). |
| Shutdown waited out the persistence timeout | `PersistenceQueue.EnqueueAndWait` | up to 30 s of extra overlap when a download was being handled | **Fixed** (section 22). |
| Nitrado exposes the ADM in steps | Nitrado's file API | about 5 minutes; 8.5 minutes for a new boot's file (docs/NITRADO_POLLING.md) | Outside the bot. `gameLogToBotReadMs` now shows it per card. |
| The rotating cycle | `rotatingFeedInterval`, `RotatingFeed.flush` | up to 10 minutes per card when `KILLFEED_DELIVERY_MODE` is not `immediate` | By design; the owner's switch. `queuedToDiscordAcceptedMs` shows it. |
| One database round trip after another per kill | `PersistenceQueue.persistOne` and `persistenceStoreAdapter.ProcessPersistedKill`: two player upserts, season, two factions, war, two streak reads, the insert, then ranked award, life end, streak update and reset, two profiles, head-to-head, anomaly check, active events, VIP badge, bounty claim, streak note, wanted check | about 20 queries per kill, one at a time, and the engine waits for each kill before the next line. Nitrado delivers 5 minutes of kills in one download, so the last of N kills waits for N × that. | Not changed: the order of these writes is what keeps statistics and cards consistent. `botReadToQueuedMs` measures it. If it is seconds at p90, post the card first and enrich it in one or two combined queries. |
| One card, one Discord message, one at a time | `RotatingFeed.postImmediate` | Discord allows about 5 messages per 5 s per channel, so a burst of 20 kills takes about 20 s to appear. With the window full each card also costs a delete request first, plus three journal writes. | Not changed (presentation). `queuedToDiscordAcceptedMs` measures it. |
| C.A.S.E. evidence writes on the poll loop | `Engine.observeEvidence` | one database write per hit, kill and connect line, before the next line is handled, on servers with the collector enabled | Not changed. Shows in `botReadToQueuedMs`. A batch insert per download would remove it. |
| Whole-file reads that find nothing | `Engine.probeStaleSource` | when the log has not changed for 2 minutes, the whole file is downloaded once a minute (0.5-3 s each, on the poll loop) - about three times between two Nitrado steps | Not changed: it is the safety net for a stale listing. It could use the verified tail read. |
| Hit and connection cards wait on purpose | `hitfeedWindow` 5 s + `hitfeedTick` 2 s; `connectionsTick` 2 s | up to 7 s / 2 s | By design (section 10). |

**Files:** `internal/killfeed/feed_latency.go` (new), `engine.go`, `engine_poll.go`,
`engine_process.go`, `engine_discovery.go`, `engine_stale_probe.go`, `source_health.go`,
`persistence.go`, `event.go`; `internal/discord/rotating_feed.go`, `killfeed.go`, `deathfeed.go`;
`internal/app/feed_latency.go` (new), `server_worker.go`, `runtime_status.go`,
`admin_api_nitrado_usage.go`; `internal/nitrado/partial_read.go`; `cmd/nitrado-delta-probe`.
