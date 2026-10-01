//go:build integration

package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Migration 0068 on a database that already holds historical ledger rows: every existing attempt and
// its history stay byte-identical, the CHECK is replaced by the named, wider one, the custom/ path is
// then accepted, anything else is still refused, and a second startup applies nothing.
func TestShopAttemptArtifactPathMigrationOnHistoricalData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db := isolatedDB(t, ctx, fmt.Sprintf("mig_artifact_%d", time.Now().UnixNano()))
	cut := indexOfMigration("0068_shop_delivery_attempt_artifact_path")
	if cut < 0 {
		t.Fatal("0068 is not registered")
	}
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
	user := one(`INSERT INTO app_users(discord_user_id, discord_username) VALUES('900000068','u') RETURNING id`)
	org := one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES('O','o-mig68',$1) RETURNING id`, user)
	guild := one(`INSERT INTO guilds(discord_guild_id) VALUES('g-mig68') RETURNING id`)
	conn := one(`INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, bot_installed, permissions_verified) VALUES($1,$2,'G',TRUE,TRUE) RETURNING id`, org, guild)
	server := one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO','123','DAYZ','PLAYSTATION','S','ACTIVE',$2) RETURNING id`, guild, org)
	inst := one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, org, conn, server)
	player := one(`INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,'p','P') RETURNING id`, guild)
	delivery := func(key string) int64 {
		p := one(`INSERT INTO shop_purchases(organization_id, installation_id, game_server_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
			VALUES($1,$2,$3,$4,'PENDING_FULFILLMENT',1,'MANUAL',$5,NOW()) RETURNING id`, org, inst, server, player, key)
		return one(`INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, map_key, coord_x, coord_z, status)
			VALUES($1,$2,$3,$4,$5,'MANUAL_COORDINATE','chernarusplus',4621.1,8397.2,'MANUAL_READY') RETURNING id`, p, org, inst, server, player)
	}
	// Like the repository: every ledger change carries an actor and evidence note in its transaction
	// (0054 refuses a change without champion.actor).
	attempt := func(d int64, path string) error {
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('champion.actor', 'migration-test', true), set_config('champion.evidence', 'plan created', true)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO shop_delivery_attempts(organization_id, installation_id, delivery_id, attempt, attempt_id, fingerprint, artifact_path,
 class_name, quantity, pos_x, pos_y, pos_z, drop_source_file, drop_source_offset)
VALUES($1,$2,$3,1,$4,$5,$6,'BandageDressing',1,4621.1,319.6,8397.2,'dayzps/config/DayZServer_PS4_x64_2026-09-29_08-23-54.ADM',853)`,
			org, inst, d, fmt.Sprintf("champion:d%d:a1", d), strings.Repeat("ab", 32), path); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Historical data under 0054's CHECK: a legacy attempt (and the history row its trigger writes).
	if err := attempt(delivery("mig68-legacy"), "champion/champion_shop_delivery.json"); err != nil {
		t.Fatal(err)
	}
	// Before 0068 the custom/ path is refused.
	if err := attempt(delivery("mig68-early"), "custom/champion_shop_delivery.json"); err == nil {
		t.Fatal("0054 must refuse custom/ before 0068")
	}
	snapshot := func() string {
		t.Helper()
		var a, e string
		// Migration 0100 adds three columns with defaults; they are left out so the comparison is of
		// the values the historical rows already had (their defaults are asserted below).
		if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(string_agg((to_jsonb(x) - 'fulfilment_mode' - 'buyer_answer' - 'buyer_answered_at')::text, '|' ORDER BY x.id), '') FROM shop_delivery_attempts x`).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(string_agg(row_to_json(x)::text, '|' ORDER BY x.id), '') FROM shop_delivery_attempt_events x`).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return a + " || " + e
	}
	before := snapshot()
	if !strings.Contains(before, "champion/champion_shop_delivery.json") {
		t.Fatalf("fixture: %s", before)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("historical ledger rows changed:\n%s\n%s", before, after)
	}
	var modes string
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(string_agg(DISTINCT fulfilment_mode || ':' || (buyer_answer IS NULL)::text, ','), '') FROM shop_delivery_attempts`).Scan(&modes); err != nil {
		t.Fatal(err)
	}
	if modes != "OBSERVED:true" {
		t.Fatalf("historical attempts after 0100: %s, want the manual mode and no buyer answer", modes)
	}
	var n int
	var def string
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*), MIN(pg_get_constraintdef(oid)) FROM pg_constraint
 WHERE conrelid = 'shop_delivery_attempts'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%artifact_path%'`).Scan(&n, &def); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !strings.Contains(def, "champion/champion_shop_delivery.json") || !strings.Contains(def, "custom/champion_shop_delivery.json") {
		t.Fatalf("constraint: %d %s", n, def)
	}
	// After 0068: custom/ accepted, anything else refused.
	if err := attempt(delivery("mig68-custom"), "custom/champion_shop_delivery.json"); err != nil {
		t.Fatalf("custom/ after 0068: %v", err)
	}
	if err := attempt(delivery("mig68-bad"), "custom/other.json"); err == nil {
		t.Fatal("an unknown path must stay refused")
	}
	// Idempotent: running the migration SQL again (as a manual re-apply would) keeps one named CHECK,
	// and a second startup applies nothing.
	if _, err := db.Pool.Exec(ctx, ShopAttemptArtifactPathSQL); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	var m int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint WHERE conrelid = 'shop_delivery_attempts'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%artifact_path%'`).Scan(&m); err != nil || m != 1 {
		t.Fatalf("after re-apply: %d %v", m, err)
	}
	before2 := len(migrations)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != before2 {
		t.Fatalf("second startup: %d rows, %d registered (%v)", rows, before2, err)
	}
}
