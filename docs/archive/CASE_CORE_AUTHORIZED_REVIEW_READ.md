
> Clean-main extraction of fixture-only work previously reviewed in stacked PRs #132–#134. This branch has no live HTTP/Discord caller and cannot be used to accept current-source evidence or activate a detector.

# C.A.S.E. core — fixture-only authorized review queue

This candidate is stacked on #132 and depends on already-applied migration 0063. It introduces `ListAuthorizedSynthetic`, an inert repository-only read method with no HTTP/Discord/game-worker caller. It takes an exact guild/server/installation and bounded 1–50-item page. The organization OWNER/ADMIN membership, installation organization and both guild/server joins are checked **inside the same SQL statement** as the returned neutral case rows, avoiding a separate check-then-read race.

The method returns opaque case IDs, detector metadata, neutral status and bounded counts; never names, positions, raw ADM source paths or private review notes. Unauthorized, revoked, foreign or absent scopes yield no rows. Invalid cursors, page sizes or disabled fixture flag are rejected. Fixture tests use a fresh disposable PostgreSQL schema and test role downgrade, outsider, installation selection and cursor behavior.

This is **not** a production identity or installation-capability contract. The future endpoint must authenticate the caller and select installation using existing protected infrastructure, check owner controls and least-privilege permissions, rate-limit and audit access. Do not wire or merge this branch into production as a release authorization. No finding admission, sender, live notification or enforcement is added. Issue #114 real current-source evidence and gate B remain open.
