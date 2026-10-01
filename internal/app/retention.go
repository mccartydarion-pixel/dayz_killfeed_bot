package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Retention dashboard (docs/RETENTION.md): who is playing, who came back and who stopped, read
// from the rollups the presence pipeline writes. Read-only and aggregate, except the lapsed-player
// list, which names players - hence RETENTION_VIEW at Administrator.

func (a *App) registerRetentionRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/retention", a.handleRetention)
	h("GET "+adminBase+"/retention/lapsed", a.handleRetentionLapsed)
}

type retentionSummaryDTO struct {
	ActiveToday           int      `json:"activeToday"`
	Active7d              int      `json:"active7d"`
	Active30d             int      `json:"active30d"`
	New7d                 int      `json:"new7d"`
	New30d                int      `json:"new30d"`
	TrackedPlayers        int      `json:"trackedPlayers"`
	Stickiness            *float64 `json:"stickiness"`            // activeToday / active30d; null with nobody active
	AverageSessionSeconds *float64 `json:"averageSessionSeconds"` // last 30 days; null with no observed session
	ObservedHours30d      float64  `json:"observedHours30d"`
	CollectingSince       *string  `json:"collectingSince"` // earliest day on record (date); null with no data
}

type retentionDayDTO struct {
	Day             string `json:"day"`
	Active          int    `json:"active"`
	New             int    `json:"new"`
	Returning       int    `json:"returning"`
	ObservedSeconds int64  `json:"observedSeconds"`
	Sessions        int    `json:"sessions"`
}

type retentionCohortDTO struct {
	WeekStart string `json:"weekStart"`
	Size      int    `json:"size"`
	// Retained[i] is how many of the cohort were seen in week i+1 after their first week; null
	// while that week is still in progress or in the future.
	Retained []*int `json:"retained"`
}

type peakHourDTO struct {
	Weekday     int     `json:"weekday"` // 0 = Sunday, in the requested time zone
	Hour        int     `json:"hour"`
	AveragePeak float64 `json:"averagePeak"`
	MaxPeak     int     `json:"maxPeak"`
	Samples     int     `json:"samples"`
}

type shopRevenueDTO struct {
	ProductID   *int64 `json:"productId"`
	ProductName string `json:"productName"`
	Purchases   int    `json:"purchases"`
	Units       int    `json:"units"`
	Points      int64  `json:"points"`
	Buyers      int    `json:"buyers"`
}

type retentionResponseDTO struct {
	Days      int                  `json:"days"`
	TimeZone  string               `json:"timeZone"`
	Summary   retentionSummaryDTO  `json:"summary"`
	Daily     []retentionDayDTO    `json:"daily"`
	Cohorts   []retentionCohortDTO `json:"cohorts"`
	PeakHours []peakHourDTO        `json:"peakHours"`
	Shop      []shopRevenueDTO     `json:"shop"`
}

// timeZoneName bounds what is passed to PostgreSQL as a zone name: IANA names and plain offsets.
var timeZoneName = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)

func queryInt(w http.ResponseWriter, r *http.Request, name string, fallback, min, max int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min || v > max {
		writeSaaSError(w, codeInvalidRequest, name+" must be between "+strconv.Itoa(min)+" and "+strconv.Itoa(max))
		return 0, false
	}
	return v, true
}

