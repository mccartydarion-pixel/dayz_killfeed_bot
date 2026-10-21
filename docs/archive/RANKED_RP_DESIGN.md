# CHAMPIONS Ranked Points — implementation plan

Scope: server ranks only. Each server has its own season, RP standings, and Player Hub progress.

## Confirmed product rules

- A player starts Unranked. Seasonal RP from eligible enemy kills advances through Rookie, Bronze, Silver, Gold, Platinum, Diamond, and Master.
- A server owner's season reset archives and resets that server's standings.
- A repeat kill of the same victim by the same attacker within five minutes gives no RP. It still counts in the ordinary killfeed and combat statistics.
- `/setup` creates a dedicated server-ranks channel with one persistent edited standings message. The board refreshes every three hours.
- The Player Hub shows that server's RP, tier, next-tier progress, and standing. Ranked Points remain distinct from Champ Credits.

## Source boundaries and unresolved values

Local RP uses persisted player and kill IDs on the selected game server. Missing player IDs, self-kills, and uncertain event attribution do not earn RP. A separate fixture must check indistinguishable same-second kills from two servers in one guild, because existing kill deduplication is guild-scoped.

The RP award per kill, cumulative tier thresholds, season duration/start time, tie-break order, participation controls, and whether historical kills are backfilled still need product decisions. Freeze award and threshold rules in each season record. Do not infer them from this module's test fixture numbers.

## Implementation order

1. Verify kill attribution and duplicate kill paths for one server and multi-server guilds using sanitized fixtures.
2. Add server season records and an immutable award ledger keyed to persisted kills. Keep awards transactional and replay safe, with a five-minute same-victim window per server.
3. Define RP values and tier thresholds, freeze them per season, and test boundaries, ties, resets, and archives.
4. In staging, replay normal, duplicate, delayed, conflicting, self-kill and repeat-victim events. Reconcile RP totals against ledger sums before enabling live award ingestion. Keep existing kill persistence and economy independent of RP failure.
5. Add per-server ranked queries and connect PR #160's `RankReader` only after the source is trustworthy. Provide the dedicated `/setup` route and persistent message.
6. Add per-player Player Hub progress and optional DMs. Validate privacy and server scoping.
7. Release behind explicit flags and staged verification. A production migration, live RP awarding, and announcements require separate production approval.

The V3 rank source must use the selected server's active season; standings from separate servers must stay separate.
