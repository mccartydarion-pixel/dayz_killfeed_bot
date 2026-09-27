# C.A.S.E. core: scoped review queue read model (unreleased)

This change is deliberately based on the unmerged [review schema PR #130](https://github.com/mccartydarion-pixel/dayz_killfeed_bot/pull/130). It adds only a read-only repository that retrieves neutral case summaries by exact guild, game server and installation, with a strict 50-item page and opaque ID cursor. It returns counts of exact linked evidence and audit entries but no player identifiers, coordinates, raw ADM paths, case notes or accusation. An absent/foreign scope returns no rows; invalid scopes and cursors fail closed.

**No live HTTP endpoint or caller is added.** A future endpoint must authenticate the staff member, resolve the selected installation, verify the player-location/evidence capability and owner controls, enforce rate limits, record access and only then call the repository. An internal repository method alone is not authorization. Fixture test writes occur only in a fresh disposable PostgreSQL schema; they are not evidence of a live cheating case.

The implementation is dependent on migration 0063 in PR #130 and must not be merged into production ahead of that migration. Neither this read-only query nor the schema enables case creation, staff review writes, private Discord messages, shadow detector evaluation or enforcement. Current-source data gate #114 and release tracker #124 remain open. 
