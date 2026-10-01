//go:build integration

package repository_test

import (
	"context"
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

// Phase 6.26B on real PostgreSQL: refunds, disputes and voids revoke exactly the coverage they
// reverse, from live state, idempotently and in any order, with an append-only audit history;
// legacy (pre-0064) coverage is reconstructed first; base billing is never touched.
func TestCASERefundDisputeAndVoidCoverageLifecycle(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	user := q(`INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,'case-rev') RETURNING id`, fmt.Sprintf("case-rev-u-%d", m))
	org := q(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Case Rev',$1,$2) RETURNING id`, fmt.Sprintf("case-rev-o-%d", m), user)
	fixture := func(tag string) (int64, int64, int64) {
		g := q(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("case-rev-g-%s-%d", tag, m))
		srv := q(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
			VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`, g, fmt.Sprintf("case-rev-s-%s-%d", tag, m), org)
		c := q(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`, org, g)
		i := q(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id,status) VALUES($1,$2,$3,'CONFIGURING') RETURNING id`, org, c, srv)
		return g, srv, i
	}
	guild1, server, inst := fixture("a")
	guild2, server2, inst2 := fixture("b")
	customer, baseSub := fmt.Sprintf("cus_rev_%d", m), fmt.Sprintf("sub_base_rev_%d", m)
	baseEnd := time.Now().UTC().Add(25 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO subscriptions(organization_id,plan,status,provider,provider_customer_id,
		provider_subscription_id,provider_price_id,billing_interval,current_period_end)
		VALUES($1,'LOW','ACTIVE','stripe',$2,$3,'price_base_low','MONTHLY',$4)`, org, customer, baseSub, baseEnd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		bg := context.Background()
		for _, s := range []string{
			`DELETE FROM case_addon_coverage_events WHERE addon_id IN (SELECT id FROM case_addon_subscriptions WHERE organization_id=$1)`,
			`DELETE FROM case_addon_invoice_coverage WHERE addon_id IN (SELECT id FROM case_addon_subscriptions WHERE organization_id=$1)`,
			`DELETE FROM case_addon_webhook_events WHERE addon_id IN (SELECT id FROM case_addon_subscriptions WHERE organization_id=$1)`,
			`DELETE FROM case_addon_subscriptions WHERE organization_id=$1`,
			`DELETE FROM organizations WHERE id=$1`,
		} {
			db.Pool.Exec(bg, s, org)
		}
		db.Pool.Exec(bg, `DELETE FROM billing_webhook_events WHERE event_id LIKE $1`, fmt.Sprintf("%%_%d", m))
		db.Pool.Exec(bg, `DELETE FROM app_users WHERE id=$1`, user)
		db.Pool.Exec(bg, `DELETE FROM guilds WHERE id=ANY($1)`, []int64{guild1, guild2})
	}()

	const secret = "whsec_case_reversal"
	catalog, _ := billing.LoadCatalog(`[{"key":"LOW","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base_low"},"isPublic":true}]`)
	provider := billing.NewFakeProvider()
	baseRepo := repository.NewSubscriptionRepository(db.Pool)
	caseRepo := repository.NewCaseAddonSubscriptionRepository(db.Pool)
	svc := billing.NewService(baseRepo, catalog, provider, billing.Options{WebhookSecret: secret})
	if err := svc.ConfigureCaseAddons(caseRepo, billing.CaseOptions{Enabled: true, AccessEnabled: true, VerifiedThrough: casebilling.Pro,
		PriceIDs: map[casebilling.Tier]string{casebilling.Watch: "price_watch", casebilling.Pro: "price_pro"}}); err != nil {
		t.Fatal(err)
	}
	deliver := func(payload string) {
		t.Helper()
		ts := time.Now()
		sig := fmt.Sprintf("t=%d,v1=%x", ts.Unix(), stripewebhook.ComputeSignature(ts, []byte(payload), secret))
		if err := svc.HandleWebhook(ctx, []byte(payload), sig); err != nil {
			t.Fatalf("webhook must be acknowledged: %v", err)
		}
	}
	access := func(i, s int64) string {
		t.Helper()
		c, err := svc.CaseAccess(ctx, org, i, s, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(c)
	}
	row := func(i int64) *repository.CaseAddonSubscription {
		t.Helper()
		r, err := caseRepo.GetScoped(ctx, org, i)
		if err != nil || r == nil {
			t.Fatalf("add-on: %+v %v", r, err)
		}
		return r
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ledgerStatus := func(inv string) string {
		t.Helper()
		var s string
		if err := db.Pool.QueryRow(ctx, `SELECT status FROM case_addon_invoice_coverage WHERE provider_invoice_id=$1`, inv).Scan(&s); err != nil {
			t.Fatalf("ledger %s: %v", inv, err)
		}
		return s
	}
	baseBefore, _ := baseRepo.GetForOrganization(ctx, org)
	assertBase := func(step string) {
		t.Helper()
		b, err := baseRepo.GetForOrganization(ctx, org)
		if err != nil || b.Plan != "LOW" || b.Status != repository.SubscriptionActive || b.ProviderSubscriptionID != baseSub || !b.CurrentPeriodEnd.Equal(*baseBefore.CurrentPeriodEnd) {
			t.Fatalf("%s: base changed: %+v %v", step, b, err)
		}
	}
	W, P := "[case.watch]", "[case.watch case.pro]"
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	end := time.Now().UTC().Add(20 * 24 * time.Hour).Truncate(time.Second)
	end2 := end.Add(30 * 24 * time.Hour)
	id := func(s string) string { return fmt.Sprintf("%s_%d", s, m) }

	// --- installation 1: a genuinely purchased Watch add-on on the LOW customer -----------------
	res, err := caseRepo.ReserveCaseCheckout(ctx, org, inst, server, "CASE_WATCH", customer)
	if err != nil {
		t.Fatal(err)
	}
	session := id("cs_rev")
	if err := caseRepo.StoreCaseCheckout(ctx, res.ID, session, "https://checkout.stripe.example/rev"); err != nil {
		t.Fatal(err)
	}
	sub := id("sub_case_rev")
	meta := billing.CaseMetadata(billing.CaseCheckoutInput{AddonID: res.ID, OrganizationID: org, InstallationID: inst, GameServerID: server, Tier: casebilling.Watch})
	metaJSON := fmt.Sprintf(`{"champion_product_kind":"CASE_ADDON","champion_case_addon_id":"%d","champion_organization_id":"%d","champion_installation_id":"%d","champion_game_server_id":"%d","champion_case_tier":"CASE_WATCH"}`,
		res.ID, org, inst, server)
	putSub := func(price string, s, e time.Time) {
		provider.Put(billing.SubscriptionState{SubscriptionID: sub, CustomerID: customer, PriceID: price, StripeStatus: "active",
			StripeInterval: "month", CurrentPeriodStart: s, CurrentPeriodEnd: e, Metadata: meta})
	}
	type ln struct {
		amount int64
		price  string
		s, e   time.Time
	}
	caseLines := func(subID string, lines []ln) []billing.CaseInvoiceLine {
		out := []billing.CaseInvoiceLine{}
		for _, l := range lines {
			out = append(out, billing.CaseInvoiceLine{Amount: l.amount, PriceID: l.price, SubscriptionID: subID, SubscriptionItem: true, PeriodStart: l.s.Unix(), PeriodEnd: l.e.Unix()})
		}
		return out
	}
	// payInvoice registers the invoice at "Stripe" (reachable via its PaymentIntent) and delivers
	// the signed invoice.paid event exactly as staging receives it.
	deliveries := map[string]int{}
	payInvoice := func(inv, pi string, amountPaid int64, lines ...ln) {
		t.Helper()
		deliveries[inv]++
		provider.PutCaseInvoice(billing.CaseInvoice{ID: id(inv), CustomerID: customer, SubscriptionID: sub, Status: "paid", Currency: "usd",
			AmountPaid: amountPaid, Lines: caseLines(sub, lines)}, id(pi))
		provider.PutCasePaymentState(id(pi), billing.CasePaymentState{AmountCaptured: amountPaid})
		js := []string{}
		for _, l := range lines {
			js = append(js, fmt.Sprintf(`{"amount":%d,"parent":{"type":"subscription_item_details","subscription_item_details":{"subscription":"%s"}},"pricing":{"price_details":{"price":"%s"}},"period":{"start":%d,"end":%d}}`,
				l.amount, sub, l.price, l.s.Unix(), l.e.Unix()))
		}
		deliver(fmt.Sprintf(`{"id":"%s","type":"invoice.paid","api_version":"2026-08-26.dahlia","data":{"object":{"id":"%s","object":"invoice","status":"paid","customer":"%s",
			"amount_paid":%d,"currency":"usd","subscription":null,"parent":{"type":"subscription_details","subscription_details":{"subscription":"%s","metadata":%s}},"lines":{"data":[%s]}}}}`,
			id(fmt.Sprintf("evt_paid_%s_%d", inv, deliveries[inv])), id(inv), customer, amountPaid, sub, metaJSON, strings.Join(js, ",")))
	}
	reversal := func(evt, typ, object, pi string, st billing.CasePaymentState) {
		t.Helper()
		provider.PutCasePaymentState(id(pi), st)
		deliver(fmt.Sprintf(`{"id":"%s","type":"%s","api_version":"2026-08-26.dahlia","data":{"object":{"id":"%s","object":"%s","payment_intent":"%s"}}}`,
			id(evt), typ, id("obj_"+evt), object, id(pi)))
	}
	putSub("price_watch", start, end)
	deliver(fmt.Sprintf(`{"id":"%s","type":"checkout.session.completed","data":{"object":{"id":"%s","mode":"subscription","customer":"%s","subscription":"%s","metadata":%s}}}`,
		id("evt_rev_checkout"), session, customer, sub, metaJSON))
	payInvoice("in_w1", "pi_w1", 499, ln{499, "price_watch", start, end})
	if r := row(inst); r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end) || r.CoverageState != "OK" || ledgerStatus(id("in_w1")) != "PAID" {
		t.Fatalf("setup: %+v", r)
	}
	if a := access(inst, server); a != W {
		t.Fatalf("setup access %s", a)
	}

	// 1. Full refund revokes exactly that invoice's coverage; history records before/after.
	reversal("evt_refund_w1", "charge.refunded", "charge", "pi_w1", billing.CasePaymentState{AmountCaptured: 499, AmountRefunded: 499})
	if r := row(inst); r.PaidThrough != nil || r.PaidTier != "" || r.CoverageState != "REFUNDED" || ledgerStatus(id("in_w1")) != "REFUNDED" {
		t.Fatalf("1: full refund: %+v", r)
	}
	if a := access(inst, server); a != "[]" {
		t.Fatalf("1: refunded coverage still grants %s", a)
	}
	var hPrev, hNew string
	var hBefore, hAfter *time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT previous_status,new_status,paid_through_before,paid_through_after FROM case_addon_coverage_events WHERE stripe_event_id=$1`,
		id("evt_refund_w1")).Scan(&hPrev, &hNew, &hBefore, &hAfter); err != nil || hPrev != "PAID" || hNew != "REFUNDED" || hBefore == nil || !hBefore.Equal(end) || hAfter != nil {
		t.Fatalf("1: history: %s -> %s, %v -> %v (%v)", hPrev, hNew, hBefore, hAfter, err)
	}
	// 2. Duplicate delivery: no second history row, no change.
	historyN := count(`SELECT COUNT(*) FROM case_addon_coverage_events WHERE addon_id=$1`, res.ID)
	deliver(fmt.Sprintf(`{"id":"%s","type":"charge.refunded","data":{"object":{"id":"x","object":"charge","payment_intent":"%s"}}}`, id("evt_refund_w1"), id("pi_w1")))
	if count(`SELECT COUNT(*) FROM case_addon_coverage_events WHERE addon_id=$1`, res.ID) != historyN {
		t.Fatal("2: duplicate refund event wrote history")
	}
	// 3. The refund is reversed at Stripe (charge.refund.updated): live state restores coverage.
	reversal("evt_refund_w1_reversed", "charge.refund.updated", "refund", "pi_w1", billing.CasePaymentState{AmountCaptured: 499})
	if r := row(inst); r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end) || r.CoverageState != "OK" || access(inst, server) != W {
		t.Fatalf("3: reversed refund: %+v", r)
	}
	// 4. A Pro upgrade proration, then refund of ONLY that proration: falls back to paid Watch.
	putSub("price_pro", start, end)
	now := time.Now().UTC().Truncate(time.Second)
	payInvoice("in_up", "pi_up", 494, ln{-494, "price_watch", now, end}, ln{988, "price_pro", now, end})
	if r := row(inst); r.PaidTier != "CASE_PRO" || access(inst, server) != P {
		t.Fatalf("4: paid upgrade: %+v", r)
	}
	reversal("evt_refund_up", "charge.refunded", "charge", "pi_up", billing.CasePaymentState{AmountCaptured: 494, AmountRefunded: 494})
	if r := row(inst); r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end) || r.CoverageState != "REFUNDED" || access(inst, server) != W {
		t.Fatalf("4: refunded upgrade must fall back to the paid Watch invoice: %+v", r)
	}
	// 5. Dispute on the Watch invoice suspends it (no coverage left); the out-of-order pair
	// closed(won) then a stale created both read live "won" and restore it.
	reversal("evt_dispute_w1", "charge.dispute.created", "dispute", "pi_w1", billing.CasePaymentState{AmountCaptured: 499, DisputeStatuses: []string{"needs_response"}})
	if ledgerStatus(id("in_w1")) != "DISPUTED" || access(inst, server) != "[]" {
		t.Fatalf("5: open dispute must suspend: %s %s", ledgerStatus(id("in_w1")), access(inst, server))
	}
	reversal("evt_dispute_w1_won", "charge.dispute.closed", "dispute", "pi_w1", billing.CasePaymentState{AmountCaptured: 499, DisputeStatuses: []string{"won"}})
	reversal("evt_dispute_w1_stale", "charge.dispute.updated", "dispute", "pi_w1", billing.CasePaymentState{AmountCaptured: 499, DisputeStatuses: []string{"won"}})
	if ledgerStatus(id("in_w1")) != "DISPUTE_WON" || access(inst, server) != W {
		t.Fatalf("5: won dispute must restore: %s %s", ledgerStatus(id("in_w1")), access(inst, server))
	}
	// 6. A renewal paid as Pro, then a LOST dispute on it: revoked, falling back to the earlier
	// Watch coverage that is still in its paid period.
	putSub("price_pro", end, end2)
	payInvoice("in_r", "pi_r", 999, ln{999, "price_pro", end, end2})
	if r := row(inst); r.PaidTier != "CASE_PRO" || !r.PaidThrough.Equal(end2) {
		t.Fatalf("6: renewal: %+v", r)
	}
	reversal("evt_dispute_r_lost", "charge.dispute.closed", "dispute", "pi_r", billing.CasePaymentState{AmountCaptured: 999, DisputeStatuses: []string{"lost"}})
	if r := row(inst); ledgerStatus(id("in_r")) != "DISPUTE_LOST" || r.PaidTier != "CASE_WATCH" || !r.PaidThrough.Equal(end) || r.CoverageState != "DISPUTE_LOST" || access(inst, server) != W {
		t.Fatalf("6: lost dispute: %+v %s", r, access(inst, server))
	}
	// A late, distinct invoice.paid event for the lost invoice cannot undo the reversal.
	payInvoice("in_r", "pi_r", 999, ln{999, "price_pro", end, end2})
	if ledgerStatus(id("in_r")) != "DISPUTE_LOST" || access(inst, server) != W {
		t.Fatal("6: invoice.paid replay revived a lost dispute")
	}
	// 7. A partial refund on the Watch invoice is kept (flagged in the ledger).
	reversal("evt_partial_w1", "charge.refunded", "charge", "pi_w1", billing.CasePaymentState{AmountCaptured: 499, AmountRefunded: 100, DisputeStatuses: []string{"won"}})
	if ledgerStatus(id("in_w1")) != "PARTIALLY_REFUNDED" || access(inst, server) != W {
		t.Fatalf("7: partial refund: %s %s", ledgerStatus(id("in_w1")), access(inst, server))
	}
	// 8. An unpaid invoice voided: history only, no coverage row, access unchanged.
	provider.PutCaseInvoice(billing.CaseInvoice{ID: id("in_void"), CustomerID: customer, SubscriptionID: sub, Status: "void",
		Lines: caseLines(sub, []ln{{988, "price_pro", now, end}})}, "")
	deliver(fmt.Sprintf(`{"id":"%s","type":"invoice.voided","data":{"object":{"id":"%s","object":"invoice","status":"void","customer":"%s","parent":{"type":"subscription_details","subscription_details":{"subscription":"%s"}}}}}`,
		id("evt_void"), id("in_void"), customer, sub))
	if count(`SELECT COUNT(*) FROM case_addon_invoice_coverage WHERE provider_invoice_id=$1`, id("in_void")) != 0 ||
		count(`SELECT COUNT(*) FROM case_addon_coverage_events WHERE stripe_event_id=$1 AND new_status='VOIDED' AND previous_status IS NULL`, id("evt_void")) != 1 ||
		access(inst, server) != W {
		t.Fatal("8: voided unpaid invoice must be history-only")
	}
	// 9. A refund of the customer's BASE invoice (same customer) never touches C.A.S.E.
	provider.PutCaseInvoice(billing.CaseInvoice{ID: id("in_base"), CustomerID: customer, SubscriptionID: baseSub, Status: "paid", AmountPaid: 599,
		Lines: caseLines(baseSub, []ln{{599, "price_base_low", start, baseEnd}})}, id("pi_base"))
	historyN = count(`SELECT COUNT(*) FROM case_addon_coverage_events WHERE addon_id=$1`, res.ID)
	reversal("evt_base_refund", "charge.refunded", "charge", "pi_base", billing.CasePaymentState{AmountCaptured: 599, AmountRefunded: 599})
	if count(`SELECT COUNT(*) FROM case_addon_coverage_events WHERE addon_id=$1`, res.ID) != historyN ||
		count(`SELECT COUNT(*) FROM billing_webhook_events WHERE event_id=$1`, id("evt_base_refund")) != 1 {
		t.Fatal("9: a base refund must be handled by base (acknowledged, ignored), never by C.A.S.E.")
	}

	// --- installation 2: an add-on paid BEFORE 0064 (legacy, not backfilled) -------------------
	res2, err := caseRepo.ReserveCaseCheckout(ctx, org, inst2, server2, "CASE_PRO", customer)
	if err != nil {
		t.Fatal(err)
	}
	if err := caseRepo.StoreCaseCheckout(ctx, res2.ID, id("cs_legacy"), "https://checkout.stripe.example/legacy"); err != nil {
		t.Fatal(err)
	}
	sub2 := id("sub_case_legacy")
	legacy := repository.CaseWebhookState{EventID: id("evt_legacy_paid"), EventType: "invoice.paid", AddonID: res2.ID, OrganizationID: org, InstallationID: inst2,
		GameServerID: server2, Tier: "CASE_PRO", CustomerID: customer, SubscriptionID: sub2, PriceID: "price_pro", Status: "ACTIVE",
		CheckoutSessionID: id("cs_legacy"), CurrentPeriodEnd: &end, PaidThrough: &end, PaidTier: "CASE_PRO"}
	if err := caseRepo.ApplyCaseWebhook(ctx, legacy); err != nil { // pre-0064 path: no ledger row
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE case_addon_subscriptions SET coverage_backfilled=FALSE WHERE id=$1`, res2.ID); err != nil {
		t.Fatal(err)
	}
	// At "Stripe": its original Watch invoice and a paid Pro upgrade proration, same period end.
	provider.PutCaseInvoice(billing.CaseInvoice{ID: id("in_l1"), CustomerID: customer, SubscriptionID: sub2, Status: "paid", AmountPaid: 499,
		Lines: caseLines(sub2, []ln{{499, "price_watch", start, end}})}, id("pi_l1"))
	provider.PutCaseInvoice(billing.CaseInvoice{ID: id("in_l2"), CustomerID: customer, SubscriptionID: sub2, Status: "paid", AmountPaid: 494,
		Lines: caseLines(sub2, []ln{{-494, "price_watch", now, end}, {988, "price_pro", now, end}})}, id("pi_l2"))
	if a := access(inst2, server2); a != P {
		t.Fatalf("legacy setup access %s", a)
	}
	// Refunding the legacy WATCH invoice must reconstruct the ledger first, so the separately paid
	// Pro proration keeps its coverage (a naive recompute would drop Pro).
	reversal("evt_refund_l1", "charge.refunded", "charge", "pi_l1", billing.CasePaymentState{AmountCaptured: 499, AmountRefunded: 499})
	r2 := row(inst2)
	if !r2.CoverageBackfilled || ledgerStatus(id("in_l1")) != "REFUNDED" || ledgerStatus(id("in_l2")) != "PAID" || r2.PaidTier != "CASE_PRO" || access(inst2, server2) != P {
		t.Fatalf("legacy reconstruction: %+v access %s", r2, access(inst2, server2))
	}
	// Installation 1 was never affected by installation 2's events.
	if access(inst, server) != W {
		t.Fatal("cross-installation leakage")
	}
	assertBase("end")
}
