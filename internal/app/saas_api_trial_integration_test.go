//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Customer Onboarding V2 over real PostgreSQL (docs/BILLING.md "No-card trial"): one
// no-card 14-day trial per account, trial start/state endpoints, the backend installation limit,
// billing-required after expiry, and that none of it touches Stripe or overwrites a paid plan.
// Every Stripe call goes through billing.FakeProvider.

func trialPath(orgID int64, suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/trial%s", orgID, suffix)
}

func stripeCalls(p *billing.FakeProvider) int { return len(p.Calls) }

// connectExtraGuild connects another Discord guild to orgID through the real handler.
func connectExtraGuild(t *testing.T, a *App, verifier *fakeDiscordVerifier, orgID int64, ownerDiscordID string) int64 {
	t.Helper()
	guildID := fmt.Sprintf("guild-extra-%d", time.Now().UnixNano())
	verifier.guildFound[guildID] = true
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", connectGuildRequest{
		DiscordGuildID: guildID, GuildName: "Extra Guild", Permissions: strconv.FormatInt(int64(requiredGuildPermissions), 10),
	}), ownerDiscordID), map[string]string{"organizationID": strconv.FormatInt(orgID, 10)})
	rr := httptest.NewRecorder()
	a.handleConnectDiscordGuild(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("connect extra guild: %d %s", rr.Code, rr.Body.String())
	}
	return decodeBody[DiscordGuildConnectionSummary](t, rr).ID
}

func postCreateInstallation(a *App, orgID, connID int64, actor string) *httptest.ResponseRecorder {
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", createInstallationRequest{DiscordGuildConnectionID: connID}), actor),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10)})
	rr := httptest.NewRecorder()
	a.handleCreateInstallation(rr, req)
	return rr
}

func errorCodeOf(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	return decodeBody[apiErrorEnvelope](t, rr).Error.Code
}

