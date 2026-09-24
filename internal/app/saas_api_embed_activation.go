package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Embed Designer runtime activation (docs/EMBED_RUNTIME.md "Activation"). Each installation
// chooses, per route, between the Champion Default card and its saved Custom Embed. The choice is
// stored per installation and route (installation_embed_activation) and never touches another
// installation. A custom template is rendered ONLY when all four hold:
//
//  1. the operator rollout switch CHAMPION_CUSTOM_EMBEDS_ENABLED is on (a safety gate customers
//     cannot change);
//  2. the route has a runtime publisher that renders templates (embedrender.RouteSupported);
//  3. the installation selected Custom Embed for the route;
//  4. the saved template is valid and enabled.
//
// Otherwise the Champion default card is posted, exactly as before.

// embedActivationStore is the persistence the activation API needs
// (*repository.EmbedTemplateRepository implements it).
type embedActivationStore interface {
	ListActivations(ctx context.Context, organizationID, installationID int64) (map[string]string, error)
	SetActivation(ctx context.Context, organizationID, installationID int64, routeKey, mode string, userID int64) error
}

// Runtime states and reasons reported to the website.
const (
	embedRuntimeActive  = "ACTIVE"  // the custom template is what the runtime posts
	embedRuntimeDefault = "DEFAULT" // Champion Default is selected: the default card is posted by choice
	embedRuntimeBlocked = "BLOCKED" // Custom Embed is selected but cannot be used (see reason)

	embedReasonGlobalDisabled   = "GLOBAL_DISABLED"   // operator rollout switch is off
	embedReasonRouteUnsupported = "ROUTE_UNSUPPORTED" // no runtime publisher renders this route
	embedReasonNoTemplate       = "NO_TEMPLATE"       // nothing saved for the route
	embedReasonTemplateInvalid  = "TEMPLATE_INVALID"  // the saved template fails validation
	embedReasonTemplateDisabled = "TEMPLATE_DISABLED" // the saved template is switched off
)

// EmbedActivationDTO is one route's three separate states: whether a template is saved, which
// presentation is selected, and whether the runtime actually uses it (with the reason if not).
type EmbedActivationDTO struct {
	RouteKey        string `json:"routeKey"`
	Mode            string `json:"mode"` // DEFAULT | CUSTOM
	TemplateSaved   bool   `json:"templateSaved"`
	TemplateEnabled bool   `json:"templateEnabled"`
	TemplateValid   bool   `json:"templateValid"`
	RouteSupported  bool   `json:"routeSupported"`
	GlobalEnabled   bool   `json:"globalEnabled"`
	Runtime         string `json:"runtime"`                 // ACTIVE | DEFAULT | BLOCKED
	BlockedReason   string `json:"blockedReason,omitempty"` // set when Runtime is BLOCKED
	// CanActivate reports whether Custom Embed may be selected now; ActivationUnavailable says why
	// not. The global switch never prevents SELECTING Custom Embed (the choice is kept and takes
	// effect when the operator enables custom rendering) - it only makes the runtime BLOCKED.
	CanActivate           bool   `json:"canActivate"`
	ActivationUnavailable string `json:"activationUnavailable,omitempty"`
}

// embedActivationStatus derives the status of one route from its stored template and mode.
func (a *App) embedActivationStatus(routeKey string, stored *embedtemplates.Stored, mode string) EmbedActivationDTO {
	if mode != repository.EmbedModeCustom {
		mode = repository.EmbedModeDefault
	}
	st := EmbedActivationDTO{RouteKey: routeKey, Mode: mode, RouteSupported: embedrender.RouteSupported(routeKey), GlobalEnabled: a.EmbedRenderer.Enabled()}
	if stored != nil {
		st.TemplateSaved = true
		st.TemplateEnabled = stored.Config.Enabled
		_, err := embedtemplates.Validate(stored.Config, routeKey)
		st.TemplateValid = err == nil
	}
	switch {
	case !st.RouteSupported:
		st.ActivationUnavailable = embedReasonRouteUnsupported
	case !st.TemplateSaved:
		st.ActivationUnavailable = embedReasonNoTemplate
	case !st.TemplateValid:
		st.ActivationUnavailable = embedReasonTemplateInvalid
	case !st.TemplateEnabled:
		st.ActivationUnavailable = embedReasonTemplateDisabled
	}
	st.CanActivate = st.ActivationUnavailable == ""
	switch {
	case mode == repository.EmbedModeDefault:
		st.Runtime = embedRuntimeDefault
	case !st.CanActivate:
		st.Runtime, st.BlockedReason = embedRuntimeBlocked, st.ActivationUnavailable
	case !st.GlobalEnabled:
		st.Runtime, st.BlockedReason = embedRuntimeBlocked, embedReasonGlobalDisabled
	default:
		st.Runtime = embedRuntimeActive
	}
	return st
}

// embedModes loads the installation's selected modes (absent = DEFAULT). A failure is reported
// to the caller, never silently shown as DEFAULT.
func (a *App) embedModes(ctx context.Context, orgID, instID int64) (map[string]string, error) {
	if a.EmbedActivations == nil {
		return map[string]string{}, nil
	}
	return a.EmbedActivations.ListActivations(ctx, orgID, instID)
}

type embedActivationRequest struct {
	Mode string `json:"mode"`
}

// handlePutEmbedActivation is PUT .../embed-templates/{routeKey}/activation (OWNER/ADMIN):
// {"mode":"CUSTOM"} activates the saved template for this installation's route, {"mode":"DEFAULT"}
// returns it to the Champion default card. Neither changes the saved template. Activation of an
// unsupported route, or without a saved, valid, enabled template, is refused with the reason.
func (a *App) handlePutEmbedActivation(w http.ResponseWriter, r *http.Request) {
	orgID, instID, routeKey, userID, ok := a.embedTemplateContext(w, r, true, true)
	if !ok {
		return
	}
	if a.EmbedActivations == nil {
		writeSaaSError(w, codeInternalError, "embed activation service unavailable")
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	var req embedActivationRequest
	if err := dec.Decode(&req); err != nil || (req.Mode != repository.EmbedModeDefault && req.Mode != repository.EmbedModeCustom) {
		writeSaaSError(w, codeInvalidRequest, `mode must be "DEFAULT" or "CUSTOM"`)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeSaaSError(w, codeInvalidRequest, "invalid activation payload")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stored, err := a.EmbedTemplates.Get(ctx, orgID, instID, routeKey)
	if err != nil {
		a.embedFailed(w, "load", err)
		return
	}
	if req.Mode == repository.EmbedModeCustom {
		if st := a.embedActivationStatus(routeKey, stored, req.Mode); !st.CanActivate {
			code := codeConflict
			if st.ActivationUnavailable == embedReasonRouteUnsupported {
				code = codeEmbedCustomNotSupported
			}
			writeSaaSError(w, code, "custom embed cannot be activated: "+st.ActivationUnavailable)
			return
		}
	}
	if err := a.EmbedActivations.SetActivation(ctx, orgID, instID, routeKey, req.Mode, userID); err != nil {
		a.embedFailed(w, "activate", err)
		return
	}
	a.EmbedRenderer.Invalidate(instID, routeKey) // the next event uses the new selection
	slog.Info("component=saas_api", "event", "embed_activation_changed", "organization_id", orgID, "installation_id", instID,
		"route_key", routeKey, "mode", req.Mode, "acting_user_id", userID)
	writeSaaSJSON(w, http.StatusOK, a.embedActivationStatus(routeKey, stored, req.Mode))
}
