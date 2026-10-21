# C.A.S.E. neutral review history API

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/anti-cheat/cases/{caseID}/history`
returns a bounded page of immutable state-transition summaries for exactly one
case in the selected installation. Existing service and acting-user auth,
`PLAYER_LOCATION_VIEW`, read rate limit and audit apply. The repository checks
OWNER/ADMIN membership and the case's installation/guild/server joins in the
same SELECT that returns history rows; foreign or revoked scopes yield no rows.

Only audit ID, before/after status, reason code and timestamp are returned.
Private notes, actor identity, player data, coordinates, raw ADM paths and
Discord destinations are omitted. The endpoint cannot modify a case, activate
a detector, send an alert or enforce a sanction. It relies on the already
deployed inert case-review schema and neutral queue.

This is read plumbing, not end-to-end acceptance. No live finding producer or
staff mutation route exists yet. Gates A–G remain open. (This note was written
as the description of the change that added the route; the route is merged.)
