// Package app: Champion SaaS customer API.
//
// This file and its saas_api_*.go siblings implement the server-to-server
// HTTP bridge between the Champion website and the authoritative Go SaaS
// repositories (internal/repository/saas_*_repository.go). See
// docs/SAAS_HTTP_API.md for the full route/DTO/auth contract the website
// team should wire against.
//
// Every request goes through the same four-stage chain (section 3 of the
// task this was built from): service auth -> acting user resolution ->
// organization membership validation -> resource authorization. No handler
// skips a stage even if it looks redundant for that specific route.
package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// actingUserHeader carries the website-verified Discord user ID for the
// person making this request. The website has already authenticated this
// person through Auth.js/Discord OAuth on its own side; this header is only
// ever trusted because the request already passed service authentication
// (validSaaSServiceAuth) - a browser can never reach these handlers directly
// (WEBSITE_API_SECRET is never shipped to client JS), so it can never forge
// this header on its own.
const actingUserHeader = "X-Champion-Acting-User"

// --- error contract (section 15) ---------------------------------------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

const (
	codeUnauthorized            = "UNAUTHORIZED"
	codeForbidden               = "FORBIDDEN"
	codeNotFound                = "NOT_FOUND"
	codeConflict                = "CONFLICT"
	codeInvalidRequest          = "INVALID_REQUEST"
	codeDiscordUnavailable      = "DISCORD_UNAVAILABLE"
	codeInstallationNotVerified = "INSTALLATION_NOT_VERIFIED"
	codeInternalError           = "INTERNAL_ERROR"
	// codeNitradoUnavailable mirrors codeDiscordUnavailable's exact pattern
	// for the same reason: a live external dependency (here, the Nitrado
	// API) is unreachable or rejected the request, not a client mistake.
	codeNitradoUnavailable = "NITRADO_UNAVAILABLE"
)

// httpStatusForCode maps an error code to its HTTP status, so every handler
// stays consistent without repeating the mapping inline.
var httpStatusForCode = map[string]int{
	codeUnauthorized:            http.StatusUnauthorized,
	codeForbidden:               http.StatusForbidden,
	codeNotFound:                http.StatusNotFound,
	codeConflict:                http.StatusConflict,
	codeInvalidRequest:          http.StatusBadRequest,
	codeDiscordUnavailable:      http.StatusServiceUnavailable,
	codeInstallationNotVerified: http.StatusUnprocessableEntity,
	codeInternalError:           http.StatusInternalServerError,
	codeNitradoUnavailable:      http.StatusServiceUnavailable,
}

