package database

import (
	"strings"
	"testing"
)

// The first C.A.S.E. billing migration is strictly additive and can never
// modify the base organization's subscription row or previous plan pricing.
func TestCASEAddonMigrationIsAdditiveAndScoped(t *testing.T) {
	var previous, found int = -1, -1
	for i, m := range migrations {
		switch m.Name {
		case "0053_installation_embed_activation":
			previous = i
		case "0056_case_addon_subscriptions":
			found = i
		}
	}
	if previous < 0 || found <= previous {
		t.Fatalf("CASE migration must follow 0053, got previous=%d found=%d", previous, found)
	}
	// Only the Shop ledger migrations (0054/0055, when present) may sit between 0053 and C.A.S.E.
	for _, m := range migrations[previous+1 : found] {
		if m.Name != "0054_shop_delivery_attempts" && m.Name != "0055_shop_delivery_attempt_evidence" {
			t.Fatalf("unexpected migration %s between 0053 and the first C.A.S.E. migration", m.Name)
		}
	}
	sql := strings.Join(strings.Fields(migrations[found].SQL), " ")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS case_addon_subscriptions",
		"organization_id BIGINT NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT",
		"installation_id BIGINT NOT NULL",
		"game_server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE RESTRICT",
		"FOREIGN KEY (installation_id, organization_id) REFERENCES installations(id, organization_id) ON DELETE RESTRICT",
		"CONSTRAINT uq_case_addon_org_installation UNIQUE (organization_id, installation_id)",
		"CONSTRAINT uq_case_addon_org_server UNIQUE (organization_id, game_server_id)",
		"provider_subscription_id IS NOT NULL",
		"CHECK (status IN ('PENDING','TRIAL','ACTIVE','PAST_DUE','CANCELED','SUSPENDED'))",
		"CHECK (tier IN ('CASE_WATCH','CASE_PRO','CASE_COMMAND'))",
		"CREATE UNIQUE INDEX IF NOT EXISTS uq_case_addon_provider_subscription",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing scoped schema invariant %q", want)
		}
	}
	for _, forbidden := range []string{
		"ALTER TABLE subscriptions", "DROP TABLE subscriptions",
		"UPDATE subscriptions", "INSERT INTO subscriptions", "DELETE FROM subscriptions",
		"ALTER TABLE installations", "UPDATE installations", "INSERT INTO case_addon_subscriptions",
	} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("addon migration must not touch existing paid state: found %q", forbidden)
		}
	}
}
