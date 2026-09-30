
> Clean-main extraction of fixture-only work previously reviewed in stacked PRs #132–#134. This branch has no live HTTP/Discord caller and cannot be used to accept current-source evidence or activate a detector.

# C.A.S.E. core: synthetic atomic staff-review transaction

**Staging/fixture contract only, no runtime caller.** This stacked branch builds on unmerged case schema #130 and bounded read-only repository #131. It does not add a case admission route, case creation, live detector, sender, publisher, scheduler, startup hook or enforcement.

The isolated repository method `ApplySynthetic` requires explicit fixture flags, checks the exact installation/guild/server relationship and current organization OWNER/ADMIN membership inside its database transaction, locks the exact scoped case row, checks idempotency action fields, appends immutable audit history, and updates neutral status atomically. It rejects invalid transitions, missing/foreign cases, unauthorized/revoked membership, changed notes under a repeated action key and unsafe note characters. A disposable PostgreSQL test injects a failing status update after the audit insert to verify that both changes roll back together.

**Not production authorization:** fixture flags are caller assertions, not verified HTTP credentials or Champion client-admin capabilities. A real review endpoint must independently authenticate the user, verify per-installation staff capability and owner policy, apply CSRF/rate-limit/audit controls, recheck identity and entitlement in its transaction, and validate admitted detector/source/evidence quality. Reusing this fixture-only method as a live endpoint is prohibited.

Schema migration #130 is separately production-impacting and unapproved. The deployed bot remains unchanged, current-source evidence is unverified, CASE-MOV-001 blocked, and no player verdict or private C.A.S.E. alert is enabled. This is a historical fixture contract; follow the sole active Core 8 roadmap in PR #155.

## Concurrent permission revocation
The synthetic repository now holds a PostgreSQL `FOR SHARE OF m` lock on the exact qualifying organization membership before taking the scoped case-row lock. Role UPDATE and membership DELETE must serialize with review and audit commit; this prevents a revocation from committing while an already-authorized review is stalled on its case row. Lock order is membership then case, including action-key replays. The disposable PostgreSQL integration test holds a case lock, observes the membership lock with NOWAIT, verifies a concurrent role downgrade cannot commit through it, completes the review, then downgrades and rejects replay. This establishes a fixture transaction ordering guarantee, **not** production HTTP identity or client-admin capability verification.
