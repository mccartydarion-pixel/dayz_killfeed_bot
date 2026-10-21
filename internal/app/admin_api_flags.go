// Package app: Owner Hub feature flags (docs/ADMIN_API.md "Feature flags").
//
// Per-installation overrides of the environment rollout switches. The catalog is fixed in
// internal/featureflags; the owner sets, clears and reads overrides here, each write audited.
package app

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type flagStateDTO struct {
	featureflags.Definition
	// Effective is what the installation gets right now; Default is the environment answer;
	// Override is the owner's stored decision, if any.
	//
	// Source says where Effective comes from: "override" (the stored decision), "owner_access"
	// (no override, and the installation belongs to a platform owner's own organization, for
	// which this flag is on - docs/ADMIN_API.md "Platform owner access") or "default" (the
	// environment). OwnerAccess is true whenever owner access applies to this flag for this
	// installation, including when an override currently hides it.
	Effective   bool   `json:"effective"`
	Default     bool   `json:"default"`
	Source      string `json:"source"`
	OwnerAccess bool   `json:"ownerAccess"`
	// SourceLabel is the same thing in plain words, for the Owner Hub to show.
	SourceLabel string  `json:"sourceLabel"`
	Override    *bool   `json:"override"`
	Reason      *string `json:"reason"`
	UpdatedBy   *string `json:"updatedBy"`
	UpdatedAt   *string `json:"updatedAt"`
}

// flagDefault is the environment's answer for one installation, exactly as the consumer
// would compute it without an override.
func (a *App) flagDefault(inst *repository.InstallationSuspension, key string) bool {
	switch key {
	case featureflags.CustomEmbeds:
		return a.EmbedRenderer != nil && a.EmbedRenderer.Enabled() && (a.Config == nil || a.Config.CustomEmbedsEnabled)
	case featureflags.ShopCanary:
		if a.Config == nil || !a.Config.ShopCanaryExecution.Enabled {
			return false
		}
		for _, id := range a.Config.ShopCanaryExecution.InstallationIDs {
			if id == inst.InstallationID {
				return true
			}
		}
		return false
	case featureflags.CaseEvidence:
		return inst.GameServerID != nil && caseEvidenceEnvForServer(*inst.GameServerID)
	case featureflags.CaseBuildEvidence:
		return inst.GameServerID != nil && caseEvidenceEnvForServer(*inst.GameServerID) && caseBuildEvidenceEnvForServer(*inst.GameServerID)
	case featureflags.MapRotation:
		return a.Config != nil && a.Config.MapRotationEnabled
	}
	return false
}

// Where a flag's effective value comes from (flagStateDTO.Source).
const (
	flagSourceDefault     = "default"
	flagSourceOwnerAccess = "owner_access"
	flagSourceOverride    = "override"
)

// flagOwnerAccess reports whether owner access switches key on for the installation, as the
// consumer would see it. Custom embeds still need the renderer itself (customEmbedsFor), so
// owner access cannot turn them on where there is none.
func (a *App) flagOwnerAccess(installationID int64, key string) bool {
	if a.FeatureFlags == nil || !a.FeatureFlags.OwnerAccess(installationID, key) {
		return false
	}
	if key == featureflags.CustomEmbeds && (a.EmbedRenderer == nil || !a.EmbedRenderer.Enabled()) {
		return false
	}
	return true
}

func (a *App) flagStates(ctx context.Context, inst *repository.InstallationSuspension) ([]flagStateDTO, error) {
	stored, err := a.PlatformOwner.InstallationOverrides(ctx, inst.InstallationID)
	if err != nil {
		return nil, err
	}
	byKey := map[string]featureflags.Override{}
	for _, o := range stored {
		byKey[o.Flag] = o
	}
	out := make([]flagStateDTO, 0, len(featureflags.Catalog))
	for _, def := range featureflags.Catalog {
		st := flagStateDTO{Definition: def, Default: a.flagDefault(inst, def.Key)}
		st.Effective, st.Source, st.SourceLabel = st.Default, flagSourceDefault, "Environment default"
		st.OwnerAccess = a.flagOwnerAccess(inst.InstallationID, def.Key)
		if st.OwnerAccess {
			st.Effective, st.Source, st.SourceLabel = true, flagSourceOwnerAccess, "On through owner access: this server belongs to a platform owner"
		}
		if o, ok := byKey[def.Key]; ok {
			v := o.Enabled
			st.Override, st.Effective = &v, v
			st.Source, st.SourceLabel = flagSourceOverride, "Set here for this server"
			st.Reason, st.UpdatedBy = optStr(o.Reason), optStr(o.UpdatedBy)
			at := o.UpdatedAt.UTC().Format(time.RFC3339)
			st.UpdatedAt = &at
		}
		out = append(out, st)
	}
	return out, nil
}

