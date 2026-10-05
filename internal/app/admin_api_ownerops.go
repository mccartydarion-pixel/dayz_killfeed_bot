// Package app: Owner Hub operations pages (docs/OWNER_OPS.md).
//
// Read models for the platform owner - configuration checks, the fleet wall, customer health,
// the onboarding funnel, revenue and the daily briefing - plus the three things the owner can
// change here: the automation switches, broadcasts to customers, and a read-only "view as
// customer" session. Everything is under /api/admin and wrapped by adminRoute; every write
// needs a reason and is recorded in platform_audit_log.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	ownerOpsTimeout = 20 * time.Second
	// viewAsSeconds is how long a "view as customer" session lasts on the website.
	viewAsSeconds = 30 * 60
	// impersonatorHeader names the platform admin behind a "view as customer" request. The
	// website sends it on every request made in such a session; see requireSaaSServiceAuth.
	impersonatorHeader = "X-Champion-Impersonator"
	// stuckAfter is how long an organization may sit on one onboarding step before it is listed.
	stuckAfter = 24 * time.Hour
)

// --- runtime view of one server ---------------------------------------------------------------------

type serverRuntime struct {
	WorkerRunning bool
	SourceState   string
	SourceDetail  string
	ErrorClass    string
	LastCycleAt   *time.Time
}

// serverRuntime reads the in-memory state of one game server's worker. No database, no Nitrado.
func (a *App) serverRuntime(serverID int64, now time.Time) serverRuntime {
	var rt serverRuntime
	if serverID <= 0 || a.WorkerManager == nil {
		return rt
	}
	rt.WorkerRunning = a.WorkerManager.Running(serverID)
	a.presenceMu.Lock()
	eng := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if eng == nil || !rt.WorkerRunning {
		return rt
	}
	h := eng.SourceHealth()
	rt.SourceState, rt.SourceDetail = killfeed.ClassifyADMSourceHealth(h, now)
	rt.ErrorClass = h.LastErrorClass
	if !h.LastCycleAt.IsZero() {
		at := h.LastCycleAt
		rt.LastCycleAt = &at
	}
	return rt
}

// ownerOpsRuntimeReady reports whether worker state can be trusted: the worker manager exists
// and the process has been up long enough for every worker to have started.
func (a *App) ownerOpsRuntimeReady() bool {
	if a.WorkerManager == nil {
		return false
	}
	if a.ownerOpsReady != nil {
		return a.ownerOpsReady()
	}
	return a.HealthRegistry == nil || a.HealthRegistry.Uptime() >= 3*time.Minute
}

func serverState(f repository.FleetFact, rt serverRuntime, runtimeReady bool) ownerops.ServerState {
	return ownerops.ServerState{InstallationStatus: f.Status, ServerActive: f.ServerActive, RuntimeReady: runtimeReady,
		WorkerRunning: rt.WorkerRunning, SourceState: rt.SourceState, SourceErrorClass: rt.ErrorClass, BotInstalled: f.BotInstalled}
}

func (a *App) ownerOpsAvailable(w http.ResponseWriter) bool {
	if a.PlatformOps == nil {
		writeSaaSError(w, codeInternalError, "owner operations unavailable")
		return false
	}
	return true
}

// --- fleet wall -------------------------------------------------------------------------------------

type fleetServerDTO struct {
	InstallationID      int64    `json:"installationId"`
	OrganizationID      int64    `json:"organizationId"`
	Organization        string   `json:"organization"`
	DiscordGuild        string   `json:"discordGuild"`
	Status              string   `json:"status"`
	ServerID            *int64   `json:"serverId"`
	ServerName          string   `json:"serverName"`
	Platform            string   `json:"platform"`
	FeedState           string   `json:"feedState"`
	WorkerRunning       bool     `json:"workerRunning"`
	SourceState         string   `json:"sourceState"`
	SourceDetail        string   `json:"sourceDetail"`
	LastCycleAt         *string  `json:"lastCycleAt"`
	LastKillAt          *string  `json:"lastKillAt"`
	Kills24h            int      `json:"kills24h"`
	PlayersOnline       int      `json:"playersOnline"`
	ActivePlayers7d     int      `json:"activePlayers7d"`
	ActivePlayersPrev7d int      `json:"activePlayersPrev7d"`
	BotInstalled        bool     `json:"botInstalled"`
	NitradoStatus       string   `json:"nitradoStatus"`
	NitradoLastOKAt     *string  `json:"nitradoLastOkAt"`
	NitradoLastFailAt   *string  `json:"nitradoLastFailAt"`
	OpenIncidents       []string `json:"openIncidents"`
}

type fleetSummaryDTO struct {
	Total         int `json:"total"`
	Live          int `json:"live"`
	Quiet         int `json:"quiet"`
	Degraded      int `json:"degraded"`
	Broken        int `json:"broken"`
	SettingUp     int `json:"settingUp"`
	Suspended     int `json:"suspended"`
	PlayersOnline int `json:"playersOnline"`
	Kills24h      int `json:"kills24h"`
	OpenIncidents int `json:"openIncidents"`
}

type fleetView struct {
	Summary fleetSummaryDTO
	Servers []fleetServerDTO
	Facts   []repository.FleetFact
	// FeedByInstallation is each installation's feed state.
	FeedByInstallation map[int64]string
}

// feedRank orders feed states for the wall: what needs the owner first.
var feedRank = map[string]int{ownerops.FeedNoWorker: 0, ownerops.FeedStalled: 1, ownerops.FeedDegraded: 2, ownerops.FeedLive: 3,
	ownerops.FeedQuiet: 4, ownerops.FeedSettingUp: 5, ownerops.FeedSuspended: 6}

