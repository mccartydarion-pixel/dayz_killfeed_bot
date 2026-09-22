//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// End-to-end Champion Owner Billing API tests (Champion Access Model Phase 2, Part C/D) over the
// real routes and a real PostgreSQL: the plan catalog, the payment/invoice history list (pagination,
// organization/status filters), the organization detail's recent-payments extension, and platform-
// admin-only authorization (a regular Player, a Client OWNER and a Client ADMIN must all be denied).

// insertBillingTransaction inserts one billing_transactions row directly (bypassing the webhook
// path, which internal/billing's own unit tests already cover) - this file's tests are about
// whether the ADMIN list/detail endpoints correctly read, filter, paginate and scope what is
// already there, not about webhook parsing.
func (w *factionWorld) insertBillingTransaction(orgID int64, status string, amountCents int64, eventID string) int64 {
	w.t.Helper()
	var id int64
	now := time.Now()
	var paidAt, failedAt *time.Time
	if status == "PAID" {
		paidAt = &now
	} else {
		failedAt = &now
	}
	if err := w.a.DB.Pool.QueryRow(context.Background(), `
INSERT INTO billing_transactions(organization_id, provider, provider_invoice_id, status, amount_cents, currency, paid_at, failed_at, stripe_event_id)
VALUES($1,'stripe',$2,$3,$4,'usd',$5,$6,$7) RETURNING id`,
		orgID, "in_"+eventID, status, amountCents, paidAt, failedAt, eventID).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func TestAdminBillingPlansReturnsFullCatalogIncludingPrivate(t *testing.T) {
	w := newFactionWorld(t)
	resp := w.getJSON("/api/admin/billing/plans", adminFounderID)
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("expected all 3 catalog plans (public and private), got %v", resp)
	}
	names := map[string]bool{}
	for _, raw := range items {
		item := raw.(map[string]any)
		names[item["key"].(string)] = true
		if _, leaked := item["stripePriceId"]; leaked {
			t.Fatalf("the Owner plan catalog must never leak a Stripe price id: %v", item)
		}
	}
	if !names["PRO"] || !names["STARTER"] || !names["LEGACY"] {
		t.Fatalf("expected PRO, STARTER and LEGACY (private) all present for the Owner, got %v", names)
	}
}

func TestAdminBillingPaymentsListAndFilters(t *testing.T) {
	w := newFactionWorld(t)
	w.insertBillingTransaction(w.a1.OrgID, "PAID", 1999, fmt.Sprintf("evt_admin_paid_%d", time.Now().UnixNano()))
	w.insertBillingTransaction(w.a1.OrgID, "FAILED", 1999, fmt.Sprintf("evt_admin_failed_%d", time.Now().UnixNano()))
	w.insertBillingTransaction(w.b1.OrgID, "PAID", 999, fmt.Sprintf("evt_admin_other_%d", time.Now().UnixNano()))

	all := w.getJSON("/api/admin/billing/payments", adminFounderID)
	items := all["items"].([]any)
	if len(items) < 3 {
		t.Fatalf("expected at least the 3 seeded transactions, got %d: %v", len(items), all)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		for _, banned := range []string{"cardNumber", "cvc", "paymentMethod", "rawPayload"} {
			if _, leaked := item[banned]; leaked {
				t.Fatalf("must never expose payment-method detail: %v", item)
			}
		}
	}

	// Organization filter.
	orgFiltered := w.getJSON(fmt.Sprintf("/api/admin/billing/payments?organizationId=%d", w.a1.OrgID), adminFounderID)
	for _, raw := range orgFiltered["items"].([]any) {
		item := raw.(map[string]any)
		if int64(item["organizationId"].(float64)) != w.a1.OrgID {
			t.Fatalf("organization filter leaked another org's row: %v", item)
		}
	}
	if len(orgFiltered["items"].([]any)) != 2 {
		t.Fatalf("expected exactly org A's 2 transactions, got %v", orgFiltered)
	}

	// Status filter.
	statusFiltered := w.getJSON(fmt.Sprintf("/api/admin/billing/payments?organizationId=%d&status=FAILED", w.a1.OrgID), adminFounderID)
	items = statusFiltered["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["status"] != "FAILED" {
		t.Fatalf("expected exactly 1 FAILED transaction, got %v", statusFiltered)
	}

	// Pagination: limit=1 must return exactly 1 item and a usable cursor.
	page1 := w.getJSON(fmt.Sprintf("/api/admin/billing/payments?organizationId=%d&limit=1", w.a1.OrgID), adminFounderID)
	items = page1["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 item with limit=1, got %d", len(items))
	}
	cursor, _ := page1["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("expected a nextCursor with more rows remaining: %v", page1)
	}
	page2 := w.getJSON(fmt.Sprintf("/api/admin/billing/payments?organizationId=%d&limit=1&cursor=%s", w.a1.OrgID, cursor), adminFounderID)
	items2 := page2["items"].([]any)
	if len(items2) != 1 || items[0].(map[string]any)["id"] == items2[0].(map[string]any)["id"] {
		t.Fatalf("page 2 must return a DIFFERENT row than page 1: page1=%v page2=%v", page1, page2)
	}
}

