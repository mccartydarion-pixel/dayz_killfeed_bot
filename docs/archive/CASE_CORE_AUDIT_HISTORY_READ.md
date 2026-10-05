# C.A.S.E. fixture audit history read

Gate D requires an authorized staff member to inspect the immutable chronology of a reviewed case. This offline repository method is a bounded, read-only fixture contract. It returns transition statuses, reason code and timestamp, while excluding notes, actor identity, player data and raw evidence.

The query checks exact guild, server, installation and case scope together with current OWNER/ADMIN membership in the same SQL statement. A descending audit-ID cursor limits each page to 50 entries. A fixture flag is mandatory; there is no HTTP route or runtime caller.

Disposable PostgreSQL tests cover authorized review, foreign installation and outsider isolation, role revocation, cursor bounds and disabled fixture mode. This does not authenticate a browser user, admit a real case, establish current-source evidence, validate a detector or authorize delivery. Gates A–G remain open.