// writeSaaSJSON writes a successful JSON response.
func writeSaaSJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeSaaSError writes the documented {"error":{"code","message"}} shape.
// message is always a fixed, safe, human string - callers must never pass a
// raw error's Error() text (which can carry SQL fragments or internal
// detail) into message; log the real error separately with slog instead.
func writeSaaSError(w http.ResponseWriter, code, message string) {
	status, ok := httpStatusForCode[code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeSaaSJSON(w, status, apiErrorEnvelope{Error: apiError{Code: code, Message: message}})
}

// --- service + acting-user authentication (sections 2-3) ---------------

// requireSaaSServiceAuth verifies the WEBSITE_API_SECRET bearer token,
// reusing the exact constant-time comparison the runtime status API already
// uses (validBearerToken, runtime_status.go). Every SaaS route must call
// this before touching the request further.
func (a *App) requireSaaSServiceAuth(w http.ResponseWriter, r *http.Request) bool {
	if a.Config == nil || a.Config.WebsiteAPISecret == "" || !validBearerToken(r.Header.Get("Authorization"), a.Config.WebsiteAPISecret) {
		writeSaaSError(w, codeUnauthorized, "missing or invalid service authentication")
		return false
	}
	return true
}

// resolveActingUser reads the trusted acting-Discord-user header and
// resolves it to its app_users row. Returns nil (having already written the
// UNAUTHORIZED response) if the header is missing or the Discord account has
// never synced (POST /api/saas/users/sync must run at least once per user -
// see saas_api_users.go).
func (a *App) resolveActingUser(w http.ResponseWriter, r *http.Request) *repository.AppUser {
	discordUserID := strings.TrimSpace(r.Header.Get(actingUserHeader))
	if discordUserID == "" {
		writeSaaSError(w, codeUnauthorized, "missing acting user")
		return nil
	}
	if a.SaaSUsers == nil {
		writeSaaSError(w, codeInternalError, "user directory unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := a.SaaSUsers.GetByDiscordID(ctx, discordUserID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "acting user lookup failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve acting user")
		return nil
	}
	if user == nil {
		writeSaaSError(w, codeUnauthorized, "acting user has not synced")
		return nil
	}
	return user
}

// requireOrganizationMember verifies the acting user belongs to
// organizationID, returning their role. Every organization-scoped route
// must call this (or requireOrganizationRole below) before any repository
// lookup that isn't itself already organization-scoped.
func (a *App) requireOrganizationMember(w http.ResponseWriter, r *http.Request, organizationID, userID int64) (role string, ok bool) {
	if a.SaaSOrganizations == nil {
		writeSaaSError(w, codeInternalError, "organization directory unavailable")
		return "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	role, member, err := a.SaaSOrganizations.VerifyMembership(ctx, organizationID, userID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "membership check failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify membership")
		return "", false
	}
	if !member {
		// Deliberately FORBIDDEN, not NOT_FOUND: this organization ID may
		// well exist, the acting user just isn't a member of it. Returning
		// NOT_FOUND either way (so existence can't be inferred) is also
		// defensible, but FORBIDDEN with a generic message leaks no more
		// than that already, and matches this task's explicit code list.
		writeSaaSError(w, codeForbidden, "not a member of this organization")
		return "", false
	}
	return role, true
}

// requireOrganizationRole is requireOrganizationMember plus an OWNER/ADMIN
// gate, for the mutation endpoints section 8 restricts to those roles
// (MEMBER stays read-only).
func (a *App) requireOrganizationRole(w http.ResponseWriter, r *http.Request, organizationID, userID int64) (role string, ok bool) {
	role, ok = a.requireOrganizationMember(w, r, organizationID, userID)
	if !ok {
		return "", false
	}
	if role != repository.RoleOwner && role != repository.RoleAdmin {
		writeSaaSError(w, codeForbidden, "OWNER or ADMIN role required")
		return "", false
	}
	return role, true
}

// pathInt64 parses a PathValue as a positive int64, writing an
// INVALID_REQUEST response and returning ok=false on failure.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeSaaSError(w, codeInvalidRequest, "invalid "+name)
		return 0, false
	}
	return id, true
}

// --- rate limiting (section 17) -----------------------------------------

// saasRateLimiter is a simple in-memory sliding-window limiter keyed by
// caller identity (the acting Discord user ID). Sized for a single Go
// process, matching this codebase's existing in-memory-state style (e.g.
// killfeed.Deduplicator) - no external dependency needed at this traffic
// scale. Only applied to the specific write-heavy/expensive endpoints
// section 17 names (user sync, organization creation, Discord
// verification); ordinary dashboard reads are never rate limited.
type saasRateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	hits   map[string][]time.Time
}

func newSaaSRateLimiter(window time.Duration, max int) *saasRateLimiter {
	return &saasRateLimiter{window: window, max: max, hits: make(map[string][]time.Time)}
}

// Allow reports whether key may proceed, recording this attempt if so.
func (l *saasRateLimiter) Allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// rateLimitKey prefers the acting-user header (the real caller identity for
// server-to-server traffic); falls back to remote address only for routes
// reached before the acting user is known.
func rateLimitKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(actingUserHeader)); v != "" {
		return v
	}
	return r.RemoteAddr
}

// enforceRateLimit writes a 429-shaped error and returns false when key has
// exceeded limiter's budget.
func enforceRateLimit(w http.ResponseWriter, limiter *saasRateLimiter, key string) bool {
	if limiter == nil || limiter.Allow(key) {
		return true
	}
	writeSaaSJSON(w, http.StatusTooManyRequests, apiErrorEnvelope{Error: apiError{Code: "RATE_LIMITED", Message: "too many requests, slow down"}})
	return false
}

