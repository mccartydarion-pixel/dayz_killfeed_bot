// Package app: platform-owner deletes (docs/ADMIN_API.md "Deleting a community or an
// installation").
//
// The two destructive routes of the Owner Hub. They are registered through adminHandle like
// every other /api/admin write, so only a platform owner reaches them (platform staff are
// refused by adminRoute, organization owners have no route to them at all: nothing under
// /api/saas calls into this file). Each needs a reason and a typed confirmation that the server
// compares itself, and writes its platform_audit_log row in the same transaction as the delete.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// ownerDeleteRequest is the body of both delete routes. Confirm is what the admin typed: the
// organization's exact name, or the installation id.
type ownerDeleteRequest struct {
	Reason  string `json:"reason"`
	Confirm string `json:"confirm"`
}

func readOwnerDeleteRequest(w http.ResponseWriter, r *http.Request) (ownerDeleteRequest, bool) {
	var req ownerDeleteRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "could not read request body")
		return req, false
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeSaaSError(w, codeInvalidRequest, "request body must be JSON")
			return req, false
		}
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		writeSaaSError(w, codeInvalidRequest, "a reason is required for every owner action")
		return req, false
	}
	if utf8.RuneCountInString(req.Reason) > ownerReasonMax {
		writeSaaSError(w, codeInvalidRequest, "reason is too long (max 500 characters)")
		return req, false
	}
	if strings.TrimSpace(req.Confirm) == "" {
		writeSaaSError(w, codeInvalidRequest, "confirm is required: type the exact name (or the installation id) to delete")
		return req, false
	}
	return req, true
}

// deleteBlockedMessage is the one-line refusal: every reason, in plain English.
func deleteBlockedMessage(what string, blockers []repository.DeleteBlocker) string {
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		parts = append(parts, b.Message)
	}
	return what + " cannot be deleted: " + strings.Join(parts, " ")
}

// organizationDeleteCheckDTO is GET .../delete-check: the plan plus whether the organization
// belongs to a platform owner (the website warns before the owner deletes their own).
type organizationDeleteCheckDTO struct {
	*repository.OrganizationDeletePlan
	Deletable                 bool `json:"deletable"`
	PlatformOwnerOrganization bool `json:"platformOwnerOrganization"`
}

func (a *App) platformOwnerOrganization(organizationID int64, ownerDiscordID string) bool {
	return entitlements.OwnerOrganization(organizationID) || (a.Config != nil && a.Config.IsPlatformAdmin(ownerDiscordID))
}

// handleOwnerOrganizationDeleteCheck is GET /api/admin/organizations/{id}/delete-check: what
// deleting the organization would remove, and every reason it would be refused. Changes nothing.
func (a *App) handleOwnerOrganizationDeleteCheck(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	plan, err := a.PlatformOwner.PlanOrganizationDelete(ctx, orgID)
	if err != nil {
		a.adminReadFailed(w, "organization delete check", err)
		return
	}
	if plan == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	a.writeAdminJSON(w, http.StatusOK, organizationDeleteCheckDTO{OrganizationDeletePlan: plan, Deletable: plan.Deletable(),
		PlatformOwnerOrganization: a.platformOwnerOrganization(orgID, plan.OwnerDiscordID)})
}

