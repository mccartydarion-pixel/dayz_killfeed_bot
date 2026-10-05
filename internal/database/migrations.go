package database

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Migration is one ordered, named schema change.
type Migration struct {
	Name string
	SQL  string
}

// EconomyBackfillSQL is migration 0030's one-time data backfill: every historical
// point award was a credit (there were never debits), so the spendable balance
// starts equal to the lifetime total, and each ledger row gets its running
// balance. It is a constant so a test can run it against legacy-shaped rows.
const EconomyBackfillSQL = `
UPDATE player_points SET balance = lifetime_points WHERE balance = 0 AND lifetime_points > 0;
UPDATE point_transactions t SET balance_after = r.running
FROM (SELECT id, SUM(amount) OVER (PARTITION BY guild_id, player_id ORDER BY id) AS running FROM point_transactions) r
WHERE t.id = r.id AND t.balance_after IS NULL;
`

// migrations is the ordered list of schema changes. New migrations append at the
// end; never edit an applied migration. Each runs once, transactionally.
//
// The entries live in the migrations_NNNN_NNNN.go files, one fragment per number range, and are
// joined here in execution order. Append a new migration to the last fragment (or start a new
// fragment and add it at the end of this list). TestMigrationRegistryMatchesGolden pins the
// resulting order, names and SQL.
var migrations = slices.Concat(
	migrations0001to0023,
	migrations0024to0035,
	migrations0036to0048,
	migrations0049to0064,
	migrations0065to0125,
	migrations0126onward,
)

// NitradoTailTrustSQL keeps which Nitrado services have proven that partial (seek) reads match
// full downloads (docs/NITRADO_POLLING.md), so a restart does not repeat the verification.
const NitradoTailTrustSQL = `
CREATE TABLE IF NOT EXISTS nitrado_tail_trust (
    service_id TEXT PRIMARY KEY,
    matches INT NOT NULL DEFAULT 0,
    trusted BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

// PvPDeathBackfillSQL gives the victim of every existing PvP kill a deaths row of type PVP, with
// the kill's own time, season, server and ADM source. Idempotent: the row's fingerprint is derived
// from the kill's, and (guild_id, event_fingerprint) is unique. A self-kill is not a PvP death. A
// kill whose victim already has a death row within five seconds of it is skipped - nobody dies
// twice in five seconds, so that row is the same death and must not be counted again.
const PvPDeathBackfillSQL = `
INSERT INTO deaths (guild_id, server_id, session_id, event_fingerprint, player_id, season_id, death_type, event_time, created_at,
    source_file, source_offset, source_local_time)
SELECT k.guild_id, k.server_id, k.session_id, 'pvp:' || k.event_fingerprint, k.victim_player_id, k.season_id, 'PVP', k.event_time, k.created_at,
    k.source_file, k.source_offset, k.source_local_time
FROM kills k
WHERE k.victim_player_id IS NOT NULL AND k.killer_player_id IS DISTINCT FROM k.victim_player_id
  AND NOT EXISTS (
    SELECT 1 FROM deaths d
    WHERE d.guild_id = k.guild_id AND d.player_id = k.victim_player_id
      AND COALESCE(d.event_time, d.created_at) BETWEEN COALESCE(k.event_time, k.created_at) - INTERVAL '5 seconds'
                                                   AND COALESCE(k.event_time, k.created_at) + INTERVAL '5 seconds')
ON CONFLICT (guild_id, event_fingerprint) DO NOTHING;
`

// RankedSameVictimCooldownSQL adds a season's frozen same-victim wait in minutes (0 = no wait).
// Archived seasons live in the same table (status='ARCHIVED'), so one column covers both.
const RankedSameVictimCooldownSQL = `
ALTER TABLE ranked_seasons ADD COLUMN IF NOT EXISTS same_victim_cooldown_minutes INTEGER NOT NULL DEFAULT 5
    CHECK (same_victim_cooldown_minutes BETWEEN 0 AND 120);
