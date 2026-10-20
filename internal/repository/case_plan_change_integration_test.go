//go:build integration

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Real PostgreSQL: paid_tier follows invoice coverage (not the billed tier),
// a bound subscription may change tier, and a CANCELED add-on is retained as
// history while the server purchases again under a NEW add-on identity.
func TestCASETierChangeAndResubscribeLifecycle(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL required")
		}
		t.Skip("disposable test database not available")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("requires explicitly allowed integration database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	marker := time.Now().UnixNano()
	q := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	user := q(`INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,'case-tier') RETURNING id`, fmt.Sprintf("case-tier-user-%d", marker))
	org := q(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Case Tier',$1,$2) RETURNING id`, fmt.Sprintf("case-tier-org-%d", marker), user)
	guild := q(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("case-tier-guild-%d", marker))
	server := q(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
	VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`, guild, fmt.Sprintf("case-tier-server-%d", marker), org)
	server2 := q(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
	VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`, guild, fmt.Sprintf("case-tier-server2-%d", marker), org)
	connection := q(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`, org, guild)
	installation := q(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id,status)
	VALUES($1,$2,$3,'READY') RETURNING id`, org, connection, server)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO subscriptions(organization_id,plan,status) VALUES($1,'LOW','ACTIVE')`, org); err != nil {
		t.Fatal(err)
	}
	defer func() {
		bg := context.Background()
		db.Pool.Exec(bg, `DELETE FROM case_addon_webhook_events WHERE addon_id IN (SELECT id FROM case_addon_subscriptions WHERE organization_id=$1)`, org)
		db.Pool.Exec(bg, `DELETE FROM case_addon_subscriptions WHERE organization_id=$1`, org)
		db.Pool.Exec(bg, `DELETE FROM organizations WHERE id=$1`, org)
		db.Pool.Exec(bg, `DELETE FROM app_users WHERE id=$1`, user)
		db.Pool.Exec(bg, `DELETE FROM guilds WHERE id=$1`, guild)
	}()
	repo := repository.NewCaseAddonSubscriptionRepository(db.Pool)
	us := func(tt time.Time) time.Time { return tt.Truncate(time.Microsecond) }
	get := func() *repository.CaseAddonSubscription {
		t.Helper()
		row, err := repo.GetScoped(ctx, org, installation)
		if err != nil || row == nil {
			t.Fatalf("scoped read: %+v %v", row, err)
		}
		return row
	}
	res, err := repo.ReserveCaseCheckout(ctx, org, installation, server, "CASE_WATCH", "cus_tier")
	if err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("cs_tier_%d", marker)
	if err := repo.StoreCaseCheckout(ctx, res.ID, session, "https://checkout.stripe.example/tier"); err != nil {
		t.Fatal(err)
	}
	subID := fmt.Sprintf("sub_tier_%d", marker)
	end := time.Now().UTC().Add(20 * 24 * time.Hour).Truncate(time.Second)
	base := repository.CaseWebhookState{AddonID: res.ID, OrganizationID: org, InstallationID: installation, GameServerID: server,
		CustomerID: "cus_tier", SubscriptionID: subID, Status: "ACTIVE", CurrentPeriodEnd: &end}
	ev := func(id, typ, tier, price string, paid *time.Time, paidTier string) repository.CaseWebhookState {
		in := base
		in.EventID = fmt.Sprintf("evt_tier_%s_%d", id, marker)
		in.EventType = typ
		in.Tier, in.PriceID, in.PaidThrough, in.PaidTier = tier, price, paid, paidTier
		return in
	}
	checkout := ev("checkout", "checkout.session.completed", "CASE_WATCH", "price_watch", nil, "")
	checkout.CheckoutSessionID = session
	if err := repo.ApplyCaseWebhook(ctx, checkout); err != nil {
		t.Fatal(err)
	}
	paid1 := ev("paid1", "invoice.paid", "CASE_WATCH", "price_watch", &end, "CASE_WATCH")
	if applied, err := repo.ApplyCaseWebhookResult(ctx, paid1); err != nil || !applied {
		t.Fatalf("first invoice.paid delivery: applied=%v err=%v", applied, err)
	}
	if r := get(); r.PaidTier != "CASE_WATCH" || r.PaidThrough == nil || !r.PaidThrough.Equal(us(end)) {
		t.Fatalf("Watch paid: %+v", r)
	}
	// Replaying the same event id is acknowledged as a no-op (Phase 6.21 staging replay), even
	// with different content: the recorded marker wins and nothing is updated.
	beforeReplay := get()
	replay := paid1
	later := end.Add(30 * 24 * time.Hour)
	replay.PaidThrough, replay.CurrentPeriodEnd = &later, &later
	if applied, err := repo.ApplyCaseWebhookResult(ctx, replay); err != nil || applied {
		t.Fatalf("replayed event id: applied=%v err=%v (want no-op)", applied, err)
	}
	if r := get(); !r.UpdatedAt.Equal(beforeReplay.UpdatedAt) || !r.PaidThrough.Equal(*beforeReplay.PaidThrough) {
		t.Fatalf("replay changed the add-on: before %+v after %+v", beforeReplay, r)
	}
	// An unknown paid tier is rejected; the schema requires paid_tier with coverage.
	if err := repo.ApplyCaseWebhook(ctx, ev("badtier", "invoice.paid", "CASE_WATCH", "price_watch", &end, "LOW")); !errors.Is(err, repository.ErrCaseWebhookMismatch) {
		t.Fatalf("paid coverage with a non-C.A.S.E. tier: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE case_addon_subscriptions SET paid_tier=NULL WHERE id=$1`, res.ID); err == nil {
		t.Fatal("paid_through without paid_tier accepted by schema")
	}
	// Upgrade confirmed at Stripe (price swapped), proration not yet paid.
	if err := repo.SaveCaseTierChange(ctx, org, installation, subID, "price_watch", "price_pro", "CASE_PRO"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCaseTierChange(ctx, org, installation, subID, "price_watch", "price_pro", "CASE_PRO"); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	if err := repo.SaveCaseTierChange(ctx, org, installation, subID+"x", "price_watch", "price_pro", "CASE_PRO"); !errors.Is(err, repository.ErrCaseCheckoutConflict) {
		t.Fatalf("tier change on another subscription: %v", err)
	}
	if err := repo.ApplyCaseWebhook(ctx, ev("updated", "customer.subscription.updated", "CASE_PRO", "price_pro", nil, "")); err != nil {
		t.Fatalf("bound subscription tier change rejected: %v", err)
	}
	if r := get(); r.Tier != "CASE_PRO" || r.PaidTier != "CASE_WATCH" {
		t.Fatalf("unpaid upgrade: %+v", r)
	}
	// Proration invoice (same period end) upgrades paid coverage to Pro...
	if err := repo.ApplyCaseWebhook(ctx, ev("proration", "invoice.paid", "CASE_PRO", "price_pro", &end, "CASE_PRO")); err != nil {
		t.Fatal(err)
	}
	if r := get(); r.PaidTier != "CASE_PRO" {
		t.Fatalf("paid upgrade: %+v", r)
	}
	// ...and the original Watch invoice for that period, delivered late as a
	// separate event, cannot lower it.
	if err := repo.ApplyCaseWebhook(ctx, ev("latewatch", "invoice.paid", "CASE_PRO", "price_pro", &end, "CASE_WATCH")); err != nil {
		t.Fatal(err)
	}
	if r := get(); r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(us(end)) {
		t.Fatalf("late Watch invoice lowered paid tier: %+v", r)
	}
	// Downgrade: billing tier Watch, paid Pro retained; renewal moves to Watch.
	if err := repo.SaveCaseTierChange(ctx, org, installation, subID, "price_pro", "price_watch", "CASE_WATCH"); err != nil {
		t.Fatal(err)
	}
	if r := get(); r.Tier != "CASE_WATCH" || r.PaidTier != "CASE_PRO" {
		t.Fatalf("downgrade: %+v", r)
	}
	next := end.Add(30 * 24 * time.Hour)
	renew := ev("renew", "invoice.paid", "CASE_WATCH", "price_watch", &next, "CASE_WATCH")
	renew.CurrentPeriodEnd = &next
	if err := repo.ApplyCaseWebhook(ctx, renew); err != nil {
		t.Fatal(err)
	}
	if r := get(); r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(us(next)) {
		t.Fatalf("renewal: %+v", r)
	}
	// Mutual exclusivity / duplicates while an add-on is current.
	if _, err := repo.ReserveCaseCheckout(ctx, org, installation, server, "CASE_PRO", "cus_tier"); !errors.Is(err, repository.ErrCaseCheckoutConflict) {
		t.Fatalf("second add-on while current: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO case_addon_subscriptions(organization_id,installation_id,game_server_id,tier,status)
	VALUES($1,$2,$3,'CASE_PRO','PENDING')`, org, installation, server2); err == nil {
		t.Fatal("two current add-ons on one installation accepted")
	}
	// Cancellation completes at Stripe; the server may buy again.
	del := ev("deleted", "customer.subscription.deleted", "CASE_WATCH", "price_watch", nil, "")
	del.Status = "CANCELED"
	if err := repo.ApplyCaseWebhook(ctx, del); err != nil {
		t.Fatal(err)
	}
	again, err := repo.ReserveCaseCheckout(ctx, org, installation, server, "CASE_PRO", "cus_tier")
	if err != nil || again.ID == res.ID || again.Attempt != 1 {
		t.Fatalf("re-subscribe after cancel: %+v %v", again, err)
	}
	if r := get(); r.ID != again.ID || r.Status != "PENDING" {
		t.Fatalf("current row after re-subscribe: %+v", r)
	}
	listed, err := repo.ListByOrganization(ctx, org)
	if err != nil || len(listed) != 1 || listed[0].ID != again.ID {
		t.Fatalf("listing must show only the current add-on: %+v %v", listed, err)
	}
	// A late event for the OLD subscription only reaches the old history row.
	lateOld := ev("lateold", "customer.subscription.updated", "CASE_WATCH", "price_watch", nil, "")
	lateOld.Status = "CANCELED"
	if err := repo.ApplyCaseWebhook(ctx, lateOld); err != nil {
		t.Fatal(err)
	}
	old, err := repo.GetByCaseSubscriptionID(ctx, subID)
	if err != nil || old == nil || old.ID != res.ID || old.Status != "CANCELED" {
		t.Fatalf("old history row: %+v %v", old, err)
	}
	if r := get(); r.ID != again.ID || r.ProviderSubscriptionID != "" {
		t.Fatalf("late old event touched new reservation: %+v", r)
	}
	var plan, status string
	if err := db.Pool.QueryRow(ctx, `SELECT plan,status FROM subscriptions WHERE organization_id=$1`, org).Scan(&plan, &status); err != nil {
		t.Fatal(err)
	}
	if plan != "LOW" || status != "ACTIVE" {
		t.Fatalf("tier lifecycle changed base billing %s/%s", plan, status)
	}
}