func TestAdminBillingPaymentsUnknownStatusReturnsEmptyNotError(t *testing.T) {
	w := newFactionWorld(t)
	resp := w.getJSON("/api/admin/billing/payments?status=REFUNDED", adminFounderID) // never a status this system writes
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("an unrecognized status must yield an empty page, not an error, got %v", resp)
	}
}

func TestAdminOrganizationDetailIncludesRecentPayments(t *testing.T) {
	w := newFactionWorld(t)
	w.insertBillingTransaction(w.a1.OrgID, "PAID", 1999, fmt.Sprintf("evt_detail_%d", time.Now().UnixNano()))

	resp := w.getJSON(fmt.Sprintf("/api/admin/organizations/%d", w.a1.OrgID), adminFounderID)
	payments, ok := resp["recentPayments"].([]any)
	if !ok || len(payments) != 1 {
		t.Fatalf("expected the organization detail to include its 1 recent payment, got %v", resp)
	}
	if int64(payments[0].(map[string]any)["organizationId"].(float64)) != w.a1.OrgID {
		t.Fatalf("%v", payments[0])
	}
	// The existing subscription block (plan/status/trial/period/cancelAtPeriodEnd) must still be
	// present unchanged - this phase only ADDS recentPayments, never replaces the existing shape.
	if resp["subscription"] == nil {
		t.Fatalf("expected the existing subscription block to still be present: %v", resp)
	}
}

// --- authorization: platform-admin only (Part G.28/29) --------------------------------------------

func TestAdminBillingRoutesDenyNonPlatformAdmins(t *testing.T) {
	w := newFactionWorld(t)
	paths := []string{"/api/admin/billing/plans", "/api/admin/billing/payments"}
	for _, path := range paths {
		// A regular Player (no organization role at all).
		w.expect(w.do(http.MethodGet, path, w.players[0], nil), http.StatusForbidden, "player: "+path)
		// A Client OWNER of a real organization.
		w.expect(w.do(http.MethodGet, path, w.a1.OwnerDiscordID, nil), http.StatusForbidden, "client owner: "+path)
		// A Client ADMIN of a real organization.
		w.expect(w.do(http.MethodGet, path, w.admin, nil), http.StatusForbidden, "client admin: "+path)
		// No acting user at all.
		w.expect(w.do(http.MethodGet, path, "", nil), http.StatusUnauthorized, "unauthenticated: "+path)
	}
	// The platform admin IS allowed.
	w.expect(w.do(http.MethodGet, "/api/admin/billing/plans", adminFounderID, nil), http.StatusOK, "founder: plans")
	w.expect(w.do(http.MethodGet, "/api/admin/billing/payments", adminFounderID, nil), http.StatusOK, "founder: payments")
}