// handleOwnerDeleteOrganization is POST /api/admin/organizations/{id}/delete: permanently
// delete an EMPTY organization (repository.DeleteEmptyOrganization defines empty and enforces it
// in the delete's own transaction). Body: reason, confirm (the organization's exact name).
func (a *App) handleOwnerDeleteOrganization(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := readOwnerDeleteRequest(w, r)
	if !ok {
		return
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	plan, err := a.PlatformOwner.DeleteEmptyOrganization(ctx, orgID, req.Confirm, admin.DiscordID, req.Reason)
	switch {
	case errors.Is(err, repository.ErrDeleteConfirmMismatch):
		writeSaaSError(w, codeInvalidRequest, "the name you typed does not match this community's name. Type it exactly, including capital letters.")
		return
	case errors.Is(err, repository.ErrDeleteBlocked):
		a.ownerAudit(ctx, admin, "organization.delete_refused", "organization", orgID, &orgID, req.Reason, "REFUSED", plan, nil)
		writeSaaSError(w, codeConflict, deleteBlockedMessage("This community", plan.Blockers))
		return
	case err != nil:
		ownerFailed(w, "delete organization", err)
		return
	case plan == nil:
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	if a.OwnerAccess != nil {
		_ = a.OwnerAccess.Refresh(context.WithoutCancel(ctx))
	}
	slog.Info("component=admin_api", "event", "owner_action", "acting_admin_discord_id", admin.DiscordID, "action", "organization.deleted", "target_type", "organization", "target_id", orgID, "result", "OK")
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"deleted": true, "organization": plan})
}

// handleOwnerInstallationDeleteCheck is GET /api/admin/installations/{id}/delete-check.
func (a *App) handleOwnerInstallationDeleteCheck(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	id, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	plan, err := a.PlatformOwner.PlanInstallationDelete(ctx, id)
	if err != nil {
		a.adminReadFailed(w, "installation delete check", err)
		return
	}
	if plan == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"installation": plan, "deletable": plan.Deletable(),
		"platformOwnerOrganization": a.platformOwnerOrganization(plan.OrganizationID, plan.OwnerDiscordID)})
}

// handleOwnerDeleteInstallation is POST /api/admin/installations/{id}/delete: remove one
// installation and its configuration (repository.DeleteInstallation). Its game server is switched
// off and kept with all gameplay data; the worker is stopped here after the commit. Body: reason,
// confirm (the installation id).
func (a *App) handleOwnerDeleteInstallation(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := readOwnerDeleteRequest(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	plan, err := a.PlatformOwner.DeleteInstallation(ctx, id, req.Confirm, admin.DiscordID, req.Reason)
	switch {
	case errors.Is(err, repository.ErrDeleteConfirmMismatch):
		writeSaaSError(w, codeInvalidRequest, "what you typed does not match. Type the installation number "+strconv.FormatInt(id, 10)+" to confirm.")
		return
	case errors.Is(err, repository.ErrDeleteBlocked):
		a.ownerAudit(ctx, admin, "installation.delete_refused", "installation", id, &plan.OrganizationID, req.Reason, "REFUSED", plan, nil)
		writeSaaSError(w, codeConflict, deleteBlockedMessage("This installation", plan.Blockers)+" Suspend it instead if it should stop running.")
		return
	case err != nil:
		ownerFailed(w, "delete installation", err)
		return
	case plan == nil:
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	workerStopped := false
	if plan.GameServerID != nil {
		a.DisconnectServer(*plan.GameServerID)
		workerStopped = true
	}
	if a.OwnerAccess != nil {
		_ = a.OwnerAccess.Refresh(context.WithoutCancel(ctx))
	}
	slog.Info("component=admin_api", "event", "owner_action", "acting_admin_discord_id", admin.DiscordID, "action", "installation.deleted", "target_type", "installation", "target_id", id, "result", "OK")
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"deleted": true, "installation": plan, "gameServerSwitchedOff": workerStopped})
}

// registerDeleteAPI wires the delete routes under /api/admin.
func (a *App) registerDeleteAPI() {
	if a.HTTPServer == nil {
		return
	}
	a.adminHandle("GET /api/admin/organizations/{organizationID}/delete-check", a.handleOwnerOrganizationDeleteCheck)
	a.adminHandle("POST /api/admin/organizations/{organizationID}/delete", a.handleOwnerDeleteOrganization)
	a.adminHandle("GET /api/admin/installations/{installationID}/delete-check", a.handleOwnerInstallationDeleteCheck)
	a.adminHandle("POST /api/admin/installations/{installationID}/delete", a.handleOwnerDeleteInstallation)
}
