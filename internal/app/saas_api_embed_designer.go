package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Embed Designer V2 (docs/EMBED_DESIGNER_V2.md): preview an UNSAVED draft with the
// production renderer, and send it as a test message to the installation's routed
// Discord channel. Both call embedrender.RenderEvent - the exact function live
// publishing uses - so a preview cannot differ from what an event posts. Neither
// persists anything, invalidates the renderer cache, or touches any gameplay system:
// this file performs template validation, rendering, route lookup and (test only) one
// Discord send. It never accepts a channel, guild or token from the request.

// Embed Designer error codes (registered in httpStatusForCode).
const (
	codeEmbedTemplateInvalid       = "EMBED_TEMPLATE_INVALID"
	codeEmbedTemplateNotRenderable = "EMBED_TEMPLATE_NOT_RENDERABLE"
	codeEmbedCustomNotSupported    = "EMBED_CUSTOM_RENDERING_NOT_SUPPORTED"
	codeEmbedRouteNotConfigured    = "EMBED_ROUTE_NOT_CONFIGURED"
	codeEmbedChannelUnavailable    = "EMBED_TEST_CHANNEL_UNAVAILABLE"
	codeEmbedSendForbidden         = "EMBED_TEST_SEND_FORBIDDEN"
	codeEmbedLinksRequired         = "EMBED_TEST_EMBED_LINKS_REQUIRED"
	codeEmbedRateLimited           = "EMBED_TEST_RATE_LIMITED"
	codeEmbedSendFailed            = "EMBED_TEST_SEND_FAILED"
)

// Why a draft rendered nothing.
const (
	reasonTemplateDisabled = "CUSTOM_TEMPLATE_DISABLED"
	reasonNoContent        = "TEMPLATE_PRODUCED_NO_CONTENT"
)

const (
	maxDraftVariables     = 64  // above the largest route vocabulary (KILLFEED), still bounded
	maxDraftVariableValue = 512 // raw; the renderer then caps each value at embedrender.MaxValue
)

// embedDesignerNow stamps preview/test renders; tests pin it.
var embedDesignerNow = func() time.Time { return time.Now().UTC().Truncate(time.Second) }

// embedRouteLabels name routes in the test message notice.
var embedRouteLabels = map[string]string{
	"KILLFEED": "Killfeed", "HITFEED": "Hitfeed", "PVE_FEED": "PvE Feed", "BOUNTY_TRACKING": "Bounty Tracking",
	"CONNECTIONS": "Connections", "ECONOMY": "Economy",
}

// EmbedDraftRequest is the body of preview and test: a draft template and sample
// values. Unknown keys (a channelId, guildId, token ...) are rejected.
type EmbedDraftRequest struct {
	Template  embedtemplates.Config `json:"template"`
	Variables map[string]string     `json:"variables"`
}

// EmbedDTO is the rendered Discord embed, as the website shows it.
type EmbedDTO struct {
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Color       int             `json:"color"`
	Author      *EmbedAuthorDTO `json:"author,omitempty"`
	Thumbnail   *EmbedURLDTO    `json:"thumbnail,omitempty"`
	Image       *EmbedURLDTO    `json:"image,omitempty"`
	Footer      *EmbedFooterDTO `json:"footer,omitempty"`
	Timestamp   string          `json:"timestamp,omitempty"`
	Fields      []EmbedFieldDTO `json:"fields"`
}

type EmbedAuthorDTO struct {
	Name    string `json:"name"`
	IconURL string `json:"iconUrl,omitempty"`
}
type EmbedURLDTO struct {
	URL string `json:"url"`
}
type EmbedFooterDTO struct {
	Text    string `json:"text"`
	IconURL string `json:"iconUrl,omitempty"`
}
type EmbedFieldDTO struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type EmbedMetrics struct {
	FieldCount int `json:"fieldCount"`
	TotalText  int `json:"totalText"`
}

