package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Client Admin Control Plane Phase 1 (docs/CLIENT_ADMIN.md): the capability-based tenant
// administration permission system. This file holds the actor-resolution middleware every new
// admin capability route in saas_api_client_admin.go calls through, plus the permissions
// management API itself (list/set/delete Discord-role -> Champion-level mappings).
//
// Deliberately separate from three other, already-existing authorization concepts:
//   - Champion's own platform-admin allowlist (admin_api.go's requirePlatformAdmin) - gates
//     Champion's own staff, never a Client's admins.
//   - organization_members' flat OWNER/ADMIN/MEMBER role - still governs the onboarding/billing/
//     settings routes built in earlier phases; untouched by this phase.
//   - Nothing here grants a capability just because someone holds an org ADMIN/MEMBER role -
//     every Level above the organization-owner bootstrap (below) requires an explicit Discord
//     role mapping a Client configures themselves.

const (
	codeAdminForbidden          = "ADMIN_FORBIDDEN"
	codeAdminEscalationDenied   = "ADMIN_ESCALATION_DENIED"
	codeAdminConfirmationNeeded = "ADMIN_CONFIRMATION_REQUIRED"
	codeAdminDiscordUnavailable = "ADMIN_DISCORD_UNAVAILABLE"

	adminTimeout = 10 * time.Second
	// discordRoleCacheTTL bounds how long an actor's fetched Discord roles are reused across a
	// short burst of admin actions before a fresh live lookup runs again - long enough to avoid
	// one Discord REST round trip per action in a rapid sequence, short enough that a role
	// change in Discord (e.g. a demotion) takes effect for this actor within seconds, not
	// minutes.
	discordRoleCacheTTL = 15 * time.Second
)

func init() {
	httpStatusForCode[codeAdminForbidden] = http.StatusForbidden
	httpStatusForCode[codeAdminEscalationDenied] = http.StatusForbidden
	httpStatusForCode[codeAdminConfirmationNeeded] = http.StatusConflict
	httpStatusForCode[codeAdminDiscordUnavailable] = http.StatusServiceUnavailable
}

type discordRoleCacheEntry struct {
	roles     []string
	expiresAt time.Time
}

// memberRolesCached wraps a.saasDiscordVerifier.MemberRoles with the short TTL cache described
// above, keyed by guildID+userID.
func (a *App) memberRolesCached(guildID, discordUserID string) ([]string, error) {
	key := guildID + "|" + discordUserID
	a.discordRoleCacheMu.Lock()
	if e, ok := a.discordRoleCache[key]; ok && time.Now().Before(e.expiresAt) {
		a.discordRoleCacheMu.Unlock()
		return e.roles, nil
	}
	a.discordRoleCacheMu.Unlock()

	if a.saasDiscordVerifier == nil {
		return nil, errors.New("discord unavailable")
	}
	roles, err := a.saasDiscordVerifier.MemberRoles(guildID, discordUserID)
	if err != nil {
		return nil, err
	}
	a.discordRoleCacheMu.Lock()
	if a.discordRoleCache == nil {
		a.discordRoleCache = make(map[string]discordRoleCacheEntry)
	}
	a.discordRoleCache[key] = discordRoleCacheEntry{roles: roles, expiresAt: time.Now().Add(discordRoleCacheTTL)}
	a.discordRoleCacheMu.Unlock()
	return roles, nil
}

// actorLevel resolves user's effective permissions.Level for scope: the organization's owner is
// always bootstrapped to LevelOwner (task's implicit requirement that SOME actor can configure
// the very first Discord-role mapping - see this file's package doc comment); every other Level
// comes only from an explicit installation_role_permissions mapping matching one of the actor's
// current live Discord roles in that installation's guild. If several mapped roles disagree, the
// HIGHEST mapped Level wins (a Client who is both "Moderator" and "Admin" role-wise gets
// Administrator, not the lower one).
// actorLevel also returns the live Discord role IDs it resolved (empty/nil when the organization-
// owner bootstrap short-circuited before any Discord lookup was needed) - Part 2 of Client Admin
// Control Plane Phase 1 (docs/CLIENT_ADMIN.md "Current Actor Client Admin Permissions") surfaces
// these to the website "only if already readily available", which this is.
func (a *App) actorLevel(ctx context.Context, scope repository.AdminScope, user *repository.AppUser) (level permissions.Level, discordRoleIDs []string, err error) {
	if a.SaaSOrganizations != nil {
		org, err := a.SaaSOrganizations.GetByID(ctx, scope.OrganizationID)
		if err != nil {
			return permissions.LevelNone, nil, err
		}
		if org != nil && org.OwnerUserID == user.ID {
			return permissions.LevelOwner, nil, nil
		}
	}
	if a.Permissions == nil {
		return permissions.LevelNone, nil, nil
	}
	roles, err := a.memberRolesCached(scope.DiscordGuildID, user.DiscordUserID)
	if err != nil {
		return permissions.LevelNone, nil, err
	}
	if len(roles) == 0 {
		return permissions.LevelNone, roles, nil
	}
	levelNames, err := a.Permissions.LevelsForRoles(ctx, scope.InstallationID, roles)
	if err != nil {
		return permissions.LevelNone, roles, err
	}
	best := permissions.LevelNone
	for _, name := range levelNames {
		if l, ok := permissions.ParseLevel(name); ok && l > best {
			best = l
		}
	}
	return best, roles, nil
}

