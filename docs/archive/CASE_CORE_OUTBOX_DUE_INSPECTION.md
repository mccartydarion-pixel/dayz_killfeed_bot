
> Clean-main extraction of fixture-only work previously reviewed in stacked PRs #132–#134. This branch has no live HTTP/Discord caller and cannot be used to accept current-source evidence or activate a detector.

# C.A.S.E. core — synthetic scoped due-outbox diagnostic

This draft is stacked on #133, #132 and #131. The already deployed 0063 schema stores an inert outbox. This change adds only a bounded read-only diagnostic for exact guild/server/installation, with no claim, lease, status mutation, scheduler, destination lookup, HTTP route or Discord client.

The query shows only pending or retry-wait items that are due, under the attempt cap, whose exact scoped case is REVIEWED and has at least one exact linked evidence record. That is a deliberately **incomplete necessary-condition filter**, not authorization for delivery. Current-source quality, detector validation, reviewer authentication, installation opt-in, private CASE_ALERTS route, destination reachability, receipt reconciliation and real evidence access all remain release gates. The diagnostic returns opaque IDs/keys only and never a message payload or player information.

Disposable PostgreSQL tests cover no evidence, scope isolation, due window, fixture flag, bounded page and resolved-case exclusion. Do not merge/deploy as a live outbox worker or mark gate E complete. No action can generate an alert or sanction.
