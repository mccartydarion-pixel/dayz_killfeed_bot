package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/adminrepo"
	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Owner Billing API (Champion Access Model Phase 2, Part C/D) - platform-admin only
// (requirePlatformAdmin, the same gate every /api/admin route already uses), read-only, under
// /api/admin/billing/... . Reuses the existing billing.Service catalog and the
// repository.SubscriptionRepository billing_transactions methods added for webhook persistence -
// no second billing model, no second auth system.

func (a *App) registerAdminBillingRoutes() {
	if a.HTTPServer == nil {
		return
	}
	a.HTTPServer.Handle("GET /api/admin/billing/plans", a.adminRoute(a.handleAdminBillingPlans))
	a.HTTPServer.Handle("GET /api/admin/billing/payments", a.adminRoute(a.handleAdminBillingPayments))
}

// --- plan catalog (Part C) ------------------------------------------------------------------------

// adminBillingPlanDTO is the Owner Hub's plan catalog view - every plan (public AND private,
// unlike the customer-facing GET /api/saas/billing/plans, which filters to isPublic only), still
// never carrying a Stripe price id (Part C.13: "Preferred: do NOT include them unless operationally
// necessary" - Champion's own catalog values are enough for an Owner Hub display).
type adminBillingPlanDTO struct {
	Key         string           `json:"key"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Features    []string         `json:"features"`
	Limits      map[string]int   `json:"limits"`
	Monthly     *billingPriceDTO `json:"monthly"`
	Yearly      *billingPriceDTO `json:"yearly"`
	IsPublic    bool             `json:"isPublic"`
	Popular     bool             `json:"popular"`
	TrialDays   int              `json:"trialDays"`
	SortOrder   int              `json:"sortOrder"`
}

func adminPlanDTO(p billing.Plan) adminBillingPlanDTO {
	return adminBillingPlanDTO{
		Key: p.Key, Name: p.Name, Description: p.Description, Features: append([]string{}, p.Features...), Limits: p.Limits,
		Monthly: priceDTO(p.Monthly), Yearly: priceDTO(p.Yearly), IsPublic: p.IsPublic, Popular: p.Popular, TrialDays: p.TrialDays, SortOrder: p.SortOrder,
	}
}

// handleAdminBillingPlans is GET /api/admin/billing/plans: the full catalog (public and private),
// for the Owner Hub's plan management/display page.
func (a *App) handleAdminBillingPlans(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if a.Billing == nil {
		writeSaaSError(w, codeInternalError, "billing unavailable")
		return
	}
	items := make([]adminBillingPlanDTO, 0)
	for _, p := range a.Billing.Catalog().Plans() {
		items = append(items, adminPlanDTO(p))
	}
	a.writeAdminJSON(w, http.StatusOK, adminList(items, 0, len(items)))
}

// --- payment / invoice history (Part D) -------------------------------------------------------------

// adminBillingTransactionDTO is Part D.20's safe field list - never a Stripe payment-method, card
// number or raw webhook payload. "plan" is deliberately omitted: billing_transactions does not
// capture which plan a payment was for at the time (Champion does not denormalize it from the
// webhook, which carries a price id, not a Champion plan key, without a second catalog lookup that
// could itself be wrong if the catalog changed since) - showing the organization's CURRENT plan
// next to a historical payment would misrepresent history, so it is left out rather than guessed.
type adminBillingTransactionDTO struct {
	ID               int64   `json:"id"`
	OrganizationID   int64   `json:"organizationId"`
	OrganizationName string  `json:"organizationName"`
	Status           string  `json:"status"`
	AmountCents      int64   `json:"amountCents"`
	Currency         string  `json:"currency"`
	PeriodStart      *string `json:"periodStart"`
	PeriodEnd        *string `json:"periodEnd"`
	PaidAt           *string `json:"paidAt"`
	FailedAt         *string `json:"failedAt"`
	InvoiceReference string  `json:"invoiceReference"`
	CreatedAt        string  `json:"createdAt"`
}

func adminBillingPaymentDTO(t repository.BillingTransactionRow) adminBillingTransactionDTO {
	return adminBillingTransactionDTO{
		ID: t.ID, OrganizationID: t.OrganizationID, OrganizationName: t.OrganizationName, Status: t.Status,
		AmountCents: t.AmountCents, Currency: t.Currency, PeriodStart: tsPtr(t.PeriodStart), PeriodEnd: tsPtr(t.PeriodEnd),
		PaidAt: tsPtr(t.PaidAt), FailedAt: tsPtr(t.FailedAt), InvoiceReference: t.ProviderInvoiceID, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func tsPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// isBillingTransactionStatus validates the ?status filter against the two statuses this system
// ever writes (Part D.17 - never OPEN/VOID/REFUNDED, which nothing here produces).
func isBillingTransactionStatus(v string) bool {
	return v == repository.TransactionPaid || v == repository.TransactionFailed
}

// handleAdminBillingPayments is GET /api/admin/billing/payments: cursor-paginated, optionally
// filtered by organization and status, newest first (Part D.19). A plan filter and a date-range
// filter were both left out - see docs/BILLING.md's Phase 2 section for why (no plan is captured
// per-transaction, and cursor pagination by id already gives a stable, simple ordering a date range
// would only complicate without a clear product need yet).
func (a *App) handleAdminBillingPayments(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if a.SaaSSubscriptions == nil {
		writeSaaSError(w, codeInternalError, "billing unavailable")
		return
	}
	p, ok := parseAdminListParams(w, r)
	if !ok {
		return
	}
	f := repository.BillingTransactionFilter{Limit: p.Limit, Cursor: p.Cursor}
	if status, unknown := adminEnumParam(p.Query, "status", isBillingTransactionStatus); unknown {
		a.writeAdminJSON(w, http.StatusOK, adminList([]adminBillingTransactionDTO{}, 0, p.Limit))
		return
	} else {
		f.Status = status
	}
	if raw := strings.TrimSpace(p.Query.Get("organizationId")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeSaaSError(w, codeInvalidRequest, "invalid organizationId")
			return
		}
		f.OrganizationID = id
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, next, err := a.SaaSSubscriptions.ListBillingTransactions(ctx, f)
	if err != nil {
		a.adminReadFailed(w, "billing payments", err)
		return
	}
	items := make([]adminBillingTransactionDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminBillingPaymentDTO(row))
	}
	a.writeAdminJSON(w, http.StatusOK, adminList(items, next, p.Limit))
}

// --- customer detail billing extension (Part D.21) --------------------------------------------------

// adminOrganizationDetailDTO extends adminrepo.Organization (already carrying current plan,
// subscription status, trial ends, current period end and cancelAtPeriodEnd via its Subscription
// field - Part D.21's first five items were already covered before this phase) with the
// organization's most recent payments, scoped strictly to this one organization.
type adminOrganizationDetailDTO struct {
	*adminrepo.Organization
	RecentPayments []adminBillingTransactionDTO `json:"recentPayments"`
}

const adminRecentPaymentsLimit = 10
