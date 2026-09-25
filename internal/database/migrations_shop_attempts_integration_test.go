//go:build integration

package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Migration compatibility for the Shop attempt ledger (0054, and 0055 where present) on a FRESH,
// isolated schema of the disposable test database: the schema is built up to 0053, real Shop rows are
// seeded as production has them, then Migrate applies the ledger migrations. Existing rows must be
// untouched, existing Shop status changes must keep working, every migration must be recorded exactly
// once, and concurrent startups must serialize instead of failing.

func isolatedDB(t *testing.T, ctx context.Context, schema string) *DB {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	admin, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		admin.Close()
	})
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	db, err := Connect(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func indexOfMigration(name string) int {
	for i, m := range migrations {
		if m.Name == name {
			return i
		}
	}
	return -1
}

func TestShopLedgerMigrationsOnExistingData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schema := fmt.Sprintf("mig_ledger_%d", time.Now().UnixNano())
	db := isolatedDB(t, ctx, schema)
	cut := indexOfMigration("0054_shop_delivery_attempts")
	if cut < 0 {
		t.Fatal("0054 is not registered")
	}
	// Build the schema exactly as production has it before the ledger (0001 .. 0053).
	if _, err := db.Pool.Exec(ctx, `CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:cut] {
		if _, err := db.Pool.Exec(ctx, m.SQL); err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, m.Name); err != nil {
			t.Fatal(err)
		}
	}
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	// Existing Shop data: an open coordinate order, a fulfilled one, a refunded one.
	user := one(`INSERT INTO app_users(discord_user_id, discord_username) VALUES('900000001','u') RETURNING id`)
	org := one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES('O','o-mig',$1) RETURNING id`, user)
	guild := one(`INSERT INTO guilds(discord_guild_id) VALUES('g-mig') RETURNING id`)
	conn := one(`INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, bot_installed, permissions_verified) VALUES($1,$2,'G',TRUE,TRUE) RETURNING id`, org, guild)
	server := one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO','123','DAYZ','PLAYSTATION','S','ACTIVE',$2) RETURNING id`, guild, org)
	inst := one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, org, conn, server)
	player := one(`INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,'p','P') RETURNING id`, guild)
	order := func(key, pstatus, dstatus string) int64 {
		p := one(`INSERT INTO shop_purchases(organization_id, installation_id, game_server_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
			VALUES($1,$2,$3,$4,$5,10,'MANUAL',$6,NOW()) RETURNING id`, org, inst, server, player, pstatus, key)
		extra := ""
		switch dstatus {
		case "FULFILLED":
			extra = ", fulfilled_at"
		case "CANCELLED":
			extra = ", cancelled_at, cancel_reason"
		}
		vals := map[string]string{"MANUAL_READY": "", "FULFILLED": ", NOW()", "CANCELLED": ", NOW(), 'REFUNDED'"}[dstatus]
		return one(`INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, map_key, coord_x, coord_z, status`+extra+`)
			VALUES($1,$2,$3,$4,$5,'MANUAL_COORDINATE','chernarusplus',100,200,$6`+vals+`) RETURNING id`, p, org, inst, server, player, dstatus)
	}
	open := order("mig-open-0001", "PENDING_FULFILLMENT", "MANUAL_READY")
	open2 := order("mig-open-0002", "PENDING_FULFILLMENT", "MANUAL_READY")
	order("mig-done-0003", "FULFILLED", "FULFILLED")
	order("mig-refd-0004", "REFUNDED", "CANCELLED")
	snapshot := func() string {
		t.Helper()
		var s string
		if err := db.Pool.QueryRow(ctx, `SELECT string_agg(format('%s:%s:%s:%s', d.id, d.status, p.status, p.total_points), ',' ORDER BY d.id)
			FROM shop_deliveries d JOIN shop_purchases p ON p.id = d.purchase_id`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()

	// Startup: Migrate applies only what is missing (0054, and 0055 where registered).
	start := time.Now()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger migrations applied on existing data in %s", time.Since(start).Round(time.Millisecond))
	if after := snapshot(); after != before {
		t.Fatalf("existing Shop rows changed:\n%s\n%s", before, after)
	}
	var names []string
	rows, err := db.Pool.Query(ctx, `SELECT name FROM schema_migrations ORDER BY applied_at, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if len(names) != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, %d migrations registered", len(names), len(migrations))
	}
	// The ledger migrations were applied after everything else, in registration order.
	for i, m := range migrations[cut:] {
		if got := names[cut+i]; got != m.Name {
			t.Fatalf("migration order: position %d is %s, want %s", cut+i, got, m.Name)
		}
	}
	// Runs exactly once: a second startup applies nothing.
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(migrations) {
		t.Fatalf("second startup: %d rows (%v)", n, err)
	}
	// Existing Shop status changes work unchanged with the new triggers and no attempts: manual
	// fulfilment of one open order, refund-cancellation of the other (what Fulfill/Refund execute).
	if _, err := db.Pool.Exec(ctx, `UPDATE shop_deliveries SET status='FULFILLED', fulfilled_at=NOW() WHERE id=$1 AND status='MANUAL_READY'`, open); err != nil {
		t.Fatalf("manual fulfilment without attempts: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE shop_deliveries SET status='CANCELLED', cancelled_at=NOW(), cancel_reason='REFUNDED' WHERE id=$1 AND status='MANUAL_READY'`, open2); err != nil {
		t.Fatalf("refund cancellation without attempts: %v", err)
	}
	// Deleting a purchase still cascades (tenant/player deletion keeps working).
	if _, err := db.Pool.Exec(ctx, `DELETE FROM shop_purchases WHERE idempotency_key='mig-refd-0004'`); err != nil {
		t.Fatalf("cascade delete: %v", err)
	}
}

func TestConcurrentStartupsMigrateOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schema := fmt.Sprintf("mig_concurrent_%d", time.Now().UnixNano())
	first := isolatedDB(t, ctx, schema)
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	dbs := []*DB{first}
	for i := 0; i < 2; i++ {
		db, err := Connect(ctx, url+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		dbs = append(dbs, db)
	}
	// Three instances start at the same moment on an empty schema.
	var wg sync.WaitGroup
	errs := make([]error, len(dbs))
	for i, db := range dbs {
		wg.Add(1)
		go func(i int, db *DB) { defer wg.Done(); errs[i] = db.Migrate(ctx) }(i, db)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("instance %d failed to start: %v", i, err)
		}
	}
	var n, distinct int
	if err := first.Pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(DISTINCT name) FROM schema_migrations`).Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) || distinct != n {
		t.Fatalf("migrations recorded %d times (%d distinct), %d registered", n, distinct, len(migrations))
	}
}
