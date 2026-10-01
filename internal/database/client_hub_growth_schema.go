package database

// Client Hub growth tools (docs/CLIENT_HUB_GROWTH.md). Every migration here is additive: new
// tables or nullable/defaulted columns only, so no existing row changes meaning.

// EventBuilderSQL lets owner-built events carry their template and announcement state. announce
// controls whether Champion posts the "upcoming" and "started" cards; the *_announced_at columns
// make each card post exactly once.
const EventBuilderSQL = `
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS template_key TEXT;
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS announce BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS schedule_announced_at TIMESTAMPTZ;
ALTER TABLE competitive_events ADD COLUMN IF NOT EXISTS start_announced_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_competitive_events_announce ON competitive_events(guild_id, status) WHERE announce AND start_announced_at IS NULL;
`

// SeasonPlannerSQL stores season changes an owner schedules ahead: a stats season rollover
// (archive the active stats season, start a named new one) or a per-server Ranked reset (archive
// the server's Ranked season and start a new one on the SAME rules). At most one pending action
// per kind and server; notice_sent_at makes the "scheduled" card post once.
const SeasonPlannerSQL = `
CREATE TABLE IF NOT EXISTS scheduled_season_actions (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT REFERENCES game_servers(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('STATS_SEASON','RANKED_RESET')),
    run_at TIMESTAMPTZ NOT NULL,
    new_season_name TEXT NOT NULL DEFAULT '',
    announce BOOLEAN NOT NULL DEFAULT TRUE,
    status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','RUNNING','DONE','FAILED','CANCELLED')),
    created_by TEXT NOT NULL DEFAULT '',
    notice_sent_at TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    executed_at TIMESTAMPTZ,
    result_detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((kind = 'RANKED_RESET') = (server_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_scheduled_season_actions_open ON scheduled_season_actions(guild_id, kind, COALESCE(server_id, 0)) WHERE status IN ('PENDING','RUNNING');
CREATE INDEX IF NOT EXISTS idx_scheduled_season_actions_due ON scheduled_season_actions(guild_id, run_at) WHERE status = 'PENDING';
`

// InviteTrackingSQL records which Discord invite each member joined through (code and inviter
// only; no message content) and when they left, so the Client Hub can show which invites bring
// players who stay, link and play.
const InviteTrackingSQL = `
CREATE TABLE IF NOT EXISTS discord_invite_joins (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    member_discord_id TEXT NOT NULL,
    invite_code TEXT,
    inviter_discord_id TEXT,
    inviter_name TEXT,
    joined_at TIMESTAMPTZ NOT NULL,
    left_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_discord_invite_joins_guild_joined ON discord_invite_joins(guild_id, joined_at DESC);
CREATE INDEX IF NOT EXISTS idx_discord_invite_joins_member ON discord_invite_joins(guild_id, member_discord_id, joined_at DESC);
`

// RewardRulesSQL stores automatic Champion Point rewards an owner configures. Payouts are ordinary
// SYSTEM_REWARD ledger rows whose source_key ("reward:...") makes each one pay at most once.
const RewardRulesSQL = `
CREATE TABLE IF NOT EXISTS reward_rules (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('RANK_REACHED','WEEKLY_ACTIVE','SEASON_TOP')),
    tier TEXT NOT NULL DEFAULT '',
    points BIGINT NOT NULL CHECK (points BETWEEN 1 AND 1000000),
    min_hours INTEGER NOT NULL DEFAULT 0,
    min_days INTEGER NOT NULL DEFAULT 0,
    places INTEGER NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (guild_id, kind, tier)
);
`

// PlayerNameHistorySQL records every in-game name change from now on (a trigger on players), for
// the staff player timeline. Earlier changes were never stored and are not reconstructed.
const PlayerNameHistorySQL = `
CREATE TABLE IF NOT EXISTS player_name_history (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    old_name TEXT NOT NULL,
    new_name TEXT NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_player_name_history_player ON player_name_history(player_id, changed_at DESC);
CREATE OR REPLACE FUNCTION players_record_name_change() RETURNS trigger AS $fn$
BEGIN
    INSERT INTO player_name_history(guild_id, player_id, old_name, new_name) VALUES (NEW.guild_id, NEW.id, OLD.display_name, NEW.display_name);
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_players_name_history ON players;
CREATE TRIGGER trg_players_name_history AFTER UPDATE OF display_name ON players
    FOR EACH ROW WHEN (OLD.display_name IS DISTINCT FROM NEW.display_name) EXECUTE FUNCTION players_record_name_change();
`
