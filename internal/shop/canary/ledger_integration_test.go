//go:build integration

package canary

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/database"
)

// The proposed ledger migration is applied INSIDE a transaction that is always rolled back: the
// disposable CI database never keeps it, and it is never registered as a migration.
func TestProposedLedgerMigration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) // never committed
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	exec := func(sql string, args ...any) error { _, err := tx.Exec(ctx, sql, args...); return err }
	// refused runs sql in a savepoint and requires it to fail.
	refused := func(name, sql string, args ...any) {
		t.Helper()
		sp, err := tx.Begin(ctx)
		must(err)
		_, err = sp.Exec(ctx, sql, args...)
		if err == nil {
			// Fire the deferred FULFILLED check now: a savepoint release would not.
			_, err = sp.Exec(ctx, `SET CONSTRAINTS trg_shop_delivery_attempt_fulfilled IMMEDIATE`)
		}
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		must(sp.Rollback(ctx))
		must(exec(`SET CONSTRAINTS trg_shop_delivery_attempt_fulfilled DEFERRED`))
	}
	must(exec(ProposedAttemptMigrationSQL))
	must(exec(`SELECT set_config('champion.actor', 'integration-test', true)`))

	// Two tenants, each with an installation, a player and open canary purchases.
	tag := fmt.Sprintf("canary%d", time.Now().UnixNano())
	type tenant struct{ org, inst, server, player, guild int64 }
	seed := func(n int) tenant {
		var tn tenant
		var user, conn int64
		must(tx.QueryRow(ctx, `INSERT INTO app_users(discord_user_id, discord_username) VALUES($1,$2) RETURNING id`, fmt.Sprintf("%s-%d", tag, n), "u").Scan(&user))
		must(tx.QueryRow(ctx, `INSERT INTO organizations(name, slug, owner_user_id) VALUES($1,$2,$3) RETURNING id`, "Org", fmt.Sprintf("%s-org%d", tag, n), user).Scan(&tn.org))
		must(tx.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("%s-g%d", tag, n)).Scan(&tn.guild))
		must(tx.QueryRow(ctx, `INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, bot_installed, permissions_verified) VALUES($1,$2,'G',TRUE,TRUE) RETURNING id`, tn.org, tn.guild).Scan(&conn))
		must(tx.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO',$2,'DAYZ','PLAYSTATION','S','ACTIVE',$3) RETURNING id`, tn.guild, fmt.Sprintf("svc-%s-%d", tag, n), tn.org).Scan(&tn.server))
		must(tx.QueryRow(ctx, `INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, tn.org, conn, tn.server).Scan(&tn.inst))
		must(tx.QueryRow(ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'Canary') RETURNING id`, tn.guild, fmt.Sprintf("%s-p%d", tag, n)).Scan(&tn.player))
		return tn
	}
	a, b := seed(1), seed(2)
	delivery := func(tn tenant, key string) int64 {
		var purchase, id int64
		must(tx.QueryRow(ctx, `INSERT INTO shop_purchases(organization_id, installation_id, game_server_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
			VALUES($1,$2,$3,$4,'PENDING_FULFILLMENT',1,'MANUAL',$5,NOW()) RETURNING id`, tn.org, tn.inst, tn.server, tn.player, key).Scan(&purchase))
		must(tx.QueryRow(ctx, `INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, map_key, coord_x, coord_z, status)
			VALUES($1,$2,$3,$4,$5,'MANUAL_COORDINATE','chernarusplus',4621.1,8397.2,'MANUAL_READY') RETURNING id`, purchase, tn.org, tn.inst, tn.server, tn.player).Scan(&id))
		return id
	}
	fp := strings.Repeat("ab", 32)
	insert := `INSERT INTO shop_delivery_attempts(organization_id, installation_id, delivery_id, attempt, attempt_id, fingerprint, artifact_path, class_name, quantity,
		pos_x, pos_y, pos_z, drop_source_file, drop_source_offset) VALUES($1,$2,$3,$4,$5,$6,'champion/champion_shop_delivery.json','BandageDressing',1,4621.1,319.6,8397.2,'x.ADM',10)`
	aid := func(d int64, n int) string { return fmt.Sprintf("champion:d%d:a%d", d, n) }
	move := func(attemptID, from, to string, tn tenant, set string) int64 {
		t.Helper()
		sql := `UPDATE shop_delivery_attempts SET state=$3` + set + ` WHERE attempt_id=$1 AND state=$2 AND organization_id=$4 AND installation_id=$5`
		tag, err := tx.Exec(ctx, sql, attemptID, from, to, tn.org, tn.inst)
		must(err)
		return tag.RowsAffected()
	}

	d1 := delivery(a, tag+"k1")
	// Tenant isolation: another tenant's scope cannot attach to this delivery.
	refused("cross-tenant attempt", insert, b.org, b.inst, d1, 1, aid(d1, 1), fp)
	// The preview identity and malformed ids are refused.
	refused("placeholder attempt id", insert, a.org, a.inst, d1, 1, "champion:d0:a1", fp)
	refused("wrong attempt number", insert, a.org, a.inst, d1, 2, aid(d1, 2), fp)
	// No actor, no change.
	must(exec(`SELECT set_config('champion.actor', '', true)`))
	refused("no actor", insert, a.org, a.inst, d1, 1, aid(d1, 1), fp)
	must(exec(`SELECT set_config('champion.actor', 'integration-test', true)`))
	must(exec(insert, a.org, a.inst, d1, 1, aid(d1, 1), fp))
	refused("second open attempt", insert, a.org, a.inst, d1, 2, aid(d1, 2), fp)
	refused("skipping states", `UPDATE shop_delivery_attempts SET state='FILE_STAGED', staged_sha256='x', staged_at=NOW(), staged_boot_file='b' WHERE attempt_id=$1`, aid(d1, 1))
	// Compare-and-set: wrong expected state or wrong tenant moves nothing.
	if n := move(aid(d1, 1), "FILE_STAGED", "AWAITING_RESTART", a, ""); n != 0 {
		t.Fatal("CAS from a wrong state moved the attempt")
	}
	if n := move(aid(d1, 1), "PLAN_CREATED", "FILE_PREPARED", b, ""); n != 0 {
		t.Fatal("another tenant moved the attempt")
	}
	if n := move(aid(d1, 1), "PLAN_CREATED", "FILE_PREPARED", a, ""); n != 1 {
		t.Fatal("PLAN_CREATED -> FILE_PREPARED")
	}
	// From FILE_PREPARED the delivery can be neither refunded (cancelled) nor fulfilled by hand.
	refused("refund while an upload may be in flight", `UPDATE shop_deliveries SET status='CANCELLED', cancelled_at=NOW(), cancel_reason='REFUNDED' WHERE id=$1`, d1)
	refused("manual fulfil while staged", `UPDATE shop_deliveries SET status='FULFILLED', fulfilled_at=NOW() WHERE id=$1`, d1)
	refused("staged without digest", `UPDATE shop_delivery_attempts SET state='FILE_STAGED' WHERE attempt_id=$1`, aid(d1, 1))
	move(aid(d1, 1), "FILE_PREPARED", "FILE_STAGED", a, ", staged_sha256='s', staged_at=NOW(), staged_boot_file='boot1.ADM', before_sha256='e'")
	move(aid(d1, 1), "FILE_STAGED", "AWAITING_RESTART", a, "")
	refused("restart without boot identity", `UPDATE shop_delivery_attempts SET state='RESTART_OBSERVED' WHERE attempt_id=$1`, aid(d1, 1))
	move(aid(d1, 1), "AWAITING_RESTART", "RESTART_OBSERVED", a, ", restart_boot_file='boot2.ADM', restart_observed_at=NOW()")
	refused("verification without unstage", `UPDATE shop_delivery_attempts SET state='VERIFICATION_REQUIRED', unstage_verified_at=NOW(), unstaged_sha256='e' WHERE attempt_id=$1`, aid(d1, 1))
	move(aid(d1, 1), "RESTART_OBSERVED", "UNSTAGE_REQUIRED", a, "")
	refused("verification without verified unstage", `UPDATE shop_delivery_attempts SET state='VERIFICATION_REQUIRED' WHERE attempt_id=$1`, aid(d1, 1))
	move(aid(d1, 1), "UNSTAGE_REQUIRED", "VERIFICATION_REQUIRED", a, ", unstage_verified_at=NOW(), unstaged_sha256='e'")
	refused("identity is immutable", `UPDATE shop_delivery_attempts SET pos_y=10 WHERE attempt_id=$1`, aid(d1, 1))
	refused("refund during verification", `UPDATE shop_deliveries SET status='CANCELLED', cancelled_at=NOW(), cancel_reason='REFUNDED' WHERE id=$1`, d1)
	refused("fulfilled without verifier", `UPDATE shop_delivery_attempts SET state='FULFILLED', fulfilled_at=NOW() WHERE attempt_id=$1`, aid(d1, 1))
	refused("attempt fulfilled without the delivery", `UPDATE shop_delivery_attempts SET state='FULFILLED', fulfilled_at=NOW(), verified_by='owner' WHERE attempt_id=$1`, aid(d1, 1))
	// Gate I: both in one transaction.
	move(aid(d1, 1), "VERIFICATION_REQUIRED", "FULFILLED", a, ", fulfilled_at=NOW(), verified_by='owner'")
	must(exec(`UPDATE shop_deliveries SET status='FULFILLED', fulfilled_at=NOW() WHERE id=$1`, d1))
	must(exec(`SET CONSTRAINTS trg_shop_delivery_attempt_fulfilled IMMEDIATE`)) // the commit-time check passes
	must(exec(`SET CONSTRAINTS trg_shop_delivery_attempt_fulfilled DEFERRED`))
	refused("terminal attempt changed", `UPDATE shop_delivery_attempts SET failure_reason='x' WHERE attempt_id=$1`, aid(d1, 1))
	var events int
	must(tx.QueryRow(ctx, `SELECT COUNT(*) FROM shop_delivery_attempt_events e JOIN shop_delivery_attempts x ON x.id=e.attempt_row_id WHERE x.attempt_id=$1 AND e.actor='integration-test'`, aid(d1, 1)).Scan(&events))
	if events != 7 {
		t.Fatalf("event log: %d transitions recorded, want 7", events)
	}

	// Crash / uncertain outcome: FAILED_REVIEW blocks any new attempt; a human may then refund.
	d2 := delivery(a, tag+"k2")
	must(exec(insert, a.org, a.inst, d2, 1, aid(d2, 1), fp))
	move(aid(d2, 1), "PLAN_CREATED", "FILE_PREPARED", a, "")
	move(aid(d2, 1), "FILE_PREPARED", "FILE_STAGED", a, ", staged_sha256='s', staged_at=NOW(), staged_boot_file='b'")
	refused("review without reason", `UPDATE shop_delivery_attempts SET state='FAILED_REVIEW' WHERE attempt_id=$1`, aid(d2, 1))
	move(aid(d2, 1), "FILE_STAGED", "FAILED_REVIEW", a, ", failure_reason='two boots before unstage'")
	refused("retry after uncertain attempt", insert, a.org, a.inst, d2, 2, aid(d2, 2), fp)
	must(exec(`UPDATE shop_deliveries SET status='CANCELLED', cancelled_at=NOW(), cancel_reason='REFUNDED' WHERE id=$1`, d2))

	// Proven-unspawned attempts allow a retry; a refunded delivery allows none.
	d3 := delivery(a, tag+"k3")
	must(exec(insert, a.org, a.inst, d3, 1, aid(d3, 1), fp))
	move(aid(d3, 1), "PLAN_CREATED", "FILE_PREPARED", a, "")
	move(aid(d3, 1), "FILE_PREPARED", "FILE_STAGED", a, ", staged_sha256='s', staged_at=NOW(), staged_boot_file='b'")
	refused("unstaged without verification", `UPDATE shop_delivery_attempts SET state='UNSTAGED' WHERE attempt_id=$1`, aid(d3, 1))
	move(aid(d3, 1), "FILE_STAGED", "UNSTAGED", a, ", unstage_verified_at=NOW(), unstaged_sha256='e'")
	must(exec(insert, a.org, a.inst, d3, 2, aid(d3, 2), fp))
	// PLAN_CREATED does not block a refund, but the refunded attempt can then never reach the server.
	must(exec(`UPDATE shop_deliveries SET status='CANCELLED', cancelled_at=NOW(), cancel_reason='REFUNDED' WHERE id=$1`, d3))
	refused("prepare after refund", `UPDATE shop_delivery_attempts SET state='FILE_PREPARED' WHERE attempt_id=$1`, aid(d3, 2))
	move(aid(d3, 2), "PLAN_CREATED", "ABANDONED", a, "")
	refused("attempt on a refunded delivery", insert, a.org, a.inst, d3, 3, aid(d3, 3), fp)

	// The transition statement the application uses, as proposed.
	d4 := delivery(b, tag+"k4")
	must(exec(insert, b.org, b.inst, d4, 1, aid(d4, 1), fp))
	var id int64
	must(tx.QueryRow(ctx, ProposedTransitionSQL, aid(d4, 1), "PLAN_CREATED", "FILE_PREPARED", b.org, b.inst).Scan(&id))
	if err := tx.QueryRow(ctx, ProposedTransitionSQL, aid(d4, 1), "PLAN_CREATED", "FILE_PREPARED", b.org, b.inst).Scan(&id); err != pgx.ErrNoRows {
		t.Fatalf("a repeated CAS must move nothing: %v", err)
	}
	rows, err := tx.Query(ctx, ProposedOpenAttemptsSQL, b.org, b.inst)
	must(err)
	open := 0
	for rows.Next() {
		open++
	}
	rows.Close()
	if open != 1 {
		t.Fatalf("open attempts for reconcile: %d", open)
	}
}
