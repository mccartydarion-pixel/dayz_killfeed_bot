package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/securitymarket"
)

// The player and Client Hub paths share one service catalog, scoped to the
// installation. Every service is unavailable except the Base Raid Alarm,
// Perimeter Watch, Base Black Box and Faction Security, each when the server
// owner has it on and is selling it for Champion Points.
func (a *App) registerSecurityMarketplaceRoutes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/security-marketplace"
	a.HTTPServer.Handle("GET "+base+"/catalog", a.handleSecurityMarketplaceCatalog)
	a.HTTPServer.Handle("GET "+base+"/admin/catalog", a.handleSecurityMarketplaceAdminCatalog)
	a.HTTPServer.Handle("POST "+base+"/purchases", a.handleSecurityMarketplacePurchase)
	a.HTTPServer.Handle("GET "+base+"/black-box", a.handlePlayerBaseBlackBox)
	a.HTTPServer.Handle("GET "+base+"/faction-security", a.handleGetPlayerFactionSecurity)
	a.HTTPServer.Handle("PUT "+base+"/faction-security", a.handleSetPlayerFactionSecurity)
	a.HTTPServer.Handle("GET "+base+"/base-requests", a.handleGetPlayerBaseRequests)
	a.HTTPServer.Handle("POST "+base+"/base-requests", a.handleCreatePlayerBaseRequest)
	a.HTTPServer.Handle("POST "+base+"/base-requests/{requestID}/cancel", a.handleCancelPlayerBaseRequest)
}

type securityMarketplaceCatalogResponse struct {
	InstallationID int64                         `json:"installationId"`
	GameServerID   *int64                        `json:"gameServerId"`
	Items          []securitymarket.Availability `json:"items"`
}

// securityServiceOn reports whether the feature behind a sellable service is
// switched on, so a player never pays for something that isn't running.
func (a *App) securityServiceOn(ctx context.Context, s repository.SecurityScope, serviceID string) (bool, error) {
	switch serviceID {
	case repository.ServiceBaseRaidAlarm:
		st, err := repository.NewBaseRaidAlarmRepository(a.DB.Pool).GetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID)
		return err == nil && st.Enabled, err
	case repository.ServicePerimeterWatch:
		st, err := repository.NewPerimeterWatchRepository(a.DB.Pool).GetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID)
		return err == nil && st.Enabled, err
	case repository.ServiceBaseBlackBox:
		st, err := repository.NewBaseBlackBoxRepository(a.DB.Pool).GetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID)
		return err == nil && st.Enabled, err
	case repository.ServiceFactionSecurity:
		st, err := repository.NewFactionSecurityRepository(a.DB.Pool).GetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID)
		return err == nil && st.Enabled, err
	}
	return false, nil
}