func (a *App) buildFleet(ctx context.Context, now time.Time, stats bool) (fleetView, []repository.PlatformIncident, error) {
	view := fleetView{Servers: []fleetServerDTO{}, FeedByInstallation: map[int64]string{}}
	facts, err := a.PlatformOps.FleetFacts(ctx, now, stats)
	if err != nil {
		return view, nil, err
	}
	open, err := a.PlatformOps.OpenIncidents(ctx)
	if err != nil {
		return view, nil, err
	}
	kinds := map[int64][]string{}
	for _, inc := range open {
		kinds[inc.InstallationID] = append(kinds[inc.InstallationID], inc.Kind)
	}
	view.Facts = facts
	view.Summary.OpenIncidents = len(open)
	for _, f := range facts {
		rt := a.serverRuntime(f.ServerID, now)
		state := ownerops.FeedState(serverState(f, rt, a.WorkerManager != nil))
		view.FeedByInstallation[f.InstallationID] = state
		dto := fleetServerDTO{InstallationID: f.InstallationID, OrganizationID: f.OrganizationID, Organization: f.OrganizationName, DiscordGuild: f.GuildName,
			Status: f.Status, ServerName: f.ServerName, Platform: f.Platform, FeedState: state, WorkerRunning: rt.WorkerRunning,
			SourceState: rt.SourceState, SourceDetail: rt.SourceDetail, LastCycleAt: nullableTimeStr(rt.LastCycleAt), LastKillAt: nullableTimeStr(f.LastKillAt),
			Kills24h: f.Kills24h, PlayersOnline: f.PlayersOnline, ActivePlayers7d: f.ActivePlayers7d, ActivePlayersPrev7d: f.ActivePlayersPrev7d,
			BotInstalled: f.BotInstalled, NitradoStatus: f.NitradoStatus, NitradoLastOKAt: nullableTimeStr(f.NitradoLastOKAt),
			NitradoLastFailAt: nullableTimeStr(f.NitradoLastFailAt), OpenIncidents: kinds[f.InstallationID]}
		if dto.OpenIncidents == nil {
			dto.OpenIncidents = []string{}
		}
		if f.ServerID > 0 {
			id := f.ServerID
			dto.ServerID = &id
		}
		view.Servers = append(view.Servers, dto)
		view.Summary.Total++
		view.Summary.PlayersOnline += f.PlayersOnline
		view.Summary.Kills24h += f.Kills24h
		switch state {
		case ownerops.FeedLive:
			view.Summary.Live++
		case ownerops.FeedQuiet:
			view.Summary.Quiet++
		case ownerops.FeedDegraded:
			view.Summary.Degraded++
		case ownerops.FeedNoWorker, ownerops.FeedStalled:
			view.Summary.Broken++
		case ownerops.FeedSettingUp:
			view.Summary.SettingUp++
		case ownerops.FeedSuspended:
			view.Summary.Suspended++
		}
	}
	sort.SliceStable(view.Servers, func(i, j int) bool {
		ri, rj := feedRank[view.Servers[i].FeedState], feedRank[view.Servers[j].FeedState]
		if ri != rj {
			return ri < rj
		}
		return view.Servers[i].InstallationID < view.Servers[j].InstallationID
	})
	return view, open, nil
}

// handleAdminFleet is GET /api/admin/fleet: every installation's server on one screen.
func (a *App) handleAdminFleet(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	now := time.Now().UTC()
	view, _, err := a.buildFleet(ctx, now, true)
	if err != nil {
		a.adminReadFailed(w, "fleet", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"generatedAt": now.Format(time.RFC3339), "runtimeReady": a.WorkerManager != nil,
		"summary": view.Summary, "servers": view.Servers})
}

// --- customer health --------------------------------------------------------------------------------

type customerHealthDTO struct {
	OrganizationID      int64                   `json:"organizationId"`
	Name                string                  `json:"name"`
	Owner               string                  `json:"owner"`
	OwnerDiscordID      string                  `json:"ownerDiscordId"`
	OwnerLastLoginAt    *string                 `json:"ownerLastLoginAt"`
	Plan                string                  `json:"plan"`
	SubscriptionStatus  string                  `json:"subscriptionStatus"`
	TrialEndsAt         *string                 `json:"trialEndsAt"`
	CancelAtPeriodEnd   bool                    `json:"cancelAtPeriodEnd"`
	FeedState           string                  `json:"feedState"`
	ActivePlayers7d     int                     `json:"activePlayers7d"`
	ActivePlayersPrev7d int                     `json:"activePlayersPrev7d"`
	OpenIncidents       int                     `json:"openIncidents"`
	Score               int                     `json:"score"`
	Risk                string                  `json:"risk"`
	Factors             []ownerops.HealthFactor `json:"factors"`
}

// bestFeed picks the healthiest of two feed states: an organization is judged by the server
// that works, not by a spare installation it never finished.
func bestFeed(a, b string) string {
	rank := map[string]int{"": -1, ownerops.FeedSuspended: 0, ownerops.FeedSettingUp: 1, ownerops.FeedNoWorker: 2, ownerops.FeedStalled: 3,
		ownerops.FeedDegraded: 4, ownerops.FeedQuiet: 5, ownerops.FeedLive: 6}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (a *App) buildCustomerHealth(now time.Time, orgs []repository.OrganizationFact, view fleetView, open []repository.PlatformIncident) []customerHealthDTO {
	type agg struct {
		feed           string
		players, prev  int
		openIncidents  int
		hasServerFacts bool
	}
	byOrg := map[int64]*agg{}
	get := func(id int64) *agg {
		if byOrg[id] == nil {
			byOrg[id] = &agg{}
		}
		return byOrg[id]
	}
	for _, f := range view.Facts {
		g := get(f.OrganizationID)
		if f.ServerID > 0 || f.Status == repository.InstallationSuspended {
			g.feed = bestFeed(g.feed, view.FeedByInstallation[f.InstallationID])
		} else if g.feed == "" {
			g.feed = ownerops.FeedSettingUp
		}
		g.players += f.ActivePlayers7d
		g.prev += f.ActivePlayersPrev7d
	}
	for _, inc := range open {
		get(inc.OrganizationID).openIncidents++
	}
	out := make([]customerHealthDTO, 0, len(orgs))
	for _, o := range orgs {
		g := get(o.ID)
		h := ownerops.ScoreHealth(ownerops.HealthInput{Now: now, FeedState: g.feed, ActivePlayers7d: g.players, ActivePlayersPrev7d: g.prev,
			OwnerLastLoginAt: o.OwnerLastLoginAt, SubscriptionStatus: o.Status, TrialEndsAt: o.TrialEndsAt, CancelAtPeriodEnd: o.CancelAtPeriodEnd,
			OpenIncidents: g.openIncidents})
		out = append(out, customerHealthDTO{OrganizationID: o.ID, Name: o.Name, Owner: o.OwnerName, OwnerDiscordID: o.OwnerDiscordID,
			OwnerLastLoginAt: nullableTimeStr(o.OwnerLastLoginAt), Plan: o.Plan, SubscriptionStatus: o.Status, TrialEndsAt: nullableTimeStr(o.TrialEndsAt),
			CancelAtPeriodEnd: o.CancelAtPeriodEnd, FeedState: g.feed, ActivePlayers7d: g.players, ActivePlayersPrev7d: g.prev,
			OpenIncidents: g.openIncidents, Score: h.Score, Risk: h.Risk, Factors: h.Factors})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].OrganizationID < out[j].OrganizationID
	})
	return out
}

// handleAdminCustomerHealth is GET /api/admin/customer-health: every organization scored,
// worst first, with the reasons.
func (a *App) handleAdminCustomerHealth(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	now := time.Now().UTC()
	view, open, err := a.buildFleet(ctx, now, true)
	if err != nil {
		a.adminReadFailed(w, "customer health", err)
		return
	}
	orgs, err := a.PlatformOps.OrganizationFacts(ctx)
	if err != nil {
		a.adminReadFailed(w, "customer health", err)
		return
	}
	items := a.buildCustomerHealth(now, orgs, view, open)
	summary := map[string]int{"healthy": 0, "watch": 0, "atRisk": 0}
	for _, it := range items {
		switch it.Risk {
		case ownerops.RiskAtRisk:
			summary["atRisk"]++
		case ownerops.RiskWatch:
			summary["watch"]++
		default:
			summary["healthy"]++
		}
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"generatedAt": now.Format(time.RFC3339), "summary": summary, "items": items})
}

