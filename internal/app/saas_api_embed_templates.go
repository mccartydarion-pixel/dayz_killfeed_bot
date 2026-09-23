package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// Embed template persistence (Embed Designer Phase 2) and its runtime status (Phase 4).
// Saving, changing or deleting a template changes a Discord message only when the
// rollout flag CHAMPION_CUSTOM_EMBEDS_ENABLED is on and the route is one the runtime
// renders (docs/EMBED_RUNTIME.md); the save/reset handlers invalidate the renderer's
// cache so the next event sees the change. See docs/SAAS_API.md.

// maxEmbedTemplateBody bounds a template request: a maximal valid template is well
// under this.
const maxEmbedTemplateBody = 64 << 10

// codePayloadTooLarge maps to 413 (registered in httpStatusForCode).
const codePayloadTooLarge = "PAYLOAD_TOO_LARGE"

// Runtime rendering status reported on every response so a client never presents a
// saved template as live when it is not: ENABLED only when the rollout flag
// (CHAMPION_CUSTOM_EMBEDS_ENABLED) is on AND the route has a runtime publisher that
// renders templates (embedrender.SupportedRoutes); otherwise NOT_ENABLED.
const (
	runtimeRenderingEnabled = "ENABLED"
	runtimeRenderingOff     = "NOT_ENABLED"
)

func (a *App) runtimeRenderingFor(routeKey string) string {
	if a.EmbedRenderer.Enabled() && embedrender.RouteSupported(routeKey) {
		return runtimeRenderingEnabled
	}
	return runtimeRenderingOff
}

// EmbedTemplateResponse is one route's state. Customized=false means the route uses
// the Champion default; Template is then null (defaults are never copied into the
// database or fabricated by the backend).
type EmbedTemplateResponse struct {
	RouteKey   string                 `json:"routeKey"`
	Customized bool                   `json:"customized"`
	Template   *embedtemplates.Config `json:"template"`
	Variables  []string               `json:"variables"`
	// VariableDefinitions is the backend-owned metadata for every approved variable
	// (label, description, category, example, availability, format), additive to
	// the older name list.
	VariableDefinitions []embedtemplates.VariableDefinition `json:"variableDefinitions"`
	CreatedAt           *string                             `json:"createdAt"`
	UpdatedAt           *string                             `json:"updatedAt"`
	RuntimeRendering    string                              `json:"runtimeRendering"`
}

// EmbedTemplateListResponse lists only the customized routes, plus the approved
// variables per route and the limits the server enforces.
type EmbedTemplateListResponse struct {
	InstallationID   int64                   `json:"installationId"`
	Templates        []EmbedTemplateResponse `json:"templates"`
	CustomizedRoutes []string                `json:"customizedRoutes"`
	Variables        map[string][]string     `json:"variables"`
	// VariableDefinitions: per route, the metadata for each approved variable.
	VariableDefinitions map[string][]embedtemplates.VariableDefinition `json:"variableDefinitions"`
	Limits              embedTemplateLimits                            `json:"limits"`
	// RuntimeRoutes are the routes whose publishers render templates (independent of
	// the rollout flag); RuntimeRendering is the deployment-wide flag state.
	RuntimeRoutes    []string `json:"runtimeRoutes"`
	RuntimeRendering string   `json:"runtimeRendering"`
}

type embedTemplateLimits struct {
	Title       int `json:"title"`
	Description int `json:"description"`
	Fields      int `json:"fields"`
	FieldLabel  int `json:"fieldLabel"`
	FieldValue  int `json:"fieldValue"`
	FooterText  int `json:"footerText"`
	AuthorName  int `json:"authorName"`
	TotalText   int `json:"totalText"`
}

func embedLimits() embedTemplateLimits {
	return embedTemplateLimits{Title: embedtemplates.MaxTitle, Description: embedtemplates.MaxDescription, Fields: embedtemplates.MaxFields,
		FieldLabel: embedtemplates.MaxFieldLabel, FieldValue: embedtemplates.MaxFieldValue, FooterText: embedtemplates.MaxFooterText,
		AuthorName: embedtemplates.MaxAuthorName, TotalText: embedtemplates.MaxTotalText}
}

func (a *App) embedResponse(routeKey string, s *embedtemplates.Stored) EmbedTemplateResponse {
	out := EmbedTemplateResponse{RouteKey: routeKey, Variables: embedtemplates.Variables(routeKey), VariableDefinitions: embedtemplates.VariableDefinitions(routeKey), RuntimeRendering: a.runtimeRenderingFor(routeKey)}
	if s != nil {
		cfg := s.Config
		cfg.RouteKey = routeKey
		out.Customized, out.Template = true, &cfg
		created, updated := s.CreatedAt.UTC().Format(time.RFC3339Nano), s.UpdatedAt.UTC().Format(time.RFC3339Nano)
		out.CreatedAt, out.UpdatedAt = &created, &updated
	}
	return out
}

