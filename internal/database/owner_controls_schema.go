package database

// OwnerControlsSQL is the platform owner's write surface (docs/ADMIN_API.md "Owner controls"):
// a cross-tenant audit table that every owner action writes to first, and the few columns the
// actions need. Additive only; nothing here is read by customer-facing code except the ban and
// grant columns, which gate access.
const OwnerControlsSQL = `
CREATE TABLE IF NOT EXISTS platform_audit_log (
    id BIGSERIAL PRIMARY KEY,
    actor_discord_id TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id BIGINT,
    organization_id BIGINT,
    reason TEXT NOT NULL DEFAULT '',
    before_state JSONB,
    after_state JSONB,
    result TEXT NOT NULL DEFAULT 'OK',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_platform_audit_log_created ON platform_audit_log(id DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_log_org ON platform_audit_log(organization_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_log_target ON platform_audit_log(target_type, target_id, id DESC);

ALTER TABLE app_users ADD COLUMN IF NOT EXISTS banned_at TIMESTAMPTZ;
ALTER TABLE app_users ADD COLUMN IF NOT EXISTS ban_reason TEXT;

ALTER TABLE installations ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMPTZ;
ALTER TABLE installations ADD COLUMN IF NOT EXISTS suspended_reason TEXT;
ALTER TABLE installations ADD COLUMN IF NOT EXISTS status_before_suspend TEXT;

ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS owner_grant_until TIMESTAMPTZ;
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS owner_grant_reason TEXT;
`