// --- onboarding funnel ------------------------------------------------------------------------------

type funnelStageDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type funnelStuckDTO struct {
	OrganizationID     int64  `json:"organizationId"`
	Name               string `json:"name"`
	Owner              string `json:"owner"`
	OwnerDiscordID     string `json:"ownerDiscordId"`
	Stage              string `json:"stage"`
	StageLabel         string `json:"stageLabel"`
	NextStep           string `json:"nextStep"`
	CreatedAt          string `json:"createdAt"`
	LastProgressAt     string `json:"lastProgressAt"`
	DaysStuck          int    `json:"daysStuck"`
	IntendedPlan       string `json:"intendedPlan"`
	SubscriptionStatus string `json:"subscriptionStatus"`
}

func progressOf(o repository.OrganizationFact) ownerops.Progress {
	return ownerops.Progress{DiscordConnected: o.DiscordConnected, NitradoConnected: o.NitradoConnected, ServerSelected: o.ServerSelected,
		SetupComplete: o.SetupComplete, HasKill: o.HasKill}
}

// stuckOrganizations lists the organizations that have not reached their first kill and have
// not moved for stuckAfter, most recently active first (the warmest leads).
func stuckOrganizations(now time.Time, orgs []repository.OrganizationFact) []funnelStuckDTO {
	out := []funnelStuckDTO{}
	last := len(ownerops.Stages) - 1
	for _, o := range orgs {
		idx := ownerops.StageIndex(progressOf(o))
		if idx >= last || now.Sub(o.LastProgressAt) < stuckAfter {
			continue
		}
		stage := ownerops.Stages[idx]
		out = append(out, funnelStuckDTO{OrganizationID: o.ID, Name: o.Name, Owner: o.OwnerName, OwnerDiscordID: o.OwnerDiscordID,
			Stage: stage, StageLabel: ownerops.StageLabel(stage), NextStep: ownerops.NextStep(stage),
			CreatedAt: o.CreatedAt.UTC().Format(time.RFC3339), LastProgressAt: o.LastProgressAt.UTC().Format(time.RFC3339),
			DaysStuck: int(now.Sub(o.LastProgressAt).Hours() / 24), IntendedPlan: o.IntendedPlan, SubscriptionStatus: o.Status})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastProgressAt > out[j].LastProgressAt })
	return out
}

// handleAdminFunnel is GET /api/admin/funnel?days=: how far the organizations created in the
// window got, and who is stuck.
func (a *App) handleAdminFunnel(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	days, ok := queryInt(w, r, "days", 30, 1, 365)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -days)
	orgs, err := a.PlatformOps.OrganizationFacts(ctx)
	if err != nil {
		a.adminReadFailed(w, "funnel", err)
		return
	}
	accounts, withoutOrg, err := a.PlatformOps.SignupCounts(ctx, since)
	if err != nil {
		a.adminReadFailed(w, "funnel", err)
		return
	}
	counts := make([]int, len(ownerops.Stages))
	paying, onTrial := 0, 0
	for _, o := range orgs {
		if o.CreatedAt.Before(since) {
			continue
		}
		idx := ownerops.StageIndex(progressOf(o))
		for i := 0; i <= idx; i++ {
			counts[i]++
		}
		if o.Status == repository.SubscriptionActive && o.StripeBilled {
			paying++
		}
		if o.Status == repository.SubscriptionTrial {
			onTrial++
		}
	}
	stages := make([]funnelStageDTO, len(ownerops.Stages))
	for i, key := range ownerops.Stages {
		stages[i] = funnelStageDTO{Key: key, Label: ownerops.StageLabel(key), Count: counts[i]}
	}
	stuck := stuckOrganizations(now, orgs)
	if len(stuck) > 100 {
		stuck = stuck[:100]
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"generatedAt": now.Format(time.RFC3339), "windowDays": days,
		"accounts": accounts, "accountsWithoutOrganization": withoutOrg, "stages": stages, "paying": paying, "onTrial": onTrial, "stuck": stuck})
}

// --- revenue ----------------------------------------------------------------------------------------

type revenuePlanDTO struct {
	Plan        string `json:"plan"`
	Name        string `json:"name"`
	Subscribers int    `json:"subscribers"`
	MRRCents    int64  `json:"mrrCents"`
}

type revenueMonthDTO struct {
	Month       string `json:"month"` // YYYY-MM
	PaidCents   int64  `json:"paidCents"`
	PaidCount   int    `json:"paidCount"`
	FailedCount int    `json:"failedCount"`
}

type revenueView struct {
	Currency        string
	MRRCents        int64
	ByPlan          []revenuePlanDTO
	Addons          []revenuePlanDTO
	Paying          int
	Trial           int
	TrialExpired    int
	PastDue         int
	Canceling       int
	Canceled30d     int
	OwnerGrants     int
	Unpriced        int
	TrialsUsed      int
	TrialsConverted int
	TrialsEnding    []string
}

func (a *App) catalog() *billing.Catalog {
	if a.Billing == nil {
		return nil
	}
	return a.Billing.Catalog()
}

