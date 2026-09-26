-- Reliability release (PR #108) production preflight. READ-ONLY: every statement is a
-- SELECT inside a READ ONLY transaction that is rolled back. Run BEFORE the deploy and
-- again AFTER it (the "after" run is the live acceptance baseline), e.g.
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v guild="'<DISCORD_GUILD_ID>'" -f 2026-09-26-release-preflight-queries.sql
-- Complements 2026-09-26-evidence-queries.sql (counter/linking root causes).
-- Validated against the deployed schema (0055, 605f1ec) and the release schema (0057).
BEGIN TRANSACTION READ ONLY;
SET LOCAL statement_timeout = '15s';

-- R1. Migration state. Before the deploy: last = 0055_shop_delivery_attempt_evidence,
-- feed_journal_table = NULL, role_sync_column = 0. After: 0057, table present, 1.
SELECT COUNT(*) AS applied, MAX(name) AS last_migration FROM schema_migrations;
SELECT to_regclass('public.discord_feed_cards') AS feed_journal_table;
SELECT COUNT(*) AS role_sync_column FROM information_schema.columns
WHERE table_name = 'player_links' AND column_name = 'role_sync_status';

-- R2. Migration 0056 cost: ALTER TABLE player_links (metadata-only on PG 11+, constant
-- default) plus one partial CREATE INDEX that briefly blocks writes to player_links.
SELECT COUNT(*) AS player_links_rows, pg_size_pretty(pg_total_relation_size('player_links')) AS player_links_size;
SELECT current_setting('server_version') AS postgres_version;

-- R3. Legacy per-guild channels (pre-V2 GuildSetup). A legacy killfeed/death channel
-- is used only when the guild has no KILLFEED route.
SELECT g.id AS guild_row_id, g.killfeed_channel_id AS legacy_killfeed, g.death_channel_id AS legacy_death,
       g.online_players_channel_id AS legacy_online, g.verified_role_id
FROM guilds g WHERE g.discord_guild_id = :guild;

-- R4. V2 routes that decide where cards and the counter go. The death feed follows
-- the KILLFEED route when one exists (one combat channel, two 10-card windows).
SELECT i.id AS installation_id, i.game_server_id, cr.route_key, cr.channel_id, cr.managed_by_champion, cr.updated_at
FROM installations i
JOIN discord_guild_connections dgc ON dgc.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = dgc.guild_id
JOIN installation_channel_routes cr ON cr.installation_id = i.id
WHERE g.discord_guild_id = :guild AND cr.route_key IN ('KILLFEED', 'PVE_FEED', 'ONLINE_COUNTER', 'LINK_GAMERTAG')
ORDER BY i.id, cr.route_key;

-- R5. Conflicts to resolve before the release (each row is one finding):
--   KILLFEED_ROUTE_DIFFERS_FROM_LEGACY : cards go to the route; the legacy channel keeps
--                                         whatever the old process last posted there.
--   LEGACY_DEATH_CHANNEL_SHADOWED      : deaths go to the KILLFEED route, not the legacy death channel.
--   ONLINE_ROUTE_DIFFERS_FROM_LEGACY   : the counter renames the route channel only (P0 root cause A).
SELECT 'KILLFEED_ROUTE_DIFFERS_FROM_LEGACY' AS finding, g.killfeed_channel_id AS legacy, cr.channel_id AS route
FROM guilds g JOIN discord_guild_connections dgc ON dgc.guild_id = g.id
JOIN installations i ON i.discord_guild_connection_id = dgc.id
JOIN installation_channel_routes cr ON cr.installation_id = i.id AND cr.route_key = 'KILLFEED'
WHERE g.discord_guild_id = :guild AND g.killfeed_channel_id IS NOT NULL AND g.killfeed_channel_id <> cr.channel_id
UNION ALL
SELECT 'LEGACY_DEATH_CHANNEL_SHADOWED', g.death_channel_id, cr.channel_id
FROM guilds g JOIN discord_guild_connections dgc ON dgc.guild_id = g.id
JOIN installations i ON i.discord_guild_connection_id = dgc.id
JOIN installation_channel_routes cr ON cr.installation_id = i.id AND cr.route_key = 'KILLFEED'
WHERE g.discord_guild_id = :guild AND g.death_channel_id IS NOT NULL AND g.death_channel_id <> cr.channel_id
UNION ALL
SELECT 'ONLINE_ROUTE_DIFFERS_FROM_LEGACY', g.online_players_channel_id, cr.channel_id
FROM guilds g JOIN discord_guild_connections dgc ON dgc.guild_id = g.id
JOIN installations i ON i.discord_guild_connection_id = dgc.id
JOIN installation_channel_routes cr ON cr.installation_id = i.id AND cr.route_key = 'ONLINE_COUNTER'
WHERE g.discord_guild_id = :guild AND g.online_players_channel_id IS NOT NULL AND g.online_players_channel_id <> cr.channel_id;

-- R6. Servers the release will run workers for (one kill feed + one death feed each).
SELECT gs.id AS server_id, gs.provider_service_id, gs.status, gs.active
FROM game_servers gs JOIN guilds g ON g.id = gs.guild_id
WHERE g.discord_guild_id = :guild ORDER BY gs.id;

-- R7. Links: VERIFIED links existing before 0056 keep role_sync_status NULL and are never
-- reconciled automatically; only links verified after the release are.
SELECT pl.status, COUNT(*) AS links FROM player_links pl JOIN guilds g ON g.id = pl.guild_id
WHERE g.discord_guild_id = :guild GROUP BY pl.status ORDER BY pl.status;

-- R8. Durable event baseline for duplicate/loss checks after the deploy.
SELECT COUNT(*) AS kills_last_24h, MAX(k.created_at) AS last_kill_row
FROM kills k JOIN guilds g ON g.id = k.guild_id
WHERE g.discord_guild_id = :guild AND k.created_at > NOW() - INTERVAL '24 hours';

-- R9. C.A.S.E. baseline (the release changes no C.A.S.E. code, table or setting):
-- live row estimates of every case_* / live_sync_* table, to compare after the deploy.
SELECT relname, n_live_tup FROM pg_stat_user_tables
WHERE relname LIKE 'case\_%' OR relname LIKE 'live\_sync\_%' ORDER BY relname;

ROLLBACK;
