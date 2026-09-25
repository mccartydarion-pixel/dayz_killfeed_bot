package canary

// ProposedAttemptMigrationName / ProposedAttemptMigrationSQL are the durable attempt ledger PROPOSAL.
// They are deliberately NOT registered in internal/database/migrations.go: deploying the table is its
// own approval. The rules are the ones nitradodelivery.MemoryLedger enforces in memory: at most one
// open attempt per delivery, compare-and-set transitions, no new attempt after an uncertain one, and
// FULFILLED only from VERIFICATION_REQUIRED.
const ProposedAttemptMigrationName = "0054_shop_delivery_attempts"

const ProposedAttemptMigrationSQL = `
CREATE TABLE IF NOT EXISTS shop_delivery_attempts (
    id BIGSERIAL PRIMARY KEY,
    organization_id BIGINT NOT NULL,
    installation_id BIGINT NOT NULL,
    delivery_id BIGINT NOT NULL REFERENCES shop_deliveries(id) ON DELETE RESTRICT,
    attempt INT NOT NULL CHECK (attempt >= 1),
    attempt_id TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('PLAN_CREATED','FILE_PREPARED','FILE_STAGED','AWAITING_RESTART','RESTART_OBSERVED',
        'UNSTAGE_REQUIRED','VERIFICATION_REQUIRED','FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW')),
    fingerprint TEXT NOT NULL,
    artifact_path TEXT NOT NULL,
    before_sha256 TEXT,
    staged_sha256 TEXT,
    unstaged_sha256 TEXT,
    staged_at TIMESTAMPTZ,
    staged_boot_file TEXT,
    restart_boot_file TEXT,
    restart_observed_at TIMESTAMPTZ,
    unstage_verified_at TIMESTAMPTZ,
    verified_by TEXT,
    fulfilled_at TIMESTAMPTZ,
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (delivery_id, attempt),
    CHECK (state <> 'FULFILLED' OR (verified_by IS NOT NULL AND fulfilled_at IS NOT NULL AND unstage_verified_at IS NOT NULL)),
    CHECK (state NOT IN ('VERIFICATION_REQUIRED','FULFILLED') OR unstage_verified_at IS NOT NULL)
);
-- At most one open attempt per delivery.
CREATE UNIQUE INDEX IF NOT EXISTS uq_shop_delivery_attempts_open ON shop_delivery_attempts(delivery_id)
    WHERE state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW');
CREATE INDEX IF NOT EXISTS idx_shop_delivery_attempts_installation ON shop_delivery_attempts(installation_id, state, id DESC);
-- Append-only transition history (who/what moved it, with the evidence digest).
CREATE TABLE IF NOT EXISTS shop_delivery_attempt_events (
    id BIGSERIAL PRIMARY KEY,
    attempt_row_id BIGINT NOT NULL REFERENCES shop_delivery_attempts(id) ON DELETE RESTRICT,
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    actor TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '' CHECK (length(evidence) <= 1024),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

// ProposedTransitionSQL is the compare-and-set a durable ledger uses: it moves an attempt only from the
// expected state, so two workers can never both advance it.
const ProposedTransitionSQL = `UPDATE shop_delivery_attempts SET state=$3, updated_at=NOW()
 WHERE attempt_id=$1 AND state=$2 AND organization_id=$4 AND installation_id=$5 RETURNING id`
