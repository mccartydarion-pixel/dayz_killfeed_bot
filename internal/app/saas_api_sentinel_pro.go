package app

import (
	"context"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Sentinel Pro: one purchase that covers the Base Raid Alarm, Perimeter Watch,
// Base Black Box and Faction Security. It has no switch of its own: it can be
// bought while the owner sells it and at least one covered service is on, and
// a player gets whichever covered services are on. Server owner only.

// handleGetSentinelPro is GET .../admin/case/sentinel-pro.
func (a *App) handleGetSentinelPro(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	offer, err := sales.GetOffer(ctx, scope, repository.ServiceSentinelPro)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Sentinel Pro")
		return
	}
	sold, active, err := sales.RecentSales(ctx, scope, repository.ServiceSentinelPro, 10)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load Sentinel Pro")
		return
	}
	type included struct {
		ID string `json:"id"`
		On bool   `json:"on"`
	}
	includes := make([]included, 0, len(repository.SentinelProCovers))
	for _, id := range repository.SentinelProCovers {
		on, err := a.securityServiceOn(ctx, scope, id)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not load Sentinel Pro")
			return
		}
		includes = append(includes, included{ID: id, On: on})
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"offer": offer, "sales": sold, "activeSubscribers": active, "includes": includes})
}

// handleSetSentinelProOffer is PUT .../admin/case/sentinel-pro/offer.
func (a *App) handleSetSentinelProOffer(w http.ResponseWriter, r *http.Request) {
	a.setSecurityOffer(w, r, repository.ServiceSentinelPro, "SENTINEL_PRO_OFFER_SAVED")
}
