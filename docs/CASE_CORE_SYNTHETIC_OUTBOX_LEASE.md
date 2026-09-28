# C.A.S.E. fixture-only durable outbox claim

Gate E requires a lease before any future private delivery worker can process a due outbox row. `ClaimDueSynthetic` is an isolated, explicit-call fixture transaction with no runtime caller, sender, route lookup or receipt operation.

A claim requires positive exact guild/server/installation scope, a bounded lease duration, a due PENDING/RETRY_WAIT row under the attempt cap, a REVIEWED case, a matching immutable review audit and exact linked evidence. PostgreSQL locks the selected row with `FOR UPDATE OF o SKIP LOCKED`; the transaction assigns a random opaque lease token, increments attempts once and records a bounded lease expiry. There is no fallback to ADMIN_ALERTS or any other installation.

The disposable PostgreSQL regression checks disabled fixture mode, early and foreign scope, status-only review exclusion, two simultaneous claimants yielding one lease, persisted token/attempt count, and no repeated claim while LEASED. This is necessary concurrency groundwork only. It does not verify a real detector, live evidence, reviewer, owner opt-in, private route permissions, remote receipt or exactly-once Discord delivery. There is no send or enforcement path.