// handleOwnerFlagCatalog is GET /api/admin/flags: the catalog with each flag's environment
// default as configured on this deployment.
func (a *App) handleOwnerFlagCatalog(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	items := make([]map[string]any, 0, len(featureflags.Catalog))
	for _, def := range featureflags.Catalog {
		env := ""
		switch def.Key {
		case featureflags.CustomEmbeds:
			env = os.Getenv("CHAMPION_CUSTOM_EMBEDS_ENABLED")
		case featureflags.ShopCanary:
			env = strings.TrimSpace(os.Getenv("CHAMPION_SHOP_CANARY_EXECUTION")) + " [" + strings.TrimSpace(os.Getenv("CHAMPION_SHOP_CANARY_INSTALLATION_IDS")) + "]"
		case featureflags.CaseEvidence:
			env = strings.TrimSpace(os.Getenv("CASE_EVIDENCE_ENABLED")) + " [" + strings.TrimSpace(os.Getenv("CASE_EVIDENCE_SERVER_IDS")) + "]"
		case featureflags.CaseBuildEvidence:
			env = strings.TrimSpace(os.Getenv("CASE_BUILD_EVIDENCE_ENABLED")) + " [" + strings.TrimSpace(os.Getenv("CASE_BUILD_EVIDENCE_SERVER_IDS")) + "]"
		case featureflags.MapRotation:
			env = os.Getenv("CHAMPION_MAP_ROTATION_ENABLED")
		}
		items = append(items, map[string]any{"key": def.Key, "label": def.Label, "description": def.Description, "envVar": def.EnvVar, "restartRequired": def.RestartRequired, "envValue": strings.TrimSpace(env),
			"ownerDefaultOn": def.OwnerDefaultOn, "ownerDefaultNote": def.OwnerDefaultNote})
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleOwnerInstallationFlags is GET /api/admin/installations/{id}/flags.
func (a *App) handleOwnerInstallationFlags(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	inst, ok := a.ownerInstallationContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	states, err := a.flagStates(ctx, inst)
	if err != nil {
		a.adminReadFailed(w, "feature flags", err)
		return
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"installationId": inst.InstallationID, "flags": states})
}

type flagRequest struct {
	Reason  string `json:"reason"`
	Enabled *bool  `json:"enabled"` // nil = clear the override
}

// handleOwnerSetInstallationFlag is PUT /api/admin/installations/{id}/flags/{flag}: body
// {reason, enabled:true|false} sets an override; {reason} (no enabled) clears it.
func (a *App) handleOwnerSetInstallationFlag(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	flag := strings.ToLower(strings.TrimSpace(r.PathValue("flag")))
	if !featureflags.Known(flag) {
		writeSaaSError(w, codeNotFound, "unknown feature flag")
		return
	}
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	body := flagRequest{Reason: req.Reason, Enabled: req.Enabled}
	inst, ok := a.ownerInstallationContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	before, err := a.flagStates(ctx, inst)
	if err != nil {
		a.adminReadFailed(w, "feature flags", err)
		return
	}
	action := "installation.flag_cleared"
	if body.Enabled != nil {
		action = "installation.flag_set"
		err = a.PlatformOwner.SetOverride(ctx, featureflags.Override{InstallationID: inst.InstallationID, Flag: flag, Enabled: *body.Enabled, Reason: body.Reason, UpdatedBy: admin.DiscordID})
	} else {
		err = a.PlatformOwner.ClearOverride(ctx, inst.InstallationID, flag)
	}
	if err != nil {
		ownerFailed(w, "update feature flag", err)
		return
	}
	if a.FeatureFlags != nil {
		_ = a.FeatureFlags.Refresh(ctx)
	}
	if a.EmbedRenderer != nil && flag == featureflags.CustomEmbeds {
		a.EmbedRenderer.InvalidateAll()
	}
	after, err := a.flagStates(ctx, inst)
	if err != nil {
		a.adminReadFailed(w, "feature flags", err)
		return
	}
	a.ownerAudit(ctx, admin, action, "installation", inst.InstallationID, &inst.OrganizationID, body.Reason, "OK", flagByKey(before, flag), flagByKey(after, flag))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"installationId": inst.InstallationID, "flags": after})
}

func flagByKey(states []flagStateDTO, key string) any {
	for _, s := range states {
		if s.Key == key {
			return map[string]any{"flag": key, "effective": s.Effective, "default": s.Default, "override": s.Override, "source": s.Source}
		}
	}
	return nil
}

func (a *App) registerFlagsAPI() {
	if a.HTTPServer == nil {
		return
	}
	a.adminHandle("GET /api/admin/flags", a.handleOwnerFlagCatalog)
	a.adminHandle("GET /api/admin/installations/{installationID}/flags", a.handleOwnerInstallationFlags)
	a.adminHandle("PUT /api/admin/installations/{installationID}/flags/{flag}", a.handleOwnerSetInstallationFlag)
}
