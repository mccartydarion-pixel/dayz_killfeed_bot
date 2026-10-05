package database

// InteractiveReadIndexesSQL (migration 0126) adds the one index the interactive read paths were
// still missing after 0111 (query_hygiene_schema.go). Additive only: no table or row changes.
//
//   - player_server_activity (guild_id, server_id) WHERE currently_connected: "who is online on
//     this server" (the live map's occupancy count and player list, the UAV, the online counter)
//     filters on guild, server and currently_connected. The table has one row per player ever seen
//     on a server, so each of those reads walked every player of the server to find the few dozen
//     connected. The partial index holds only the connected rows.
//
// Lock impact: CREATE INDEX (migrations run inside a transaction, so CONCURRENTLY is not
// available) holds a SHARE lock that blocks writes to player_server_activity while it builds.
// The table is small - players x servers, not events - and built in about 20 ms at 100,000 rows.
//
// Deliberately NOT here: indexes on kills, deaths and player_location_events. They are the
// largest tables, written on every kill and every position sample, and an in-transaction build
// blocks those writes for its whole duration inside Migrate's 30 s budget (measured on a small
// test machine: about 3 s for 2 million kills, 3.5 s for 2.6 million deaths, 3.6 s for 5 million
// location rows). The read paths were rewritten to work well with the existing indexes instead.
// Three indexes would still help (per-server player counts in the player directory and stats
// page; newest position per player); an operator can create them at a quiet time, outside a
// migration, without blocking writes:
//
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_kills_server_killer ON kills (guild_id, server_id, killer_player_id);
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deaths_server_player ON deaths (guild_id, server_id, player_id);
//	CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_player_location_events_server_player_time ON player_location_events (server_id, player_id, observed_at DESC, id DESC);
const InteractiveReadIndexesSQL = `
CREATE INDEX IF NOT EXISTS idx_player_server_activity_connected ON player_server_activity (guild_id, server_id) WHERE currently_connected;
`