// embedTemplateContext runs the shared checks: service auth, acting user, organization
// membership (and, for writes, the OWNER/ADMIN role), the installation belonging to
// that organization (a foreign or unknown installation is the same 404), and - when
// withRoute - a valid route key. It has written the response when ok is false.
func (a *App) embedTemplateContext(w http.ResponseWriter, r *http.Request, write, withRoute bool) (organizationID, installationID int64, routeKey string, userID int64, ok bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, good := pathInt64(w, r, "organizationID")
	if !good {
		return
	}
	if write {
		if _, good = a.requireOrganizationRole(w, r, organizationID, user.ID); !good {
			return
		}
	} else if _, good = a.requireOrganizationMember(w, r, organizationID, user.ID); !good {
		return
	}
	installationID, good = pathInt64(w, r, "installationID")
	if !good {
		return
	}
	if withRoute {
		routeKey = r.PathValue("routeKey")
		if !embedtemplates.ValidRoute(routeKey) {
			writeSaaSError(w, codeInvalidRequest, "invalid routeKey")
			return
		}
	}
	if a.SaaSInstallations == nil || a.EmbedTemplates == nil {
		writeSaaSError(w, codeInternalError, "embed template service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	inst, err := a.SaaSInstallations.GetScoped(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "embed template installation lookup failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load installation")
		return
	}
	if inst == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	return organizationID, installationID, routeKey, user.ID, true
}

func (a *App) embedFailed(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, embedtemplates.ErrInstallationNotFound) {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	slog.Warn("component=saas_api", "msg", "embed template "+what+" failed", "err", err.Error())
	writeSaaSError(w, codeInternalError, "could not "+what+" embed template")
}

// handleListEmbedTemplates is GET .../embed-templates. Members may read (the same
// convention as channel routes and settings); only OWNER/ADMIN may write.
func (a *App) handleListEmbedTemplates(w http.ResponseWriter, r *http.Request) {
	orgID, instID, _, _, ok := a.embedTemplateContext(w, r, false, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stored, err := a.EmbedTemplates.List(ctx, orgID, instID)
	if err != nil {
		a.embedFailed(w, "load", err)
		return
	}
	resp := EmbedTemplateListResponse{InstallationID: instID, Templates: []EmbedTemplateResponse{}, CustomizedRoutes: []string{},
		Variables: embedtemplates.AllVariables(), VariableDefinitions: embedtemplates.AllVariableDefinitions(), Limits: embedLimits(), RuntimeRoutes: embedrender.SupportedRoutes(), RuntimeRendering: runtimeRenderingOff}
	if a.EmbedRenderer.Enabled() {
		resp.RuntimeRendering = runtimeRenderingEnabled
	}
	for i := range stored {
		resp.Templates = append(resp.Templates, a.embedResponse(stored[i].Config.RouteKey, &stored[i]))
		resp.CustomizedRoutes = append(resp.CustomizedRoutes, stored[i].Config.RouteKey)
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handleGetEmbedTemplate is GET .../embed-templates/{routeKey}: the stored template,
// or {customized:false, template:null}.
func (a *App) handleGetEmbedTemplate(w http.ResponseWriter, r *http.Request) {
	orgID, instID, routeKey, _, ok := a.embedTemplateContext(w, r, false, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stored, err := a.EmbedTemplates.Get(ctx, orgID, instID, routeKey)
	if err != nil {
		a.embedFailed(w, "load", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, a.embedResponse(routeKey, stored))
}

// handlePutEmbedTemplate is PUT .../embed-templates/{routeKey} (OWNER/ADMIN): validate,
// normalize, upsert, and return the normalized stored template.
func (a *App) handlePutEmbedTemplate(w http.ResponseWriter, r *http.Request) {
	orgID, instID, routeKey, userID, ok := a.embedTemplateContext(w, r, true, true)
	if !ok {
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxEmbedTemplateBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields() // typed structure only: an unexpected key is rejected, not stored
	var cfg embedtemplates.Config
	if err := dec.Decode(&cfg); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeSaaSError(w, codePayloadTooLarge, "embed template payload is too large")
			return
		}
		writeSaaSError(w, codeInvalidRequest, "invalid embed template payload")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // exactly one JSON value
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeSaaSError(w, codePayloadTooLarge, "embed template payload is too large")
			return
		}
		writeSaaSError(w, codeInvalidRequest, "invalid embed template payload")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stored, err := a.EmbedTemplates.Save(ctx, orgID, instID, routeKey, cfg)
	var invalid *embedtemplates.ValidationError
	if errors.As(err, &invalid) {
		issues := invalid.Issues
		if len(issues) > 5 {
			issues = append(issues[:5:5], "and more")
		}
		writeSaaSError(w, codeInvalidRequest, "invalid embed template: "+strings.Join(issues, "; "))
		return
	}
	if err != nil {
		a.embedFailed(w, "save", err)
		return
	}
	a.EmbedRenderer.Invalidate(instID, routeKey) // an in-process save is visible on the next event
	// Never the template contents.
	slog.Info("component=saas_api", "event", "embed_template_saved", "organization_id", orgID, "installation_id", instID, "route_key", routeKey, "acting_user_id", userID)
	writeSaaSJSON(w, http.StatusOK, a.embedResponse(routeKey, &stored))
}

// handleDeleteEmbedTemplate is DELETE .../embed-templates/{routeKey} (OWNER/ADMIN):
// reset the route to the Champion default. Idempotent: 200 with customized:false
// whether or not a custom template existed.
func (a *App) handleDeleteEmbedTemplate(w http.ResponseWriter, r *http.Request) {
	orgID, instID, routeKey, userID, ok := a.embedTemplateContext(w, r, true, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	deleted, err := a.EmbedTemplates.Delete(ctx, orgID, instID, routeKey)
	if err != nil {
		a.embedFailed(w, "reset", err)
		return
	}
	a.EmbedRenderer.Invalidate(instID, routeKey) // an in-process reset is visible on the next event
	slog.Info("component=saas_api", "event", "embed_template_deleted", "organization_id", orgID, "installation_id", instID, "route_key", routeKey, "acting_user_id", userID, "existed", deleted)
	writeSaaSJSON(w, http.StatusOK, a.embedResponse(routeKey, nil))
}
