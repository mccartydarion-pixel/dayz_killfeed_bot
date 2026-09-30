//go:build integration

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Phase 6.24 end-to-end payment-failure protection on real PostgreSQL: the real billing.Service
// and repositories, Stripe-signed webhooks in the 2026-08-26.dahlia shapes staging receives, and
// FakeProvider standing in for Stripe (no network, no charge). Scenarios, in order:
//  1. declined Watch->Pro upgrade          5. no unpaid Pro unlock at any step
//  2. pending update, no invoice payment   6. duplicate failed-payment webhook is a no-op
//  3. invoice.payment_failed reconciled    7. the LOW base subscription never changes
//  4. paid Watch access preserved          8. later successful payments recover (upgrade, then renewal)
func TestCASEPaymentFailureCannotUnlockOrDowngradePaidCoverage(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	m := time.Now().UnixNano()
	q := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	user := q(`INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,'case-fail') RETURNING id`, fmt.Sprintf("case-fail-u-%d", m))
	org := q(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Case Fail',$1,$2) RETURNING id`, fmt.Sprintf("case-fail-o-%d", m), user)
	guild := q(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("case-fail-g-%d", m))
	server := q(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
		VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`, guild, fmt.Sprintf("case-fail-s-%d", m), org)
	conn := q(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`, org, guild)
	inst := q(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id,status)
		VALUES($1,$2,$3,'CONFIGURING') RETURNING id`, org, conn, server)
	customer, baseSub := fmt.Sprintf("cus_fail_%d", m), fmt.Sprintf("sub_base_%d", m)
	baseEnd := time.Now().UTC().Add(25 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO subscriptions(organization_id,plan,status,provider,provider_customer_id,
		provider_subscription_id,provider_price_id,billing_interval,current_period_end)
		VALUES($1,'LOW','ACTIVE','stripe',$2,$3,'price_base_low','MONTHLY',$4)`, org, customer, baseSub, baseEnd); err != nil {
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

	const secret = "whsec_case_payment_failure"
	catalog, err := billing.LoadCatalog(`[{"key":"LOW","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base_low"},"isPublic":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	provider := billing.NewFakeProvider()
	baseRepo := repository.NewSubscriptionRepository(db.Pool)
	caseRepo := repository.NewCaseAddonSubscriptionRepository(db.Pool)
	svc := billing.NewService(baseRepo, catalog, provider, billing.Options{WebhookSecret: secret})
	if err := svc.ConfigureCaseAddons(caseRepo, billing.CaseOptions{
		Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro,
		PriceIDs: map[casebilling.Tier]string{casebilling.Watch: "price_watch", casebilling.Pro: "price_pro"},
	}); err != nil {
		t.Fatal(err)
	}
	access := func() []casebilling.Capability {
		t.Helper()
		caps, err := svc.CaseAccess(ctx, org, inst, server, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return caps
	}
	row := func() *repository.CaseAddonSubscription {
		t.Helper()
		r, err := caseRepo.GetScoped(ctx, org, inst)
		if err != nil || r == nil {
			t.Fatalf("add-on row: %+v %v", r, err)
		}
		return r
	}
	baseBefore, err := baseRepo.GetForOrganization(ctx, org)
	if err != nil || baseBefore == nil {
		t.Fatal(err)
	}
	assertBaseUnchanged := func(step string) {
		t.Helper()
		b, err := baseRepo.GetForOrganization(ctx, org)
		if err != nil || b.Plan != "LOW" || b.Status != repository.SubscriptionActive || b.ProviderSubscriptionID != baseSub ||
			b.ProviderPriceID != "price_base_low" || b.ProviderCustomerID != customer || !b.CurrentPeriodEnd.Equal(*baseBefore.CurrentPeriodEnd) {
			t.Fatalf("%s: LOW base subscription changed: %+v (%v)", step, b, err)
		}
	}
	has := func(caps []casebilling.Capability, c casebilling.Capability) bool {
		for _, x := range caps {
			if x == c {
				return true
			}
		}
		return false
	}
	deliver := func(payload string) {
		t.Helper()
		ts := time.Now()
		sig := fmt.Sprintf("t=%d,v1=%x", ts.Unix(), stripewebhook.ComputeSignature(ts, []byte(payload), secret))
		if err := svc.HandleWebhook(ctx, []byte(payload), sig); err != nil {
			t.Fatalf("webhook must be acknowledged: %v", err)
		}
	}

	// Setup: a genuinely purchased, paid Watch add-on on the same customer as LOW.
	res, err := caseRepo.ReserveCaseCheckout(ctx, org, inst, server, "CASE_WATCH", customer)
	if err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("cs_fail_%d", m)
	if err := caseRepo.StoreCaseCheckout(ctx, res.ID, session, "https://checkout.stripe.example/fail"); err != nil {
		t.Fatal(err)
	}
	addonSub := fmt.Sprintf("sub_case_fail_%d", m)
	meta := billing.CaseMetadata(billing.CaseCheckoutInput{AddonID: res.ID, OrganizationID: org, InstallationID: inst, GameServerID: server, Tier: casebilling.Watch})
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	end := time.Now().UTC().Add(20 * 24 * time.Hour).Truncate(time.Second)
	end2 := end.Add(30 * 24 * time.Hour)
	putSub := func(price, status string, pending bool, s, e time.Time) {
		provider.Put(billing.SubscriptionState{SubscriptionID: addonSub, CustomerID: customer, PriceID: price, StripeStatus: status,
			StripeInterval: "month", CurrentPeriodStart: s, CurrentPeriodEnd: e, Metadata: meta, PendingUpdate: pending})
	}
	putSub("price_watch", "active", false, start, end)
	metaJSON := fmt.Sprintf(`{"champion_product_kind":"CASE_ADDON","champion_case_addon_id":"%d","champion_organization_id":"%d","champion_installation_id":"%d","champion_game_server_id":"%d","champion_case_tier":"CASE_WATCH"}`,
		res.ID, org, inst, server)
	line := func(amount int64, price string, s, e time.Time) string {
		return fmt.Sprintf(`{"amount":%d,"parent":{"type":"subscription_item_details","subscription_item_details":{"subscription":"%s","proration":%v}},"pricing":{"price_details":{"price":"%s"}},"period":{"start":%d,"end":%d}}`,
			amount, addonSub, amount != 999 && amount != 499, price, s.Unix(), e.Unix())
	}
	invoice := func(evID, typ, invID, status string, lines ...string) string {
		return fmt.Sprintf(`{"id":"%s_%d","type":"%s","api_version":"2026-08-26.dahlia","data":{"object":{"id":"%s_%d","object":"invoice","status":"%s","customer":"%s","subscription":null,
			"parent":{"type":"subscription_details","subscription_details":{"subscription":"%s","metadata":%s}},"lines":{"data":[%s]}}}}`,
			evID, m, typ, invID, m, status, customer, addonSub, metaJSON, strings.Join(lines, ","))
	}
	deliver(fmt.Sprintf(`{"id":"evt_fail_checkout_%d","type":"checkout.session.completed","data":{"object":{"id":"%s","mode":"subscription","customer":"%s","subscription":"%s","metadata":%s}}}`,
		m, session, customer, addonSub, metaJSON))
	deliver(invoice("evt_fail_watch_paid", "invoice.paid", "in_watch", "paid", line(499, "price_watch", start, end)))
	if r := row(); r.Status != "ACTIVE" || r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end) {
		t.Fatalf("setup: paid Watch not active: %+v", r)
	}
	if caps := access(); !has(caps, casebilling.CapWatch) || has(caps, casebilling.CapPro) {
		t.Fatalf("setup: want Watch only, got %v", caps)
	}

	// 1. Declined upgrade: Stripe holds the change (pending_if_incomplete); nothing local moves.
	provider.SetCaseUpgradeProration(494, true)
	out, err := svc.CaseChangeTier(ctx, org, inst, "CASE_PRO", time.Now().Unix())
	if err != nil || !out.Pending {
		t.Fatalf("1: declined upgrade must be PAYMENT_PENDING: %+v %v", out, err)
	}
	if r := row(); r.Tier != "CASE_WATCH" || r.ProviderPriceID != "price_watch" || r.PaidTier != "CASE_WATCH" {
		t.Fatalf("1: declined upgrade changed the add-on: %+v", r)
	}
	if st, _ := provider.GetSubscription(ctx, addonSub); st.PriceID != "price_watch" || !st.PendingUpdate {
		t.Fatalf("1: Stripe must keep Watch with a pending update: %+v", st)
	}
	// 2. The pending update arrives as customer.subscription.updated with no invoice payment.
	deliver(fmt.Sprintf(`{"id":"evt_fail_pending_%d","type":"customer.subscription.updated","data":{"object":{"id":"%s","status":"active","customer":"%s","metadata":%s,
		"items":{"data":[{"current_period_start":%d,"current_period_end":%d,"price":{"id":"price_watch","recurring":{"interval":"month"}}}]}}}}`,
		m, addonSub, customer, metaJSON, start.Unix(), end.Unix()))
	if r := row(); r.Status != "ACTIVE" || r.Tier != "CASE_WATCH" || r.PaidTier != "CASE_WATCH" {
		t.Fatalf("2: pending update changed the add-on: %+v", r)
	}
	if _, err := svc.CasePreviewTierChange(ctx, org, inst, "CASE_PRO"); !errors.Is(err, billing.ErrCaseChangePending) {
		t.Fatalf("2: a second upgrade while one is pending: %v", err)
	}
	// 3/4/5. The declined proration invoice (Watch credit + Pro charge, same paid period) fails:
	// paid Watch access stays, no Pro unlock, no false paid tier.
	now := time.Now().UTC().Truncate(time.Second)
	failed := invoice("evt_fail_upgrade_failed", "invoice.payment_failed", "in_upgrade", "open",
		line(-494, "price_watch", now, end), line(988, "price_pro", now, end))
	deliver(failed)
	afterFail := row()
	if afterFail.Status != "ACTIVE" || afterFail.PaidTier != "CASE_WATCH" || !afterFail.PaidThrough.Equal(end) {
		t.Fatalf("3/4: declined upgrade invoice revoked or altered paid Watch: %+v", afterFail)
	}
	if caps := access(); !has(caps, casebilling.CapWatch) || has(caps, casebilling.CapPro) {
		t.Fatalf("4/5: want paid Watch kept and no Pro, got %v", caps)
	}
	// 6. The same failed-payment event redelivered is a no-op.
	deliver(failed)
	if r := row(); !r.UpdatedAt.Equal(afterFail.UpdatedAt) || r.Status != "ACTIVE" {
		t.Fatalf("6: duplicate failed-payment webhook changed the add-on: before %+v after %+v", afterFail, r)
	}
	var addons int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM case_addon_subscriptions WHERE organization_id=$1`, org).Scan(&addons); err != nil || addons != 1 {
		t.Fatalf("no duplicate add-on expected, got %d (%v)", addons, err)
	}
	if _, err := svc.CaseCheckout(ctx, httptest.NewRequest("POST", "/", nil), org, billing.CaseCheckoutRequest{InstallationID: inst, Tier: casebilling.Watch},
		repository.NewInstallationRepository(db.Pool)); !errors.Is(err, repository.ErrCaseCheckoutConflict) {
		t.Fatalf("a second add-on checkout for the same server must be refused: %v", err)
	}
	assertBaseUnchanged("after declined upgrade")

	// 8a. The customer fixes the card and the proration invoice is paid: Stripe applies the
	// pending change, and only then does Pro unlock.
	putSub("price_pro", "active", false, start, end)
	deliver(invoice("evt_fail_upgrade_paid", "invoice.paid", "in_upgrade", "paid",
		line(-494, "price_watch", now, end), line(988, "price_pro", now, end)))
	if r := row(); r.Tier != "CASE_PRO" || r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(end) {
		t.Fatalf("8a: paid upgrade not recovered: %+v", r)
	}
	if caps := access(); !has(caps, casebilling.CapPro) {
		t.Fatalf("8a: want Pro after payment, got %v", caps)
	}
	// A failed renewal for the NEXT period ends paid access (no grace policy) but keeps records.
	deliver(invoice("evt_fail_renewal_failed", "invoice.payment_failed", "in_renewal", "open", line(999, "price_pro", end, end2)))
	if r := row(); r.Status != "PAST_DUE" || r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(end) {
		t.Fatalf("renewal failure: %+v", r)
	}
	if caps := access(); len(caps) != 0 {
		t.Fatalf("PAST_DUE must grant nothing, got %v", caps)
	}
	// 8b. The renewal is paid later: access returns with the new paid period.
	putSub("price_pro", "active", false, end, end2)
	deliver(invoice("evt_fail_renewal_paid", "invoice.paid", "in_renewal", "paid", line(999, "price_pro", end, end2)))
	if r := row(); r.Status != "ACTIVE" || r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(end2) {
		t.Fatalf("8b: renewal not recovered: %+v", r)
	}
	if caps := access(); !has(caps, casebilling.CapPro) {
		t.Fatalf("8b: want Pro after renewal payment, got %v", caps)
	}
	// 7. LOW never changed through any of it.
	assertBaseUnchanged("end")
}
