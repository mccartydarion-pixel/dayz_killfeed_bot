package casebilling

import (
	"strings"
	"time"
)

// AccessInput is fully server-authored. It must be built from a scoped
// installation, the stored base subscription and the stored add-on row.
// Never accept organization/server/status/provider values from a browser.
type AccessInput struct {
	BillingEnabled       bool
	CapabilitiesVerified bool

	OrganizationID      int64
	InstallationID      int64
	SelectedGameServerID int64
	BaseStatus           string

	AddonOrganizationID  int64
	AddonInstallationID  int64
	BoundGameServerID    int64
	Tier                 Tier
	Status               string
	Provider             string
	ProviderSubscriptionID string
	ProviderPriceID      string
	CurrentPeriodEnd     *time.Time
	TrialEndsAt          *time.Time
}

// Resolve fails closed. It returns *only* capabilities for the same organization,
// installation and game server, with a currently paid/trialing Stripe add-on and
// an ACTIVE base subscription. A C.A.S.E. cancellation scheduled at period end
// retains access while Stripe reports ACTIVE and the paid period has not ended.
// PAST_DUE currently has no grace entitlement; a future explicit policy can
// change that without touching the base subscription.
func Resolve(in AccessInput, now time.Time) []Capability {
	if !in.BillingEnabled || !in.CapabilitiesVerified || strings.TrimSpace(in.BaseStatus) != "ACTIVE" {
		return nil
	}
	if in.OrganizationID <= 0 || in.InstallationID <= 0 || in.SelectedGameServerID <= 0 ||
		in.AddonOrganizationID != in.OrganizationID ||
		in.AddonInstallationID != in.InstallationID ||
		in.BoundGameServerID != in.SelectedGameServerID {
		return nil
	}
	if in.Provider != "stripe" ||
		strings.TrimSpace(in.ProviderSubscriptionID) == "" ||
		strings.TrimSpace(in.ProviderPriceID) == "" ||
		in.CurrentPeriodEnd == nil || !in.CurrentPeriodEnd.After(now) {
		return nil
	}
	switch in.Status {
	case "ACTIVE":
		// Access lasts only for the provider-confirmed current period.
	case "TRIAL":
		if in.TrialEndsAt == nil || !in.TrialEndsAt.After(now) {
			return nil
		}
	default:
		return nil
	}
	return tierCapabilities(in.Tier)
}

// Has never relies on the website's visibility or its own plan labels.
func Has(in AccessInput, capability Capability, now time.Time) bool {
	for _, key := range Resolve(in, now) {
		if key == capability {
			return true
		}
	}
	return false
}
