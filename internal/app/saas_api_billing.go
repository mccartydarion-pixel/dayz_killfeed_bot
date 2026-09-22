package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Billing API (docs/BILLING.md): the public plan catalog, an organization's subscription
// summary, Stripe Checkout/Customer Portal session creation, plan changes, cancel/reactivate, and
// the Stripe webhook. Every handler here only translates HTTP - all Stripe-specific logic and every
// database write lives in internal/billing, which is the only package that imports the Stripe SDK.
//
// Checkout, the portal, plan changes and cancel/reactivate all require organization OWNER/ADMIN
// (docs/BILLING.md "Checkout authorization"); a faction role grants nothing here, matching every
// other billing-adjacent surface in this codebase (economy, shop). The webhook route is the one
// exception to the whole file: Stripe calls it directly, so it authenticates via the
// Stripe-Signature header instead of the service-auth + acting-user chain every other route uses.

const (
	codeInvalidPlan          = "INVALID_PLAN"
	codeNoActiveSubscription = "NO_ACTIVE_SUBSCRIPTION"
	codeNoBillingCustomer    = "NO_BILLING_CUSTOMER"
	codeBillingUnavailable   = "BILLING_UNAVAILABLE"

	billingTimeout = 15 * time.Second // Stripe API calls (checkout/portal/subscription updates) need more than a DB round trip
	maxWebhookBody = 512 * 1024       // Stripe's own payloads are well under this; refuses anything absurd before it is even read
)

func init() {
	httpStatusForCode[codeInvalidPlan] = http.StatusBadRequest
	httpStatusForCode[codeNoActiveSubscription] = http.StatusConflict
	httpStatusForCode[codeNoBillingCustomer] = http.StatusConflict
	httpStatusForCode[codeBillingUnavailable] = http.StatusServiceUnavailable
}

func (a *App) registerBillingRoutes() {
	if a.saasBillingActionLimiter == nil {
		a.saasBillingActionLimiter = newSaaSRateLimiter(time.Minute, 20)
	}
	h := a.HTTPServer.Handle
	h("GET /api/saas/billing/plans", a.handleBillingPlans)
	const base = "/api/saas/organizations/{organizationID}/billing"
	h("GET "+base+"/subscription", a.handleBillingSubscription)
	h("POST "+base+"/checkout", a.handleBillingCheckout)
	h("POST "+base+"/portal", a.handleBillingPortal)
	h("POST "+base+"/plan", a.handleBillingChangePlan)
	h("POST "+base+"/cancel", a.handleBillingCancel)
	h("POST "+base+"/reactivate", a.handleBillingReactivate)
	// Not behind requireSaaSServiceAuth: Stripe calls this directly and authenticates with its own
	// HMAC signature (billing.VerifyWebhookEvent), never the website's bearer token.
	h("POST /api/saas/billing/webhook", a.handleStripeWebhook)
}

// billingRequest is the resolved (acting user, organization, role) a billing handler needs.
type billingRequest struct {
	user  *repository.AppUser
	orgID int64
	role  string // "" when the route only required membership, not requireOrganizationRole
}

// billingContext runs the standard chain (service auth, acting user, organizationID) and, when
// requireRole is true, additionally requires OWNER/ADMIN (docs/BILLING.md "Checkout authorization");
// otherwise it only requires membership (read routes).
func (a *App) billingContext(w http.ResponseWriter, r *http.Request, requireRole bool) (br billingRequest, ok bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, good := pathInt64(w, r, "organizationID")
	if !good {
		return
	}
	if a.Billing == nil {
		writeSaaSError(w, codeInternalError, "billing unavailable")
		return
	}
	var role string
	if requireRole {
		role, ok = a.requireOrganizationRole(w, r, orgID, user.ID)
	} else {
		role, ok = a.requireOrganizationMember(w, r, orgID, user.ID)
	}
	if !ok {
		return billingRequest{}, false
	}
	return billingRequest{user: user, orgID: orgID, role: role}, true
}

// billingFailed maps a billing.Service error to the stable error contract.
func billingFailed(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, billing.ErrUnknownPlan):
		writeSaaSError(w, codeInvalidPlan, "unknown plan")
	case errors.Is(err, billing.ErrPlanNotSold):
		writeSaaSError(w, codeInvalidPlan, "this plan is not sold on the requested billing interval")
	case errors.Is(err, billing.ErrNoActiveSubscription):
		writeSaaSError(w, codeNoActiveSubscription, "this organization has no active billing subscription yet")
	case errors.Is(err, billing.ErrNoBillingCustomer):
		writeSaaSError(w, codeNoBillingCustomer, "this organization has not started billing yet")
	case errors.Is(err, billing.ErrInvalidReturnPath):
		writeSaaSError(w, codeInvalidRequest, "invalid returnPath")
	case errors.Is(err, billing.ErrProviderNotConfigured):
		writeSaaSError(w, codeBillingUnavailable, "billing is not configured on this environment")
	default:
		slog.Error("component=saas_api", "msg", what+" failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "billing request failed")
	}
}