// adminActor bundles the outcome of the standard admin-route preamble: service auth, acting
// user, path ids, scope resolution, and the actor's resolved Level (+ the Discord roles that
// resolution used) - so every capability handler in saas_api_client_admin.go starts from the
// same known-good state.
type adminActor struct {
	user           *repository.AppUser
	scope          repository.AdminScope
	level          permissions.Level
	discordRoleIDs []string
}

// resolveAdminActor runs the standard preamble (service auth, acting user, path ids, scope
// resolution, Level resolution) shared by requireCapability and handleClientAdminMe - everything
// except the final capability gate itself, which differs between "must have this one capability"
// and "report whatever Level/capabilities the actor has, even none". On failure it has already
// written the appropriate error response.
func (a *App) resolveAdminActor(w http.ResponseWriter, r *http.Request) (ac adminActor, ok bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, good := pathInt64(w, r, "organizationID")
	if !good {
		return
	}
	instID, good := pathInt64(w, r, "installationID")
	if !good {
		return
	}
	if a.ClientAdmin == nil {
		writeSaaSError(w, codeInternalError, "admin control plane unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	scope, err := a.ClientAdmin.Scope(ctx, orgID, instID)
	if errors.Is(err, repository.ErrInstallationScopeNotFound) {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "msg", "admin scope resolution failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve installation")
		return
	}
	level, roles, err := a.actorLevel(ctx, scope, user)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "actor level resolution failed", "err", err.Error())
		writeSaaSError(w, codeAdminDiscordUnavailable, "could not verify Discord roles")
		return
	}
	return adminActor{user: user, scope: scope, level: level, discordRoleIDs: roles}, true
}

// requireCapability is resolveAdminActor plus the capability gate every mutation/read route in
// saas_api_client_admin.go needs - a missing capability writes ADMIN_FORBIDDEN and returns ok=false.
func (a *App) requireCapability(w http.ResponseWriter, r *http.Request, capability permissions.Capability) (ac adminActor, ok bool) {
	ac, ok = a.resolveAdminActor(w, r)
	if !ok {
		return adminActor{}, false
	}
	if !permissions.Allows(ac.level, capability) {
		writeSaaSError(w, codeAdminForbidden, "missing required permission: "+string(capability))
		return adminActor{}, false
	}
	return ac, true
}

