# C.A.S.E. core: exact-installation neutral review read model

This clean-main branch extracts only the read-only repository and disposable-PostgreSQL tests from the original stacked #131. Migration 0063 was independently merged and observed applied in production as #130; this PR does **not** touch migration registration or schema.

The query returns at most 50 neutral case summaries by exact guild, server and installation with an opaque descending ID cursor. It includes counts of exact linked evidence and audit records, but no player identifiers, coordinates, raw ADM paths, private review notes or accusations. Foreign or absent scopes yield no rows; invalid scope and cursor fail closed.

There is NO production caller, HTTP endpoint, authenticated staff authorization, case writer, collector, sender, scheduler, detector or enforcement in this change. A future route must independently authenticate the caller, resolve selected installation/capability and owner controls, rate-limit and audit before invoking any reader. Passing CI does not clear source gate #114 or authorize live cases.
