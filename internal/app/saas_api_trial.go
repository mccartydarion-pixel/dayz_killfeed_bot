package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Customer Onboarding V2 (docs/BILLING.md "No-card trial"): Start Free Trial -> 14 days
// -> configure the first service -> activate a paid plan when ready. Starting the trial needs no
// payment method and creates no Stripe object; the trial's one installation and the paid plans'
// installation capacity are enforced here on the backend.

const (
	codeBillingRequired          = "BILLING_REQUIRED"
	codeInstallationLimitReached = "INSTALLATION_LIMIT_REACHED"
)

func init() {
	httpStatusForCode[codeBillingRequired] = http.StatusPaymentRequired
	httpStatusForCode[codeInstallationLimitReached] = http.StatusConflict
}

func (a *App) registerTrialRoutes() {
	const base = "/api/saas/organizations/{organizationID}/trial"
	a.HTTPServer.Handle("GET "+base, a.handleGetTrial)
	a.HTTPServer.Handle("POST "+base+"/start", a.handleStartTrial)
}

// TrialStateDTO is the persisted onboarding state (docs/SAAS_API.md "Trial").
type TrialStateDTO struct {
	TrialStatus           string  `json:"trialStatus"`
	SubscriptionStatus    *string `json:"subscriptionStatus"`
	SelectedPlan          *string `json:"selectedPlan"`
	TrialStartedAt        *string `json:"trialStartedAt"`
	TrialEndsAt           *string `json:"trialEndsAt"`
	DaysRemaining         int     `json:"daysRemaining"`
	BillingRequired       bool    `json:"billingRequired"`
	PaymentMethodRequired bool    `json:"paymentMethodRequired"`
	Started               bool    `json:"started"`
	InstallationLimit     int     `json:"installationLimit"`
	InstallationCount     int     `json:"installationCount"`
}

func (a *App) trialStateDTO(ctx context.Context, orgID int64, sub *repository.Subscription, st billing.TrialState) TrialStateDTO {
	dto := TrialStateDTO{
		TrialStatus: st.TrialStatus, SubscriptionStatus: optStr(st.SubscriptionStatus), SelectedPlan: optStr(st.SelectedPlan),
		TrialStartedAt: nullableTimeStr(st.TrialStartedAt), TrialEndsAt: nullableTimeStr(st.TrialEndsAt), DaysRemaining: st.DaysRemaining,
		BillingRequired: st.BillingRequired, PaymentMethodRequired: st.PaymentMethodRequired, Started: st.Started,
		InstallationLimit: billing.InstallationLimit(sub, a.billingCatalog()),
	}
	if a.SaaSInstallations != nil {
		if n, err := a.SaaSInstallations.CountByOrganization(ctx, orgID); err == nil {
			dto.InstallationCount = n
		}
	}
	return dto
}

// billingCatalog is the loaded plan catalog, or nil when billing is not wired.
func (a *App) billingCatalog() *billing.Catalog {
	if a.Billing == nil {
		return nil
	}
	return a.Billing.Catalog()
}

// handleGetTrial is GET .../trial: any organization member. Read-only - it never starts a trial.
func (a *App) handleGetTrial(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationMember(w, r, orgID, user.ID); !ok {
		return
	}
	if a.SaaSSubscriptions == nil {
		writeSaaSError(w, codeInternalError, "subscription service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sub, err := a.SaaSSubscriptions.GetForOrganization(ctx, orgID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "load trial state failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load trial state")
		return
	}
	writeSaaSJSON(w, http.StatusOK, a.trialStateDTO(ctx, orgID, sub, billing.StateOf(sub, time.Now())))
}

type startTrialRequest struct {
	PlanKey string `json:"planKey"`
}

// handleStartTrial is POST .../trial/start: organization OWNER/ADMIN only. It starts the acting
// user's one no-card trial on this organization (or returns the existing state unchanged - a
// running trial keeps its clock, an expired trial is not restarted, a paid subscription is never
// replaced) and records the selected plan. No checkout, no Stripe call, no charge.
func (a *App) handleStartTrial(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationRole(w, r, orgID, user.ID); !ok {
		return
	}
	if a.SaaSSubscriptions == nil {
		writeSaaSError(w, codeInternalError, "subscription service unavailable")
		return
	}
	var req startTrialRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	st, err := billing.StartTrial(ctx, a.SaaSSubscriptions, a.billingCatalog(), orgID, user.ID, req.PlanKey, time.Now())
	if err != nil {
		if errors.Is(err, billing.ErrUnknownPlan) {
			writeSaaSError(w, codeInvalidPlan, "unknown plan")
			return
		}
		slog.Warn("component=saas_api", "msg", "start trial failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not start trial")
		return
	}
	sub, err := a.SaaSSubscriptions.GetForOrganization(ctx, orgID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load trial state")
		return
	}
	slog.Info("component=saas_api", "event", "trial_start_requested", "organization_id", orgID, "acting_user_id", user.ID,
		"started", st.Started, "trial_status", st.TrialStatus, "selected_plan", st.SelectedPlan)
	writeSaaSJSON(w, http.StatusOK, a.trialStateDTO(ctx, orgID, sub, st))
}

// installationCapacity decides whether organizationID may create another installation: an
// expired trial or ended subscription needs paid activation first (BILLING_REQUIRED), and the
// plan's capacity is returned for CreateWithinLimit to enforce atomically. Existing installations
// are never touched here.
func (a *App) installationCapacity(ctx context.Context, w http.ResponseWriter, orgID int64) (limit int, ok bool) {
	if a.SaaSSubscriptions == nil {
		writeSaaSError(w, codeInternalError, "subscription service unavailable")
		return 0, false
	}
	sub, err := a.SaaSSubscriptions.GetForOrganization(ctx, orgID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "load subscription for installation failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify subscription")
		return 0, false
	}
	st := billing.StateOf(sub, time.Now())
	if st.BillingRequired {
		msg := "activate a paid plan to set up a service"
		switch st.TrialStatus {
		case billing.TrialNotStarted:
			msg = "start your free trial or activate a paid plan to set up a service"
		case billing.TrialExpired:
			msg = "your free trial has ended - activate a paid plan to set up a service; your existing data is kept"
		case billing.TrialNotEligible:
			msg = "this account has already used its free trial - activate a paid plan to set up a service"
		}
		writeSaaSError(w, codeBillingRequired, msg)
		return 0, false
	}
	return billing.InstallationLimit(sub, a.billingCatalog()), true
}

func installationLimitMessage(limit int) string {
	services := "services"
	if limit == 1 {
		services = "service"
	}
	return fmt.Sprintf("your plan includes %d %s and all are set up - reconfigure an existing service, or upgrade your plan to add another", limit, services)
}