// securityCatalog returns the catalog for one installation, with each
// sellable service opened when the owner has it switched on and on sale, and
// the player's paid time if known.
func (a *App) securityCatalog(ctx context.Context, scope repository.EconomyScope, playerID int64) []securitymarket.Availability {
	items := securitymarket.UnverifiedCatalog()
	if scope.ServerID <= 0 || a.DB == nil || a.DB.Pool == nil {
		return items
	}
	s := repository.SecurityScope{InstallationID: scope.InstallationID, GuildID: scope.GuildID, ServerID: scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	for i := range items {
		id := items[i].Service.ID
		if !repository.SellableSecurityService(id) {
			continue
		}
		offer, err := sales.GetOffer(ctx, s, id)
		if err != nil {
			slog.Warn("component=security_market", "msg", "read offer failed", "service", id, "err", err.Error())
			continue
		}
		on, err := a.securityServiceOn(ctx, s, id)
		if err != nil {
			slog.Warn("component=security_market", "msg", "read service switch failed", "service", id, "err", err.Error())
			continue
		}
		if offer.Enabled && on {
			items[i].Status, items[i].Reason, items[i].Purchasable = "AVAILABLE", "", true
			items[i].PricePoints, items[i].DurationDays = offer.PricePoints, offer.DurationDays
		} else {
			items[i].Reason = "NOT_OFFERED"
		}
		if playerID > 0 {
			if until, err := sales.ActiveUntil(ctx, s.InstallationID, playerID, id); err == nil {
				items[i].ActiveUntil = until
			}
		}
	}
	return items
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
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	var playerID int64
	if !admin {
		// The same verified DayZ link used by the existing Player Hub wallet
		// prevents a signed-in player from enumerating another guild's catalog.
		acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
		if err != nil {
			economyFailed(w, "verify security-store player", err)
			return
		}
		playerID = acct.AccountID
	}
	var serverID *int64
	if er.scope.ServerID > 0 {
		id := er.scope.ServerID
		serverID = &id
	}
	writeSaaSJSON(w, http.StatusOK, securityMarketplaceCatalogResponse{
		InstallationID: er.scope.InstallationID, GameServerID: serverID,
		Items: a.securityCatalog(ctx, er.scope, playerID),
	})
}

func (a *App) handleSecurityMarketplaceCatalog(w http.ResponseWriter, r *http.Request) {
	a.securityMarketplaceCatalog(w, r, false)
}

func (a *App) handleSecurityMarketplaceAdminCatalog(w http.ResponseWriter, r *http.Request) {
	a.securityMarketplaceCatalog(w, r, true)
}

type securityPurchaseBody struct {
	ServiceID      string `json:"serviceId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type securityPurchaseResponse struct {
	Currency         economy.Currency            `json:"currency"`
	Purchase         repository.SecurityPurchase `json:"purchase"`
	RemainingBalance int64                       `json:"remainingBalance"`
	Duplicate        bool                        `json:"duplicate"`
}

var securityKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,64}$`)

// handleSecurityMarketplacePurchase is POST .../security-marketplace/purchases:
// a verified player buys a sellable base service (Base Raid Alarm, Perimeter
// Watch, Base Black Box or Faction Security) at the owner's current price.
// 201 for a new purchase, 200 for a replay of the same idempotency key.
func (a *App) handleSecurityMarketplacePurchase(w http.ResponseWriter, r *http.Request) {
	er, ok := a.scopedContext(w, r, "")
	if !ok {
		return
	}
	if a.saasShopPurchaseLimiter != nil && !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	var body securityPurchaseBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if !repository.SellableSecurityService(body.ServiceID) {
		writeSaaSError(w, codeInvalidRequest, "this service can't be bought")
		return
	}
	if !securityKeyRe.MatchString(body.IdempotencyKey) {
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	if er.scope.ServerID <= 0 || a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeConflict, "the installation has no DayZ server selected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "verify security-store player", err)
		return
	}
	s := repository.SecurityScope{InstallationID: er.scope.InstallationID, GuildID: er.scope.GuildID, ServerID: er.scope.ServerID}
	notForSale := "the " + repository.SecurityServiceLabel(body.ServiceID) + " isn't for sale on this server right now"
	// The feature must be switched on, or the player would pay for nothing.
	if on, err := a.securityServiceOn(ctx, s, body.ServiceID); err != nil || !on {
		writeSaaSError(w, codeConflict, notForSale)
		return
	}
	res, err := repository.NewSecurityServiceRepository(a.DB.Pool).Purchase(ctx, s, acct.AccountID, body.ServiceID, body.IdempotencyKey)
	switch {
	case errors.Is(err, repository.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "you don't have enough Champion Points")
		return
	case errors.Is(err, repository.ErrSecurityOfferUnavailable):
		writeSaaSError(w, codeConflict, notForSale)
		return
	case err != nil:
		slog.Error("component=security_market", "msg", "purchase failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the purchase couldn't be completed; nothing was charged")
		return
	}
	slog.Info("component=security_market", "event", "purchase", "installation_id", s.InstallationID, "player_id", acct.AccountID,
		"purchase_id", res.Purchase.ID, "price_points", res.Purchase.PricePoints, "duplicate", res.Duplicate)
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeSaaSJSON(w, status, securityPurchaseResponse{Currency: economy.ChampionPoints, Purchase: res.Purchase,
		RemainingBalance: res.BalanceAfter, Duplicate: res.Duplicate})
}
