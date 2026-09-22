// Package app: Champion platform-admin (founder) API.
//
// The one HTTP surface allowed to read across organizations. It is READ-ONLY
// (only GET routes are registered, so any other method is a 405 from the mux),
// lives under /api/admin (never /api/saas), and every route is wrapped by
// adminRoute, which runs requirePlatformAdmin before the handler can execute.
// See docs/ADMIN_API.md.
package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/adminrepo"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// adminReader is the cross-tenant read model (implemented by
// *adminrepo.Repository); an interface so handlers are unit-testable.
type adminReader interface {
	Ping(ctx context.Context) error
	Overview(ctx context.Context) (adminrepo.Overview, error)
	ListOrganizations(ctx context.Context, f adminrepo.OrganizationFilter) ([]adminrepo.Organization, int64, error)
	GetOrganization(ctx context.Context, id int64) (*adminrepo.Organization, error)
	ListSubscriptions(ctx context.Context, f adminrepo.SubscriptionFilter) ([]adminrepo.SubscriptionRow, int64, error)
	ListInstallations(ctx context.Context, f adminrepo.InstallationFilter) ([]adminrepo.InstallationSummary, int64, error)
	GetInstallation(ctx context.Context, id int64) (*adminrepo.InstallationDetail, error)
	InstallationHealth(ctx context.Context) (adminrepo.HealthSummary, []adminrepo.HealthInstallation, error)
}

// adminIdentity is the safe identity of an authorized platform admin.
type adminIdentity struct{ DiscordID string }

type adminHandler func(w http.ResponseWriter, r *http.Request, admin adminIdentity)

// requirePlatformAdmin is the single authorization gate for /api/admin. It
// checks, in order:
//
//  1. the internal service secret (the same WEBSITE_API_SECRET bearer every
//     /api/saas route requires)          -> 401 when missing or wrong;
//  2. an acting Discord user id            -> 401 when absent;
//  3. that id on the CHAMPION_ADMIN_DISCORD_IDS allowlist -> 403 otherwise.
//
// Organization roles (OWNER/ADMIN/MEMBER) and Discord guild permissions are
// deliberately never consulted: neither makes anyone a platform admin, and an
// empty allowlist means nobody is (fail closed). It has already written the
// response when it returns ok=false.
func (a *App) requirePlatformAdmin(w http.ResponseWriter, r *http.Request) (adminIdentity, bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return adminIdentity{}, false
	}
	id := strings.TrimSpace(r.Header.Get(actingUserHeader))
	if id == "" {
		writeSaaSError(w, codeUnauthorized, "missing acting user")
		return adminIdentity{}, false
	}
	if a.Config == nil || !a.Config.IsPlatformAdmin(id) {
		slog.Warn("component=admin_api", "event", "admin_denied", "acting_admin_discord_id", clipForLog(id, 40), "route", adminRouteLabel(r))
		writeSaaSError(w, codeForbidden, "platform admin access required")
		return adminIdentity{}, false
	}
	return adminIdentity{DiscordID: id}, true
}

// adminRoute wraps a handler so it cannot run without requirePlatformAdmin and
// so every admin read is logged (event, admin, route, tenant ids - never
// headers, query text or response bodies).
func (a *App) adminRoute(h adminHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("component=admin_api", "event", "admin_panic", "route", adminRouteLabel(r), "panic", fmt.Sprint(rec))
				writeSaaSError(w, codeInternalError, "internal error")
			}
		}()
		admin, ok := a.requirePlatformAdmin(w, r)
		if !ok {
			return
		}
		if a.adminSaaS == nil {
			writeSaaSError(w, codeInternalError, "admin data source unavailable")
			return
		}
		attrs := []any{"event", "admin_read", "acting_admin_discord_id", admin.DiscordID, "route", adminRouteLabel(r)}
		if v := r.PathValue("organizationID"); v != "" {
			attrs = append(attrs, "organization_id", clipForLog(v, 20))
		}
		if v := r.PathValue("installationID"); v != "" {
			attrs = append(attrs, "installation_id", clipForLog(v, 20))
		}
		slog.Info("component=admin_api", attrs...)
		h(w, r, admin)
	}
}

