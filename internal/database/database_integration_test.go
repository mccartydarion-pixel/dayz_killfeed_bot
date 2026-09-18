//go:build integration

package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgreSQLMigrationsApplyCleanly(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var migrationCount int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if migrationCount == 0 {
		t.Fatal("expected migrations to be recorded")
	}

	var tableCount int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('guilds','game_servers','server_configs','nitrado_connections','player_links')`).Scan(&tableCount); err != nil {
		t.Fatalf("query required tables: %v", err)
	}
	if tableCount != 5 {
		t.Fatalf("expected all required runtime tables, found %d", tableCount)
	}

	// SaaS foundation (0024_saas_foundation) must apply cleanly on top of the
	// full pre-existing chain, and reuse - not duplicate - game_servers and
	// nitrado_connections (see docs/SAAS_SCHEMA.md).
	var saasTableCount int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('app_users','organizations','organization_members','discord_guild_connections','installations','installation_setup_progress','installation_settings','subscriptions')`).Scan(&saasTableCount); err != nil {
		t.Fatalf("query required SaaS tables: %v", err)
	}
	if saasTableCount != 8 {
		t.Fatalf("expected all SaaS foundation tables, found %d", saasTableCount)
	}

	var reusedColumnCount int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='public' AND column_name='organization_id' AND table_name IN ('game_servers','nitrado_connections')`).Scan(&reusedColumnCount); err != nil {
		t.Fatalf("query reused table columns: %v", err)
	}
	if reusedColumnCount != 2 {
		t.Fatalf("expected organization_id added to both reused tables, found %d", reusedColumnCount)
	}
}
