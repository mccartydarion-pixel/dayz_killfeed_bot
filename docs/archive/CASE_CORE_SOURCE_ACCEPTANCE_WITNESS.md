# C.A.S.E. #114 — pure current-source acceptance witness (offline)

This isolated candidate models a **human-review candidate** from the already-protected integrity response. It does not add any endpoint, SQL, ADM poller, event processing, live detector, Discord send, case admission, migration, or enforcement. It is deliberately not called by production code.

The pure function requires exact selected/accepted/latest pseudonymous source agreement; a running worker and enabled collector; existing `RETAINED_EVENTS_OBSERVED` classification; complete latest event offset and ingestion timestamp; a plausible capture time and, when observed, evidence ingestion not preceding the last source change. Missing, historical, quiet or inconsistent inputs remain `NOT_OBSERVED`. The positive result is **CURRENT_SOURCE_REVIEW_CANDIDATE**, never PASS. Human review must compare the protected capture to the server/installation, source continuity, bounded 200-row sample and observational limits. Non-atomic snapshots do not establish a continuous stream. Ingestion timestamp is not gameplay time. No movement/speed inference or player allegation follows.

This is an operator contract and synthetic unit test only. Real current-source retained evidence for Champions has not been independently observed in #114; nothing closes gates A/B/C or lifts CASE-MOV-001 (safe speed pairs remain zero). A routine qualifying event from the current source and controlled staff readback are required. Do not manufacture traffic, add a second poller or publish private source identities.

All release controls remain default-off; no production impact or deployment is requested by this PR.
