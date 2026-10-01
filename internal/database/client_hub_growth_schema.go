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
