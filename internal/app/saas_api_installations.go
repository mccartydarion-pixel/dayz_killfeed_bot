package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type createInstallationRequest struct {
	DiscordGuildConnectionID int64 `json:"discordGuildConnectionId"`
}

// handleCreateInstallation is POST .../installations (section 7). Only
// OWNER/ADMIN may create one (section 8's role gate applied consistently to
// every setup-shaped mutation, not just the setup-progress PATCH). The
// referenced guild connection must belong to the same organization -
// GetScoped enforces that by construction.
//
// Onboarding V2: this is the "initial setup / new service" operation, gated by
// the subscription - BILLING_REQUIRED when there is no running trial or paid
// plan, INSTALLATION_LIMIT_REACHED at the plan's capacity (the trial allows
// one). It never modifies or replaces an existing installation; reconfiguring
// one is the server-selection / channel routes on that installation.
func (a *App) handleCreateInstallation(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationRole(w, r, organizationID, user.ID); !ok {
		return
	}
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	var req createInstallationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DiscordGuildConnectionID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "discordGuildConnectionId is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	conn, err := a.SaaSGuildConnections.GetScoped(ctx, organizationID, req.DiscordGuildConnectionID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "guild connection lookup failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify guild connection")
		return
	}
	if conn == nil {
		writeSaaSError(w, codeNotFound, "guild connection not found")
		return
	}

	limit, ok := a.installationCapacity(ctx, w, organizationID)
	if !ok {
		return
	}
	inst, err := a.SaaSInstallations.CreateWithinLimit(ctx, organizationID, conn.ID, limit)
	if err != nil {
		if err == repository.ErrDuplicate {
			writeSaaSError(w, codeConflict, "an installation already exists for this guild connection")
			return
		}
		if err == repository.ErrInstallationLimitReached {
			writeSaaSError(w, codeInstallationLimitReached, installationLimitMessage(limit))
			return
		}
		slog.Warn("component=saas_api", "msg", "create installation failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not create installation")
		return
	}
	writeSaaSJSON(w, http.StatusCreated, a.buildInstallationSummary(ctx, organizationID, *inst))
}

// handleGetInstallation is GET .../installations/{installationID} (section
// 7): any member may read; tenant scoping comes from GetScoped.
func (a *App) handleGetInstallation(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationMember(w, r, organizationID, user.ID); !ok {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSInstallations == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	inst, err := a.SaaSInstallations.GetScoped(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get installation failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load installation")
		return
	}
	if inst == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, a.buildInstallationSummary(ctx, organizationID, *inst))
}

// handleGetSetupProgress is GET .../installations/{installationID}/setup
// (section 8): any member may read.
func (a *App) handleGetSetupProgress(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationMember(w, r, organizationID, user.ID); !ok {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSInstallations == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	progress, err := a.SaaSInstallations.GetSetupProgress(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get setup progress failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load setup progress")
		return
	}
	if progress == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toSetupProgressSummary(*progress))
}

// setupSteps is the known, ordered set of onboarding steps. current_step
// must always be one of these - never an arbitrary client-supplied string.
var setupSteps = map[string]bool{
	"DISCORD":    true,
	"NITRADO":    true,
	"SERVER":     true,
	"CHANNELS":   true,
	"VALIDATION": true,
	"COMPLETE":   true,
}

// validateSetupProgressOrder enforces the same linear dependency chain the
// step names imply: discord -> nitrado -> server -> channels -> validation.
// A later flag may only be true if every earlier flag in the chain is also
// true in the resulting (post-merge) state - this is what stops "arbitrary
// impossible step advancement" (section 8), e.g. marking validation
// complete while Discord was never connected.
func validateSetupProgressOrder(p repository.InstallationSetupProgress) bool {
	if p.NitradoCompleted && !p.DiscordCompleted {
		return false
	}
	if p.ServerSelected && !p.NitradoCompleted {
		return false
	}
	if p.ChannelsCompleted && !p.ServerSelected {
		return false
	}
	if p.ValidationCompleted && !p.ChannelsCompleted {
		return false
	}
	return true
}

type setupProgressPatchRequest struct {
	CurrentStep         *string `json:"currentStep"`
	DiscordCompleted    *bool   `json:"discordCompleted"`
	NitradoCompleted    *bool   `json:"nitradoCompleted"`
	ServerSelected      *bool   `json:"serverSelected"`
	ChannelsCompleted   *bool   `json:"channelsCompleted"`
	ValidationCompleted *bool   `json:"validationCompleted"`
}

// handleUpdateSetupProgress is PATCH .../installations/{installationID}/setup
// (section 8). Only OWNER/ADMIN may modify setup - MEMBER stays read-only.
// This is a true PATCH: only fields present in the request body are
// changed; everything else keeps its current stored value. The merged
// result is validated against validateSetupProgressOrder before it is
// persisted - an invalid transition is rejected with INVALID_REQUEST and
// nothing is written.
func (a *App) handleUpdateSetupProgress(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if _, ok := a.requireOrganizationRole(w, r, organizationID, user.ID); !ok {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSInstallations == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	var req setupProgressPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	if req.CurrentStep != nil && !setupSteps[strings.ToUpper(strings.TrimSpace(*req.CurrentStep))] {
		writeSaaSError(w, codeInvalidRequest, "unknown setup step")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	current, err := a.SaaSInstallations.GetSetupProgress(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get setup progress failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load setup progress")
		return
	}
	if current == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}

	merged := *current
	if req.CurrentStep != nil {
		merged.CurrentStep = strings.ToUpper(strings.TrimSpace(*req.CurrentStep))
	}
	if req.DiscordCompleted != nil {
		merged.DiscordCompleted = *req.DiscordCompleted
	}
	if req.NitradoCompleted != nil {
		merged.NitradoCompleted = *req.NitradoCompleted
	}
	if req.ServerSelected != nil {
		merged.ServerSelected = *req.ServerSelected
	}
	if req.ChannelsCompleted != nil {
		merged.ChannelsCompleted = *req.ChannelsCompleted
	}
	if req.ValidationCompleted != nil {
		merged.ValidationCompleted = *req.ValidationCompleted
	}

	if !validateSetupProgressOrder(merged) {
		writeSaaSError(w, codeInvalidRequest, "setup steps must complete in order")
		return
	}

	if err := a.SaaSInstallations.UpdateSetupProgress(ctx, organizationID, installationID, merged); err != nil {
		slog.Warn("component=saas_api", "msg", "update setup progress failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not update setup progress")
		return
	}

	updated, err := a.SaaSInstallations.GetSetupProgress(ctx, organizationID, installationID)
	if err != nil || updated == nil {
		writeSaaSError(w, codeInternalError, "could not reload setup progress")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toSetupProgressSummary(*updated))
}