`

// RankedLedgerFoundationSQL creates server-scoped seasonal RP storage.
// No existing kills or economy rows are rewritten.
const RankedLedgerFoundationSQL = `
CREATE TABLE IF NOT EXISTS ranked_seasons (
    id BIGSERIAL PRIMARY KEY,
    scope TEXT NOT NULL DEFAULT 'SERVER' CHECK (scope = 'SERVER'),
    platform TEXT NOT NULL CHECK (platform IN ('PLAYSTATION','XBOX')),
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT,
    status TEXT NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','ACTIVE','ARCHIVED')),
    rp_per_kill BIGINT NOT NULL CHECK (rp_per_kill > 0),
    thresholds BIGINT[] NOT NULL CHECK (array_length(thresholds,1)=7),
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at IS NULL OR ends_at > starts_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ranked_active_server ON ranked_seasons(server_id) WHERE status='ACTIVE' AND scope='SERVER';
CREATE TABLE IF NOT EXISTS ranked_awards (
    id BIGSERIAL PRIMARY KEY,
    season_id BIGINT NOT NULL REFERENCES ranked_seasons(id) ON DELETE RESTRICT,
    kill_id BIGINT NOT NULL REFERENCES kills(id) ON DELETE RESTRICT,
    source_key TEXT NOT NULL CHECK (length(source_key)>0),
    attacker_key TEXT NOT NULL CHECK (length(attacker_key)>0),
    victim_key TEXT NOT NULL CHECK (length(victim_key)>0),
    event_time TIMESTAMPTZ NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('AWARDED','COOLDOWN','OUT_OF_ORDER')),
    amount BIGINT NOT NULL CHECK (amount>=0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(season_id,kill_id),
    UNIQUE(season_id,source_key),
    CHECK ((outcome='AWARDED' AND amount>0) OR (outcome<>'AWARDED' AND amount=0))
);
CREATE INDEX IF NOT EXISTS idx_ranked_awards_standings ON ranked_awards(season_id,attacker_key) WHERE outcome='AWARDED';
CREATE INDEX IF NOT EXISTS idx_ranked_awards_repeat ON ranked_awards(season_id,attacker_key,victim_key,event_time DESC) WHERE outcome='AWARDED';
`

// DiscordFeedCardsSQL (migration 0066) is the immediate-mode feed journal: each queued card is
// recorded before it is posted, marked when Discord confirms it (message_id) and again when it
// leaves the channel, so a restart - including a crash - neither loses queued cards nor leaves the
// previous process's cards in the channel. feed_key is "<route>:<server id>".
const DiscordFeedCardsSQL = `
CREATE TABLE IF NOT EXISTS discord_feed_cards (
    id BIGSERIAL PRIMARY KEY,
    feed_key TEXT NOT NULL,
    nonce TEXT NOT NULL,
    embed JSONB NOT NULL,
    detected_at TIMESTAMPTZ,
    enqueued_at TIMESTAMPTZ NOT NULL,
    channel_id TEXT,
    message_id TEXT,
    posted_at TIMESTAMPTZ,
    removed_at TIMESTAMPTZ,
    dropped_at TIMESTAMPTZ,
    drop_reason TEXT,
    UNIQUE (feed_key, nonce)
);
CREATE INDEX IF NOT EXISTS idx_discord_feed_cards_open ON discord_feed_cards(feed_key, id)
    WHERE removed_at IS NULL AND dropped_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_discord_feed_cards_enqueued ON discord_feed_cards(enqueued_at);
`

// PlayerLinkRoleSyncSQL (migration 0065) records whether the Verified Discord role was actually
// assigned for a VERIFIED link - distinct from the link itself being verified.
const PlayerLinkRoleSyncSQL = `
ALTER TABLE player_links ADD COLUMN IF NOT EXISTS role_sync_status TEXT;
ALTER TABLE player_links ADD COLUMN IF NOT EXISTS role_sync_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE player_links ADD COLUMN IF NOT EXISTS role_sync_last_attempt_at TIMESTAMPTZ;
ALTER TABLE player_links ADD COLUMN IF NOT EXISTS role_synced_at TIMESTAMPTZ;
ALTER TABLE player_links ADD COLUMN IF NOT EXISTS role_sync_error TEXT;
CREATE INDEX IF NOT EXISTS idx_player_links_role_pending ON player_links(guild_id, role_sync_last_attempt_at)
    WHERE status = 'VERIFIED' AND role_sync_status IN ('PENDING', 'FAILED');
`

// CaseShadowLedgerSQL is additive. The composite FK ensures that an evidence
// link belongs to the SAME guild and game server as the evaluation. A blocked
// evaluation is diagnostic only; this schema does not store scores or sanctions.
const CaseShadowLedgerSQL = `
CREATE UNIQUE INDEX IF NOT EXISTS uq_case_evidence_scope_id
 ON case_evidence_events(guild_id,server_id,id);
CREATE TABLE IF NOT EXISTS case_shadow_evaluations (
 id BIGSERIAL PRIMARY KEY,
 guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
 server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
 detector_id TEXT NOT NULL CHECK (char_length(detector_id) BETWEEN 1 AND 80),
 detector_version TEXT NOT NULL CHECK (char_length(detector_version) BETWEEN 1 AND 40),
 fingerprint CHAR(64) NOT NULL,
 status TEXT NOT NULL CHECK (status = 'BLOCKED'),
 reason_codes TEXT[] NOT NULL CHECK (cardinality(reason_codes) > 0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 CONSTRAINT uq_case_shadow_fingerprint UNIQUE(guild_id,server_id,detector_id,detector_version,fingerprint),
 CONSTRAINT uq_case_shadow_scope_id UNIQUE(guild_id,server_id,id)
);
CREATE TABLE IF NOT EXISTS case_shadow_evaluation_evidence (
 guild_id BIGINT NOT NULL,
 server_id BIGINT NOT NULL,
 evaluation_id BIGINT NOT NULL,
 evidence_id BIGINT NOT NULL,
 PRIMARY KEY(evaluation_id,evidence_id),
 FOREIGN KEY(guild_id,server_id,evaluation_id)
  REFERENCES case_shadow_evaluations(guild_id,server_id,id) ON DELETE CASCADE,
 FOREIGN KEY(guild_id,server_id,evidence_id)
  REFERENCES case_evidence_events(guild_id,server_id,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_case_shadow_scope_recent
 ON case_shadow_evaluations(guild_id,server_id,id DESC);
`

// LiveSyncCommandLineCleanupSQL (migration 0052, Champion Live Sync phase 2.1, docs/
// CHAMPION_LIVE_SYNC.md section 7.8): parser cls-1.1 stored each RPT's command-line header with its IP
// and service name redacted but its game port and config file name kept - one record per RPT. Parser
// cls-1.2 stores no evidence for that line. This removes exactly those records (RPT, LOG_HEADER,
// parser cls-1.1, an "==" line carrying "-port=") and nothing else.
const LiveSyncCommandLineCleanupSQL = `
DELETE FROM live_sync_records
WHERE family = 'RPT' AND category = 'LOG_HEADER' AND parser = 'cls-1.1'
  AND evidence LIKE '==%' AND evidence LIKE '%-port=%';
`

// ShopDeliveryBackfillSQL gives every purchase that has no delivery record a MANUAL_PICKUP one,
// derived from - never changing - the purchase's own fulfillment/refund state: an open purchase is
// MANUAL_READY; a fulfilled purchase (including one refunded after it was handed over) keeps its
// historical fulfillment; a purchase refunded, cancelled or failed before fulfillment is
// CANCELLED. Idempotent (ON CONFLICT on the one-delivery-per-purchase key).
const ShopDeliveryBackfillSQL = shopDeliveryBackfillInsert + ` ON CONFLICT (purchase_id) DO NOTHING;
`

// ShopDeliveryHealSQL is the same derivation for one purchase ($1): the repository runs it inside
// fulfill/refund/read paths so a purchase written by an older instance during a rolling deploy
// (after this migration ran) still gets its delivery record. A no-op when the record exists.
const ShopDeliveryHealSQL = shopDeliveryBackfillInsert + ` AND sp.id = $1 ON CONFLICT (purchase_id) DO NOTHING`

const shopDeliveryBackfillInsert = `
INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, status,
    created_at, updated_at, fulfilled_at, fulfilled_by_user_id, cancelled_at, cancelled_by_user_id, cancel_reason)
SELECT sp.id, sp.organization_id, sp.installation_id, sp.game_server_id, sp.player_id, 'MANUAL_PICKUP',
    CASE WHEN sp.fulfilled_at IS NOT NULL THEN 'FULFILLED'
         WHEN sp.status IN ('REFUNDED','CANCELLED','FAILED') THEN 'CANCELLED'
         ELSE 'MANUAL_READY' END,
    sp.created_at, NOW(), sp.fulfilled_at, sp.fulfilled_by_user_id,
    CASE WHEN sp.fulfilled_at IS NULL AND sp.status IN ('REFUNDED','CANCELLED','FAILED') THEN COALESCE(sp.refunded_at, sp.cancelled_at, sp.updated_at) END,
    CASE WHEN sp.fulfilled_at IS NULL AND sp.status = 'REFUNDED' THEN sp.refunded_by_user_id END,
    CASE WHEN sp.fulfilled_at IS NOT NULL THEN NULL
         WHEN sp.status = 'REFUNDED' THEN 'REFUNDED'
         WHEN sp.status = 'CANCELLED' THEN 'PURCHASE_CANCELLED'
         WHEN sp.status = 'FAILED' THEN 'PURCHASE_FAILED' END
FROM shop_purchases sp
WHERE NOT EXISTS (SELECT 1 FROM shop_deliveries d WHERE d.purchase_id = sp.id)`

// TrialGrantBackfillSQL records, for every owner whose organization already had a trial before
// 0047 (every organization received one at creation), that their one trial is used - so existing
// trials are preserved as they are and those owners' next organizations get none. Idempotent.
const TrialGrantBackfillSQL = `
INSERT INTO trial_grants(user_id, organization_id, granted_at)
SELECT DISTINCT ON (m.user_id) m.user_id, s.organization_id, s.created_at
FROM subscriptions s
JOIN organization_members m ON m.organization_id = s.organization_id AND m.role = 'OWNER'
WHERE s.trial_ends_at IS NOT NULL OR s.trial_consumed
ORDER BY m.user_id, s.created_at, s.organization_id
ON CONFLICT DO NOTHING;
`

// admLocationAxisFixSQL repairs player_location_events rows written before the ADM axis fix.
// DayZ's ADM prints "pos=<x, z, y>" (PluginAdminLog.GetPlayerPrefix builds it from engine
// [0],[2],[1]: east, north, altitude), but the Phase 3 writer stored the second value as y and
// the third as z - so z held altitude and y held the real north coordinate. Heatmaps, zone
// distance and location history all key on x/z, so every pre-fix ADM row is swapped back.
// Postgres evaluates every SET expression against the row's old values, so "z = y, y = z" is a
// true swap. Rows with a NULL y cannot be repaired (the north coordinate was never stored) and
// the writer never produced any, so they are left alone rather than guessed at. The
// (player_id, server_id, event_type, observed_at) dedupe key does not include coordinates, so
// the swap can never collide. Runs exactly once via schema_migrations; rows written afterwards
// already use the corrected mapping (killfeed.Position.MapX/MapZ/Altitude).
const admLocationAxisFixSQL = `
UPDATE player_location_events
   SET z = y, y = z
 WHERE source = 'ADM' AND y IS NOT NULL;
`

// Migrate applies all pending migrations in order, each transactionally. A
// failure stops startup (returns an error) rather than running a partial schema.
func (d *DB) Migrate(ctx context.Context) error {
	if d == nil || d.Pool == nil {
		return fmt.Errorf("database not connected")
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Ensure the migrations bookkeeping table exists first (under the migration lock, so two
	// instances starting on an empty database do not race on CREATE TABLE).
	if err := d.withMigrationLock(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		applied, err := d.isApplied(ctx, m.Name)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.Name, err)
		}
		if applied {
			continue
		}

		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.Name, err)
		}
		// Two instances starting together (a rolling deploy with overlap, or a restart during a
		// deploy) serialize here, and the loser re-checks inside the lock and skips the migration
		// instead of failing on a duplicate object or schema_migrations key. The lock is released
		// with the transaction.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock migration %s: %w", m.Name, err)
		}
		var already bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, m.Name).Scan(&already); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("recheck migration %s: %w", m.Name, err)
		}
		if already {
			_ = tx.Rollback(ctx)
			continue
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, m.Name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", m.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
		slog.Info("component=database", "msg", "migration applied", "name", m.Name)
	}
	return nil
}

// migrationLockKey is the transaction-level advisory lock that serializes concurrent Migrate calls.
const migrationLockKey int64 = 0x43484d5047524154 // "CHMPGRAT"

func (d *DB) withMigrationLock(ctx context.Context, sql string) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d *DB) isApplied(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}
