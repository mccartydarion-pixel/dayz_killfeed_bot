package database

// FeatureFlagsSQL stores the platform owner's per-installation feature overrides (Owner Hub
// "Feature flags", internal/featureflags). One row per (installation, flag); a missing row
// means the environment default applies. Additive only.
const FeatureFlagsSQL = `
CREATE TABLE IF NOT EXISTS installation_feature_flags (
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    flag TEXT NOT NULL,
    enabled BOOLEAN NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (installation_id, flag)
);
`