func (a *App) buildRevenue(ctx context.Context, now time.Time, orgs []repository.OrganizationFact) (revenueView, error) {
	v := revenueView{Currency: "usd", ByPlan: []revenuePlanDTO{}, Addons: []revenuePlanDTO{}, TrialsEnding: []string{}}
	cat := a.catalog()
	byPlan := map[string]*revenuePlanDTO{}
	for _, o := range orgs {
		if o.TrialConsumed {
			v.TrialsUsed++
		}
		switch o.Status {
		case repository.SubscriptionTrial:
			v.Trial++
			if o.TrialEndsAt != nil {
				if !o.TrialEndsAt.After(now) {
					v.TrialExpired++
				} else if o.TrialEndsAt.Sub(now) <= 72*time.Hour {
					v.TrialsEnding = append(v.TrialsEnding, o.Name)
				}
			}
		case repository.SubscriptionPastDue:
			v.PastDue++
		case repository.SubscriptionCanceled:
			if o.CanceledAt != nil && now.Sub(*o.CanceledAt) <= 30*24*time.Hour {
				v.Canceled30d++
			}
		}
		if o.Status != repository.SubscriptionActive && o.Status != repository.SubscriptionPastDue {
			continue
		}
		if !o.StripeBilled {
			if o.Status == repository.SubscriptionActive {
				v.OwnerGrants++
			}
			continue
		}
		if o.Status == repository.SubscriptionActive {
			v.Paying++
			if o.TrialConsumed {
				v.TrialsConverted++
			}
			if o.CancelAtPeriodEnd {
				v.Canceling++
			}
		}
		// Past-due subscriptions still count toward MRR until Stripe gives up on them.
		plan, interval, ok := billing.Plan{}, "", false
		if cat != nil {
			plan, interval, ok = cat.PlanForPrice(o.PriceID)
		}
		if !ok {
			v.Unpriced++
			continue
		}
		price := plan.Monthly
		if strings.EqualFold(interval, repository.BillingIntervalYearly) {
			price = plan.Yearly
		}
		if price == nil {
			v.Unpriced++
			continue
		}
		row := byPlan[plan.Key]
		if row == nil {
			row = &revenuePlanDTO{Plan: plan.Key, Name: plan.Name}
			byPlan[plan.Key] = row
		}
		row.Subscribers++
		cents := ownerops.MonthlyCents(price.AmountCents, interval)
		row.MRRCents += cents
		v.MRRCents += cents
		if price.Currency != "" {
			v.Currency = strings.ToLower(price.Currency)
		}
	}
	for _, row := range byPlan {
		v.ByPlan = append(v.ByPlan, *row)
	}
	sort.Slice(v.ByPlan, func(i, j int) bool { return v.ByPlan[i].MRRCents > v.ByPlan[j].MRRCents })

	addons, err := a.PlatformOps.ActiveAddons(ctx)
	if err != nil {
		return v, err
	}
	if len(addons) > 0 && a.Billing != nil {
		prices := map[string]revenuePlanDTO{}
		for _, p := range a.Billing.CasePlans() {
			prices[string(p.Tier)] = revenuePlanDTO{Plan: string(p.Tier), Name: p.Name, MRRCents: ownerops.MonthlyCents(p.AmountCents, p.Interval)}
		}
		for _, ad := range addons {
			row := prices[ad.Tier]
			if row.Plan == "" {
				row = revenuePlanDTO{Plan: ad.Tier, Name: ad.Tier}
			}
			row.Subscribers = ad.Active
			row.MRRCents *= int64(ad.Active)
			v.Addons = append(v.Addons, row)
			v.MRRCents += row.MRRCents
		}
	}
	return v, nil
}

// handleAdminRevenue is GET /api/admin/revenue: recurring revenue from the catalog prices of
// the active Stripe subscriptions, and the payments actually recorded per month.
func (a *App) handleAdminRevenue(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	now := time.Now().UTC()
	orgs, err := a.PlatformOps.OrganizationFacts(ctx)
	if err != nil {
		a.adminReadFailed(w, "revenue", err)
		return
	}
	v, err := a.buildRevenue(ctx, now, orgs)
	if err != nil {
		a.adminReadFailed(w, "revenue", err)
		return
	}
	months, err := a.PlatformOps.PaymentsByMonth(ctx, now, 12)
	if err != nil {
		a.adminReadFailed(w, "revenue", err)
		return
	}
	// One currency is reported: the catalog's, or - with nothing priced yet - the one the
	// newest payment was made in. A second currency would need its own series.
	currency, matched := v.Currency, false
	for _, m := range months {
		matched = matched || m.Currency == currency
	}
	if !matched && len(months) > 0 {
		currency = months[len(months)-1].Currency
		if v.MRRCents == 0 {
			v.Currency = currency
		}
	}
	monthDTOs := []revenueMonthDTO{}
	for _, m := range months {
		if m.Currency != currency {
			continue
		}
		monthDTOs = append(monthDTOs, revenueMonthDTO{Month: m.Month.Format("2006-01"), PaidCents: m.PaidCents, PaidCount: m.PaidCount, FailedCount: m.FailedCount})
	}
	var rate *float64
	if v.TrialsUsed > 0 {
		x := float64(v.TrialsConverted) / float64(v.TrialsUsed)
		rate = &x
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{
		"generatedAt": now.Format(time.RFC3339), "currency": v.Currency, "mrrCents": v.MRRCents, "arrCents": v.MRRCents * 12,
		"byPlan": v.ByPlan, "addons": v.Addons,
		"counts": map[string]int{"paying": v.Paying, "trial": v.Trial, "trialExpired": v.TrialExpired, "pastDue": v.PastDue,
			"canceling": v.Canceling, "canceled30d": v.Canceled30d, "ownerGrants": v.OwnerGrants, "unpriced": v.Unpriced},
		"trialConversion": map[string]any{"trialsUsed": v.TrialsUsed, "converted": v.TrialsConverted, "rate": rate},
		"months":          monthDTOs,
	})
}

// --- configuration check ----------------------------------------------------------------------------

type configSwitchDTO struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"` // the environment variable, or "Owner Hub" for an owner switch
	Detail  string `json:"detail"`
}

type liveInstallationDTO struct {
	OrganizationID int64  `json:"organizationId"`
	Organization   string `json:"organization"`
	InstallationID int64  `json:"installationId"`
	ServerID       int64  `json:"serverId"`
	ServerName     string `json:"serverName"`
	Status         string `json:"status"`
	WorkerRunning  bool   `json:"workerRunning"`
}

func (a *App) configSwitches(settings repository.OwnerOpsSettings) []configSwitchDTO {
	cfg := a.Config
	on := func(b bool, yes, no string) string {
		if b {
			return yes
		}
		return no
	}
	out := []configSwitchDTO{
		{Key: "plan_gating", Label: "Plan gating", Enabled: entitlements.Enforced(), Source: "CHAMPION_PLAN_GATING_ENABLED",
			Detail: on(entitlements.Enforced(), "Survivor organizations are restricted to the base feature set.", "Every plan gets every feature.")},
		{Key: "live_sync_watchers", Label: "Live sync watchers", Enabled: liveSyncWatchersEnabled(), Source: "CHAMPION_LIVE_SYNC_WATCHERS",
			Detail: "Per-family log watchers that feed the live sync diagnostics."},
	}
	if cfg != nil {
		out = append(out,
			configSwitchDTO{Key: "custom_embeds", Label: "Custom embeds", Enabled: cfg.CustomEmbedsEnabled, Source: "CHAMPION_CUSTOM_EMBEDS_ENABLED",
				Detail: "Environment default; an installation's feature flag overrides it."},
			configSwitchDTO{Key: "case_billing", Label: "C.A.S.E. add-on sales", Enabled: cfg.CaseBillingEnabled, Source: "CASE_BILLING_ENABLED", Detail: "Whether C.A.S.E. add-ons can be bought."},
			configSwitchDTO{Key: "case_access", Label: "C.A.S.E. paid access", Enabled: cfg.CaseAccessEnabled, Source: "CASE_ACCESS_ENABLED", Detail: "Whether C.A.S.E. features check for a paid add-on."},
			configSwitchDTO{Key: "discord_presence", Label: "Discord presence", Enabled: cfg.DiscordPresenceEnabled, Source: "DISCORD_PRESENCE_ENABLED", Detail: "The bot's rotating status line."},
		)
	}
	checkout := a.Billing != nil && a.Billing.CheckoutConfigured()
	out = append(out,
		configSwitchDTO{Key: "stripe_checkout", Label: "Stripe checkout", Enabled: checkout, Source: "STRIPE_SECRET_KEY / CHAMPION_BILLING_PLANS_JSON",
			Detail: on(checkout, "Customers can start a paid subscription.", "No Stripe provider is configured; nobody can pay.")},
		configSwitchDTO{Key: "owner_alerts", Label: "Incident alerts to you", Enabled: settings.AlertsEnabled, Source: "Owner Hub", Detail: "DM the platform admins when an incident opens or resolves."},
		configSwitchDTO{Key: "self_heal", Label: "Self-healing", Enabled: settings.SelfHealEnabled, Source: "Owner Hub", Detail: "Restart stalled or missing workers automatically."},
		configSwitchDTO{Key: "customer_notices", Label: "Customer notices", Enabled: settings.CustomerNoticesEnabled, Source: "Owner Hub", Detail: "DM an organization's owner when their setup needs them."},
		configSwitchDTO{Key: "briefing", Label: "Daily briefing", Enabled: settings.BriefingEnabled, Source: "Owner Hub", Detail: "DM the platform admins a summary once a day."},
	)
	for _, def := range featureflags.Catalog {
		if def.Key == featureflags.CustomEmbeds {
			continue
		}
		out = append(out, configSwitchDTO{Key: "flag_" + def.Key, Label: def.Label, Enabled: a.flagEnvSwitch(def.Key), Source: def.EnvVar,
			Detail: "Environment switch; it applies to the installations or servers the environment lists, and an installation's feature flag overrides it."})
	}
	return out
}

