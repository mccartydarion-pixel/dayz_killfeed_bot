package database

// QueryHygieneIndexesSQL (migration 0111, docs/PERFORMANCE.md "Query hygiene") adds the indexes
// the existing read paths were missing. Additive only: no table or row changes.
//
//   - players (guild_id, LOWER(display_name)): every case-insensitive name lookup
//     (StatsRepository.GetPlayerProfile / GetPlayerSeasonStats, PlayerRepository,
//     LinkRepository, AnalyticsRepository) filters on LOWER(display_name) = LOWER($n), which the
//     plain idx_players_guild_name (guild_id, display_name) can never serve. The expression must
//     match the queries' exactly - LOWER(display_name) - for the planner to pick it.
//   - kills (guild_id, LOWER(weapon_display)): AnalyticsRepository.WeaponStats, the same pattern.
//   - kills (guild_id, server_id, COALESCE(event_time, created_at)): the time window every
//     per-server read (fight replay, heatmaps, kills-by-hour, network boards) filters and sorts
//     on. A plain created_at range cannot replace it: event_time is NULL for every ADM-sourced
//     row today (Event.Timestamp is never set - docs/CHAMPION_LIVE_SYNC.md A2/A8) and, where it
//     is set, it is the log line's own clock, which after an outage or a backfill read sits hours
//     or days before created_at - no fixed margin is safe. The expression index serves the
//     predicate as written, for NULL and non-NULL event_time alike.
//   - live_sync_records (detected_at): the hourly retention sweep selects rows by detected_at;
//     without this it sequentially scans the highest-volume table every hour.
//   - combat_anomaly_flags (created_at): the new retention sweep for a table that is only ever
//     read within a ten-minute window.
const QueryHygieneIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_players_guild_lower_name ON players (guild_id, LOWER(display_name));
CREATE INDEX IF NOT EXISTS idx_kills_guild_lower_weapon ON kills (guild_id, LOWER(weapon_display));
CREATE INDEX IF NOT EXISTS idx_kills_server_event_window ON kills (guild_id, server_id, (COALESCE(event_time, created_at)));
CREATE INDEX IF NOT EXISTS idx_live_sync_records_detected ON live_sync_records (detected_at);
CREATE INDEX IF NOT EXISTS idx_combat_anomaly_flags_created ON combat_anomaly_flags (created_at);
`
