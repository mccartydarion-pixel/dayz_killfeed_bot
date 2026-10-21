package billing

import (
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Platform owner access (docs/ADMIN_API.md "Platform owner access"): an organization owned by a
// platform owner is never locked out by its own billing state. Its subscription row is left
// exactly as it is and is still reported as it is (plan, status, trial dates); only the two
// decisions that would lock it out are overridden here - "billing required" and the installation
// capacity. Customer organizations are never affected: for them StateFor is StateOf and
// InstallationLimitFor is InstallationLimit.

// OwnerInstallations is how many installations a platform owner's own organization may run.
const OwnerInstallations = 1000

// StateFor is StateOf for organizationID, with billing never required for a platform owner's own
// organization.
func StateFor(organizationID int64, sub *repository.Subscription, now time.Time) TrialState {
	st := StateOf(sub, now)
	if entitlements.OwnerOrganization(organizationID) {
		st.BillingRequired = false
		st.PlatformOwnerAccess = true
	}
	return st
}

// InstallationLimitFor is InstallationLimit for organizationID; a platform owner's own
// organization gets OwnerInstallations.
func InstallationLimitFor(organizationID int64, sub *repository.Subscription, catalog *Catalog) int {
	if entitlements.OwnerOrganization(organizationID) {
		return OwnerInstallations
	}
	return InstallationLimit(sub, catalog)
}