func adminRouteLabel(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.Method + " " + r.URL.Path
}

func clipForLog(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// --- response writing + secret guard ----------------------------------------------------------

// adminSecretValues are configured secrets that must never appear in an admin
// response. Every DTO is built from explicit non-secret columns, so this is a
// backstop, not the primary protection: should a secret ever reach a body (say a
// customer stored one in a guild name), the response is withheld instead of sent.
func (a *App) adminSecretValues() []string {
	var vals []string
	add := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= 8 {
			vals = append(vals, v)
		}
	}
	if a.Config != nil {
		add(a.Config.WebsiteAPISecret)
		add(a.Config.DiscordToken)
		add(a.Config.NitradoToken)
		add(a.Config.CredentialEncryptionKey)
		add(a.Config.DatabaseURL)
		if u, err := url.Parse(a.Config.DatabaseURL); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok {
				add(pw)
			}
		}
	}
	for _, name := range []string{"CHAMPION_SAAS_API_SECRET", "DISCORD_CLIENT_SECRET", "RAILWAY_TOKEN", "DATABASE_URL"} {
		add(os.Getenv(name))
	}
	return vals
}

func (a *App) writeAdminJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("component=admin_api", "event", "admin_marshal_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "internal error")
		return
	}
	for _, secret := range a.adminSecretValues() {
		if bytes.Contains(body, []byte(secret)) {
			slog.Error("component=admin_api", "event", "admin_response_withheld", "reason", "configured secret present in response body")
			writeSaaSError(w, codeInternalError, "response withheld")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// --- list parameters ----------------------------------------------------------------------------

type adminListParams struct {
	Limit  int
	Cursor int64
	Search string
	Query  url.Values
}

const adminCursorPrefix = "adm1:"

func encodeAdminCursor(id int64) *string {
	if id <= 0 {
		return nil
	}
	s := base64.RawURLEncoding.EncodeToString([]byte(adminCursorPrefix + strconv.FormatInt(id, 10)))
	return &s
}

func decodeAdminCursor(s string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), adminCursorPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(string(raw), adminCursorPrefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// parseAdminListParams validates limit/cursor/search. limit defaults to 50; 0 or
// a negative or non-numeric limit is rejected (never "unlimited"); anything above
// 100 is clamped to 100.
func parseAdminListParams(w http.ResponseWriter, r *http.Request) (adminListParams, bool) {
	q := r.URL.Query()
	p := adminListParams{Limit: adminrepo.DefaultLimit, Query: q}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeSaaSError(w, codeInvalidRequest, "limit must be a positive integer (max 100)")
			return p, false
		}
		if n > adminrepo.MaxLimit {
			n = adminrepo.MaxLimit
		}
		p.Limit = n
	}
	if raw := strings.TrimSpace(q.Get("cursor")); raw != "" {
		id, ok := decodeAdminCursor(raw)
		if !ok {
			writeSaaSError(w, codeInvalidRequest, "invalid cursor")
			return p, false
		}
		p.Cursor = id
	}
	if raw := strings.TrimSpace(q.Get("search")); raw != "" {
		if utf8.RuneCountInString(raw) > adminrepo.MaxSearchLen {
			writeSaaSError(w, codeInvalidRequest, "search is too long")
			return p, false
		}
		p.Search = raw
	}
	return p, true
}

// adminEnumParam reads an optional filter validated against the real vocabulary
// (case-insensitive). An unknown value is not an error: no row can have it, so the
// caller answers with an empty page (and never queries) - which keeps a typo in a
// free-text filter box from breaking the whole page.
func adminEnumParam(q url.Values, name string, known func(string) bool) (value string, unknown bool) {
	v := strings.ToUpper(strings.TrimSpace(q.Get(name)))
	if v == "" {
		return "", false
	}
	if !known(v) {
		return "", true
	}
	return v, false
}

// adminPlanParam accepts a plan name: short, letters/digits/underscore/hyphen only.
// Anything else cannot match a plan, so it too means "no rows".
func adminPlanParam(q url.Values) (value string, unknown bool) {
	v := strings.TrimSpace(q.Get("plan"))
	if v == "" {
		return "", false
	}
	if len(v) > 40 {
		return "", true
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", true
		}
	}
	return v, false
}