// EmbedDestination is the preflight view of a route's Discord destination.
type EmbedDestination struct {
	Configured     bool     `json:"configured"`
	ChannelID      string   `json:"channelId,omitempty"`
	ChannelName    string   `json:"channelName,omitempty"`
	Reachable      bool     `json:"reachable"`
	CanView        bool     `json:"canView"`
	CanSend        bool     `json:"canSend"`
	CanEmbed       bool     `json:"canEmbed"`
	CanReadHistory bool     `json:"canReadHistory"`
	Missing        []string `json:"missing,omitempty"`
}

// EmbedPreviewResponse is POST .../embed-templates/{routeKey}/preview.
type EmbedPreviewResponse struct {
	RouteKey         string `json:"routeKey"`
	RuntimeRendering string `json:"runtimeRendering"`
	// CustomRenderingSupported: the route's live publisher renders templates
	// (embedrender.RouteSupported), independent of the rollout flag.
	CustomRenderingSupported bool              `json:"customRenderingSupported"`
	Renderable               bool              `json:"renderable"`
	Reason                   string            `json:"reason,omitempty"`
	Embed                    *EmbedDTO         `json:"embed"`
	Metrics                  EmbedMetrics      `json:"metrics"`
	Warnings                 []string          `json:"warnings"`
	Destination              *EmbedDestination `json:"destination,omitempty"`
	// Conditional content (the same rules live events follow): lines and fields omitted because
	// a variable they reference is absent from the sample, and those variables' names.
	OmittedLines    int      `json:"omittedLines"`
	OmittedFields   int      `json:"omittedFields"`
	AbsentVariables []string `json:"absentVariables"`
}

// EmbedTestResponse is POST .../embed-templates/{routeKey}/test.
type EmbedTestResponse struct {
	Sent             bool             `json:"sent"`
	RouteKey         string           `json:"routeKey"`
	RuntimeRendering string           `json:"runtimeRendering"`
	Channel          EmbedTestChannel `json:"channel"`
	MessageID        string           `json:"messageId"`
	SentAt           string           `json:"sentAt"`
}

type EmbedTestChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// designerError is a customer-safe failure: a code from the list above and a
// message that never contains internal details.
type designerError struct{ code, message string }

func (e *designerError) Error() string { return e.message }

