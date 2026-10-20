package database

// RankedRPBoostsSQL stores double RP windows (docs/RANKED_DOUBLE_RP.md) and records on every award
// the multiplier it was earned under. Additive: existing awards read as multiplier 1.
const RankedRPBoostsSQL = `
CREATE TABLE IF NOT EXISTS ranked_rp_boosts (
    id BIGSERIAL PRIMARY KEY,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    multiplier SMALLINT NOT NULL DEFAULT 2 CHECK (multiplier BETWEEN 2 AND 3),
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    stopped_at TIMESTAMPTZ,
    stopped_by TEXT NOT NULL DEFAULT '',
    scheduled_announced_at TIMESTAMPTZ,
    start_announced_at TIMESTAMPTZ,
    end_announced_at TIMESTAMPTZ,
    CHECK (ends_at > starts_at)
);
CREATE INDEX IF NOT EXISTS idx_ranked_rp_boosts_server ON ranked_rp_boosts(server_id, starts_at);

ALTER TABLE ranked_awards ADD COLUMN IF NOT EXISTS multiplier SMALLINT NOT NULL DEFAULT 1;
`