// flagEnvSwitch is whether a feature flag's environment switch is on at all (the lists that
// narrow it to particular installations or servers are not evaluated here).
func (a *App) flagEnvSwitch(key string) bool {
	on := func(name string) bool { return strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "true") }
	switch key {
	case featureflags.ShopCanary:
		return a.Config != nil && a.Config.ShopCanaryExecution.Enabled
	case featureflags.CaseEvidence:
		return on("CASE_EVIDENCE_ENABLED")
	case featureflags.CaseBuildEvidence:
		return on("CASE_EVIDENCE_ENABLED") && on("CASE_BUILD_EVIDENCE_ENABLED")
	case featureflags.MapRotation:
		return a.Config != nil && a.Config.MapRotationEnabled
	}
	return false
}

func (a *App) buildConfigChecks(orgs []repository.OrganizationFact, facts []repository.FleetFact, now time.Time) ([]ownerops.Check, []liveInstallationDTO) {
	checks := []ownerops.Check{}
	cat := a.catalog()

	// The catalog itself.
	catalogCheck := ownerops.Check{ID: "catalog_configured", Title: "The plan catalog is configured", Severity: ownerops.SeverityOK, Items: []string{}}
	switch {
	case cat == nil || cat.Empty():
		catalogCheck.Severity, catalogCheck.Detail = ownerops.SeverityWarn, "No plan catalog is loaded (CHAMPION_BILLING_PLANS_JSON), so nothing can be sold and no price can be checked."
	default:
		unpriced := []string{}
		for _, p := range cat.PublicPlans() {
			if p.Monthly == nil && p.Yearly == nil {
				unpriced = append(unpriced, p.Key)
			}
		}
		catalogCheck.Detail = strconv.Itoa(len(cat.Plans())) + " plan(s) in the catalog; every public plan has a price."
		if len(unpriced) > 0 {
			catalogCheck.Severity, catalogCheck.Detail, catalogCheck.Items = ownerops.SeverityWarn, "These public plans have no price, so they are listed but cannot be bought.", unpriced
		}
	}
	checks = append(checks, catalogCheck)

	subs := make([]ownerops.SubscriptionFact, 0, len(orgs))
	survivors, expired := []string{}, []string{}
	for _, o := range orgs {
		if o.Plan == "" && o.Status == "" {
			continue
		}
		subs = append(subs, ownerops.SubscriptionFact{OrganizationID: o.ID, OrganizationName: o.Name, Plan: o.Plan, Status: o.Status,
			StripeBilled: o.StripeBilled && o.Status != repository.SubscriptionCanceled, PriceID: o.PriceID})
		// A platform owner's own organization is never restricted, whatever its plan.
		if !entitlements.OwnerOrganization(o.ID) && strings.EqualFold(o.Plan, entitlements.PlanSurvivor) && (o.Status == repository.SubscriptionActive || o.Status == repository.SubscriptionPastDue) {
			survivors = append(survivors, o.Name+" (#"+strconv.FormatInt(o.ID, 10)+")")
		}
		if o.Status == repository.SubscriptionTrial && o.TrialEndsAt != nil && !o.TrialEndsAt.After(now) {
			expired = append(expired, o.Name+" (#"+strconv.FormatInt(o.ID, 10)+")")
		}
	}
	planForPrice := func(id string) (string, bool) {
		if cat == nil {
			return "", false
		}
		p, _, ok := cat.PlanForPrice(id)
		return p.Key, ok
	}
	knownPlan := func(key string) bool {
		if cat == nil {
			return false
		}
		_, ok := cat.Get(key)
		return ok
	}
	checks = append(checks, ownerops.SubscriptionChecks(subs, planForPrice, knownPlan)...)

	gating := ownerops.Check{ID: "plan_gating_reach", Title: "Who plan gating restricts", Severity: ownerops.SeverityOK, Items: []string{}}
	switch {
	case !entitlements.Enforced():
		gating.Detail = "Plan gating is off: every plan has every feature."
	case len(survivors) == 0:
		gating.Detail = "Plan gating is on, and no organization is on the Survivor plan, so nobody is restricted yet."
	default:
		gating.Severity, gating.Items = ownerops.SeverityInfo, survivors
		gating.Detail = "Plan gating is on. These organizations are on Survivor and do not get the Champion-only features."
	}
	checks = append(checks, gating)

	trials := ownerops.Check{ID: "expired_trials", Title: "Trials past their end date", Severity: ownerops.SeverityOK, Detail: "No trial is past its end date.", Items: []string{}}
	if len(expired) > 0 {
		trials.Severity, trials.Items = ownerops.SeverityInfo, expired
		trials.Detail = "These organizations are still on a trial that has ended; they need to pay or be extended."
	}
	checks = append(checks, trials)

	rows := make([]ownerops.InstallationFact, 0, len(facts))
	live := []liveInstallationDTO{}
	for _, f := range facts {
		running := f.ServerID > 0 && a.WorkerManager != nil && a.WorkerManager.Running(f.ServerID)
		rows = append(rows, ownerops.InstallationFact{InstallationID: f.InstallationID, OrganizationID: f.OrganizationID, OrganizationName: f.OrganizationName,
			Status: f.Status, ServerID: f.ServerID, ServerName: f.ServerName, ServerActive: f.ServerActive, WorkerRunning: running})
		if f.ServerID > 0 && (f.Status == repository.InstallationReady || f.Status == repository.InstallationDegraded) {
			live = append(live, liveInstallationDTO{OrganizationID: f.OrganizationID, Organization: f.OrganizationName, InstallationID: f.InstallationID,
				ServerID: f.ServerID, ServerName: f.ServerName, Status: f.Status, WorkerRunning: running})
		}
	}
	checks = append(checks, ownerops.InstallationChecks(rows, a.ownerOpsRuntimeReady())...)

	admins := ownerops.Check{ID: "platform_admins", Title: "Platform admins", Severity: ownerops.SeverityOK, Items: []string{}}
	n := 0
	if a.Config != nil {
		n = len(a.Config.AdminDiscordIDs)
	}
	admins.Detail = strconv.Itoa(n) + " Discord account(s) on the platform-admin allowlist (CHAMPION_ADMIN_DISCORD_IDS)."
	checks = append(checks, admins)
	return checks, live
}

