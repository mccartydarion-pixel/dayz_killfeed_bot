//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Phase 6.26B: the organization sees each C.A.S.E. invoice's coverage and the audit history of
// refunds/disputes (OWNER/ADMIN only, organization-scoped) with no Stripe identifiers, and the
// servers list reports the add-on's coverageState.
func TestCASECoverageRouteShowsLedgerAndHistoryWithoutStripeIDs(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	repo, end := seedCasePremiumAccess(t, w)
	row, err := repo.GetScoped(ctx, w.f.OrgID, w.f.InstallationID)
	if err != nil || row == nil {
		t.Fatalf("seeded add-on: %+v %v", row, err)
	}
	invoiceID, piID, eventID := fmt.Sprintf("in-api-%d", row.ID), fmt.Sprintf("pi-api-%d", row.ID), fmt.Sprintf("evt-api-refund-%d", row.ID)
	applied, err := repo.ApplyCaseInvoiceReversal(ctx, repository.CaseReversalState{
		EventID: eventID, EventType: "charge.refunded", AddonID: row.ID, OrganizationID: w.f.OrgID, InstallationID: w.f.InstallationID,
		SubscriptionID: row.ProviderSubscriptionID, CustomerID: row.ProviderCustomerID, InvoiceID: invoiceID, PaymentIntentID: piID,
		Status: "REFUNDED", AmountRefunded: 999, Backfill: []repository.CaseInvoiceCoverage{},
		Adopt: &repository.CaseInvoiceCoverage{InvoiceID: invoiceID, SubscriptionID: row.ProviderSubscriptionID, PaymentIntentID: piID,
			Tier: "CASE_PRO", Currency: "usd", PeriodStart: end.Add(-30 * 24 * time.Hour), PeriodEnd: end, AmountPaid: 999},
	})
	if err != nil || !applied {
		t.Fatalf("reversal: %v %v", applied, err)
	}
	base := fmt.Sprintf("/api/saas/organizations/%d/billing/case", w.f.OrgID)
	path := fmt.Sprintf("%s/coverage?installationId=%d", base, w.f.InstallationID)

	rr := w.call(w.a.handleCaseBillingCoverage, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("coverage: %d %s", rr.Code, rr.Body.String())
	}
	for _, secret := range []string{invoiceID, piID, eventID, row.ProviderSubscriptionID, row.ProviderCustomerID} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("coverage response leaks a Stripe identifier %q: %s", secret, rr.Body.String())
		}
	}
	got := decodeBody[map[string]any](t, rr)
	invoices, _ := got["invoices"].([]any)
	history, _ := got["history"].([]any)
	if len(invoices) != 1 || len(history) != 1 {
		t.Fatalf("coverage body: %v", got)
	}
	inv := invoices[0].(map[string]any)
	if inv["tier"] != "CASE_PRO" || inv["status"] != "REFUNDED" || inv["amountPaidCents"].(float64) != 999 ||
		inv["amountRefundedCents"].(float64) != 999 || inv["periodEnd"] != end.Format(time.RFC3339) {
		t.Fatalf("invoice: %v", inv)
	}
	h := history[0].(map[string]any)
	if h["eventType"] != "charge.refunded" || h["newStatus"] != "REFUNDED" || h["paidThroughAfter"] != nil {
		t.Fatalf("history: %v", h)
	}

	servers := w.call(w.a.handleCaseBillingServers, http.MethodGet, base+"/servers", w.f.OwnerDiscordID, nil, nil)
	if servers.Code != http.StatusOK || !strings.Contains(servers.Body.String(), `"coverageState":"REFUNDED"`) {
		t.Fatalf("servers coverageState: %d %s", servers.Code, servers.Body.String())
	}

	stranger := syncUser(t, w.a, fmt.Sprintf("case-cov-stranger-%d", time.Now().UnixNano()), "Stranger")
	if rr := w.call(w.a.handleCaseBillingCoverage, http.MethodGet, path, stranger.DiscordUserID, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("non-member read coverage: %d %s", rr.Code, rr.Body.String())
	}
	other := map[string]string{"organizationID": strconv.FormatInt(w.f.OrgID+100000, 10)}
	if rr := w.call(w.a.handleCaseBillingCoverage, http.MethodGet, path, w.f.OwnerDiscordID, nil, other); rr.Code == http.StatusOK {
		t.Fatalf("cross-organization coverage read allowed: %s", rr.Body.String())
	}
	if rr := w.call(w.a.handleCaseBillingCoverage, http.MethodGet, base+"/coverage", w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing installationId: %d", rr.Code)
	}
	// Another organization's installation id yields nothing through this organization's scope.
	records, events, err := repo.ListCaseCoverage(ctx, w.f.OrgID+100000, w.f.InstallationID)
	if err != nil || len(records) != 0 || len(events) != 0 {
		t.Fatalf("coverage scoped read leaked across organizations: %v %v %v", records, events, err)
	}
}
