//go:build integration

package database

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Migration 0124 on a database that already holds servers: a server whose current name is the name
// of an audited rename becomes custom; one renamed and later reconnected (its name is no longer the
// audited one), one never renamed and one whose rename failed all keep following Nitrado. No name
// is changed, and a second startup applies nothing.
func TestServerNameSyncMigrationOnExistingServers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	db := isolatedDB(t, ctx, fmt.Sprintf("mig_srvname_%d", time.Now().UnixNano()))
	cut := indexOfMigration("0124_server_name_sync")
	if cut < 0 {
		t.Fatal("0124 is not registered")
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
	user := one(`INSERT INTO app_users(discord_user_id, discord_username) VALUES('900000124','u') RETURNING id`)
	org := one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES('O','o-mig124',$1) RETURNING id`, user)
	guild := one(`INSERT INTO guilds(discord_guild_id) VALUES('g-mig124') RETURNING id`)
	conn := one(`INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, bot_installed, permissions_verified) VALUES($1,$2,'G',TRUE,TRUE) RETURNING id`, org, guild)
	server := func(service, name string) (int64, int64) {
		s := one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO',$2,'DAYZ','PLAYSTATION',$3,'ACTIVE',$4) RETURNING id`, guild, service, name, org)
		return s, one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, org, conn, s)
	}
	audit := func(inst int64, name, result string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO admin_audit_log(organization_id, installation_id, actor_user_id, action, after_state, result) VALUES($1,$2,$3,'SERVER_NAME_EDIT',jsonb_build_object('name',$4::text),$5)`,
			org, inst, user, name, result); err != nil {
			t.Fatal(err)
		}
	}
	renamed, renamedInst := server("1", "Owner's name")
	audit(renamedInst, "First try", "success")
	audit(renamedInst, "Owner's name", "success")
	reconnected, reconnectedInst := server("2", "Nitrado name")
	audit(reconnectedInst, "Old custom", "success")
	never, _ := server("3", "Plain")
	failed, failedInst := server("4", "Failed rename")
	audit(failedInst, "Failed rename", "error")

	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(id int64, wantName string, wantCustom bool) {
		t.Helper()
		var name string
		var custom bool
		var provider *string
		if err := db.Pool.QueryRow(ctx, `SELECT display_name, display_name_custom, provider_display_name FROM game_servers WHERE id=$1`, id).Scan(&name, &custom, &provider); err != nil {
			t.Fatal(err)
		}
		if name != wantName || custom != wantCustom || provider != nil {
			t.Fatalf("server %d: name %q custom %v provider %v, want %q %v <nil>", id, name, custom, provider, wantName, wantCustom)
		}
	}
	check(renamed, "Owner's name", true)
	check(reconnected, "Nitrado name", false)
	check(never, "Plain", false)
	check(failed, "Failed rename", false)

	// A new server defaults to not custom.
	fresh := one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status) VALUES($1,'NITRADO','5','DAYZ','PLAYSTATION','New','ACTIVE') RETURNING id`, guild)
	check(fresh, "New", false)

	// A second startup applies nothing and keeps a later change.
	if _, err := db.Pool.Exec(ctx, `UPDATE game_servers SET display_name_custom=FALSE WHERE id=$1`, renamed); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	check(renamed, "Owner's name", false)
	if n := one(`SELECT COUNT(*) FROM schema_migrations WHERE name='0124_server_name_sync'`); n != 1 {
		t.Fatalf("0124 recorded %d times", n)
	}
}
