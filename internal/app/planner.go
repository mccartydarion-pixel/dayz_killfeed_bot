package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/planner"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func (a *App) registerPlannerRoutes(adminBase string) {
	a.HTTPServer.Handle("GET "+adminBase+"/planner/peak-hours", a.handlePeakHoursPlanner)
}

// GET .../admin/planner/peak-hours?days=7..180&tz=IANA&eventHours=1..6
//
// The Peak Hours Planner: the observed hourly player peaks (server_hourly_activity)
// and PvP kills per weekday/hour in the owner's time zone, plus recommended
// event windows (busiest consecutive hours), restart windows (quietest hours)
// and a wipe day (busiest day). Only sampled hours are ever recommended.
func (a *App) handlePeakHoursPlanner(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapRetentionView)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.Retention) {
		return
	}
	days, ok := queryInt(w, r, "days", 28, 7, 180)
	if !ok {
		return
	}
	eventHours, ok := queryInt(w, r, "eventHours", 2, 1, 6)
	if !ok {
		return
	}
	tz := strings.TrimSpace(r.URL.Query().Get("tz"))
	if tz == "" {
		tz = "UTC"
	}
	if !timeZoneName.MatchString(tz) {
		writeSaaSError(w, codeInvalidRequest, "tz must be an IANA time zone name")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Retention == nil {
		writeSaaSError(w, codeInternalError, "planner unavailable")
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	peaks, err := a.Retention.PeakHours(ctx, *ac.scope.ServerID, now, days, tz)
	if errors.Is(err, repository.ErrInvalidTimeZone) {
		writeSaaSError(w, codeInvalidRequest, "tz must be an IANA time zone name")
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "event", "planner_failed", "what", "peaks", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the planner")
		return
	}
	kills, err := a.Retention.KillsByHour(ctx, ac.scope.GuildID, *ac.scope.ServerID, now, days, tz)
	if err != nil {
		slog.Warn("component=saas_api", "event", "planner_failed", "what", "kills", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the planner")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"days": days, "timeZone": tz, "eventHours": eventHours, "plan": planner.Build(mergeSlots(peaks, kills), eventHours, 3)})
}

func mergeSlots(peaks []repository.PeakHour, kills []repository.HourlyKills) []planner.Slot {
	idx := map[int]int{}
	out := make([]planner.Slot, 0, len(peaks))
	for _, p := range peaks {
		idx[p.Weekday*24+p.Hour] = len(out)
		out = append(out, planner.Slot{Weekday: p.Weekday, Hour: p.Hour, AveragePeak: p.AveragePeak, MaxPeak: p.MaxPeak, Samples: p.Samples})
	}
	for _, k := range kills {
		if i, ok := idx[k.Weekday*24+k.Hour]; ok {
			out[i].Kills = k.Kills
		} else {
			// Fights in an hour with no concurrency sample: show them, never advise on them.
			out = append(out, planner.Slot{Weekday: k.Weekday, Hour: k.Hour, Kills: k.Kills})
		}
	}
	return out
}