type adminListResponse[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
	Limit      int     `json:"limit"`
}

func adminList[T any](items []T, next int64, limit int) adminListResponse[T] {
	if items == nil {
		items = []T{}
	}
	return adminListResponse[T]{Items: items, NextCursor: encodeAdminCursor(next), Limit: limit}
}

func (a *App) adminReadFailed(w http.ResponseWriter, what string, err error) {
	// The raw error can carry SQL detail; log it, send a fixed message.
	slog.Warn("component=admin_api", "event", "admin_read_failed", "what", what, "err", err.Error())
	writeSaaSError(w, codeInternalError, "could not load "+what)
}

// --- handlers --------------------------------------------------------------------------------------

func (a *App) handleAdminOverview(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := a.adminSaaS.Overview(ctx)
	if err != nil {
		a.adminReadFailed(w, "overview", err)
		return
	}
	out.BackendStatus = a.backendStatus()
	a.writeAdminJSON(w, http.StatusOK, out)
}

func (a *App) handleAdminListOrganizations(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	p, ok := parseAdminListParams(w, r)
	if !ok {
		return
	}
	f := adminrepo.OrganizationFilter{Limit: p.Limit, Cursor: p.Cursor, Search: p.Search}
	var unknownSub, unknownInst, unknownPlan bool
	f.SubscriptionStatus, unknownSub = adminEnumParam(p.Query, "subscriptionStatus", adminrepo.IsSubscriptionStatus)
	// The website's customer filter sends `status` meaning the installation status.
	instParam := "installationStatus"
	if p.Query.Get(instParam) == "" {
		instParam = "status"
	}
	f.InstallationStatus, unknownInst = adminEnumParam(p.Query, instParam, adminrepo.IsInstallationStatus)
	f.Plan, unknownPlan = adminPlanParam(p.Query)
	if unknownSub || unknownInst || unknownPlan {
		a.writeAdminJSON(w, http.StatusOK, adminList([]adminrepo.Organization{}, 0, p.Limit))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, next, err := a.adminSaaS.ListOrganizations(ctx, f)
	if err != nil {
		a.adminReadFailed(w, "organizations", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, adminList(rows, next, p.Limit))
}

func (a *App) handleAdminGetOrganization(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	id, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	d, err := a.adminSaaS.GetOrganization(ctx, id)
	if err != nil {
		a.adminReadFailed(w, "organization", err)
		return
	}
	if d == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	detail := adminOrganizationDetailDTO{Organization: d, RecentPayments: []adminBillingTransactionDTO{}}
	if a.SaaSSubscriptions != nil {
		rows, _, err := a.SaaSSubscriptions.ListBillingTransactions(ctx, repository.BillingTransactionFilter{OrganizationID: id, Limit: adminRecentPaymentsLimit})
		if err != nil {
			a.adminReadFailed(w, "organization recent payments", err)
			return
		}
		for _, row := range rows {
			detail.RecentPayments = append(detail.RecentPayments, adminBillingPaymentDTO(row))
		}
	}
	a.writeAdminJSON(w, http.StatusOK, detail)
}

func (a *App) handleAdminListSubscriptions(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	p, ok := parseAdminListParams(w, r)
	if !ok {
		return
	}
	f := adminrepo.SubscriptionFilter{Limit: p.Limit, Cursor: p.Cursor, Search: p.Search}
	var unknownStatus, unknownPlan bool
	f.Status, unknownStatus = adminEnumParam(p.Query, "status", adminrepo.IsSubscriptionStatus)
	f.Plan, unknownPlan = adminPlanParam(p.Query)
	if unknownStatus || unknownPlan {
		a.writeAdminJSON(w, http.StatusOK, adminList([]adminrepo.SubscriptionRow{}, 0, p.Limit))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, next, err := a.adminSaaS.ListSubscriptions(ctx, f)
	if err != nil {
		a.adminReadFailed(w, "subscriptions", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, adminList(rows, next, p.Limit))
}

func (a *App) handleAdminListInstallations(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	p, ok := parseAdminListParams(w, r)
	if !ok {
		return
	}
	f := adminrepo.InstallationFilter{Limit: p.Limit, Cursor: p.Cursor, Search: p.Search}
	var unknownStatus, unknownHealth bool
	f.Status, unknownStatus = adminEnumParam(p.Query, "status", adminrepo.IsInstallationStatus)
	f.Health, unknownHealth = adminEnumParam(p.Query, "health", adminrepo.IsHealth)
	if raw := strings.TrimSpace(p.Query.Get("organizationId")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeSaaSError(w, codeInvalidRequest, "invalid organizationId")
			return
		}
		f.OrganizationID = id
	}
	if unknownStatus || unknownHealth {
		a.writeAdminJSON(w, http.StatusOK, adminList([]adminrepo.InstallationSummary{}, 0, p.Limit))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, next, err := a.adminSaaS.ListInstallations(ctx, f)
	if err != nil {
		a.adminReadFailed(w, "installations", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, adminList(rows, next, p.Limit))
}

func (a *App) handleAdminGetInstallation(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	id, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	d, err := a.adminSaaS.GetInstallation(ctx, id)
	if err != nil {
		a.adminReadFailed(w, "installation", err)
		return
	}
	if d == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	// Channel names are not stored; fill them from the Discord cache when the bot
	// already knows the channel (no live Discord call, never per-row lookups).
	for i := range d.ChannelRoutes {
		if name := a.adminChannelName(d.ChannelRoutes[i].ChannelID); name != "" {
			n := name
			d.ChannelRoutes[i].ChannelName = &n
		}
	}
	a.writeAdminJSON(w, http.StatusOK, d)
}

// adminChannelName resolves a channel id from the Discord state cache only.
func (a *App) adminChannelName(channelID string) string {
	if a.adminChannelNames != nil {
		return a.adminChannelNames(channelID)
	}
	if a.Discord == nil {
		return ""
	}
	s := a.Discord.Session()
	if s == nil || s.State == nil {
		return ""
	}
	ch, err := s.State.Channel(channelID)
	if err != nil || ch == nil {
		return ""
	}
	return ch.Name
}

type adminComponent struct {
	Name                string  `json:"name"`
	State               string  `json:"state"`
	Critical            bool    `json:"critical"`
	ConsecutiveFailures int     `json:"consecutiveFailures"`
	LastSuccessAt       *string `json:"lastSuccessAt"`
	LastFailureAt       *string `json:"lastFailureAt"`
}

type adminWorker struct {
	Name            string  `json:"name"`
	State           string  `json:"state"`
	Running         bool    `json:"running"`
	StartedAt       string  `json:"startedAt"`
	LastHeartbeatAt string  `json:"lastHeartbeatAt"`
	LastSuccessAt   *string `json:"lastSuccessAt"`
	LastErrorAt     *string `json:"lastErrorAt"`
}

type adminBackendHealth struct {
	Overall       string           `json:"overall"`
	UptimeSeconds int64            `json:"uptimeSeconds"`
	Components    []adminComponent `json:"components"`
	Workers       []adminWorker    `json:"workers"`
	WorkerTotal   int              `json:"workerTotal"`
}

// adminHealthResponse is the website's AdminHealth (backendStatus + the
// installations needing attention) plus the structured runtime/database/summary.
type adminHealthResponse struct {
	BackendStatus string                         `json:"backendStatus"`
	Installations []adminrepo.HealthInstallation `json:"installations"`
	GeneratedAt   string                         `json:"generatedAt"`
	Backend       adminBackendHealth             `json:"backend"`
	Database      map[string]bool                `json:"database"`
	Discord       map[string]bool                `json:"discord"`
	Summary       adminrepo.HealthSummary        `json:"summary"`
	// EmbedRender: custom embed template rollout state and cumulative counters (no
	// player names, no template contents).
	EmbedRender adminEmbedRender `json:"embedRender"`
	// Performance: DB pool/query and route-cache counters (Champion Performance Phase 1).
	Performance adminPerformance `json:"performance"`
}

type adminEmbedRender struct {
	Enabled bool `json:"enabled"`
	embedrender.Stats
}

// adminPerformance is the internal-only performance snapshot (Champion
// Performance Phase 1, section 35 "observability summary") - never exposed
// on any customer-facing surface, only here behind requirePlatformAdmin. All
// figures are cumulative, in-process counters (reset on restart), not a
// time-windowed rate, so two consecutive reads a known interval apart are
// how an operator derives a rate.
type adminPerformance struct {
	Database adminDatabasePerformance `json:"database"`
	Routing  adminRoutingPerformance  `json:"routing"`
}

type adminDatabasePerformance struct {
	PoolTotalConns        int32   `json:"poolTotalConns"`
	PoolIdleConns         int32   `json:"poolIdleConns"`
	PoolMaxConns          int32   `json:"poolMaxConns"`
	PoolAcquiredConns     int32   `json:"poolAcquiredConns"`
	PoolAcquireCount      int64   `json:"poolAcquireCount"`
	PoolEmptyAcquireCount int64   `json:"poolEmptyAcquireCount"` // acquires that had to wait for a free connection
	PoolAcquireDurationMS int64   `json:"poolAcquireDurationMs"` // cumulative time every acquire has spent waiting
	QueryTotal            int64   `json:"queryTotal"`
	QuerySlow             int64   `json:"querySlow"` // at/above SLOW_QUERY_THRESHOLD_MS
	QueryAvgDurationMS    float64 `json:"queryAvgDurationMs"`
}

type adminRoutingPerformance struct {
	CacheHits    int64   `json:"cacheHits"`
	CacheMisses  int64   `json:"cacheMisses"`
	CacheHitRate float64 `json:"cacheHitRate"`
	CacheEntries int     `json:"cacheEntries"`
}

// performanceSnapshot reads the in-process counters this phase added
// (internal/database's query tracer + pgxpool.Stat(), internal/routing's
// resolver hit/miss counters) - never a query of its own, so calling it adds
// no additional database load.
func (a *App) performanceSnapshot() adminPerformance {
	var perf adminPerformance
	if a.DB != nil {
		if ext, ok := a.DB.ExtendedPoolStats(); ok {
			perf.Database.PoolTotalConns, perf.Database.PoolIdleConns, perf.Database.PoolMaxConns = ext.TotalConns, ext.IdleConns, ext.MaxConns
			perf.Database.PoolAcquiredConns = ext.AcquiredConns
			perf.Database.PoolAcquireCount, perf.Database.PoolEmptyAcquireCount = ext.AcquireCount, ext.EmptyAcquireCount
			perf.Database.PoolAcquireDurationMS = ext.AcquireDuration.Milliseconds()
		}
		qs := a.DB.QueryStats()
		perf.Database.QueryTotal, perf.Database.QuerySlow, perf.Database.QueryAvgDurationMS = qs.Total, qs.Slow, qs.AvgDurationMS
	}
	if a.ChannelRoutes != nil {
		rs := a.ChannelRoutes.Stats()
		perf.Routing.CacheHits, perf.Routing.CacheMisses, perf.Routing.CacheEntries = rs.Hits, rs.Misses, rs.Entries
		perf.Routing.CacheHitRate = rs.HitRate()
	}
	return perf
}

// backendStatus is the runtime health registry's overall state ("UNKNOWN" when the
// registry is not available). No invented values.
func (a *App) backendStatus() string {
	if a.HealthRegistry == nil {
		return "UNKNOWN"
	}
	return string(a.HealthRegistry.Snapshot().Overall)
}

// handleAdminHealth reports stored/in-memory state only: no live Nitrado call,
// no per-row Discord call, and no invented percentages. Free-text error messages
// from components and workers are intentionally omitted.
func (a *App) handleAdminHealth(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp := adminHealthResponse{GeneratedAt: time.Now().UTC().Format(time.RFC3339), BackendStatus: a.backendStatus()}

	resp.Backend = adminBackendHealth{Overall: resp.BackendStatus, Components: []adminComponent{}, Workers: []adminWorker{}}
	if a.HealthRegistry != nil {
		snap := a.HealthRegistry.Snapshot()
		resp.Backend.UptimeSeconds = int64(a.HealthRegistry.Uptime().Seconds())
		for _, c := range snap.Components {
			resp.Backend.Components = append(resp.Backend.Components, adminComponent{
				Name: c.Name, State: string(c.State), Critical: c.Critical, ConsecutiveFailures: c.ConsecutiveFailures,
				LastSuccessAt: nullableTimeStr(c.LastSuccessAt), LastFailureAt: nullableTimeStr(c.LastFailureAt),
			})
		}
	}
	if a.Workers != nil {
		workers := a.Workers.Snapshot(30 * time.Second)
		resp.Backend.WorkerTotal = len(workers)
		for i, wk := range workers {
			if i >= 200 {
				break
			}
			resp.Backend.Workers = append(resp.Backend.Workers, adminWorker{
				Name: wk.Name, State: string(wk.State), Running: wk.Running,
				StartedAt: wk.StartedAt.UTC().Format(time.RFC3339), LastHeartbeatAt: wk.LastHeartbeatAt.UTC().Format(time.RFC3339),
				LastSuccessAt: nullableTimeStr(wk.LastSuccessAt), LastErrorAt: nullableTimeStr(wk.LastErrorAt),
			})
		}
	}

	resp.Database = map[string]bool{"ok": a.adminSaaS.Ping(ctx) == nil}
	resp.Discord = map[string]bool{"configured": a.Discord != nil, "ready": false}
	if a.Discord != nil {
		if s := a.Discord.Session(); s != nil {
			resp.Discord["ready"] = s.DataReady
		}
	}

	summary, items, err := a.adminSaaS.InstallationHealth(ctx)
	if err != nil {
		a.adminReadFailed(w, "installation health", err)
		return
	}
	resp.Summary, resp.Installations = summary, items
	resp.EmbedRender = adminEmbedRender{Enabled: a.EmbedRenderer.Enabled(), Stats: a.EmbedRenderer.Stats()}
	resp.Performance = a.performanceSnapshot()
	a.writeAdminJSON(w, http.StatusOK, resp)
}

// registerAdminAPI wires GET /api/admin/... . Only GET is registered: anything
// else is answered 405 by the mux, so Phase 1 has no write surface at all.
func (a *App) registerAdminAPI() {
	if a.HTTPServer == nil {
		return
	}
	a.HTTPServer.Handle("GET /api/admin/overview", a.adminRoute(a.handleAdminOverview))
	a.HTTPServer.Handle("GET /api/admin/organizations", a.adminRoute(a.handleAdminListOrganizations))
	a.HTTPServer.Handle("GET /api/admin/organizations/{organizationID}", a.adminRoute(a.handleAdminGetOrganization))
	a.HTTPServer.Handle("GET /api/admin/subscriptions", a.adminRoute(a.handleAdminListSubscriptions))
	a.HTTPServer.Handle("GET /api/admin/installations", a.adminRoute(a.handleAdminListInstallations))
	a.HTTPServer.Handle("GET /api/admin/installations/{installationID}", a.adminRoute(a.handleAdminGetInstallation))
	a.HTTPServer.Handle("GET /api/admin/health", a.adminRoute(a.handleAdminHealth))
	a.registerAdminBillingRoutes()
}
