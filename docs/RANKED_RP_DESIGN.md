# CHAMPIONS Ranked Points — implementation plan

Status: foundation only. No RP award, rank source, announcement, migration, or live route is enabled by this document or `internal/ranked`.

## Confirmed product rules

- A player starts Unranked. Seasonal RP from eligible enemy kills advances through Rookie, Bronze, Silver, Gold, Platinum, Diamond, and Master.
- Elite Top 250 is the top 250 Master players by seasonal RP on each platform (PlayStation and Xbox separately). It is a position, not another RP threshold.
- Global standings combine participating CHAMPIONS servers on the same platform. A server has a separate local season and RP standings. One eligible kill may contribute to both ledgers exactly once each.
- A server owner's season reset archives and resets only their local standings. Global seasons roll over on a central CHAMPIONS schedule and retain archived results.
- A repeat kill of the same victim by the same attacker within five minutes gives no RP. It still counts in the ordinary killfeed and combat statistics.
- `/setup` creates a dedicated server-ranks channel with one persistent edited standings message. Global standings live on the site with platform views. Owners may opt into a global Discord announcement; default cadence is daily, while the site and server board may refresh every three hours.
- The Player Hub shows separate global and server RP, tiers, next-tier progress, and standing. Rank-up DM notifications are optional. Ranked Points are distinct from Champ Credits and do not change the shop balance.

## Source boundaries and unresolved values

`players` currently has a `(guild_id, dayz_player_id)` identity and `kills` has a `(guild_id, event_fingerprint)` dedupe key. Neither alone establishes a trusted console identity across guilds or guarantees that the same physical kill imported by two installations receives one global award. Before award ingestion, audit the actual console ID source and linking flow, define a platform-qualified canonical identity, and prove event deduplication across participating installations. Display names and Discord IDs must not become global identity keys. Unknown or conflicting identity means no ranked award until resolved.

The RP award per kill, cumulative tier thresholds, season duration/start time, tie-break order, participation controls, and whether historical kills are backfilled still need product decisions. Freeze award and threshold rules in each season record. Do not infer them from this module's test fixture numbers.

## Implementation order

1. Audit console identity and duplicate kill paths with real sanitized fixtures; document supported PlayStation/Xbox identifiers and conflicts.
2. Add versioned global and local season records and an immutable award ledger keyed to the canonical kill and scope. Store reason for ineligible kills. Make insertion and aggregate update transactional and replay safe. Protect the five-minute same-victim window across servers using serialized pair updates and event time, including delayed/out-of-order logs.
3. Define RP values and tier thresholds, freeze them per season, and test tier boundaries, Elite eligibility, deterministic ties, resets, and archives.
4. In staging, replay normal, duplicate, delayed, conflicting, self-kill and repeat-victim events. Reconcile RP totals against ledger sums before enabling live award ingestion. Keep existing kill persistence and economy independent of RP failure.
5. Add per-server ranked queries and connect PR #160's `RankReader` only after the source is trustworthy. Provide the dedicated `/setup` route and persistent message.
6. Add public site global standings, per-player Player Hub progress, optional DMs, and opt-in daily announcement with durable delivery dedupe. Validate privacy and cross-platform separation.
7. Release behind explicit flags and staged verification. A production migration, live RP awarding, and announcements require separate production approval.

The existing V3 board in PR #160 remains a draft with its rank embed inactive until steps 1–4 are complete.
