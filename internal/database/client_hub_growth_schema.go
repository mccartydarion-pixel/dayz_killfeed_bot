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