// handleAdminConfigCheck is GET /api/admin/config-check: which switches are on, and whether the
// catalog, the subscriptions and the installations agree with each other.
func (a *App) handleAdminConfigCheck(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	now := time.Now().UTC()
	settings, _, err := a.PlatformOps.Settings(ctx)
	if err != nil {
		a.adminReadFailed(w, "configuration check", err)
		return
	}
	orgs, err := a.PlatformOps.OrganizationFacts(ctx)
	if err != nil {
		a.adminReadFailed(w, "configuration check", err)
		return
	}
	facts, err := a.PlatformOps.FleetFacts(ctx, now, false)
	if err != nil {
		a.adminReadFailed(w, "configuration check", err)
		return
	}
	checks, live := a.buildConfigChecks(orgs, facts, now)
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"generatedAt": now.Format(time.RFC3339), "worst": ownerops.Worst(checks),
		"switches": a.configSwitches(settings), "checks": checks, "liveInstallations": live})
}

// --- automation settings ----------------------------------------------------------------------------

// readOwnerBody reads an owner write whose body carries more than the shared ownerRequest
// fields. The reason is validated exactly as readOwnerRequest does.
func readOwnerBody[T any](a *App, w http.ResponseWriter, r *http.Request) (reason string, body T, ok bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "could not read request body")
		return "", body, false
	}
	var head struct {
		Reason string `json:"reason"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if json.Unmarshal(raw, &head) != nil || json.Unmarshal(raw, &body) != nil {
			writeSaaSError(w, codeInvalidRequest, "request body must be JSON")
			return "", body, false
		}
	}
	reason = strings.TrimSpace(head.Reason)
	if reason == "" {
		writeSaaSError(w, codeInvalidRequest, "a reason is required for every owner action")
		return "", body, false
	}
	if utf8.RuneCountInString(reason) > ownerReasonMax {
		writeSaaSError(w, codeInvalidRequest, "reason is too long (max 500 characters)")
		return "", body, false
	}
	return reason, body, true
}

func (a *App) automationResponse(ctx context.Context) (map[string]any, error) {
	settings, updatedAt, err := a.PlatformOps.Settings(ctx)
	if err != nil {
		return nil, err
	}
	day, delivered, err := a.PlatformOps.LastBriefing(ctx)
	if err != nil {
		return nil, err
	}
	admins := 0
	if a.Config != nil {
		admins = len(a.Config.AdminDiscordIDs)
	}
	var last any
	if day != nil {
		last = map[string]any{"day": day.Format("2006-01-02"), "delivered": delivered}
	}
	return map[string]any{"settings": settings, "updatedAt": nullableTimeStr(updatedAt), "admins": admins,
		"discordReady": a.ownerOpsCanDM(), "lastBriefing": last}, nil
}

// handleAdminGetAutomation is GET /api/admin/automation: the owner's automation switches.
func (a *App) handleAdminGetAutomation(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	out, err := a.automationResponse(ctx)
	if err != nil {
		a.adminReadFailed(w, "automation settings", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, out)
}

// handleAdminPutAutomation is PUT /api/admin/automation: save the switches. Audited.
func (a *App) handleAdminPutAutomation(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	reason, body, ok := readOwnerBody[repository.OwnerOpsSettings](a, w, r)
	if !ok {
		return
	}
	if err := body.Validate(); err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	before, _, err := a.PlatformOps.Settings(ctx)
	if err != nil {
		ownerFailed(w, "load automation settings", err)
		return
	}
	if err := a.PlatformOps.SaveSettings(ctx, body, admin.DiscordID); err != nil {
		ownerFailed(w, "save automation settings", err)
		return
	}
	a.ownerAudit(ctx, admin, "platform.automation_updated", "platform", 0, nil, reason, "OK", before, body)
	out, err := a.automationResponse(ctx)
	if err != nil {
		a.adminReadFailed(w, "automation settings", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, out)
}

// --- incidents --------------------------------------------------------------------------------------

type incidentDTO struct {
	ID                 int64   `json:"id"`
	InstallationID     int64   `json:"installationId"`
	OrganizationID     int64   `json:"organizationId"`
	Organization       string  `json:"organization"`
	ServerName         string  `json:"serverName"`
	Kind               string  `json:"kind"`
	KindLabel          string  `json:"kindLabel"`
	Status             string  `json:"status"`
	Detail             string  `json:"detail"`
	Healable           bool    `json:"healable"`
	Attempts           int     `json:"attempts"`
	LastAction         string  `json:"lastAction"`
	LastActionAt       *string `json:"lastActionAt"`
	OwnerNotifiedAt    *string `json:"ownerNotifiedAt"`
	CustomerNotifiedAt *string `json:"customerNotifiedAt"`
	DetectedAt         string  `json:"detectedAt"`
	ResolvedAt         *string `json:"resolvedAt"`
	Resolution         string  `json:"resolution"`
}

func toIncidentDTO(p repository.PlatformIncident) incidentDTO {
	return incidentDTO{ID: p.ID, InstallationID: p.InstallationID, OrganizationID: p.OrganizationID, Organization: p.OrganizationName, ServerName: p.ServerName,
		Kind: p.Kind, KindLabel: ownerops.KindLabel(p.Kind), Status: p.Status, Detail: p.Detail, Healable: ownerops.Healable(p.Kind), Attempts: p.Attempts,
		LastAction: p.LastAction, LastActionAt: nullableTimeStr(p.LastActionAt), OwnerNotifiedAt: nullableTimeStr(p.OwnerNotifiedAt),
		CustomerNotifiedAt: nullableTimeStr(p.CustomerNotifiedAt), DetectedAt: p.DetectedAt.UTC().Format(time.RFC3339),
		ResolvedAt: nullableTimeStr(p.ResolvedAt), Resolution: p.Resolution}
}

// handleAdminIncidents is GET /api/admin/incidents?status=OPEN|RESOLVED: newest first.
func (a *App) handleAdminIncidents(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && status != repository.IncidentOpen && status != repository.IncidentResolved {
		a.writeAdminJSON(w, http.StatusOK, map[string]any{"items": []incidentDTO{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	rows, err := a.PlatformOps.ListIncidents(ctx, status, 100)
	if err != nil {
		a.adminReadFailed(w, "incidents", err)
		return
	}
	items := make([]incidentDTO, 0, len(rows))
	for _, p := range rows {
		items = append(items, toIncidentDTO(p))
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleAdminResolveIncident is POST /api/admin/incidents/{incidentID}/resolve: close an
// incident by hand. If the problem is still there the monitor opens a new one.
func (a *App) handleAdminResolveIncident(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "incidentID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	incident, resolved, err := a.PlatformOps.ResolveIncident(ctx, id, "closed by the owner: "+req.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		writeSaaSError(w, codeNotFound, "incident not found")
		return
	}
	if err != nil {
		ownerFailed(w, "resolve incident", err)
		return
	}
	if !resolved {
		writeSaaSError(w, codeConflict, "already in the requested state")
		return
	}
	a.ownerOpsForget(incident.InstallationID, incident.Kind)
	a.ownerAudit(ctx, admin, "incident.resolved", "installation", incident.InstallationID, &incident.OrganizationID, req.Reason, "OK", nil, map[string]any{"incidentId": incident.ID, "kind": incident.Kind})
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"incident": toIncidentDTO(incident)})
}

// --- broadcasts -------------------------------------------------------------------------------------

type broadcastDTO struct {
	ID               int64   `json:"id"`
	Title            string  `json:"title"`
	Body             string  `json:"body"`
	Severity         string  `json:"severity"`
	AudiencePlan     string  `json:"audiencePlan"`
	PostToDiscord    bool    `json:"postToDiscord"`
	StartsAt         string  `json:"startsAt"`
	EndsAt           *string `json:"endsAt"`
	CreatedAt        string  `json:"createdAt"`
	EndedAt          *string `json:"endedAt"`
	Active           bool    `json:"active"`
	DiscordSent      int     `json:"discordSent"`
	DiscordFailed    int     `json:"discordFailed"`
	DiscordNoChannel int     `json:"discordNoChannel"`
}

func toBroadcastDTO(b repository.PlatformBroadcast, now time.Time) broadcastDTO {
	active := b.EndedAt == nil && !b.StartsAt.After(now) && (b.EndsAt == nil || b.EndsAt.After(now))
	return broadcastDTO{ID: b.ID, Title: b.Title, Body: b.Body, Severity: b.Severity, AudiencePlan: b.AudiencePlan, PostToDiscord: b.PostToDiscord,
		StartsAt: b.StartsAt.UTC().Format(time.RFC3339), EndsAt: nullableTimeStr(b.EndsAt), CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
		EndedAt: nullableTimeStr(b.EndedAt), Active: active, DiscordSent: b.DiscordSent, DiscordFailed: b.DiscordFailed, DiscordNoChannel: b.DiscordNoChannel}
}

// handleAdminBroadcasts is GET /api/admin/broadcasts: newest first.
func (a *App) handleAdminBroadcasts(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	rows, err := a.PlatformOps.ListBroadcasts(ctx, 50)
	if err != nil {
		a.adminReadFailed(w, "broadcasts", err)
		return
	}
	now := time.Now().UTC()
	items := make([]broadcastDTO, 0, len(rows))
	for _, b := range rows {
		items = append(items, toBroadcastDTO(b, now))
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"items": items})
}

type broadcastRequest struct {
	Title         string `json:"title"`
	Body          string `json:"body"`
	Severity      string `json:"severity"`
	AudiencePlan  string `json:"audiencePlan"`
	PostToDiscord bool   `json:"postToDiscord"`
	// Days the notice stays on customers' dashboards (1-30); 0 means until ended by hand.
	Days int `json:"days"`
}

// handleAdminCreateBroadcast is POST /api/admin/broadcasts: publish a notice to customers'
// dashboards and, when asked, to each set-up installation's staff alerts channel.
func (a *App) handleAdminCreateBroadcast(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	reason, body, ok := readOwnerBody[broadcastRequest](a, w, r)
	if !ok {
		return
	}
	if body.Days < 0 || body.Days > 30 {
		writeSaaSError(w, codeInvalidRequest, "days must be between 0 and 30")
		return
	}
	now := time.Now().UTC()
	b := repository.PlatformBroadcast{Title: body.Title, Body: body.Body, Severity: body.Severity, AudiencePlan: body.AudiencePlan,
		PostToDiscord: body.PostToDiscord, StartsAt: now, CreatedBy: admin.DiscordID}
	if body.Days > 0 {
		end := now.AddDate(0, 0, body.Days)
		b.EndsAt = &end
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	created, err := a.PlatformOps.CreateBroadcast(ctx, b)
	if errors.Is(err, repository.ErrInvalidBroadcast) {
		writeSaaSError(w, codeInvalidRequest, strings.TrimPrefix(err.Error(), repository.ErrInvalidBroadcast.Error()+": "))
		return
	}
	if err != nil {
		ownerFailed(w, "create broadcast", err)
		return
	}
	a.ownerAudit(ctx, admin, "broadcast.created", "platform", created.ID, nil, reason, "OK", nil,
		map[string]any{"title": created.Title, "severity": created.Severity, "audiencePlan": created.AudiencePlan, "postToDiscord": created.PostToDiscord})
	if created.PostToDiscord {
		go a.deliverBroadcast(context.WithoutCancel(r.Context()), created)
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"broadcast": toBroadcastDTO(created, now)})
}

// handleAdminEndBroadcast is POST /api/admin/broadcasts/{broadcastID}/end: take a notice down.
func (a *App) handleAdminEndBroadcast(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "broadcastID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	b, ended, err := a.PlatformOps.EndBroadcast(ctx, id, admin.DiscordID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeSaaSError(w, codeNotFound, "broadcast not found")
		return
	}
	if err != nil {
		ownerFailed(w, "end broadcast", err)
		return
	}
	if !ended {
		writeSaaSError(w, codeConflict, "already in the requested state")
		return
	}
	a.ownerAudit(ctx, admin, "broadcast.ended", "platform", b.ID, nil, req.Reason, "OK", nil, nil)
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"broadcast": toBroadcastDTO(b, time.Now().UTC())})
}

// handleOrganizationBroadcasts is GET /api/saas/organizations/{organizationID}/broadcasts: the
// notices a member of the organization should see on their dashboard right now.
func (a *App) handleOrganizationBroadcasts(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationMember(w, r, orgID, user.ID); !ok {
		return
	}
	type itemDTO struct {
		ID       int64   `json:"id"`
		Title    string  `json:"title"`
		Body     string  `json:"body"`
		Severity string  `json:"severity"`
		StartsAt string  `json:"startsAt"`
		EndsAt   *string `json:"endsAt"`
	}
	items := []itemDTO{}
	if a.PlatformOps == nil {
		writeSaaSJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// Audience targeting, not a feature gate: a broadcast is addressed by the real plan key.
	plan, _ := a.organizationPlan(ctx, orgID)
	rows, err := a.PlatformOps.ActiveBroadcasts(ctx, plan.Key(), time.Now())
	if err != nil {
		// A notice banner must never break a dashboard: report none.
		writeSaaSJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	for _, b := range rows {
		items = append(items, itemDTO{ID: b.ID, Title: b.Title, Body: b.Body, Severity: b.Severity, StartsAt: b.StartsAt.UTC().Format(time.RFC3339), EndsAt: nullableTimeStr(b.EndsAt)})
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": items})
}

// --- view as customer -------------------------------------------------------------------------------

// handleAdminViewAs is POST /api/admin/organizations/{organizationID}/view-as: start a
// read-only "view as customer" session. It returns the organization owner's Discord id, which
// the website then acts as for reads only; the start is audited with the owner's reason. The
// backend refuses every non-GET request made in such a session (requireSaaSServiceAuth).
func (a *App) handleAdminViewAs(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	discordID, name, orgName, found, err := a.PlatformOps.ViewAsTarget(ctx, orgID)
	if err != nil {
		ownerFailed(w, "start a view-as session", err)
		return
	}
	if !found {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	a.ownerAudit(ctx, admin, "organization.viewed_as", "organization", orgID, &orgID, req.Reason, "OK", nil, map[string]any{"actingAs": name, "seconds": viewAsSeconds})
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"organizationId": orgID, "organizationName": orgName,
		"actingUser": map[string]string{"discordId": discordID, "displayName": name}, "expiresInSeconds": viewAsSeconds})
}

// rejectImpersonatedWrite enforces the read-only rule of a "view as customer" session. A
// request carrying the impersonator header must name a platform admin and must be a read.
func (a *App) rejectImpersonatedWrite(w http.ResponseWriter, r *http.Request) bool {
	admin := strings.TrimSpace(r.Header.Get(impersonatorHeader))
	if admin == "" {
		return false
	}
	if a.Config == nil || !a.Config.IsPlatformAdmin(admin) {
		writeSaaSError(w, codeForbidden, "view-as requires a platform admin")
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeSaaSError(w, codeForbidden, "view-as sessions are read-only")
		return true
	}
	return false
}

// --- briefing ---------------------------------------------------------------------------------------

func (a *App) buildBriefing(ctx context.Context, now time.Time) (ownerops.Briefing, error) {
	b := ownerops.Briefing{Day: now}
	view, open, err := a.buildFleet(ctx, now, true)
	if err != nil {
		return b, err
	}
	orgs, err := a.PlatformOps.OrganizationFacts(ctx)
	if err != nil {
		return b, err
	}
	rev, err := a.buildRevenue(ctx, now, orgs)
	if err != nil {
		return b, err
	}
	since := now.Add(-24 * time.Hour)
	if b.NewUsers24h, _, err = a.PlatformOps.SignupCounts(ctx, since); err != nil {
		return b, err
	}
	if b.PaidCents24h, b.FailedPayments24h, err = a.PlatformOps.PaymentsSince(ctx, since); err != nil {
		return b, err
	}
	if b.ResolvedIncidents, err = a.PlatformOps.ResolvedIncidentsSince(ctx, since); err != nil {
		return b, err
	}
	for _, o := range orgs {
		if !o.CreatedAt.Before(since) {
			b.NewOrganizations++
		}
	}
	b.MRRCents, b.Currency, b.ActiveSubscribers, b.TrialOrganizations, b.TrialsEndingSoon = rev.MRRCents, rev.Currency, rev.Paying, rev.Trial, rev.TrialsEnding
	b.StuckOnboarding = len(stuckOrganizations(now, orgs))
	b.LiveServers, b.BrokenServers = view.Summary.Live+view.Summary.Quiet+view.Summary.Degraded, view.Summary.Broken
	b.PlayersOnline, b.Kills24h = view.Summary.PlayersOnline, view.Summary.Kills24h
	for _, inc := range open {
		b.OpenIncidents = append(b.OpenIncidents, ownerops.KindLabel(inc.Kind)+": "+inc.OrganizationName+" / "+inc.ServerName)
	}
	for _, h := range a.buildCustomerHealth(now, orgs, view, open) {
		if h.Risk == ownerops.RiskAtRisk && (h.SubscriptionStatus == repository.SubscriptionActive || h.SubscriptionStatus == repository.SubscriptionTrial || h.SubscriptionStatus == repository.SubscriptionPastDue) {
			b.AtRisk = append(b.AtRisk, h.Name+" ("+strconv.Itoa(h.Score)+")")
		}
	}
	checks, _ := a.buildConfigChecks(orgs, view.Facts, now)
	for _, c := range checks {
		if c.Severity == ownerops.SeverityWarn || c.Severity == ownerops.SeverityFail {
			b.ConfigProblems++
		}
	}
	return b, nil
}

// handleAdminBriefing is GET /api/admin/briefing: today's briefing as it would be sent now.
func (a *App) handleAdminBriefing(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if !a.ownerOpsAvailable(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerOpsTimeout)
	defer cancel()
	now := time.Now().UTC()
	b, err := a.buildBriefing(ctx, now)
	if err != nil {
		a.adminReadFailed(w, "briefing", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"generatedAt": now.Format(time.RFC3339), "text": ownerops.BriefingText(b), "data": b})
}

// registerOwnerOpsAPI wires the Owner Hub operations routes.
func (a *App) registerOwnerOpsAPI() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	a.adminHandle("GET /api/admin/config-check", a.handleAdminConfigCheck)
	a.adminHandle("GET /api/admin/fleet", a.handleAdminFleet)
	a.adminHandle("GET /api/admin/customer-health", a.handleAdminCustomerHealth)
	a.adminHandle("GET /api/admin/funnel", a.handleAdminFunnel)
	a.adminHandle("GET /api/admin/revenue", a.handleAdminRevenue)
	a.adminHandle("GET /api/admin/briefing", a.handleAdminBriefing)
	a.adminHandle("GET /api/admin/nitrado-usage", a.handleAdminNitradoUsage)
	a.adminHandle("GET /api/admin/automation", a.handleAdminGetAutomation)
	a.adminHandle("PUT /api/admin/automation", a.handleAdminPutAutomation)
	a.adminHandle("GET /api/admin/incidents", a.handleAdminIncidents)
	a.adminHandle("POST /api/admin/incidents/{incidentID}/resolve", a.handleAdminResolveIncident)
	a.adminHandle("GET /api/admin/broadcasts", a.handleAdminBroadcasts)
	a.adminHandle("POST /api/admin/broadcasts", a.handleAdminCreateBroadcast)
	a.adminHandle("POST /api/admin/broadcasts/{broadcastID}/end", a.handleAdminEndBroadcast)
	a.adminHandle("POST /api/admin/organizations/{organizationID}/view-as", a.handleAdminViewAs)
	h("GET /api/saas/organizations/{organizationID}/broadcasts", a.handleOrganizationBroadcasts)
}