func billingAudit(event string, br billingRequest, attrs ...any) {
	base := []any{"event", event, "organization_id", br.orgID, "acting_user_id", br.user.ID}
	slog.Info("component=saas_api", append(base, attrs...)...)
}

// --- DTOs (docs/BILLING.md "Website handoff") ---------------------------------------------------------

type billingPriceDTO struct {
	AmountCents int64  `json:"amountCents"`
	Currency    string `json:"currency"`
}

// billingPlanDTO is deliberately missing any Stripe price id - the plans API returns public-safe
// pricing information only (docs/BILLING.md "Plan catalog").
type billingPlanDTO struct {
	Key         string           `json:"key"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Features    []string         `json:"features"`
	Limits      map[string]int   `json:"limits"`
	Monthly     *billingPriceDTO `json:"monthly"`
	Yearly      *billingPriceDTO `json:"yearly"`
	Popular     bool             `json:"popular"`
	TrialDays   int              `json:"trialDays"`
	SortOrder   int              `json:"sortOrder"`
}

type billingPlansResponse struct {
	Items []billingPlanDTO `json:"items"`
}

func priceDTO(p *billing.PriceMeta) *billingPriceDTO {
	if p == nil {
		return nil
	}
	return &billingPriceDTO{AmountCents: p.AmountCents, Currency: p.Currency}
}

func planDTO(p billing.Plan) billingPlanDTO {
	return billingPlanDTO{Key: p.Key, Name: p.Name, Description: p.Description, Features: append([]string{}, p.Features...), Limits: p.Limits,
		Monthly: priceDTO(p.Monthly), Yearly: priceDTO(p.Yearly), Popular: p.Popular, TrialDays: p.TrialDays, SortOrder: p.SortOrder}
}

// SubscriptionSummaryDTO is the website's one-call subscription view - a superset of the existing
// SubscriptionSummary (reused unchanged in the dashboard/hub) with the fields Billing Phase 1 adds.
// It never carries a Stripe id.
type SubscriptionSummaryDTO struct {
	Plan                  string   `json:"plan"`
	Status                string   `json:"status"`
	BillingInterval       *string  `json:"billingInterval"`
	TrialEndsAt           *string  `json:"trialEndsAt"`
	CurrentPeriodStart    *string  `json:"currentPeriodStart"`
	CurrentPeriodEnd      *string  `json:"currentPeriodEnd"`
	CancelAtPeriodEnd     bool     `json:"cancelAtPeriodEnd"`
	Entitlements          []string `json:"entitlements"`
	HasBillingCustomer    bool     `json:"hasBillingCustomer"`
	HasActiveSubscription bool     `json:"hasActiveSubscription"`
	CanManageBilling      bool     `json:"canManageBilling"` // the acting user is OWNER/ADMIN
}

func subscriptionSummaryDTO(sum *billing.Summary, canManage bool) SubscriptionSummaryDTO {
	return SubscriptionSummaryDTO{
		Plan: sum.Plan, Status: sum.Status, BillingInterval: optStr(sum.BillingInterval),
		TrialEndsAt: nullableTimeStr(sum.TrialEndsAt), CurrentPeriodStart: nullableTimeStr(sum.CurrentPeriodStart), CurrentPeriodEnd: nullableTimeStr(sum.CurrentPeriodEnd),
		CancelAtPeriodEnd: sum.CancelAtPeriodEnd, Entitlements: sum.Entitlements, HasBillingCustomer: sum.HasBillingCustomer,
		HasActiveSubscription: sum.HasActiveSubscription, CanManageBilling: canManage,
	}
}

// --- plans ----------------------------------------------------------------------------------------

// handleBillingPlans is GET /api/saas/billing/plans: any synced user, no organization context.
func (a *App) handleBillingPlans(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if a.resolveActingUser(w, r) == nil {
		return
	}
	if a.Billing == nil {
		writeSaaSError(w, codeInternalError, "billing unavailable")
		return
	}
	items := make([]billingPlanDTO, 0, len(a.Billing.Plans()))
	for _, p := range a.Billing.Plans() {
		items = append(items, planDTO(p))
	}
	writeSaaSJSON(w, http.StatusOK, billingPlansResponse{Items: items})
}

// --- subscription -----------------------------------------------------------------------------------

// handleBillingSubscription is GET .../billing/subscription: any organization member.
func (a *App) handleBillingSubscription(w http.ResponseWriter, r *http.Request) {
	br, ok := a.billingContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	sum, err := a.Billing.Summary(ctx, br.orgID)
	if err != nil {
		billingFailed(w, "load subscription summary", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, subscriptionSummaryDTO(sum, br.role == repository.RoleOwner || br.role == repository.RoleAdmin))
}

// --- checkout / portal ------------------------------------------------------------------------------

type checkoutRequestBody struct {
	PlanKey    string `json:"planKey"`
	Interval   string `json:"interval"`
	ReturnPath string `json:"returnPath"`
}
type checkoutResponseDTO struct {
	CheckoutURL string `json:"checkoutUrl"`
}

// handleBillingCheckout is POST .../billing/checkout: organization OWNER/ADMIN only.
func (a *App) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	br, ok := a.billingContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasBillingActionLimiter, rateLimitKey(r)) {
		return
	}
	var body checkoutRequestBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	interval := strings.ToUpper(strings.TrimSpace(body.Interval))
	if interval == "" {
		interval = repository.BillingIntervalMonthly
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	org, err := a.getOrganizationRow(ctx, br.orgID)
	if err != nil || org == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	res, err := a.Billing.Checkout(ctx, r, br.orgID, billing.OrgInfo{Name: org.Name}, billing.CheckoutRequest{PlanKey: body.PlanKey, Interval: interval, ReturnPath: body.ReturnPath})
	if err != nil {
		billingFailed(w, "create checkout session", err)
		return
	}
	billingAudit("billing_checkout_created", br, "plan", strings.ToUpper(strings.TrimSpace(body.PlanKey)), "interval", interval)
	writeSaaSJSON(w, http.StatusOK, checkoutResponseDTO{CheckoutURL: res.URL})
}

type portalRequestBody struct {
	ReturnPath string `json:"returnPath"`
}
type portalResponseDTO struct {
	PortalURL string `json:"portalUrl"`
}

// handleBillingPortal is POST .../billing/portal: organization OWNER/ADMIN only.
func (a *App) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	br, ok := a.billingContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasBillingActionLimiter, rateLimitKey(r)) {
		return
	}
	var body portalRequestBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	res, err := a.Billing.Portal(ctx, r, br.orgID, billing.PortalRequest{ReturnPath: body.ReturnPath})
	if err != nil {
		billingFailed(w, "create portal session", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, portalResponseDTO{PortalURL: res.URL})
}

// --- plan changes, cancel, reactivate -----------------------------------------------------------------

type changePlanRequestBody struct {
	PlanKey  string `json:"planKey"`
	Interval string `json:"interval"`
}

// handleBillingChangePlan is POST .../billing/plan: organization OWNER/ADMIN only. Applies
// immediately with Stripe's default proration for both an upgrade and a downgrade
// (docs/BILLING.md "Upgrade" / "Downgrade").
func (a *App) handleBillingChangePlan(w http.ResponseWriter, r *http.Request) {
	br, ok := a.billingContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasBillingActionLimiter, rateLimitKey(r)) {
		return
	}
	var body changePlanRequestBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	interval := strings.ToUpper(strings.TrimSpace(body.Interval))
	if interval == "" {
		interval = repository.BillingIntervalMonthly
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	sum, err := a.Billing.ChangePlan(ctx, br.orgID, body.PlanKey, interval)
	if err != nil {
		billingFailed(w, "change plan", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, subscriptionSummaryDTO(sum, true))
}

// handleBillingCancel is POST .../billing/cancel: organization OWNER/ADMIN only. Schedules
// cancellation at the end of the current billing period; the subscription (and every entitlement it
// grants) stays fully active until then (docs/BILLING.md "Cancellation").
func (a *App) handleBillingCancel(w http.ResponseWriter, r *http.Request) {
	br, ok := a.billingContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasBillingActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	sum, err := a.Billing.Cancel(ctx, br.orgID)
	if err != nil {
		billingFailed(w, "cancel subscription", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, subscriptionSummaryDTO(sum, true))
}

// handleBillingReactivate is POST .../billing/reactivate: organization OWNER/ADMIN only. Undoes a
// scheduled cancellation while the subscription is still active (docs/BILLING.md "Reactivation").
func (a *App) handleBillingReactivate(w http.ResponseWriter, r *http.Request) {
	br, ok := a.billingContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasBillingActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	sum, err := a.Billing.Reactivate(ctx, br.orgID)
	if err != nil {
		billingFailed(w, "reactivate subscription", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, subscriptionSummaryDTO(sum, true))
}

// --- webhook --------------------------------------------------------------------------------------

// handleStripeWebhook is POST /api/saas/billing/webhook: Stripe calls this directly. It does NOT go
// through requireSaaSServiceAuth/resolveActingUser - there is no acting user, and the website's
// bearer token is never sent by Stripe. Authentication is the Stripe-Signature header, verified
// against STRIPE_WEBHOOK_SECRET inside billing.Service.HandleWebhook; the raw body is read (and
// size-bounded) before any parsing, exactly as HMAC verification requires.
func (a *App) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if a.Billing == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), billingTimeout)
	defer cancel()
	if err := a.Billing.HandleWebhook(ctx, body, r.Header.Get("Stripe-Signature")); err != nil {
		if errors.Is(err, billing.ErrInvalidSignature) {
			slog.Warn("component=saas_api", "event", "billing_webhook_rejected", "err", err.Error())
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if errors.Is(err, billing.ErrProviderNotConfigured) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		slog.Error("component=saas_api", "event", "billing_webhook_failed", "err", err.Error())
		w.WriteHeader(http.StatusInternalServerError) // Stripe retries a 5xx, which is correct here
		return
	}
	w.WriteHeader(http.StatusOK)
}