func installationCount(t *testing.T, a *App, orgID int64) int {
	t.Helper()
	n, err := a.SaaSInstallations.CountByOrganization(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTrialStartIsNoCardIdempotentAndRecordsThePlan(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	before, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID)
	if before == nil || before.Status != repository.SubscriptionTrial || before.TrialEndsAt == nil {
		t.Fatalf("a new owner's organization starts its trial at creation: %+v", before)
	}
	calls := stripeCalls(w.billingProvider)

	st := w.expect(w.do(http.MethodPost, trialPath(w.a1.OrgID, "/start"), w.a1.OwnerDiscordID, map[string]any{"planKey": "starter"}), http.StatusOK, "start").JSON(t)
	if st["trialStatus"] != "ACTIVE" || st["selectedPlan"] != "STARTER" || st["billingRequired"] != false || st["paymentMethodRequired"] != false ||
		st["daysRemaining"].(float64) != 14 || st["started"] != false || st["subscriptionStatus"] != "TRIAL" || st["trialEndsAt"] == nil || st["trialStartedAt"] == nil ||
		st["installationLimit"].(float64) != 1 || st["installationCount"].(float64) != 1 {
		t.Fatalf("%v", st)
	}
	// Changing the selection (as ADMIN) keeps the same clock.
	st2 := w.expect(w.do(http.MethodPost, trialPath(w.a1.OrgID, "/start"), w.admin, map[string]any{"planKey": "PRO"}), http.StatusOK, "reselect").JSON(t)
	after, _ := w.a.SaaSSubscriptions.GetForOrganization(ctx, w.a1.OrgID)
	if st2["selectedPlan"] != "PRO" || !after.TrialEndsAt.Equal(*before.TrialEndsAt) || after.Plan != repository.SubscriptionTrial || after.IntendedPlan != "PRO" {
		t.Fatalf("state %v row %+v", st2, after)
	}
	if stripeCalls(w.billingProvider) != calls || after.ProviderCustomerID != "" {
		t.Fatal("starting a trial or picking a plan must never reach Stripe")
	}
	// GET is read-only and member-visible.
	got := w.getJSON(trialPath(w.a1.OrgID, ""), w.member)
	if got["trialStatus"] != "ACTIVE" || got["selectedPlan"] != "PRO" {
		t.Fatalf("%v", got)
	}
	// MEMBER cannot start or change it; unknown and private plans are refused.
	w.expect(w.do(http.MethodPost, trialPath(w.a1.OrgID, "/start"), w.member, map[string]any{"planKey": "PRO"}), http.StatusForbidden, "member")
	w.expect(w.do(http.MethodPost, trialPath(w.a1.OrgID, "/start"), w.admin, map[string]any{"planKey": "LEGACY"}), http.StatusBadRequest, "private plan")
	w.expect(w.do(http.MethodPost, trialPath(w.a1.OrgID, "/start"), w.admin, map[string]any{"planKey": "NOPE"}), http.StatusBadRequest, "unknown plan")
	w.expect(w.do(http.MethodGet, trialPath(w.a1.OrgID, ""), w.players[0], nil), http.StatusForbidden, "non-member")
}

func TestSecondOrganizationGetsNoSecondTrial(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("trial-owner-%d", suffix), "Owner")
	first := mustCreateOrg(t, a, owner.DiscordUserID, "First", fmt.Sprintf("trial-first-%d", suffix))
	second := mustCreateOrg(t, a, owner.DiscordUserID, "Second", fmt.Sprintf("trial-second-%d", suffix))

	s1, _ := a.SaaSSubscriptions.GetForOrganization(ctx, first.ID)
	s2, _ := a.SaaSSubscriptions.GetForOrganization(ctx, second.ID)
	if s1 == nil || s1.Status != repository.SubscriptionTrial {
		t.Fatalf("first organization: %+v", s1)
	}
	if s2 == nil || s2.Status != repository.SubscriptionInactive || s2.TrialEndsAt != nil || s2.Plan != repository.PlanNone {
		t.Fatalf("a second organization must not get a trial: %+v", s2)
	}
	// Asking explicitly changes nothing.
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", startTrialRequest{PlanKey: ""}), owner.DiscordUserID), map[string]string{"organizationID": strconv.FormatInt(second.ID, 10)})
	rr := httptest.NewRecorder()
	a.handleStartTrial(rr, req)
	st := decodeBody[TrialStateDTO](t, rr)
	if rr.Code != http.StatusOK || st.Started || st.TrialStatus != billing.TrialNotEligible || !st.BillingRequired || st.TrialEndsAt != nil {
		t.Fatalf("%d %+v", rr.Code, st)
	}
	// And the second organization cannot set up a service without paying.
	conn := connectExtraGuild(t, a, verifier, second.ID, owner.DiscordUserID)
	res := postCreateInstallation(a, second.ID, conn, owner.DiscordUserID)
	if res.Code != http.StatusPaymentRequired || errorCodeOf(t, res) != codeBillingRequired {
		t.Fatalf("expected 402 BILLING_REQUIRED, got %d %s", res.Code, res.Body.String())
	}
	if installationCount(t, a, second.ID) != 0 {
		t.Fatal("no installation may be created")
	}
}

func TestTrialAllowsExactlyOneInstallation(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	f := buildInstallationFixture(t, a, verifier) // the trial's one installation
	orig, _ := a.SaaSInstallations.GetScoped(context.Background(), f.OrgID, f.InstallationID)

	// A second guild: a new service is over the trial limit.
	conn := connectExtraGuild(t, a, verifier, f.OrgID, f.OwnerDiscordID)
	res := postCreateInstallation(a, f.OrgID, conn, f.OwnerDiscordID)
	if res.Code != http.StatusConflict || errorCodeOf(t, res) != codeInstallationLimitReached {
		t.Fatalf("expected 409 INSTALLATION_LIMIT_REACHED, got %d %s", res.Code, res.Body.String())
	}
	if msg := decodeBody[apiErrorEnvelope](t, res).Error.Message; msg == "" {
		t.Fatal("the limit error must carry an actionable message")
	}
	// Retrying the initial setup for the same guild reads as "already exists", not as a paywall.
	dup := postCreateInstallation(a, f.OrgID, f.ConnectionID, f.OwnerDiscordID)
	if dup.Code != http.StatusConflict || errorCodeOf(t, dup) != codeConflict {
		t.Fatalf("expected 409 CONFLICT for the same guild, got %d %s", dup.Code, dup.Body.String())
	}
	after, _ := a.SaaSInstallations.GetScoped(context.Background(), f.OrgID, f.InstallationID)
	if installationCount(t, a, f.OrgID) != 1 || after == nil || after.Status != orig.Status || !after.UpdatedAt.Equal(orig.UpdatedAt) {
		t.Fatalf("the existing installation must be untouched: %+v", after)
	}
}

