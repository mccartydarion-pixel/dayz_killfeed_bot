# CHAMPIONS® C.A.S.E.
## CORE 8 — ADVANCED DETECTION & RELIABILITY UPGRADE

### OBJECTIVE

Upgrade the existing CHAMPIONS® C.A.S.E. anti-cheat system into a reliable, advanced console-focused detection platform.

**We are keeping exactly eight core detectors.**

Do not add additional detector modules or expand the anti-cheat roadmap beyond the approved eight.

The objective is to improve detection accuracy, evidence quality, false-positive protection, reliability, Discord alerts, and customer configuration without introducing unnecessary complexity.

This document in PR #155 is the sole active C.A.S.E. roadmap. Earlier phase and gate labels in historical engineering notes are archival; they do not add detectors, impose a separate release checklist, or supersede this Core 8 scope. Evidence validation and production safety requirements in this document still apply.

Inspect the existing repository and C.A.S.E. architecture before making modifications. Reuse existing components wherever possible.

---

# PHASE 1 — APPROVED DETECTION MODULES

The only approved detectors are:

### 01. BASE BOOST DETECTION

Detect suspicious construction or building activity inside or around another player's registered base.

Capabilities:

- Registered base protection zones.
- Configurable monitoring radius.
- Authorized player and faction exclusions.
- Suspicious construction activity.
- Watchtower and structure-placement investigation.
- Repeated unauthorized activity correlation.
- Staff-only evidence-backed investigation reporting when validated.

Important: Boost Detection refers to unauthorized base construction and boosting, NOT kill farming or statistical boosting.

Existing build lines are parsed for a staff build feed only when the server enables placement/build logging. They can be retained as source-addressed C.A.S.E. evidence only under a separate, default-off per-server build-evidence allowlist; no real ADM build-line sample has been verified. Base ownership, authorization and zone registration are separate evidence requirements. See [Base Boost feasibility](CASE_CORE_BASE_BOOST_FEASIBILITY.md). Only claim an event was observed if the underlying telemetry supports it.

### 02. SKYWALK DETECTION

Investigate suspicious player positioning above legitimate terrain or structures.

Capabilities:

- Terrain elevation comparison.
- Custom map and structure exclusions.
- Repeated position validation.
- Movement history correlation.
- Evidence preservation.

Legitimate watchtowers, rooftops, custom structures, and elevated locations must not automatically trigger accusations.

### 03. DUPE DETECTION

Investigate suspicious activity potentially associated with item duplication.

Capabilities:

- Suspicious reconnect correlation.
- Restart-window analysis.
- Available inventory or item evidence.
- Repeated suspicious event detection.
- Incident timeline reconstruction.

Do not claim item duplication was confirmed without sufficient item-level evidence.

### 04. PC DETECTION — XBOX

Investigate platform anomalies where trustworthy platform information is available.

Capabilities:

- Verified platform metadata analysis.
- Suspicious session correlation.
- Source validation.
- Evidence-based staff investigation.

Never classify a player as using PC based on usernames, movement patterns, controller behavior, or assumptions.

If the required platform evidence is unavailable, report the capability as unsupported or insufficient evidence.

### 05. NO-CLIP DETECTION

Investigate movement that appears to pass through solid structures.

Capabilities:

- Map-aware position validation.
- Known legitimate entrance exclusions.
- Custom building support.
- Movement reconstruction.
- Repeated-event confirmation.

Sparse position snapshots alone must not establish confirmed no-clipping.

### 06. UNDERMAP DETECTION

Identify player positions that appear beneath valid terrain.

Capabilities:

- Terrain elevation comparison.
- Underground bunker and tunnel exclusions.
- Custom-map support.
- Repeated-position validation.
- Evidence preservation.

### 07. SUSPICIOUS LOGINS

Detect unusual connection patterns.

Capabilities:

- Rapid reconnect analysis.
- Repeated abnormal login sequences.
- Restart awareness.
- Session history.
- Correlation with existing incidents.

Normal reconnects, connectivity problems, and server restarts must not automatically be treated as cheating.

### 08. TELEPORT ALERTS

Investigate physically implausible changes between recorded player positions.

Capabilities:

- Position-distance calculations.
- Elapsed-time validation.
- Vehicle movement exclusions.
- Respawn awareness.
- Server restart awareness.
- Historical position comparison.

Position samples must include their actual timestamps and freshness.

Do not interpret missing intermediate movement data as proof of teleportation.

---

# PHASE 2 — SHARED INTELLIGENT DETECTION ENGINE

Improve the existing C.A.S.E. detection pipeline.

Each detector must follow four stages:

**OBSERVE**

Collect available information from verified sources.

**CORRELATE**

Compare relevant timestamps, locations, player identities, event sequences, and known legitimate explanations.

**VALIDATE**

Confirm that minimum evidence requirements are satisfied.

Check for stale logs, missing information, duplicate events, and false-positive exclusions.

**NOTIFY**

Create a staff investigation only when the configured alert threshold is satisfied.

Do not label suspicious activity as confirmed cheating.

Keep the detection architecture modular and avoid rebuilding functionality already provided by the existing C.A.S.E. evidence engine.

---

# PHASE 3 — DETECTION SENSITIVITY

