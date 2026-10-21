# C.A.S.E. core — protected evidence references per neutral case

The already-protected neutral case queue and history routes now have a matching GET `/anti-cheat/cases/{caseID}/evidence-refs` candidate. It returns at most 50 **opaque persisted evidence IDs** linked to the exact selected installation, server and guild, plus a descending cursor. No raw ADM line, source path, coordinates, player identity, staff notes or detector allegation is returned. Each ID is inspected through the existing separately protected exact-record evidence endpoint; the reference list grants no new access itself.

The route requires the existing privileged player-location-view capability, selected installation, rate limiting and audited read. The repository also verifies active same-organization OWNER/ADMIN membership and exact installation/guild/server scope within the evidence-link SELECT. Missing, foreign, or revoked scopes have no ID fallback. Disposable integration fixtures cover pagination, cross-server case isolation, unauthorized actor and role revocation.

It is read-only and cannot admit or review a case, run a detector, enqueue a message or enforce. It does not imply any live C.A.S.E. finding exists or Gate D is complete. Production merge/deploy remains separately approved, and authenticated live endpoint response still needs the later controlled acceptance phase.
