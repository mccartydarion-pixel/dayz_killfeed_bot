//go:build integration

package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Upgrade path for 0063 on a database that already holds pre-0063 add-ons.
// Everything, including the DDL that recreates the pre-0063 shape, runs in
// one transaction that is rolled back: PostgreSQL DDL is transactional.
func TestCASEPlanChangeMigrationUpgradesExistingRows(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL required")
		}
		t.Skip("TEST_DATABASE_URL not configured")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("explicit ALLOW_INTEGRATION_DB_TESTS=true required")
	}
	var sql0063 string
	for _, m := range migrations {
		if m.Name == "0063_case_plan_changes" {
			sql0063 = m.SQL
		}
	}
	if sql0063 == "" {
		t.Fatal("0063_case_plan_changes missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := Connect(ctx, url)
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
	defer tx.Rollback(ctx)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	id := func(q string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := tx.QueryRow(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		return v
	}
	// fails runs q in a savepoint and reports whether it was rejected.
	fails := func(q string, args ...any) bool {
		t.Helper()
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, q, args...)
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			t.Fatal(rbErr)
		}
		return err != nil
	}
	s := time.Now().UnixNano()
	user := id(`INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,'case-0063') RETURNING id`, fmt.Sprintf("case-0063-u-%d", s))
	org := id(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('c0063',$1,$2) RETURNING id`, fmt.Sprintf("case-0063-o-%d", s), user)
	guild := id(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("case-0063-g-%d", s))
	server := id(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
		VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`, guild, fmt.Sprintf("case-0063-s-%d", s), org)
	conn := id(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`, org, guild)
	inst := id(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id,status)
		VALUES($1,$2,$3,'READY') RETURNING id`, org, conn, server)

	// Recreate the exact pre-0063 shape.
	exec(`ALTER TABLE case_addon_subscriptions DROP CONSTRAINT case_addon_paid_tier_required`)
	exec(`DROP INDEX uq_case_addon_current_installation`)
	exec(`DROP INDEX uq_case_addon_current_server`)
	exec(`ALTER TABLE case_addon_subscriptions ADD CONSTRAINT uq_case_addon_org_installation UNIQUE (organization_id, installation_id)`)
	exec(`ALTER TABLE case_addon_subscriptions ADD CONSTRAINT uq_case_addon_org_server UNIQUE (organization_id, game_server_id)`)
	end := time.Now().UTC().Add(10 * 24 * time.Hour)
	legacy := id(`INSERT INTO case_addon_subscriptions(organization_id,installation_id,game_server_id,tier,status,provider,
		provider_customer_id,provider_subscription_id,provider_price_id,current_period_end,paid_through,checkout_session_id)
		VALUES($1,$2,$3,'CASE_PRO','ACTIVE','stripe','cus_0063',$4,'price_pro',$5,$5,$6) RETURNING id`,
		org, inst, server, fmt.Sprintf("sub_0063_%d", s), end, fmt.Sprintf("cs_0063_%d", s))

	// Apply 0063 twice: it must be idempotent on an upgraded database.
	exec(sql0063)
	exec(sql0063)

	var paidTier string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(paid_tier,'') FROM case_addon_subscriptions WHERE id=$1`, legacy).Scan(&paidTier); err != nil {
		t.Fatal(err)
	}
	if paidTier != "CASE_PRO" {
		t.Fatalf("existing paid row not backfilled: %q", paidTier)
	}
	var oldConstraints int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint WHERE conname IN ('uq_case_addon_org_installation','uq_case_addon_org_server')`).Scan(&oldConstraints); err != nil {
		t.Fatal(err)
	}
	if oldConstraints != 0 {
		t.Fatalf("pre-0063 unique constraints survived: %d", oldConstraints)
	}
	if !fails(`UPDATE case_addon_subscriptions SET paid_tier=NULL WHERE id=$1`, legacy) {
		t.Fatal("paid_through without paid_tier accepted after upgrade")
	}
	if !fails(`UPDATE case_addon_subscriptions SET paid_tier='LOW' WHERE id=$1`, legacy) {
		t.Fatal("non-C.A.S.E. paid_tier accepted")
	}
	newRow := `INSERT INTO case_addon_subscriptions(organization_id,installation_id,game_server_id,tier,status)
		VALUES($1,$2,$3,'CASE_WATCH','PENDING')`
	if !fails(newRow, org, inst, server) {
		t.Fatal("second current add-on accepted while the legacy row is ACTIVE")
	}
	// After cancellation the legacy row is history and the server may buy again.
	exec(`UPDATE case_addon_subscriptions SET status='CANCELED' WHERE id=$1`, legacy)
	exec(newRow, org, inst, server)
	if !fails(newRow, org, inst, server) {
		t.Fatal("two current add-ons accepted after re-subscription")
	}
	var history int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM case_addon_subscriptions WHERE organization_id=$1 AND status='CANCELED'`, org).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 1 {
		t.Fatalf("canceled history not retained: %d", history)
	}
}
