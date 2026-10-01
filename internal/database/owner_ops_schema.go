package database

// OwnerOpsSQL is the storage behind the Owner Hub operations features (docs/OWNER_OPS.md):
// the owner's automation switches, the incidents the fleet monitor opens and closes, the
// notices the owner broadcasts to customers, and the once-a-day briefing guard. Additive only.
const OwnerOpsSQL = `
-- Owner-controlled switches (alerts, self-healing, customer notices, the daily briefing).
-- A missing key means the default, which is OFF for everything that acts or messages.
CREATE TABLE IF NOT EXISTS platform_settings (
    key TEXT PRIMARY KEY,
    value JSONB NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One row per problem the fleet monitor found on an installation. At most one OPEN row per
-- (installation, kind); a recurrence after it resolves is a new row.
CREATE TABLE IF NOT EXISTS platform_incidents (
    id BIGSERIAL PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    organization_id BIGINT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('WORKER_DOWN','FEED_STALLED','NITRADO_ACCESS','DISCORD_ACCESS')),
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    detail TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_action TEXT NOT NULL DEFAULT '',
    last_action_at TIMESTAMPTZ,
    owner_notified_at TIMESTAMPTZ,
    customer_notified_at TIMESTAMPTZ,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    resolution TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_incidents_open ON platform_incidents(installation_id, kind) WHERE status = 'OPEN';
CREATE INDEX IF NOT EXISTS idx_platform_incidents_recent ON platform_incidents(id DESC);

-- Notices from the platform owner to customers. audience_plan '' means every organization.
CREATE TABLE IF NOT EXISTS platform_broadcasts (
    id BIGSERIAL PRIMARY KEY,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'INFO' CHECK (severity IN ('INFO','MAINTENANCE','INCIDENT')),
    audience_plan TEXT NOT NULL DEFAULT '',
    post_to_discord BOOLEAN NOT NULL DEFAULT FALSE,
    starts_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ends_at TIMESTAMPTZ,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at TIMESTAMPTZ,
    ended_by TEXT
);
CREATE INDEX IF NOT EXISTS idx_platform_broadcasts_active ON platform_broadcasts(starts_at DESC) WHERE ended_at IS NULL;

-- One row per (broadcast, installation) the Discord copy was attempted for, so a retry or a
-- second instance never posts the same notice twice.
CREATE TABLE IF NOT EXISTS platform_broadcast_deliveries (
    broadcast_id BIGINT NOT NULL REFERENCES platform_broadcasts(id) ON DELETE CASCADE,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('SENT','FAILED','NO_CHANNEL')),
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (broadcast_id, installation_id)
);

-- The day's owner briefing: the primary key is what makes it once a day across instances.
CREATE TABLE IF NOT EXISTS platform_briefings (
    day DATE PRIMARY KEY,
    body TEXT NOT NULL DEFAULT '',
    delivered INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