// decodeEmbedDraft reads exactly one EmbedDraftRequest, rejecting unknown keys.
func decodeEmbedDraft(w http.ResponseWriter, r *http.Request) (EmbedDraftRequest, bool) {
	var req EmbedDraftRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEmbedTemplateBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil {
		if _, tail := dec.Token(); !errors.Is(tail, io.EOF) {
			err = fmt.Errorf("trailing data")
			var tooBig *http.MaxBytesError
			if errors.As(tail, &tooBig) {
				err = tail
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeSaaSError(w, codePayloadTooLarge, "embed draft payload is too large")
			return req, false
		}
		writeSaaSError(w, codeEmbedTemplateInvalid, "invalid embed draft: expected {template, variables}")
		return req, false
	}
	return req, true
}

// renderEmbedDraft validates a draft exactly like a save would, checks the sample
// variables, and renders with the production path. It returns the preview
// (renderable=false with a reason when nothing would post) and the rendered embed.
func renderEmbedDraft(routeKey string, req EmbedDraftRequest, runtimeRendering string, at time.Time) (EmbedPreviewResponse, *discordgo.MessageEmbed, *designerError) {
	resp := EmbedPreviewResponse{RouteKey: routeKey, RuntimeRendering: runtimeRendering, CustomRenderingSupported: embedrender.RouteSupported(routeKey), Warnings: []string{}}
	cfg, err := embedtemplates.Validate(req.Template, routeKey)
	if err != nil {
		var invalid *embedtemplates.ValidationError
		if errors.As(err, &invalid) {
			issues := invalid.Issues
			if len(issues) > 5 {
				issues = append(issues[:5:5], "and more")
			}
			return resp, nil, &designerError{codeEmbedTemplateInvalid, "invalid embed template: " + strings.Join(issues, "; ")}
		}
		return resp, nil, &designerError{codeEmbedTemplateInvalid, "invalid embed template"}
	}
	if len(req.Variables) > maxDraftVariables {
		return resp, nil, &designerError{codeEmbedTemplateInvalid, "too many sample variables"}
	}
	approved := map[string]bool{}
	for _, v := range embedtemplates.Variables(routeKey) {
		approved[v] = true
	}
	names := make([]string, 0, len(req.Variables))
	for name := range req.Variables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !approved[name] {
			return resp, nil, &designerError{codeEmbedTemplateInvalid, "variable not approved for this route: " + truncateRunes(name, 40)}
		}
		if len([]rune(req.Variables[name])) > maxDraftVariableValue {
			return resp, nil, &designerError{codeEmbedTemplateInvalid, "sample value for " + name + " is too long"}
		}
	}

	if !resp.CustomRenderingSupported {
		resp.Warnings = append(resp.Warnings, "This route does not use custom templates in Discord yet - the preview shows the design only.")
	} else if runtimeRendering != runtimeRenderingEnabled {
		resp.Warnings = append(resp.Warnings, "Custom embeds are not enabled on this deployment yet (runtimeRendering NOT_ENABLED): live events still use the Champion default.")
	}

	emb, report, err := embedrender.RenderEventWithReport(cfg, routeKey, req.Variables, at)
	resp.OmittedLines, resp.OmittedFields, resp.AbsentVariables = report.OmittedLines, report.OmittedFields, append([]string{}, report.AbsentVariables...)
	if err != nil {
		resp.Reason = reasonNoContent
		if !cfg.Enabled {
			resp.Reason = reasonTemplateDisabled
		}
		return resp, nil, nil
	}
	resp.Renderable = true
	resp.Embed = embedToDTO(emb)
	resp.Metrics = EmbedMetrics{FieldCount: len(emb.Fields), TotalText: embedrender.TotalText(emb)}
	if report.OmittedFields > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d field(s) are hidden because their values are missing - live events omit them the same way.", report.OmittedFields))
	}
	if report.OmittedLines > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d line(s) are hidden because a variable they use is unavailable (%s) - live events omit them the same way.", report.OmittedLines, strings.Join(report.AbsentVariables, ", ")))
	}
	return resp, emb, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func embedToDTO(e *discordgo.MessageEmbed) *EmbedDTO {
	out := &EmbedDTO{Title: e.Title, Description: e.Description, Color: e.Color, Timestamp: e.Timestamp, Fields: []EmbedFieldDTO{}}
	if e.Author != nil {
		out.Author = &EmbedAuthorDTO{Name: e.Author.Name, IconURL: e.Author.IconURL}
	}
	if e.Thumbnail != nil {
		out.Thumbnail = &EmbedURLDTO{URL: e.Thumbnail.URL}
	}
	if e.Image != nil {
		out.Image = &EmbedURLDTO{URL: e.Image.URL}
	}
	if e.Footer != nil {
		out.Footer = &EmbedFooterDTO{Text: e.Footer.Text, IconURL: e.Footer.IconURL}
	}
	for _, f := range e.Fields {
		out.Fields = append(out.Fields, EmbedFieldDTO{Name: f.Name, Value: f.Value, Inline: f.Inline})
	}
	return out
}

// embedRouteLister reads an installation's routes; *repository.ChannelRouteRepository
// satisfies it.
type embedRouteLister interface {
	ListForInstallation(ctx context.Context, organizationID, installationID int64) ([]repository.ChannelRoute, error)
}

// embedDiscord is the Discord surface the designer needs; discordGuildVerifier
// satisfies it.
type embedDiscord interface {
	ListAllGuildChannels(guildID string) ([]discord.RawGuildChannel, error)
	Verify(guildID, channelID string) discord.Verification
	SendMessage(channelID string, msg *discordgo.MessageSend) (string, error)
}

