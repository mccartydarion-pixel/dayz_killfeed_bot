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

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Phase 6.25 end-to-end downgrade / cancellation lifecycle on real PostgreSQL with the real
// billing.Service, Stripe-signed webhooks and FakeProvider (no network, no charge):
//  1. Pro -> Watch downgrade is scheduled with no charge   5. cancellation reversal before expiry
//  2. Pro access is kept until the paid period ends         6. final deletion removes access, keeps the record
//  3. Watch takes over only when the renewal is paid        7. duplicate/out-of-order/late events never revive
//  4. cancellation scheduled at period end keeps access     8. the LOW base subscription never changes
func TestCASEDowngradeAndCancellationLifecycle(t *testing.T) {
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
	user := q(`INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,'case-life') RETURNING id`, fmt.Sprintf("case-life-u-%d", m))
	org := q(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Case Life',$1,$2) RETURNING id`, fmt.Sprintf("case-life-o-%d", m), user)
	guild := q(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("case-life-g-%d", m))
	server := q(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
		VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`, guild, fmt.Sprintf("case-life-s-%d", m), org)
	conn := q(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`, org, guild)
	inst := q(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id,status)
		VALUES($1,$2,$3,'CONFIGURING') RETURNING id`, org, conn, server)
	customer, baseSub := fmt.Sprintf("cus_life_%d", m), fmt.Sprintf("sub_base_life_%d", m)
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

	const secret = "whsec_case_lifecycle"
	catalog, err := billing.LoadCatalog(`[{"key":"LOW","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base_low"},"isPublic":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	provider := billing.NewFakeProvider()
	baseRepo := repository.NewSubscriptionRepository(db.Pool)
	caseRepo := repository.NewCaseAddonSubscriptionRepository(db.Pool)
	svc := billing.NewService(baseRepo, catalog, provider, billing.Options{WebhookSecret: secret})
	if err := svc.ConfigureCaseAddons(caseRepo, billing.CaseOptions{Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro,
		PriceIDs: map[casebilling.Tier]string{casebilling.Watch: "price_watch", casebilling.Pro: "price_pro"}}); err != nil {
		t.Fatal(err)
	}
	caps := func() []casebilling.Capability {
		t.Helper()
		c, err := svc.CaseAccess(ctx, org, inst, server, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	wantCaps := func(step string, want ...casebilling.Capability) {
		t.Helper()
		got := caps()
		if fmt.Sprint(got) != fmt.Sprint(append([]casebilling.Capability{}, want...)) && !(len(got) == 0 && len(want) == 0) {
			t.Fatalf("%s: access %v, want %v", step, got, want)
		}
	}
	row := func() *repository.CaseAddonSubscription {
		t.Helper()
		r, err := caseRepo.GetScoped(ctx, org, inst)
		if err != nil || r == nil {
			t.Fatalf("add-on row: %+v %v", r, err)
		}
		return r
	}
	baseBefore, _ := baseRepo.GetForOrganization(ctx, org)
	assertBase := func(step string) {
		t.Helper()
		b, err := baseRepo.GetForOrganization(ctx, org)
		if err != nil || b.Plan != "LOW" || b.Status != repository.SubscriptionActive || b.ProviderSubscriptionID != baseSub ||
			b.ProviderPriceID != "price_base_low" || !b.CurrentPeriodEnd.Equal(*baseBefore.CurrentPeriodEnd) {
			t.Fatalf("%s: LOW base changed: %+v (%v)", step, b, err)
		}
	}
	deliver := func(payload string) {
		t.Helper()
		ts := time.Now()
		sig := fmt.Sprintf("t=%d,v1=%x", ts.Unix(), stripewebhook.ComputeSignature(ts, []byte(payload), secret))
		if err := svc.HandleWebhook(ctx, []byte(payload), sig); err != nil {
			t.Fatalf("webhook must be acknowledged: %v", err)
		}
	}

	// Setup: a paid Pro add-on (purchased as Pro) on the same customer as LOW.
	res, err := caseRepo.ReserveCaseCheckout(ctx, org, inst, server, "CASE_PRO", customer)
	if err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("cs_life_%d", m)
	if err := caseRepo.StoreCaseCheckout(ctx, res.ID, session, "https://checkout.stripe.example/life"); err != nil {
		t.Fatal(err)
	}
	sub := fmt.Sprintf("sub_case_life_%d", m)
	meta := billing.CaseMetadata(billing.CaseCheckoutInput{AddonID: res.ID, OrganizationID: org, InstallationID: inst, GameServerID: server, Tier: casebilling.Pro})
	metaJSON := fmt.Sprintf(`{"champion_product_kind":"CASE_ADDON","champion_case_addon_id":"%d","champion_organization_id":"%d","champion_installation_id":"%d","champion_game_server_id":"%d","champion_case_tier":"CASE_PRO"}`,
		res.ID, org, inst, server)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	end := time.Now().UTC().Add(20 * 24 * time.Hour).Truncate(time.Second)
	end2 := end.Add(30 * 24 * time.Hour)
	putSub := func(price, status string, cancelAt bool, s, e time.Time) {
		provider.Put(billing.SubscriptionState{SubscriptionID: sub, CustomerID: customer, PriceID: price, StripeStatus: status,
			StripeInterval: "month", CurrentPeriodStart: s, CurrentPeriodEnd: e, Metadata: meta, CancelAtPeriodEnd: cancelAt})
	}
	subEvent := func(id, typ, price, status string, cancelAt bool, s, e time.Time) string {
		return fmt.Sprintf(`{"id":"%s_%d","type":"%s","data":{"object":{"id":"%s","status":"%s","cancel_at_period_end":%v,"customer":"%s","metadata":%s,
			"items":{"data":[{"current_period_start":%d,"current_period_end":%d,"price":{"id":"%s","recurring":{"interval":"month"}}}]}}}}`,
			id, m, typ, sub, status, cancelAt, customer, metaJSON, s.Unix(), e.Unix(), price)
	}
	invoicePaid := func(id, invID string, amount int64, price string, s, e time.Time) string {
		return fmt.Sprintf(`{"id":"%s_%d","type":"invoice.paid","api_version":"2026-08-26.dahlia","data":{"object":{"id":"%s_%d","object":"invoice","status":"paid","customer":"%s","subscription":null,
			"parent":{"type":"subscription_details","subscription_details":{"subscription":"%s","metadata":%s}},
			"lines":{"data":[{"amount":%d,"parent":{"type":"subscription_item_details","subscription_item_details":{"subscription":"%s"}},"pricing":{"price_details":{"price":"%s"}},"period":{"start":%d,"end":%d}}]}}}}`,
			id, m, invID, m, customer, sub, metaJSON, amount, sub, price, s.Unix(), e.Unix())
	}
	putSub("price_pro", "active", false, start, end)
	deliver(fmt.Sprintf(`{"id":"evt_life_checkout_%d","type":"checkout.session.completed","data":{"object":{"id":"%s","mode":"subscription","customer":"%s","subscription":"%s","metadata":%s}}}`,
		m, session, customer, sub, metaJSON))
	deliver(invoicePaid("evt_life_pro_paid", "in_life_pro", 999, "price_pro", start, end))
	if r := row(); r.Status != "ACTIVE" || r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(end) {
		t.Fatalf("setup: %+v", r)
	}
	wantCaps("setup", casebilling.CapWatch, casebilling.CapPro)

	// 1. Downgrade is scheduled: preview shows no charge and takes effect at the paid-through end.
	p, err := svc.CasePreviewTierChange(ctx, org, inst, "CASE_WATCH")
	if err != nil || p.Kind != billing.CaseTierDowngrade || p.AmountDueNowCents != 0 || p.EffectiveAt == nil || !p.EffectiveAt.Equal(end) || p.NextRenewalAmountCents != 499 {
		t.Fatalf("1: downgrade preview: %+v %v", p, err)
	}
	if out, err := svc.CaseChangeTier(ctx, org, inst, "CASE_WATCH", p.ProrationDate); err != nil || out.Pending || out.Kind != billing.CaseTierDowngrade {
		t.Fatalf("1: downgrade: %+v %v", out, err)
	}
	for _, c := range provider.Calls {
		if in, ok := c.Arg.(billing.CaseTierChangeInput); ok && in.Charge {
			t.Fatal("1: downgrade must not invoice a proration")
		}
	}
	deliver(subEvent("evt_life_downgraded", "customer.subscription.updated", "price_watch", "active", false, start, end))
	// 2. Pro kept until the paid period ends; billing tier is Watch.
	if r := row(); r.Tier != "CASE_WATCH" || r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(end) {
		t.Fatalf("2: after downgrade: %+v", r)
	}
	wantCaps("2: Pro retained until paid_through", casebilling.CapWatch, casebilling.CapPro)
	// 7a. A stale subscription.updated (new id, older Pro content) cannot undo the downgrade:
	// reconciliation reloads Stripe's current state.
	deliver(subEvent("evt_life_stale_pro", "customer.subscription.updated", "price_pro", "active", false, start, end))
	if r := row(); r.Tier != "CASE_WATCH" || r.ProviderPriceID != "price_watch" {
		t.Fatalf("7a: stale event reverted the downgrade: %+v", r)
	}
	// Restoring Pro inside the paid period is free, then downgrade again.
	p, _ = svc.CasePreviewTierChange(ctx, org, inst, "CASE_PRO")
	if p == nil || p.Kind != billing.CaseTierRestore || p.AmountDueNowCents != 0 {
		t.Fatalf("restore preview: %+v", p)
	}
	if _, err := svc.CaseChangeTier(ctx, org, inst, "CASE_PRO", p.ProrationDate); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.CasePreviewTierChange(ctx, org, inst, "CASE_WATCH")
	if _, err := svc.CaseChangeTier(ctx, org, inst, "CASE_WATCH", p.ProrationDate); err != nil {
		t.Fatal(err)
	}
	// 3. The renewal bills Watch; only its paid invoice moves access to Watch.
	putSub("price_watch", "active", false, end, end2)
	deliver(invoicePaid("evt_life_watch_renewal", "in_life_renewal", 499, "price_watch", end, end2))
	if r := row(); r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end2) || r.Tier != "CASE_WATCH" {
		t.Fatalf("3: renewal: %+v", r)
	}
	wantCaps("3: Watch after the downgraded renewal", casebilling.CapWatch)
	// 7b. The previous period's Pro invoice delivered late cannot restore Pro.
	deliver(invoicePaid("evt_life_late_pro", "in_life_pro", 999, "price_pro", start, end))
	if r := row(); r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end2) {
		t.Fatalf("7b: late Pro invoice: %+v", r)
	}
	wantCaps("7b", casebilling.CapWatch)
	// 4. Cancellation at period end keeps paid access; tier changes are refused meanwhile.
	if r, err := svc.CaseSetCancellation(ctx, org, inst, true); err != nil || !r.CancelAtPeriodEnd {
		t.Fatalf("4: cancel: %+v %v", r, err)
	}
	deliver(subEvent("evt_life_cancel_sched", "customer.subscription.updated", "price_watch", "active", true, end, end2))
	if r := row(); !r.CancelAtPeriodEnd || r.Status != "ACTIVE" {
		t.Fatalf("4: %+v", r)
	}
	wantCaps("4: access kept while cancellation is scheduled", casebilling.CapWatch)
	if _, err := svc.CasePreviewTierChange(ctx, org, inst, "CASE_PRO"); !errors.Is(err, billing.ErrCaseCancelScheduled) {
		t.Fatalf("4: tier change while cancelling: %v", err)
	}
	// 5. Reversal before expiry, then cancel again for the real end.
	if r, err := svc.CaseSetCancellation(ctx, org, inst, false); err != nil || r.CancelAtPeriodEnd {
		t.Fatalf("5: reactivate: %+v %v", r, err)
	}
	if st, _ := provider.GetSubscription(ctx, sub); st.CancelAtPeriodEnd {
		t.Fatal("5: Stripe still scheduled to cancel")
	}
	if _, err := svc.CaseSetCancellation(ctx, org, inst, true); err != nil {
		t.Fatal(err)
	}
	// 6. Stripe ends the subscription at the period end: access goes, the record stays.
	putSub("price_watch", "canceled", true, end, end2)
	deleted := subEvent("evt_life_deleted", "customer.subscription.deleted", "price_watch", "canceled", true, end, end2)
	deliver(deleted)
	afterDelete := row()
	if afterDelete.Status != "CANCELED" || !afterDelete.PaidThrough.Equal(end2) || afterDelete.PaidTier != "CASE_WATCH" {
		t.Fatalf("6: deletion: %+v", afterDelete)
	}
	wantCaps("6: no access after deletion")
	// 7c. Duplicate deletion, late subscription.updated claiming active, and a late paid invoice
	// cannot revive a canceled add-on.
	deliver(deleted)
	deliver(subEvent("evt_life_late_active", "customer.subscription.updated", "price_watch", "active", false, end, end2))
	deliver(invoicePaid("evt_life_late_paid_again", "in_life_renewal", 499, "price_watch", end, end2))
	if r := row(); r.Status != "CANCELED" || r.ID != afterDelete.ID {
		t.Fatalf("7c: canceled add-on revived: %+v", r)
	}
	wantCaps("7c")
	if _, err := svc.CaseSetCancellation(ctx, org, inst, false); !errors.Is(err, billing.ErrCaseNotManaged) {
		t.Fatalf("a canceled add-on cannot be reactivated: %v", err)
	}
	// 8. LOW never changed.
	assertBase("end")
}
