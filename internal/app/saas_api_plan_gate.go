package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
)

// Plan tiers (internal/entitlements). A Champion-only route answers PLAN_FEATURE_REQUIRED
// (403) to an organization whose plan doesn't include the feature, and creating a
// faction past the plan's cap answers FACTION_LIMIT_REACHED (409). The website shows an
// upgrade prompt for both. With CHAMPION_PLAN_GATING_ENABLED off every check passes.
const (
	codePlanFeatureRequired = "PLAN_FEATURE_REQUIRED"
	codeFactionLimitReached = "FACTION_LIMIT_REACHED"
)

func init() {
	httpStatusForCode[codePlanFeatureRequired] = http.StatusForbidden
	httpStatusForCode[codeFactionLimitReached] = http.StatusConflict
}

// organizationPlan returns the organization's subscription plan key ("" when it has no
// subscription row). Called only after the request's tenant checks have passed.
func (a *App) organizationPlan(ctx context.Context, organizationID int64) (string, error) {
	if a.SaaSSubscriptions == nil || organizationID <= 0 {
		return "", nil
	}
	sub, err := a.SaaSSubscriptions.GetForOrganization(ctx, organizationID)
	if err != nil || sub == nil {
		return "", err
	}
	return sub.Plan, nil
}

// requirePlanFeature writes PLAN_FEATURE_REQUIRED and returns false when the
// organization's plan does not include key. Call it AFTER the route's own
// authorization, so an outsider learns nothing about another organization's plan.
func (a *App) requirePlanFeature(w http.ResponseWriter, r *http.Request, organizationID int64, key entitlements.Key) bool {
	if !entitlements.Enforced() {
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	plan, err := a.organizationPlan(ctx, organizationID)
	if err != nil {
		slog.Warn("component=entitlements", "event", "plan_lookup_failed", "organization_id", organizationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify your plan")
		return false
	}
	if !entitlements.Has(plan, key) {
		writeSaaSError(w, codePlanFeatureRequired, planFeatureRequiredMessage(key))
		return false
	}
	return true
}

func planFeatureRequiredMessage(key entitlements.Key) string {
	return entitlements.Label(key) + " is part of the Champion plan. Upgrade to use it."
}

func factionLimitMessage(limit int) string {
	return "Your plan allows up to " + strconv.Itoa(limit) + " factions on this server. Upgrade to Champion for unlimited factions."
}