// resolveEmbedDestination finds the route's channel from the installation's own
// Channel System V2 routes (tenant-scoped), requires that channel to belong to the
// installation's Discord guild, and checks the bot's permissions there.
func resolveEmbedDestination(ctx context.Context, routes embedRouteLister, d embedDiscord, organizationID, installationID int64, guildID, routeKey string) (EmbedDestination, *designerError) {
	var dest EmbedDestination
	list, err := routes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		return dest, &designerError{codeInternalError, "could not load channel routes"}
	}
	for _, rt := range list {
		if rt.RouteKey == routeKey && rt.ChannelID != "" {
			dest.Configured, dest.ChannelID = true, rt.ChannelID
		}
	}
	if !dest.Configured {
		return dest, &designerError{codeEmbedRouteNotConfigured, "no Discord channel is set up for this route yet - run Repair Champion Discord Layout or choose a channel"}
	}
	channels, err := d.ListAllGuildChannels(guildID)
	if err != nil {
		return dest, &designerError{codeDiscordUnavailable, "could not reach Discord right now"}
	}
	inGuild := false
	for _, ch := range channels {
		if ch.ID == dest.ChannelID {
			inGuild, dest.ChannelName = true, ch.Name
		}
	}
	if !inGuild {
		return dest, &designerError{codeEmbedChannelUnavailable, "the Discord channel for this route no longer exists in your server"}
	}
	v := d.Verify(guildID, dest.ChannelID)
	dest.Reachable = v.ChannelFound
	if !v.ChannelFound {
		return dest, &designerError{codeEmbedChannelUnavailable, "Champion cannot reach the Discord channel for this route"}
	}
	missing := map[string]bool{}
	for _, m := range v.Missing {
		missing[m] = true
	}
	dest.Missing = v.Missing
	dest.CanView, dest.CanSend, dest.CanEmbed, dest.CanReadHistory = !missing["View Channel"], !missing["Send Messages"], !missing["Embed Links"], !missing["Read Message History"]
	switch {
	case !dest.CanView || !dest.CanSend:
		return dest, &designerError{codeEmbedSendForbidden, "Champion needs View Channel and Send Messages in #" + dest.ChannelName}
	case !dest.CanEmbed:
		return dest, &designerError{codeEmbedLinksRequired, "Champion needs Embed Links in #" + dest.ChannelName}
	}
	return dest, nil
}

// embedTestLimiter enforces both test-send budgets: one send per 3 s per actor and
// installation, and 10 per minute per installation.
type embedTestLimiter struct{ perActor, perInstallation *saasRateLimiter }

func newEmbedTestLimiter() *embedTestLimiter {
	return &embedTestLimiter{perActor: newSaaSRateLimiter(3*time.Second, 1), perInstallation: newSaaSRateLimiter(time.Minute, 10)}
}

func (l *embedTestLimiter) allow(userID, installationID int64) bool {
	if l == nil {
		return true
	}
	inst := strconv.FormatInt(installationID, 10)
	if !l.perActor.Allow(inst + ":" + strconv.FormatInt(userID, 10)) {
		return false
	}
	return l.perInstallation.Allow(inst)
}

// sendEmbedTest renders the draft and sends it to the route's routed channel:
// validation, render, destination preflight, one Discord send - nothing else.
func sendEmbedTest(ctx context.Context, routes embedRouteLister, d embedDiscord, organizationID, installationID int64, guildID, routeKey string, req EmbedDraftRequest, runtimeRendering string, at time.Time) (EmbedTestResponse, *designerError) {
	out := EmbedTestResponse{RouteKey: routeKey, RuntimeRendering: runtimeRendering}
	if !embedrender.RouteSupported(routeKey) {
		return out, &designerError{codeEmbedCustomNotSupported, "this route does not use custom templates in Discord yet, so there is nothing to test"}
	}
	preview, emb, derr := renderEmbedDraft(routeKey, req, runtimeRendering, at)
	if derr != nil {
		return out, derr
	}
	if !preview.Renderable {
		msg := "this draft renders nothing Discord would show"
		if preview.Reason == reasonTemplateDisabled {
			msg = "this template is disabled, so live events use the Champion default - enable it to test"
		}
		return out, &designerError{codeEmbedTemplateNotRenderable, msg}
	}
	dest, derr := resolveEmbedDestination(ctx, routes, d, organizationID, installationID, guildID, routeKey)
	if derr != nil {
		return out, derr
	}
	messageID, err := d.SendMessage(dest.ChannelID, discord.EmbedTestMessage(embedRouteLabels[routeKey], emb))
	if err != nil {
		return out, &designerError{codeEmbedSendFailed, "Discord rejected the test message - check Champion's permissions in #" + dest.ChannelName}
	}
	out.Sent, out.MessageID, out.SentAt = true, messageID, at.UTC().Format(time.RFC3339)
	out.Channel = EmbedTestChannel{ID: dest.ChannelID, Name: dest.ChannelName}
	return out, nil
}

