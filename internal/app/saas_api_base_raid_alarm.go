package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Base Raid Alarm owner settings: GET shows the switch and the latest alarms,
// PUT turns it on or off. Server owner only (UAV_MANAGE), like base
// registration. Nothing is sold and no Champion Points move.

type baseRaidAlarmSettingsRequest struct {
	Enabled         bool `json:"enabled"`
	CooldownSeconds *int `json:"cooldownSeconds"`
}

func (a *App) handleGetBaseRaidAlarm(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	repo := repository.NewBaseRaidAlarmRepository(a.DB.Pool)
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	settings, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "event", "get_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the raid alarm")
		return
	}
	recent, err := repo.RecentAlerts(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, 10)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "event", "recent_alerts_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the raid alarm")
		return
	}
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	offer, err := sales.GetOffer(ctx, scope, repository.ServiceBaseRaidAlarm)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "event", "get_offer_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the raid alarm")
		return
	}
	sold, active, err := sales.RecentSales(ctx, scope, 10)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "event", "recent_sales_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the raid alarm")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "recent": recent,
		"offer": offer, "sales": sold, "activeSubscribers": active})
}

type baseRaidAlarmOfferRequest struct {
	Enabled      bool  `json:"enabled"`
	PricePoints  int64 `json:"pricePoints"`
	DurationDays int   `json:"durationDays"`
}

// handleSetBaseRaidAlarmOffer is PUT .../admin/case/raid-alarm/offer: the
// owner sells the alarm to players for Champion Points, or stops selling it
// (then the alarm is free for every registered base again). Owner only.
func (a *App) handleSetBaseRaidAlarmOffer(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req baseRaidAlarmOfferRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid raid alarm offer")
		return
	}
	if req.PricePoints < 1 || req.PricePoints > repository.SecurityMaxPricePoints {
		writeSaaSError(w, codeInvalidRequest, "price must be between 1 and 1,000,000,000 Champion Points")
		return
	}
	if req.DurationDays < 1 || req.DurationDays > repository.SecurityMaxDurationDays {
		writeSaaSError(w, codeInvalidRequest, "length must be between 1 and 90 days")
		return
	}
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	scope := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	offer, err := repository.NewSecurityServiceRepository(a.DB.Pool).SetOffer(ctx, scope, repository.ServiceBaseRaidAlarm,
		req.Enabled, req.PricePoints, req.DurationDays, actor)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "event", "set_offer_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save the raid alarm offer")
		return
	}
	a.recordAudit(ctx, ac, "BASE_RAID_ALARM_OFFER_SAVED", "base-raid-alarm", "", "success", nil,
		map[string]any{"enabled": offer.Enabled, "pricePoints": offer.PricePoints, "durationDays": offer.DurationDays})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"offer": offer})
}

func (a *App) handleSetBaseRaidAlarm(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req baseRaidAlarmSettingsRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid raid alarm settings")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewBaseRaidAlarmRepository(a.DB.Pool)
	var cooldown int
	if req.CooldownSeconds != nil {
		cooldown = *req.CooldownSeconds
	} else {
		// Omitted: keep the saved cooldown.
		current, err := repo.GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not load the raid alarm")
			return
		}
		cooldown = current.CooldownSeconds
	}
	if cooldown < repository.BaseRaidMinCooldownSeconds || cooldown > repository.BaseRaidMaxCooldownSeconds {
		writeSaaSError(w, codeInvalidRequest, "cooldown must be between 1 and 60 minutes")
		return
	}
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	settings, err := repo.SetSettings(ctx,
		ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, req.Enabled, cooldown, actor)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "event", "set_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save the raid alarm")
		return
	}
	a.recordAudit(ctx, ac, "BASE_RAID_ALARM_SAVED", "base-raid-alarm", "", "success", nil,
		map[string]any{"enabled": settings.Enabled, "cooldownSeconds": settings.CooldownSeconds})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings})
}
