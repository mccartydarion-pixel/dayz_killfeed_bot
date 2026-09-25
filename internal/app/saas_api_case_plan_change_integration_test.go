//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// Real handlers + PostgreSQL + FakeProvider (no Stripe network): a paid Pro
// server downgrades to Watch while keeping paid Pro until paid_through, then
// restores Pro for free; non-admins and other organizations are refused, and
// the organization's base subscription is untouched.
func TestCASETierChangeRoutesPreviewThenApply(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	repo, end := seedCasePremiumAccess(t, w)
	row, err := repo.GetScoped(ctx, w.f.OrgID, w.f.InstallationID)
	if err != nil || row == nil || row.PaidTier != "CASE_PRO" {
		t.Fatalf("seeded add-on: %+v %v", row, err)
	}
	provider := billing.NewFakeProvider()
	provider.Put(billing.SubscriptionState{
		SubscriptionID: row.ProviderSubscriptionID, CustomerID: row.ProviderCustomerID, PriceID: "price_case_pro",
		StripeStatus: "active", CurrentPeriodStart: end.Add(-30 * 24 * time.Hour), CurrentPeriodEnd: end,
		Metadata: billing.CaseMetadata(billing.CaseCheckoutInput{AddonID: row.ID, OrganizationID: w.f.OrgID,
			InstallationID: w.f.InstallationID, GameServerID: w.serverID, Tier: casebilling.Pro}),
	})
	service := billing.NewService(w.a.SaaSSubscriptions, nil, provider, billing.Options{})
	if err := service.ConfigureCaseAddons(repo, billing.CaseOptions{
		Enabled: false, AccessEnabled: true, VerifiedThrough: casebilling.Pro,
		PriceIDs: map[casebilling.Tier]string{casebilling.Watch: "price_case_watch", casebilling.Pro: "price_case_pro"},
	}); err != nil {
		t.Fatal(err)
	}
	w.a.Billing = service
	path := fmt.Sprintf("/api/saas/organizations/%d/billing/case/plan", w.f.OrgID)
	body := map[string]any{"installationId": w.f.InstallationID, "tier": "CASE_WATCH"}

	stranger := syncUser(t, w.a, fmt.Sprintf("case-tier-stranger-%d", time.Now().UnixNano()), "Stranger")
	if rr := w.call(w.a.handleCaseBillingPlanPreview, http.MethodPost, path+"/preview", stranger.DiscordUserID, body, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("non-member previewed a tier change: %d %s", rr.Code, rr.Body.String())
	}
	rr := w.call(w.a.handleCaseBillingPlanPreview, http.MethodPost, path+"/preview", w.f.OwnerDiscordID, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	preview := decodeBody[map[string]any](t, rr)
	if preview["kind"] != "DOWNGRADE" || preview["amountDueNowCents"].(float64) != 0 || preview["targetTier"] != "CASE_WATCH" {
		t.Fatalf("downgrade preview: %v", preview)
	}
	body["prorationDate"] = int64(preview["prorationDate"].(float64))
	rr = w.call(w.a.handleCaseBillingPlanChange, http.MethodPost, path, w.f.OwnerDiscordID, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rr.Code, rr.Body.String())
	}
	applied := decodeBody[map[string]any](t, rr)
	if applied["status"] != "APPLIED" || applied["tier"] != "CASE_WATCH" {
		t.Fatalf("apply result: %v", applied)
	}
	row, _ = repo.GetScoped(ctx, w.f.OrgID, w.f.InstallationID)
	if row.Tier != "CASE_WATCH" || row.ProviderPriceID != "price_case_watch" || row.PaidTier != "CASE_PRO" {
		t.Fatalf("persisted downgrade: %+v", row)
	}
	if ok, err := service.CaseAllows(ctx, w.f.OrgID, w.f.InstallationID, w.serverID, casebilling.CapPro, time.Now()); err != nil || !ok {
		t.Fatalf("paid Pro revoked before paid_through: %v %v", ok, err)
	}
	// Restoring the already-paid tier within the period is free.
	body = map[string]any{"installationId": w.f.InstallationID, "tier": "CASE_PRO"}
	rr = w.call(w.a.handleCaseBillingPlanPreview, http.MethodPost, path+"/preview", w.f.OwnerDiscordID, body, nil)
	restore := decodeBody[map[string]any](t, rr)
	if rr.Code != http.StatusOK || restore["kind"] != "RESTORE" || restore["amountDueNowCents"].(float64) != 0 {
		t.Fatalf("restore preview: %d %v", rr.Code, restore)
	}
	// A stale confirmation is refused without any Stripe write.
	body["prorationDate"] = time.Now().Add(-time.Hour).Unix()
	if rr := w.call(w.a.handleCaseBillingPlanChange, http.MethodPost, path, w.f.OwnerDiscordID, body, nil); rr.Code != http.StatusConflict {
		t.Fatalf("stale confirmation: %d %s", rr.Code, rr.Body.String())
	}
	// Another organization's id in the path is not this installation's scope.
	other := map[string]string{"organizationID": strconv.FormatInt(w.f.OrgID+100000, 10)}
	if rr := w.call(w.a.handleCaseBillingPlanPreview, http.MethodPost, path+"/preview", w.f.OwnerDiscordID, body, other); rr.Code == http.StatusOK {
		t.Fatalf("cross-organization preview allowed: %s", rr.Body.String())
	}
	var plan, status, baseSub string
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT plan,status,provider_subscription_id FROM subscriptions WHERE organization_id=$1`, w.f.OrgID).
		Scan(&plan, &status, &baseSub); err != nil {
		t.Fatal(err)
	}
	if plan != "LOW" || status != "ACTIVE" || baseSub != fmt.Sprintf("sub-base-%d", w.f.OrgID) {
		t.Fatalf("tier change touched base billing: %s %s %s", plan, status, baseSub)
	}
}
