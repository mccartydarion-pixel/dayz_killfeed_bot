# C.A.S.E. neutral review queue API

`GET /api/saas/organizations/{organizationID}/installations/{installationID}/admin/anti-cheat/cases`
returns a bounded page of neutral case summaries. The existing service identity,
acting user, selected installation and `PLAYER_LOCATION_VIEW` capability are
required. The repository rechecks OWNER/ADMIN organization membership and the
installation's guild/server joins inside the same SELECT that returns rows.
Invalid cursors and limits fail closed; reads are rate limited and audited.

The response contains opaque case ID, detector identifier/version, neutral
review status, evidence/audit counts and timestamps. It contains no player
identity, coordinates, evidence lines, review notes or Discord destination.
The route cannot create or modify a case, evaluate a detector or send an alert.
Existing fixture-only write methods and their gates remain unchanged.

The route is merged (this note was written as the description of that change).
It is a read path, not a release gate exit. The bot
has no live finding producer or authenticated mutation route, so an empty
queue is expected. Gate A source continuity, Gate B detector validation,
owner controls, private delivery and live regression checks remain open.
