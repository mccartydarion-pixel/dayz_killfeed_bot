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

Concurrency model (internal/servers/workermanager.go, internal/app/app.go `runServerWorker`): **one
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
synchronously (`internal/app/app.go`'s `completeChannelsStep`), so the *same* process's very next
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
only) could not show. No production pool-exhaustion evidence was available to justify changing the
actual default size, so it was left at 10 - the goal here was making it tunable and observable, not
guessing at a new number.

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
    "poolTotalConns": 3, "poolIdleConns": 2, "poolMaxConns": 10, "poolAcquiredConns": 1,
    "poolAcquireCount": 1042, "poolEmptyAcquireCount": 0, "poolAcquireDurationMs": 12,
    "queryTotal": 8841, "querySlow": 0, "queryAvgDurationMs": 0.31
  },
  "routing": { "cacheHits": 512, "cacheMisses": 9, "cacheHitRate": 0.9826, "cacheEntries": 9 }
}
```

Counts are cumulative since process start (not a rate) - an operator derives a rate from two reads a
known interval apart. Deliberately **not** added to `GET /api/runtime/status` (the documented,
website-facing contract in `docs/runtime-status-api.md`) - extending a customer-facing contract
without a compatibility review was out of scope for this phase; the existing platform-admin API was
the correct "internal/admin mechanism" the task asked for.

## 12. Health

Audited `internal/health` (`model.go`, `queue.go`, `workers.go`) and `app.go`'s `refreshHealth`:
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

Not modified this phase. Existing coverage already exercises the relevant guarantees: a durable
byte-offset checkpoint (`killfeed.Engine`'s `DurableCheckpoint`) means a restarted worker resumes
parsing from the last acknowledged offset, never re-processing already-persisted bytes; the database's
own `UNIQUE(guild_id, event_fingerprint)` constraint is the backstop against a duplicate insert even if
the in-memory (non-durable) `Deduplicator` cache was lost on restart. `TestColdStartMismatchedCheckpointBaselinesCurrentADM`
and `TestCheckForNewerLogPreservesCheckpointAndDedupe` (`internal/killfeed`) cover this; both still
pass unchanged.
