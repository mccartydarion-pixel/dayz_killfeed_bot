package billing

import (
	"context"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// CaseAccess is the ONLY runtime source for paid C.A.S.E. capabilities.
// The caller must have authenticated and resolved the installation first.
// This method independently reloads both subscriptions on every request:
// no client-visible plan label, Discord role, stale cache or Stripe redirect
// grants an entitlement. This is safe to invoke from APIs and paid workers.
func (s *Service) CaseAccess(ctx context.Context, organizationID, installationID, gameServerID int64, now time.Time) ([]casebilling.Capability, error) {
	empty := []casebilling.Capability{}
	if s == nil || !s.caseAccessEnabled || organizationID <= 0 || installationID <= 0 || gameServerID <= 0 {
		return empty, nil
	}
	if s.store == nil || s.caseStore == nil {
		return empty, ErrProviderNotConfigured
	}
	base, err := s.store.GetForOrganization(ctx, organizationID)
	if err != nil {
		return empty, err
	}
	if base == nil || base.OrganizationID != organizationID ||
		base.Status != repository.SubscriptionActive ||
		base.Plan == repository.SubscriptionTrial || base.Plan == repository.PlanNone ||
		base.Provider != repository.ProviderStripe ||
		base.ProviderCustomerID == "" || base.ProviderSubscriptionID == "" ||
		base.ProviderPriceID == "" ||
		base.CurrentPeriodEnd == nil || !base.CurrentPeriodEnd.After(now) {
		return empty, nil
	}
	addon, err := s.caseStore.GetScoped(ctx, organizationID, installationID)
	if err != nil {
		return empty, err
	}
	if addon == nil || addon.ProviderCustomerID != base.ProviderCustomerID ||
		addon.SelectedGameServerID == nil || *addon.SelectedGameServerID != gameServerID {
		return empty, nil
	}
	return casebilling.Resolve(addon.AccessInput(organizationID, installationID,
		base.Status, s.caseAccessEnabled, s.caseVerifiedThrough), now), nil
}

// CaseAllows is the shared fail-closed paid capability check for HTTP handlers,
// Discord premium publishing and background workers. Errors are never treated
// as an entitlement; callers decide how to report/retry an unavailable store.
func (s *Service) CaseAllows(ctx context.Context, organizationID, installationID, gameServerID int64,
	capability casebilling.Capability, now time.Time) (bool, error) {
	caps, err := s.CaseAccess(ctx, organizationID, installationID, gameServerID, now)
	if err != nil {
		return false, err
	}
	for _, granted := range caps {
		if granted == capability {
			return true, nil
		}
	}
	return false, nil
}