// recordAudit writes one admin_audit_log row, best-effort (task: the action has already
// happened - a failed audit write must never itself be treated as the action failing). Callers
// pass already-sanitized before/after snapshots; never a secret.
func (a *App) recordAudit(ctx context.Context, ac adminActor, action, target, reason, result string, before, after any) {
	if a.AdminAudit == nil {
		return
	}
	entry := repository.AuditEntry{
		OrganizationID: ac.scope.OrganizationID,
		InstallationID: &ac.scope.InstallationID,
		ActorUserID:    &ac.user.ID,
		ActorDiscordID: ac.user.DiscordUserID,
		Action:         action,
		Target:         target,
		Reason:         reason,
		Result:         result,
	}
	if before != nil {
		if b, err := json.Marshal(before); err == nil {
			entry.BeforeState = b
		}
	}
	if after != nil {
		if b, err := json.Marshal(after); err == nil {
			entry.AfterState = b
		}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.AdminAudit.Record(writeCtx, entry); err != nil {
		slog.Warn("component=saas_api", "msg", "audit log write failed", "action", action, "err", err.Error())
	}
}

// requireConfirmation enforces task's "APPROVAL / CONFIRMATION TIERS" for HIGH RISK actions: the
// request body must include a typed confirmation string matching expected exactly (case
// sensitive) - never just a boolean the frontend could default to true. Returns false (and has
// already written ADMIN_CONFIRMATION_REQUIRED) when missing/mismatched.
func requireConfirmation(w http.ResponseWriter, got, expected string) bool {
	if strings.TrimSpace(got) == expected {
		return true
	}
	writeSaaSError(w, codeAdminConfirmationNeeded, "this action requires typed confirmation: \""+expected+"\"")
	return false
}

// --- permissions management API (task's "PERMISSIONS MANAGEMENT") ------------------------------

type rolePermissionDTO struct {
	ID            int64  `json:"id"`
	DiscordRoleID string `json:"discordRoleId"`
	Level         string `json:"level"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func toRolePermissionDTO(p repository.RolePermission) rolePermissionDTO {
	return rolePermissionDTO{
		ID:            p.ID,
		DiscordRoleID: p.DiscordRoleID,
		Level:         p.Level,
		CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (a *App) registerClientAdminRoutes() {
	if a.saasAdminActionLimiter == nil {
		a.saasAdminActionLimiter = newSaaSRateLimiter(time.Minute, 20)
	}
	if a.saasAdminReadLimiter == nil {
		a.saasAdminReadLimiter = newSaaSRateLimiter(time.Minute, 120)
	}
	if a.saasServerRestartLimiter == nil {
		a.saasServerRestartLimiter = newSaaSRateLimiter(5*time.Minute, 1)
	}
	if a.saasServerStopLimiter == nil {
		a.saasServerStopLimiter = newSaaSRateLimiter(time.Minute, 1)
	}
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/admin"
	h := a.HTTPServer.Handle
	h("GET "+base+"/me", a.handleClientAdminMe)
	h("GET "+base+"/permissions", a.handleListPermissions)
	h("PUT "+base+"/permissions/{discordRoleID}", a.handleSetPermission)
	h("DELETE "+base+"/permissions/{mappingID}", a.handleDeletePermission)
	h("GET "+base+"/audit-log", a.handleListAuditLog)

	a.registerClientAdminCapabilityRoutes(base)
}

// clientAdminMeResponse is the current actor's resolved Champion bot permission Level and exact
// capability set for the selected installation (Client Admin Control Plane Phase 1 Part 2,
// docs/CLIENT_ADMIN.md "Current Actor Client Admin Permissions"). These are Champion BOT
// permission levels driving the website's Client Server Admin UI - NOT Champion Platform Owner
// roles and NOT organization billing roles (organization_members' OWNER/ADMIN/MEMBER), which
// remain entirely separate and are never reported here.
type clientAdminMeResponse struct {
	Level          string                   `json:"level"`
	Capabilities   []permissions.Capability `json:"capabilities"`
	DiscordRoleIDs []string                 `json:"discordRoleIds,omitempty"`
}

// handleClientAdminMe is GET .../admin/me. Unlike every other route in this file it requires no
// specific capability - its entire job is to REPORT whatever Level (possibly none) the acting
// user resolves to, so the website can decide what to show. An actor with no mapped Level still
// gets a normal, informative 403 ADMIN_FORBIDDEN (never a fabricated "safe" 200 with an empty
// Level - task section 3 leaves the choice open, but the website's own error-handling section
// (Part 2 section 21) expects exactly this response for "no permission").
func (a *App) handleClientAdminMe(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.resolveAdminActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if ac.level == permissions.LevelNone {
		writeSaaSError(w, codeAdminForbidden, "no Champion bot permission level is mapped for this user on this installation")
		return
	}
	writeSaaSJSON(w, http.StatusOK, clientAdminMeResponse{
		Level:          ac.level.String(),
		Capabilities:   permissions.CapabilitiesForLevel(ac.level),
		DiscordRoleIDs: ac.discordRoleIDs,
	})
}

// handleListPermissions is GET .../admin/permissions (PERMISSIONS_VIEW).
func (a *App) handleListPermissions(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPermissionsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Permissions.List(ctx, ac.scope.InstallationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list permissions failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list permissions")
		return
	}
	out := make([]rolePermissionDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, toRolePermissionDTO(p))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out, "actorLevel": ac.level.String()})
}

type setPermissionRequest struct {
	Level string `json:"level"`
}

// handleSetPermission is PUT .../admin/permissions/{discordRoleID} (PERMISSIONS_MANAGE). Enforces
// the escalation ceiling (task's "PERMISSION ESCALATION SAFETY"): the actor can never grant a
// level above their own resolved Level, checked server-side via permissions.CanGrant - the
// request body's own claimed level is never trusted on its own.
func (a *App) handleSetPermission(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPermissionsManage)
	if !ok {
		return
	}
	discordRoleID := strings.TrimSpace(r.PathValue("discordRoleID"))
	if discordRoleID == "" {
		writeSaaSError(w, codeInvalidRequest, "discordRoleID is required")
		return
	}
	var req setPermissionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	targetLevel, validLevel := permissions.ParseLevel(strings.ToUpper(strings.TrimSpace(req.Level)))
	if !validLevel {
		writeSaaSError(w, codeInvalidRequest, "level must be one of OWNER, ADMINISTRATOR, MODERATOR, GATEKEEPER")
		return
	}
	if !permissions.CanGrant(ac.level, targetLevel) {
		writeSaaSError(w, codeAdminEscalationDenied, "cannot grant a permission level above your own")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	mapping, err := a.Permissions.Set(ctx, ac.scope.InstallationID, discordRoleID, targetLevel.String(), ac.user.ID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "set permission failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save permission mapping")
		return
	}
	a.recordAudit(ctx, ac, "PERMISSION_CHANGE", "role:"+discordRoleID, "", "success", nil, map[string]string{"level": targetLevel.String()})
	writeSaaSJSON(w, http.StatusOK, toRolePermissionDTO(mapping))
}

// handleDeletePermission is DELETE .../admin/permissions/{mappingID} (PERMISSIONS_MANAGE).
// Escalation-safe the same way handleSetPermission is: an actor can only remove a mapping whose
// OWN granted level is at or below their ceiling - never used to strip a higher-privileged
// mapping out from under someone with more authority than the actor.
func (a *App) handleDeletePermission(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPermissionsManage)
	if !ok {
		return
	}
	mappingID, good := pathInt64(w, r, "mappingID")
	if !good {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	existing, err := a.Permissions.Get(ctx, ac.scope.InstallationID, mappingID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get permission mapping failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve permission mapping")
		return
	}
	if existing == nil {
		writeSaaSError(w, codeNotFound, "permission mapping not found")
		return
	}
	existingLevel, _ := permissions.ParseLevel(existing.Level)
	if !permissions.CanGrant(ac.level, existingLevel) {
		writeSaaSError(w, codeAdminEscalationDenied, "cannot remove a permission level above your own")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	if err := a.Permissions.Delete(ctx, ac.scope.InstallationID, mappingID); err != nil {
		slog.Warn("component=saas_api", "msg", "delete permission failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not delete permission mapping")
		return
	}
	a.recordAudit(ctx, ac, "PERMISSION_CHANGE", "role:"+existing.DiscordRoleID, "removed", "success", toRolePermissionDTO(*existing), nil)
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// auditEntryDTO is the customer-facing shape of an audit_log row - repository.AuditEntry itself
// carries no json tags (it is a storage type, not a wire type), so it is never marshaled directly.
type auditEntryDTO struct {
	ID             int64           `json:"id"`
	ActorDiscordID string          `json:"actorDiscordId"`
	Action         string          `json:"action"`
	Target         string          `json:"target,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Result         string          `json:"result"`
	CreatedAt      string          `json:"createdAt"`
	BeforeState    json.RawMessage `json:"beforeState,omitempty"`
	AfterState     json.RawMessage `json:"afterState,omitempty"`
}

func toAuditEntryDTO(e repository.AuditEntry) auditEntryDTO {
	return auditEntryDTO{
		ID: e.ID, ActorDiscordID: e.ActorDiscordID, Action: e.Action, Target: e.Target, Reason: e.Reason,
		Result: e.Result, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339), BeforeState: e.BeforeState, AfterState: e.AfterState,
	}
}

// handleListAuditLog is GET .../admin/audit-log (PERMISSIONS_VIEW - reading who-did-what is
// itself privileged, same floor as viewing the permission map).
func (a *App) handleListAuditLog(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPermissionsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	entries, err := a.AdminAudit.List(ctx, ac.scope.OrganizationID, &ac.scope.InstallationID, 0, 50)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list audit log failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list audit log")
		return
	}
	out := make([]auditEntryDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, toAuditEntryDTO(e))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}