// --- shared DTOs (section 14) -------------------------------------------

// UserSummary is the customer-safe view of an app_users row.
type UserSummary struct {
	ID                int64  `json:"id"`
	DiscordUserID     string `json:"discordUserId"`
	DiscordUsername   string `json:"discordUsername"`
	DiscordGlobalName string `json:"discordGlobalName,omitempty"`
	Avatar            string `json:"avatar,omitempty"`
}

func toUserSummary(u repository.AppUser) UserSummary {
	return UserSummary{
		ID:                u.ID,
		DiscordUserID:     u.DiscordUserID,
		DiscordUsername:   u.DiscordUsername,
		DiscordGlobalName: u.DiscordGlobalName,
		Avatar:            u.Avatar,
	}
}

// nullableTimeStr formats a nullable time.Time as an RFC3339 pointer, or nil.
func nullableTimeStr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// registerSaaSAPI wires every /api/saas/... route onto the app's existing
// HTTP server (section 1: no second listener/port). Called once from New()
// after every dependency each handler reads is already assigned on a.
func (a *App) registerSaaSAPI() {
	if a.HTTPServer == nil {
		return
	}
	if a.saasSyncLimiter == nil {
		a.saasSyncLimiter = newSaaSRateLimiter(time.Minute, 10)
	}
	if a.saasOrgCreateLimiter == nil {
		a.saasOrgCreateLimiter = newSaaSRateLimiter(time.Hour, 5)
	}
	if a.saasDiscordVerifyLimiter == nil {
		a.saasDiscordVerifyLimiter = newSaaSRateLimiter(time.Minute, 10)
	}
	if a.saasNitradoConnectLimiter == nil {
		a.saasNitradoConnectLimiter = newSaaSRateLimiter(time.Hour, 10)
	}
	// Only wire from a.Discord when it's genuinely non-nil: assigning a nil
	// *discord.Client into the discordGuildVerifier interface field would
	// produce a non-nil interface wrapping a nil pointer (the classic Go
	// "nil interface" trap), which would defeat every a.saasDiscordVerifier
	// == nil check in saas_api_discord.go.
	if a.saasDiscordVerifier == nil && a.Discord != nil {
		a.saasDiscordVerifier = a.Discord
	}

	a.HTTPServer.Handle("POST /api/saas/users/sync", a.handleUserSync)

	a.HTTPServer.Handle("GET /api/saas/organizations", a.handleListOrganizations)
	a.HTTPServer.Handle("POST /api/saas/organizations", a.handleCreateOrganization)
	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}", a.handleGetOrganization)
	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}/dashboard", a.handleDashboard)

	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations", a.handleCreateInstallation)
	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}/installations/{installationID}", a.handleGetInstallation)
	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}/installations/{installationID}/setup", a.handleGetSetupProgress)
	a.HTTPServer.Handle("PATCH /api/saas/organizations/{organizationID}/installations/{installationID}/setup", a.handleUpdateSetupProgress)

	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/discord/guilds/eligible", a.handleEligibleGuilds)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/discord/connection", a.handleConnectDiscordGuild)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations/{installationID}/discord/verify-installation", a.handleVerifyInstallation)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations/{installationID}/discord/verify-permissions", a.handleVerifyPermissions)

	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/nitrado/connect", a.handleNitradoConnect)
	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}/nitrado/services", a.handleNitradoServices)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations/{installationID}/dayz-server", a.handleSelectDayZServer)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations/{installationID}/dayz-server/validate", a.handleValidateDayZServer)

	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}/installations/{installationID}/discord/channels", a.handleListDiscordChannels)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations/{installationID}/discord/channels", a.handleCreateDiscordChannel)
	a.HTTPServer.Handle("GET /api/saas/organizations/{organizationID}/installations/{installationID}/channels", a.handleGetChannelSettings)
	a.HTTPServer.Handle("PUT /api/saas/organizations/{organizationID}/installations/{installationID}/channels", a.handleSaveChannelSettings)
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/installations/{installationID}/channels/auto-setup", a.handleAutoSetupChannels)
}
