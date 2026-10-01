package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// handleGetSecuritySales is GET .../admin/case/security-sales?days=7|30|90:
// the Security Store's sales, Champion Points earned and paying players per
// base service, with whether each is on sale, plus base rent collected and
// where rented bases stand. Server owner only. Read-only.
func (a *App) handleGetSecuritySales(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || (n != 7 && n != 30 && n != 90) {
			writeSaaSError(w, codeInvalidRequest, "days must be 7, 30 or 90")
			return
		}
		days = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	sum, err := sales.SalesSummary(ctx, scope, time.Now().UTC().AddDate(0, 0, -days), 20)
	if err != nil {
		slog.Warn("component=security_market", "event", "sales_summary_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load Security Store sales")
		return
	}
	type serviceView struct {
		repository.SecuritySalesRow
		Label        string `json:"label"`
		OnSale       bool   `json:"onSale"`
		SwitchedOn   bool   `json:"switchedOn"`
		PricePoints  int64  `json:"pricePoints,omitempty"`
		DurationDays int    `json:"durationDays,omitempty"`
	}
	views := make([]serviceView, 0, len(sum.Services))
	for _, row := range sum.Services {
		v := serviceView{SecuritySalesRow: row, Label: repository.SecurityServiceLabel(row.ServiceID)}
		offer, err := sales.GetOffer(ctx, scope, row.ServiceID)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not load Security Store sales")
			return
		}
		if v.SwitchedOn, err = a.securityServiceOn(ctx, scope, row.ServiceID); err != nil {
			writeSaaSError(w, codeInternalError, "could not load Security Store sales")
			return
		}
		v.OnSale = offer.Enabled
		if offer.Configured {
			v.PricePoints, v.DurationDays = offer.PricePoints, offer.DurationDays
		}
		views = append(views, v)
	}
	rent, err := repository.NewBaseRentRepository(a.DB.Pool).Summary(ctx, scope, sum.Since)
	if err != nil {
		slog.Warn("component=base_rent", "event", "rent_summary_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load Security Store sales")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"days": days, "since": sum.Since, "services": views,
		"totalSales": sum.TotalSales, "totalPoints": sum.TotalPoints, "playersWithAny": sum.PlayersWithAny, "recent": sum.Recent, "rent": rent})
}