func TestInstallationLimitHoldsUnderConcurrency(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("trial-race-%d", suffix), "Owner")
	org := mustCreateOrg(t, a, owner.DiscordUserID, "Race", fmt.Sprintf("trial-race-%d", suffix))
	conns := make([]int64, 6)
	for i := range conns {
		conns[i] = connectExtraGuild(t, a, verifier, org.ID, owner.DiscordUserID)
	}
	var wg sync.WaitGroup
	codes := make([]int, len(conns))
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c int64) {
			defer wg.Done()
			codes[i] = postCreateInstallation(a, org.ID, c, owner.DiscordUserID).Code
		}(i, c)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else if c != http.StatusConflict {
			t.Fatalf("unexpected status %d (%v)", c, codes)
		}
	}
	if created != 1 || installationCount(t, a, org.ID) != 1 {
		t.Fatalf("exactly one concurrent create may win: %v, count %d", codes, installationCount(t, a, org.ID))
	}
}

func TestExpiredTrialRequiresBillingAndCannotRestart(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	ctx := context.Background()
	f := buildInstallationFixture(t, a, verifier)
	ended := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE subscriptions SET trial_ends_at=$2 WHERE organization_id=$1`, f.OrgID, ended); err != nil {
		t.Fatal(err)
	}
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", startTrialRequest{}), f.OwnerDiscordID), map[string]string{"organizationID": strconv.FormatInt(f.OrgID, 10)})
	rr := httptest.NewRecorder()
	a.handleStartTrial(rr, req)
	st := decodeBody[TrialStateDTO](t, rr)
	if rr.Code != http.StatusOK || st.Started || st.TrialStatus != billing.TrialExpired || !st.BillingRequired || st.DaysRemaining != 0 {
		t.Fatalf("%d %+v", rr.Code, st)
	}
	sub, _ := a.SaaSSubscriptions.GetForOrganization(ctx, f.OrgID)
	if sub.Status != repository.SubscriptionTrial || !sub.TrialEndsAt.Equal(ended) || sub.ProviderCustomerID != "" {
		t.Fatalf("an expired trial is never restarted, extended or sent to checkout: %+v", sub)
	}
	// A new service needs paid activation; the existing installation and its data stay.
	conn := connectExtraGuild(t, a, verifier, f.OrgID, f.OwnerDiscordID)
	res := postCreateInstallation(a, f.OrgID, conn, f.OwnerDiscordID)
	if res.Code != http.StatusPaymentRequired || errorCodeOf(t, res) != codeBillingRequired {
		t.Fatalf("expected 402 BILLING_REQUIRED, got %d %s", res.Code, res.Body.String())
	}
	if inst, _ := a.SaaSInstallations.GetScoped(ctx, f.OrgID, f.InstallationID); inst == nil {
		t.Fatal("trial expiry must preserve the existing installation")
	}
}

func TestPaidSubscriptionIsNeverOverwrittenAndUsesPlanCapacity(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	ctx := context.Background()
	cat, err := billing.LoadCatalog(billingTestCatalogJSON) // PRO: limits.installations 5
	if err != nil {
		t.Fatal(err)
	}
	provider := billing.NewFakeProvider()
	a.Billing = billing.NewService(a.SaaSSubscriptions, cat, provider, billing.Options{WebhookSecret: "whsec_test"})
	f := buildInstallationFixture(t, a, verifier)
	if _, err := a.SaaSSubscriptions.ApplyProviderState(ctx, f.OrgID, repository.ProviderState{Provider: repository.ProviderStripe, ProviderCustomerID: uniqID("cus"),
		ProviderSubscriptionID: uniqID("sub"), ProviderPriceID: "price_test_pro_month", Plan: "PRO", Status: repository.SubscriptionActive, BillingInterval: repository.BillingIntervalMonthly}); err != nil {
		t.Fatal(err)
	}
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", startTrialRequest{PlanKey: "STARTER"}), f.OwnerDiscordID), map[string]string{"organizationID": strconv.FormatInt(f.OrgID, 10)})
	rr := httptest.NewRecorder()
	a.handleStartTrial(rr, req)
	st := decodeBody[TrialStateDTO](t, rr)
	sub, _ := a.SaaSSubscriptions.GetForOrganization(ctx, f.OrgID)
	if rr.Code != http.StatusOK || st.Started || st.TrialStatus != billing.TrialConverted || st.BillingRequired || st.InstallationLimit != 5 ||
		sub.Plan != "PRO" || sub.Status != repository.SubscriptionActive || sub.IntendedPlan != "" {
		t.Fatalf("%d %+v row %+v", rr.Code, st, sub)
	}
	// The paid plan's capacity applies: a second service is allowed.
	conn := connectExtraGuild(t, a, verifier, f.OrgID, f.OwnerDiscordID)
	if res := postCreateInstallation(a, f.OrgID, conn, f.OwnerDiscordID); res.Code != http.StatusCreated {
		t.Fatalf("PRO allows 5 services: %d %s", res.Code, res.Body.String())
	}
	if len(provider.Calls) != 0 {
		t.Fatal("no Stripe calls expected")
	}
}

func TestConcurrentTrialStartsGrantOneTrialPerAccount(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("trial-conc-%d", suffix), "Owner")
	userID := mustAppUserID(t, a, owner.DiscordUserID)
	// Organizations created straight through the repository have no subscription row yet.
	var orgs []int64
	for i := 0; i < 4; i++ {
		org, err := a.SaaSOrganizations.Create(ctx, "Conc", fmt.Sprintf("trial-conc-%d-%d", suffix, i), userID)
		if err != nil {
			t.Fatal(err)
		}
		orgs = append(orgs, org.ID)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(org int64) {
			defer wg.Done()
			_, ok, err := a.SaaSSubscriptions.StartTrial(ctx, org, userID, time.Now().AddDate(0, 0, billing.TrialDays))
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(orgs[i%len(orgs)])
	}
	wg.Wait()
	trials := 0
	for _, org := range orgs {
		sub, _ := a.SaaSSubscriptions.GetForOrganization(ctx, org)
		if sub != nil && sub.Status == repository.SubscriptionTrial {
			trials++
		}
	}
	if granted != 1 || trials != 1 {
		t.Fatalf("one account, one trial: granted %d, trial rows %d", granted, trials)
	}
	if used, _ := a.SaaSSubscriptions.HasTrialGrant(ctx, userID); !used {
		t.Fatal("the grant must be recorded")
	}
}

func TestTrialBackfillCoversExistingOwners(t *testing.T) {
	// The 0047 backfill ran when the migration was applied; re-running its statement must be a
	// no-op for already-recorded owners and must record an owner whose trial predates the table.
	a, _ := saasIntegrationApp(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("trial-legacy-%d", suffix), "Owner")
	userID := mustAppUserID(t, a, owner.DiscordUserID)
	org, err := a.SaaSOrganizations.Create(ctx, "Legacy", fmt.Sprintf("trial-legacy-%d", suffix), userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaaSSubscriptions.EnsureTrial(ctx, org.ID, time.Now().AddDate(0, 0, 3)); err != nil { // a pre-V2 trial
		t.Fatal(err)
	}
	if used, _ := a.SaaSSubscriptions.HasTrialGrant(ctx, userID); used {
		t.Fatal("precondition: no grant yet")
	}
	if _, err := a.DB.Pool.Exec(ctx, database.TrialGrantBackfillSQL); err != nil {
		t.Fatal(err)
	}
	if used, _ := a.SaaSSubscriptions.HasTrialGrant(ctx, userID); !used {
		t.Fatal("an owner with a pre-V2 trial must be recorded as having used it")
	}
	// The existing trial itself is preserved, and a new organization gets none.
	sub, _ := a.SaaSSubscriptions.GetForOrganization(ctx, org.ID)
	if sub.Status != repository.SubscriptionTrial {
		t.Fatalf("%+v", sub)
	}
	next := mustCreateOrg(t, a, owner.DiscordUserID, "Next", fmt.Sprintf("trial-legacy-next-%d", suffix))
	if s, _ := a.SaaSSubscriptions.GetForOrganization(ctx, next.ID); s == nil || s.Status != repository.SubscriptionInactive {
		t.Fatalf("%+v", s)
	}
}