func writeDesignerError(w http.ResponseWriter, e *designerError) {
	writeSaaSError(w, e.code, e.message)
}

// handlePreviewEmbedTemplate is POST .../embed-templates/{routeKey}/preview. Any
// organization member may preview (it only renders; members can already read
// templates and routes). Nothing is saved or sent.
func (a *App) handlePreviewEmbedTemplate(w http.ResponseWriter, r *http.Request) {
	orgID, instID, routeKey, _, ok := a.embedTemplateContext(w, r, false, true)
	if !ok {
		return
	}
	req, ok := decodeEmbedDraft(w, r)
	if !ok {
		return
	}
	resp, _, derr := renderEmbedDraft(routeKey, req, a.runtimeRenderingFor(routeKey), embedDesignerNow())
	if derr != nil {
		writeDesignerError(w, derr)
		return
	}
	if a.SaaSChannelRoutes != nil && a.saasDiscordVerifier != nil && a.SaaSGuildConnections != nil && a.Guilds != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if _, guildID, errCode, _ := a.loadInstallationGuildSnowflake(ctx, orgID, instID); errCode == "" {
			dest, _ := resolveEmbedDestination(ctx, a.SaaSChannelRoutes, a.saasDiscordVerifier, orgID, instID, guildID, routeKey)
			resp.Destination = &dest
		}
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handleTestEmbedTemplate is POST .../embed-templates/{routeKey}/test (OWNER/ADMIN):
// render the UNSAVED draft and send it to the route's routed channel with a
// "design preview" notice and mentions disabled. Allowed for runtime-supported routes
// even while the rollout flag is off; runtimeRendering reports the truth either way.
func (a *App) handleTestEmbedTemplate(w http.ResponseWriter, r *http.Request) {
	orgID, instID, routeKey, userID, ok := a.embedTemplateContext(w, r, true, true)
	if !ok {
		return
	}
	req, ok := decodeEmbedDraft(w, r)
	if !ok {
		return
	}
	if a.SaaSChannelRoutes == nil || a.SaaSGuildConnections == nil || a.Guilds == nil {
		writeSaaSError(w, codeInternalError, "embed test service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}
	if !a.embedTestLimiter.allow(userID, instID) {
		writeSaaSError(w, codeEmbedRateLimited, "test messages are limited - wait a few seconds and try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, guildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, orgID, instID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}
	resp, derr := sendEmbedTest(ctx, a.SaaSChannelRoutes, a.saasDiscordVerifier, orgID, instID, guildID, routeKey, req, a.runtimeRenderingFor(routeKey), embedDesignerNow())
	if derr != nil {
		writeDesignerError(w, derr)
		return
	}
	a.recordEmbedTestAudit(ctx, orgID, instID, userID, routeKey, resp)
	writeSaaSJSON(w, http.StatusOK, resp)
}

// recordEmbedTestAudit writes embed_test_sent: ids, route and destination only -
// never the template, sample names or variables.
func (a *App) recordEmbedTestAudit(ctx context.Context, orgID, instID, userID int64, routeKey string, resp EmbedTestResponse) {
	slog.Info("component=saas_api", "event", "embed_test_sent", "organization_id", orgID, "installation_id", instID, "route_key", routeKey,
		"acting_user_id", userID, "channel_id", resp.Channel.ID, "message_id", resp.MessageID, "sent_at", resp.SentAt)
	if a.AdminAudit == nil {
		return
	}
	after, _ := json.Marshal(map[string]string{"routeKey": routeKey, "channelId": resp.Channel.ID, "messageId": resp.MessageID, "sentAt": resp.SentAt})
	actor := userID
	inst := instID
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.AdminAudit.Record(writeCtx, repository.AuditEntry{OrganizationID: orgID, InstallationID: &inst, ActorUserID: &actor, Action: "embed_test_sent", Target: routeKey, AfterState: after, Result: "SUCCESS"}); err != nil {
		slog.Warn("component=saas_api", "msg", "embed test audit write failed", "err", err.Error())
	}
}
