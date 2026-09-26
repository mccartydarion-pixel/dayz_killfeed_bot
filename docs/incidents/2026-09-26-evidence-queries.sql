-- P0 2026-09-26 production evidence queries. READ-ONLY: every statement is a
-- SELECT. Run inside a read-only transaction against production, e.g.
--   psql "$DATABASE_URL" -v guild="'<DISCORD_GUILD_ID>'" -f 2026-09-26-evidence-queries.sql
-- and preserve the output with the incident record before deploying.
BEGIN TRANSACTION READ ONLY;

-- A. Online counter: the legacy channel vs the ONLINE_COUNTER route.
-- Root cause A is confirmed for this guild when online_players_channel_id is
-- set, an ONLINE_COUNTER route exists with a DIFFERENT channel_id, and the
-- legacy channel is the one Discord reports as 10003 Unknown Channel.
SELECT g.id AS guild_row_id, g.discord_guild_id, g.online_players_channel_id AS legacy_online_channel
FROM guilds g WHERE g.discord_guild_id = :guild;

SELECT i.id AS installation_id, i.game_server_id, cr.route_key, cr.channel_id AS route_channel, cr.managed_by_champion, cr.updated_at
FROM installations i
JOIN discord_guild_connections dgc ON dgc.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = dgc.guild_id
JOIN installation_channel_routes cr ON cr.installation_id = i.id
WHERE g.discord_guild_id = :guild AND cr.route_key = 'ONLINE_COUNTER';

SELECT rc.*
FROM installation_retired_channels rc
JOIN installations i ON i.id = rc.installation_id
JOIN discord_guild_connections dgc ON dgc.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = dgc.guild_id
WHERE g.discord_guild_id = :guild;

-- B. PSN linking: Guild -> Organization -> Installation -> game_servers.
-- Root cause B is confirmed when the only active server row has a status
-- outside ('connected','ready','active') - e.g. ONLINE/OFFLINE written by the
-- SaaS dashboard - so the old ConnectedServerID matched zero rows.
SELECT gs.id AS server_id, gs.guild_id, gs.organization_id, gs.provider_service_id, gs.status, gs.active,
       LOWER(gs.status) IN ('connected','ready','active') AS old_resolver_matches,
       (gs.active AND UPPER(COALESCE(gs.status,'')) <> 'DISCONNECTED') AS new_resolver_matches,
       gs.updated_at
FROM game_servers gs JOIN guilds g ON g.id = gs.guild_id
WHERE g.discord_guild_id = :guild ORDER BY gs.id;

SELECT i.id AS installation_id, i.organization_id, i.game_server_id, i.status, dgc.guild_id
FROM installations i
JOIN discord_guild_connections dgc ON dgc.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = dgc.guild_id
WHERE g.discord_guild_id = :guild;

-- Activity being recorded for the server (the "server ID 1" evidence).
SELECT psa.server_id, COUNT(*) AS players, MAX(psa.last_observed_at) AS last_observed,
       SUM(psa.total_observed_seconds) AS total_observed_seconds
FROM player_server_activity psa JOIN guilds g ON g.id = psa.guild_id
WHERE g.discord_guild_id = :guild GROUP BY psa.server_id ORDER BY psa.server_id;

-- Pending links that failed to be created are not stored; recent link rows:
SELECT pl.status, COUNT(*) FROM player_links pl JOIN guilds g ON g.id = pl.guild_id
WHERE g.discord_guild_id = :guild GROUP BY pl.status;

ROLLBACK;