// handleRetention is GET .../admin/retention?days=&tz= (RETENTION_VIEW).
func (a *App) handleRetention(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapRetentionView)
	if !ok {
		return
	}
	days, ok := queryInt(w, r, "days", 30, 7, 180)
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
		writeSaaSError(w, codeInternalError, "retention unavailable")
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	serverID := *ac.scope.ServerID
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()

	fail := func(what string, err error) {
		slog.Warn("component=saas_api", "event", "retention_failed", "what", what, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load retention")
	}
	sum, err := a.Retention.Summary(ctx, serverID, now)
	if err != nil {
		fail("summary", err)
		return
	}
	daily, err := a.Retention.Daily(ctx, serverID, now, days)
	if err != nil {
		fail("daily", err)
		return
	}
	cohorts, err := a.Retention.Cohorts(ctx, serverID, now, 8)
	if err != nil {
		fail("cohorts", err)
		return
	}
	peaks, err := a.Retention.PeakHours(ctx, serverID, now, days, tz)
	if errors.Is(err, repository.ErrInvalidTimeZone) {
		writeSaaSError(w, codeInvalidRequest, "tz must be an IANA time zone name")
		return
	}
	if err != nil {
		fail("peak hours", err)
		return
	}
	shop, err := a.Retention.ShopRevenue(ctx, ac.scope.InstallationID, now.AddDate(0, 0, -days), now, 20)
	if err != nil {
		fail("shop", err)
		return
	}

	resp := retentionResponseDTO{Days: days, TimeZone: tz, Daily: make([]retentionDayDTO, 0, len(daily)), Cohorts: make([]retentionCohortDTO, 0, len(cohorts)),
		PeakHours: make([]peakHourDTO, 0, len(peaks)), Shop: make([]shopRevenueDTO, 0, len(shop))}
	resp.Summary = retentionSummaryDTO{ActiveToday: sum.ActiveToday, Active7d: sum.Active7d, Active30d: sum.Active30d, New7d: sum.New7d, New30d: sum.New30d,
		TrackedPlayers: sum.TrackedPlayers, ObservedHours30d: float64(sum.ObservedSeconds) / 3600}
	if sum.Active30d > 0 {
		v := float64(sum.ActiveToday) / float64(sum.Active30d)
		resp.Summary.Stickiness = &v
	}
	if sum.ObservedSessions > 0 {
		v := float64(sum.ObservedSeconds) / float64(sum.ObservedSessions)
		resp.Summary.AverageSessionSeconds = &v
	}
	if sum.FirstDay != nil {
		d := sum.FirstDay.Format("2006-01-02")
		resp.Summary.CollectingSince = &d
	}
	for _, d := range daily {
		resp.Daily = append(resp.Daily, retentionDayDTO{Day: d.Day.Format("2006-01-02"), Active: d.Active, New: d.New, Returning: d.Returning, ObservedSeconds: d.ObservedSeconds, Sessions: d.Sessions})
	}
	for _, c := range cohorts {
		resp.Cohorts = append(resp.Cohorts, retentionCohortDTO{WeekStart: c.WeekStart.Format("2006-01-02"), Size: c.Size, Retained: c.Retained})
	}
	for _, p := range peaks {
		resp.PeakHours = append(resp.PeakHours, peakHourDTO{Weekday: p.Weekday, Hour: p.Hour, AveragePeak: p.AveragePeak, MaxPeak: p.MaxPeak, Samples: p.Samples})
	}
	for _, s := range shop {
		resp.Shop = append(resp.Shop, shopRevenueDTO{ProductID: s.ProductID, ProductName: s.ProductName, Purchases: s.Purchases, Units: s.Units, Points: s.Points, Buyers: s.Buyers})
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

type lapsedPlayerDTO struct {
	PlayerID        int64  `json:"playerId"`
	PlayerName      string `json:"playerName"`
	LastSeenDay     string `json:"lastSeenDay"`
	DaysSinceSeen   int    `json:"daysSinceSeen"`
	ActiveDays      int    `json:"activeDays"`
	ObservedSeconds int64  `json:"observedSeconds"`
	Kills           int    `json:"kills"`
	Linked          bool   `json:"linked"`
}

// handleRetentionLapsed is GET .../admin/retention/lapsed?minDays=&maxDays=&limit= (RETENTION_VIEW):
// players last seen between minDays and maxDays ago, most invested first.
func (a *App) handleRetentionLapsed(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapRetentionView)
	if !ok {
		return
	}
	minDays, ok := queryInt(w, r, "minDays", 7, 1, 365)
	if !ok {
		return
	}
	maxDays, ok := queryInt(w, r, "maxDays", 30, 1, 365)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 50, 1, 200)
	if !ok {
		return
	}
	if maxDays < minDays {
		writeSaaSError(w, codeInvalidRequest, "maxDays must not be less than minDays")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Retention == nil {
		writeSaaSError(w, codeInternalError, "retention unavailable")
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	players, err := a.Retention.Lapsed(ctx, ac.scope.GuildID, *ac.scope.ServerID, time.Now().UTC(), minDays, maxDays, limit)
	if err != nil {
		slog.Warn("component=saas_api", "event", "retention_failed", "what", "lapsed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load lapsed players")
		return
	}
	resp := struct {
		MinDays int               `json:"minDays"`
		MaxDays int               `json:"maxDays"`
		Items   []lapsedPlayerDTO `json:"items"`
	}{MinDays: minDays, MaxDays: maxDays, Items: make([]lapsedPlayerDTO, 0, len(players))}
	for _, p := range players {
		resp.Items = append(resp.Items, lapsedPlayerDTO{PlayerID: p.PlayerID, PlayerName: p.PlayerName, LastSeenDay: p.LastSeenDay.Format("2006-01-02"),
			DaysSinceSeen: p.DaysSinceSeen, ActiveDays: p.ActiveDays, ObservedSeconds: p.ObservedSeconds, Kills: p.Kills, Linked: p.Linked})
	}
	a.recordAudit(ctx, ac, "RETENTION_LAPSED_VIEWED", "installation", "", "SUCCESS", nil, map[string]int{"minDays": minDays, "maxDays": maxDays, "returned": len(players)})
	writeSaaSJSON(w, http.StatusOK, resp)
}
