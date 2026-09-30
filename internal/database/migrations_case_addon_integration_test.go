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

// Uses only CI's explicitly allowed disposable DB. All fixture rows are
// rolled back; this never changes a live Stripe account or a real subscription.
func TestCASEAddonSchemaIsolatedFromBaseBilling(t *testing.T) {
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
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	suffix := time.Now().UnixNano()
	var userID, orgID, otherOrgID, guildID, serverID, connectionID, installationID int64
	if err := tx.QueryRow(ctx, `INSERT INTO app_users(discord_user_id,discord_username)
	VALUES($1,'case-schema-test') RETURNING id`, fmt.Sprintf("case-user-%d", suffix)).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		name string
		id *int64
	}{{"owner", &orgID}, {"other", &otherOrgID}} {
		if err := tx.QueryRow(ctx, `INSERT INTO organizations(name,slug,owner_user_id)
		VALUES($1,$2,$3) RETURNING id`, pair.name, fmt.Sprintf("case-%s-%d", pair.name, suffix), userID).Scan(pair.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`,
		fmt.Sprintf("case-guild-%d", suffix)).Scan(&guildID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
	VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		guildID, fmt.Sprintf("case-service-%d", suffix), orgID).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO discord_guild_connections(organization_id,guild_id)
	VALUES($1,$2) RETURNING id`, orgID, guildID).Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id)
	VALUES($1,$2,$3) RETURNING id`, orgID, connectionID, serverID).Scan(&installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO subscriptions(organization_id,plan,status)
	VALUES($1,'LOW','ACTIVE')`, orgID); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO case_addon_subscriptions(organization_id,installation_id,game_server_id,tier,status)
	VALUES($1,$2,$3,'CASE_WATCH','PENDING')`
	if _, err := tx.Exec(ctx, insert, orgID, installationID, serverID); err != nil {
		t.Fatal(err)
	}
	// Invalid writes must be attempted behind a savepoint because PostgreSQL
	// marks the enclosing transaction aborted after a constraint violation.
	mustReject := func(label, query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, "SAVEPOINT case_reject"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, query, args...); err == nil {
			t.Fatalf("%s unexpectedly accepted", label)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT case_reject"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT case_reject"); err != nil {
			t.Fatal(err)
		}
	}
	mustReject("cross-org installation", insert, otherOrgID, installationID, serverID)
	mustReject("duplicate purchase", insert, orgID, installationID, serverID)
	mustReject("ACTIVE without Stripe proof", `UPDATE case_addon_subscriptions SET status='ACTIVE'
	WHERE organization_id=$1 AND installation_id=$2`, orgID, installationID)
	end := time.Now().Add(time.Hour)
	if _, err := tx.Exec(ctx, `UPDATE case_addon_subscriptions
	SET status='ACTIVE', provider='stripe', provider_subscription_id='sub_schema_test',
	provider_price_id='price_schema_test', current_period_end=$3
	WHERE organization_id=$1 AND installation_id=$2`, orgID, installationID, end); err != nil {
		t.Fatal(err)
	}
	var plan, status string
	if err := tx.QueryRow(ctx, `SELECT plan,status FROM subscriptions WHERE organization_id=$1`, orgID).Scan(&plan, &status); err != nil {
		t.Fatal(err)
	}
	if plan != "LOW" || status != "ACTIVE" {
		t.Fatalf("add-on changed base billing: plan=%s status=%s", plan, status)
	}
}
