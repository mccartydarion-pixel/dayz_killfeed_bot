package casebilling

import (
	"strings"
	"time"
)

// AccessInput is fully server-authored. It must be built from a scoped
// installation, the stored base subscription and the stored add-on row.
// Never accept organization/server/status/provider values from a browser.
type AccessInput struct {
	BillingEnabled bool
	// VerifiedThrough is the highest tier whose real features passed live QA.
	// The zero value grants nothing; Command cannot be enabled by a Watch/Pro rollout.
	VerifiedThrough Tier

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
	PaidThrough          *time.Time // verified invoice.paid, never inferred from ACTIVE status
	// PaidTier is the tier that invoice.paid covered through PaidThrough. After
	// an upgrade it stays lower until the proration invoice is paid; after a
	// downgrade it stays higher until the paid period ends.
	PaidTier             Tier
	TrialEndsAt          *time.Time
	FounderTrialGranted bool // exact stored, one-time server grant; Stripe trialing alone is insufficient
}

// Resolve fails closed. It returns *only* capabilities for the same organization,
// installation and game server, with a currently paid/trialing Stripe add-on and
// an ACTIVE base subscription. A C.A.S.E. cancellation scheduled at period end
// retains access while Stripe reports ACTIVE and the paid period has not ended.
// PAST_DUE currently has no grace entitlement; a future explicit policy can
// change that without touching the base subscription.
func Resolve(in AccessInput, now time.Time) []Capability {
	if !in.BillingEnabled || strings.TrimSpace(in.BaseStatus) != "ACTIVE" {
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
	effective := in.Tier
	switch in.Status {
	case "ACTIVE":
		// Subscription ACTIVE does not prove the invoice was paid, especially
		// with delayed payment methods. Require confirmed paid coverage.
		if in.PaidThrough == nil || !in.PaidThrough.After(now) { return nil }
		// Capabilities follow what was actually paid for, never the tier a
		// pending upgrade is about to bill. Rows written before paid_tier
		// existed were backfilled to their tier (migration 0063).
		if in.PaidTier != "" {
			effective = in.PaidTier
		}
	case "TRIAL":
		if !in.FounderTrialGranted || in.TrialEndsAt == nil || !in.TrialEndsAt.After(now) {
			return nil
		}
	default:
		return nil
	}
	granted := tierCapabilities(effective)
	verified := tierCapabilities(in.VerifiedThrough)
	if len(granted) == 0 || len(verified) < len(granted) {
		return nil
	}
	return granted
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