Introduce three customer-selectable detection modes.

### RELAXED

Requires stronger or repeated evidence before issuing alerts.

Designed for servers that want fewer notifications.

### BALANCED — DEFAULT

Uses validated evidence thresholds and standard false-positive protections.

### STRICT

Surfaces a broader range of suspicious observations for staff review.

Strict mode must not bypass evidence requirements or enable automatic enforcement.

Each detector must remain individually configurable.

Configuration changes must be isolated to the correct installation and game server.

---

# PHASE 4 — DETECTOR HEALTH MONITORING

Implement reliability monitoring for all eight detectors.

Track:

- Log ingestion freshness.
- Position-data freshness.
- Polling delays.
- Missing telemetry.
- Processing errors.
- Duplicate events.
- Detector availability.
- Last successful evaluation.

Supported detector states:

ACTIVE
DISABLED
DEGRADED
INSUFFICIENT_EVIDENCE
UNSUPPORTED
ERROR

If required telemetry becomes unavailable or stale, suspend conclusions that depend on it.

Never generate fabricated detection results.

Display detector health in the client dashboard.

---

# PHASE 5 — EVIDENCE AND FALSE-POSITIVE PROTECTION

Every detection must preserve the relevant supporting evidence.

Required information where available:

- Detector identifier.
- Installation and game server.
- Player identity.
- Source log reference.
- Event timestamp.
- Observation timestamp.
- Relevant coordinates.
- Detection explanation.
- Evidence completeness.
- Known exclusions.
- Investigation status.

Clearly distinguish:

1. Observed activity.
2. Suspicious behavior.
3. Staff-confirmed violation.

Prevent duplicate Discord notifications for the same incident.

Preserve idempotent processing across polling retries, reconnects, service restarts, and journal recovery.

No detector may issue automatic bans or punitive enforcement during this development phase.

---

# PHASE 6 — DISCORD ALERT INTEGRATION

Integrate all eight detectors with the existing C.A.S.E. Discord infrastructure.

Do not create an unrelated second alert system.

Alerts must display:

CHAMPIONS® C.A.S.E.
Detection type
Player identity
Affected server
Observed behavior
Supporting evidence
Event timestamp
Investigation status

Use the existing case management system.

Respect Discord permissions and prevent ordinary players from accessing staff-only evidence.

Avoid unnecessary notifications, repeated alerts, and channel spam.

Maintain the existing /setup integration.

---

# PHASE 7 — CLIENT DASHBOARD

Provide a clean configuration interface for the eight detectors.

The server owner must be able to:

- Enable or disable eligible detectors.
- View detector availability.
- Select sensitivity.
- View health and ingestion status.
- Configure supported thresholds.
- Review relevant evidence.
- View detector history.

Unsupported features must not appear as operational.

A detector must not become available merely because its feature flag is enabled.

Its required data source and verification gates must also pass.

Keep advanced configuration understandable for ordinary server owners.

---

# PHASE 8 — TESTING AND RELEASE GATES

Each detector requires independent verification.

Test:

- Legitimate gameplay.
- Known suspicious patterns.
- Missing telemetry.
- Stale position data.
- Repeated events.
- Custom-map exclusions.
- Server restart boundaries.
- Polling recovery.
- Discord delivery failures.
- Cross-installation isolation.
- False-positive scenarios.

Use synthetic or isolated staging fixtures where possible.

Do not invent test results.

A detector must not be marked production-ready until its required console telemetry and release criteria are verified.

For unsupported capabilities, document the limitation rather than implementing speculative detection logic.

---

# NON-NEGOTIABLE SAFETY REQUIREMENTS

Preserve the existing live killfeed.

Preserve the existing economy and player balances.

Preserve existing subscriptions and billing without changing them in this anti-cheat workstream.

Preserve player verification.

Do not modify production Nitrado files.

Do not restart live servers.

Do not enable live enforcement.

Do not activate unverified detectors.

Do not introduce additional anti-cheat modules.

Production-impacting changes require separate approval.

Development, isolated tests, documentation, and pull requests may proceed under the existing project authorization.

Only merge changes that have passed their applicable verification gates.

---

# IMPLEMENTATION INSTRUCTIONS

1. Inspect the existing repository and this roadmap.
2. Identify the current implementation status of each core detector.
3. Identify shared infrastructure that already exists.
4. Keep this development roadmap scoped to exactly eight approved detectors.
5. Implement the shared reliability and evidence improvements.
6. Improve each detector individually.
7. Add automated tests and isolated staging verification.
8. Update Discord and dashboard integration where necessary.
9. Document limitations and unresolved evidence requirements.
10. Open and merge verified, non-production-impacting changes according to existing authorization.

Do not repeatedly request permission for ordinary authorized development work.

Continue sequentially through safe development tasks until the achievable scope is complete or a genuine authorization or evidence blocker is encountered.

At meaningful milestones, report:

- Completed work.
- Tests performed and actual results.
- Pull requests and merge status.
- Remaining detector work.
- Console telemetry limitations.
- Production authorization requirements.
- Next development action.

**FINAL GOAL:**

Deliver eight advanced, dependable DayZ console anti-cheat detectors that customers can understand, configure, and trust.

Quality, reliability, and evidence take priority over feature count.
