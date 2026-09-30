package app

import (
	"context"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/securitymarket"
)

// The player and Client Hub paths share one proposed service catalog. These
// read-only routes use the existing service/acting-user/installation scope.
// No capability verifier, owner configuration, checkout or entitlement exists.
func (a *App) registerSecurityMarketplaceRoutes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/security-marketplace"
	a.HTTPServer.Handle("GET "+base+"/catalog", a.handleSecurityMarketplaceCatalog)
	a.HTTPServer.Handle("GET "+base+"/admin/catalog", a.handleSecurityMarketplaceAdminCatalog)
}

type securityMarketplaceCatalogResponse struct {
	InstallationID int64                         `json:"installationId"`
	GameServerID   *int64                        `json:"gameServerId"`
	Items          []securitymarket.Availability `json:"items"`
}

func (a *App) securityMarketplaceCatalog(w http.ResponseWriter, r *http.Request, admin bool) {
	code := ""
	if admin {
		code = codeShopForbidden
	}
	er, ok := a.scopedContext(w, r, code)
	if !ok {
		return
	}
	if !admin {
		ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
		defer cancel()
		// The same verified DayZ link used by the existing Player Hub wallet
		// prevents a signed-in player from enumerating another guild's catalog.
		if _, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID); err != nil {
			economyFailed(w, "verify security-store player", err)
			return
		}
	}
	var serverID *int64
	if er.scope.ServerID > 0 {
		id := er.scope.ServerID
		serverID = &id
	}
	writeSaaSJSON(w, http.StatusOK, securityMarketplaceCatalogResponse{
		InstallationID: er.scope.InstallationID, GameServerID: serverID,
		Items: securitymarket.UnverifiedCatalog(),
	})
}

func (a *App) handleSecurityMarketplaceCatalog(w http.ResponseWriter, r *http.Request) {
	a.securityMarketplaceCatalog(w, r, false)
}

func (a *App) handleSecurityMarketplaceAdminCatalog(w http.ResponseWriter, r *http.Request) {
	a.securityMarketplaceCatalog(w, r, true)
}
