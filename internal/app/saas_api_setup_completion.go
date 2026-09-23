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

// Champion SaaS setup finalization + customer hub.
//
// The Go backend is the sole authority on whether an installation's setup
// is actually complete (section 1) - the website may request finalization,
// but every prerequisite is independently re-derived from persisted backend
// state (and one live, multi-channel Discord permission check), never
// trusted from a client-supplied boolean.

// --- finalize setup (sections 2-8) ------------------------------------------

// FinalizeSetupResponse is POST .../setup/complete's response (section 8) -
// reuses InstallationSummary rather than inventing a duplicate installation
// model.
type FinalizeSetupResponse struct {
	Completed    bool                `json:"completed"`
	Installation InstallationSummary `json:"installation"`
}

// handleFinalizeSetup is POST
// .../installations/{installationID}/setup/complete (section 2). Only
// OWNER/ADMIN may finalize. Verifies every prerequisite from persisted
// backend state (section 3) - Discord connection, bot installed, Nitrado
// connection, DayZ server selected, KILLFEED channel route configured, and
// a live multi-channel permission check across every unique configured
// route channel (sections 4/5) - before marking validationCompleted and
// moving the installation to READY. Idempotent (section 7): an
// already-READY installation short-circuits to success without re-running
// any check or touching its timestamps.
func (a *App) handleFinalizeSetup(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil || a.SaaSCredentials == nil || a.SaaSChannelRoutes == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
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

	// Section 7: idempotent - already READY is success, never re-verified,
	// never re-stamped, never downgraded by a repeat call.
	if inst.Status == repository.InstallationReady {
		writeSaaSJSON(w, http.StatusOK, FinalizeSetupResponse{Completed: true, Installation: a.buildInstallationSummary(ctx, organizationID, *inst)})
		return
	}

	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	// Section 3: "Discord connection exists" + "Champion bot is installed" -
	// resolved from the persisted guild connection, never a client claim.
	loaded, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}
	if !a.saasDiscordVerifier.Verify(discordGuildID, "").GuildFound {
		writeSaaSError(w, codeInstallationNotVerified, "Champion is not installed in this Discord server yet")
		return
	}

	// "Nitrado connection exists".
	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, organizationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get nitrado credential failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify Nitrado connection")
		return
	}
	if envelope == nil {
		writeSaaSError(w, codeInstallationNotVerified, "Nitrado is not connected for this organization yet")
		return
	}

	// "DayZ server is selected".
	if loaded.GameServerID == nil {
		writeSaaSError(w, codeInstallationNotVerified, "no DayZ server has been selected yet")
		return
	}

	// "channel configuration exists" / "KILLFEED route exists" - the new
	// route model (section 3). Only KILLFEED is globally required; no
	// other route is required merely because it exists in the default
	// blueprint (section 3's explicit instruction).
	routes, err := a.SaaSChannelRoutes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list channel routes failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load channel routes")
		return
	}
	hasKillfeed := false
	for _, rt := range routes {
		if rt.RouteKey == "KILLFEED" && rt.ChannelID != "" {
			hasKillfeed = true
			break
		}
	}
	if !hasKillfeed {
		writeSaaSError(w, codeInstallationNotVerified, "a killfeed channel route is required before setup can be completed")
		return
	}

	// "channel setup is complete" - the persisted progress flags, not
	// re-derived state, so a customer can't finalize by racing an
	// in-progress wizard.
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
	if !progress.DiscordCompleted || !progress.NitradoCompleted || !progress.ServerSelected || !progress.ChannelsCompleted {
		writeSaaSError(w, codeInstallationNotVerified, "setup is not yet complete")
		return
	}

	// "permission validation completed successfully" - a live, multi-channel
	// check across every UNIQUE configured route channel (sections 4/5):
	// several route keys sharing one channel are verified exactly once,
	// reusing the exact same blocking/warning mapping #13
	// (verify-permissions) already uses - View Channel/Send Messages block,
	// Embed Links/Read Message History stay warnings.
	channelIDs, err := a.SaaSChannelRoutes.ListDistinctChannelIDs(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list distinct channel route ids failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify channel permissions")
		return
	}
	for _, channelID := range channelIDs {
		verification := a.saasDiscordVerifier.Verify(discordGuildID, channelID)
		if !mapPermissionVerification(verification).OverallPass {
			writeSaaSError(w, codeInstallationNotVerified, "one or more configured channels are missing required Discord permissions (View Channel, Send Messages)")
			return
		}
	}

	// Section 6: UpdateSetupProgress/UpdateStatus each only stamp
	// completed_at/setup_completed_at the first time they see
	// ValidationCompleted=true/status=READY with a NULL timestamp (their
	// own SQL - see saas_installations_repository.go) - never overwritten
	// on a later, already-handled-by-the-idempotency-check-above call.
	merged := *progress
	merged.ValidationCompleted = true
	merged.CurrentStep = "COMPLETE"
	if !validateSetupProgressOrder(merged) {
		// Unreachable given the checks above, but never persist an
		// order-violating state regardless.
		writeSaaSError(w, codeInternalError, "could not finalize setup")
		return
	}
	if err := a.SaaSInstallations.UpdateSetupProgress(ctx, organizationID, installationID, merged); err != nil {
		slog.Warn("component=saas_api", "msg", "update setup progress failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not finalize setup")
		return
	}
	if err := a.SaaSInstallations.UpdateStatus(ctx, organizationID, installationID, repository.InstallationReady); err != nil {
		slog.Warn("component=saas_api", "msg", "update installation status failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not finalize setup")
		return
	}

	updated, err := a.SaaSInstallations.GetScoped(ctx, organizationID, installationID)
	if err != nil || updated == nil {
		slog.Warn("component=saas_api", "msg", "reload installation after finalize failed", "err", err)
		writeSaaSError(w, codeInternalError, "could not reload installation")
		return
	}

	slog.Info("component=saas_api", "event", "saas_setup_finalized", "installation_id", installationID)

	writeSaaSJSON(w, http.StatusOK, FinalizeSetupResponse{Completed: true, Installation: a.buildInstallationSummary(ctx, organizationID, *updated)})
}

// --- customer hub (section 9) ------------------------------------------------

// HubDiscordSummary is the hub's minimal Discord connection view.
type HubDiscordSummary struct {
	GuildName    string `json:"guildName,omitempty"`
	GuildIcon    string `json:"guildIcon,omitempty"`
	BotInstalled bool   `json:"botInstalled"`
}

// HubSummary is GET .../hub's response (section 9) - everything the
// customer landing page needs in one call. Reuses OrganizationSummary/
// SubscriptionSummary/InstallationSummary/DayZServerSummary/ChannelRouteInfo
// wherever possible (never a duplicate installation/organization model -
// same directive as FinalizeSetupResponse above). Never includes a Nitrado
// token, encrypted credential, Discord bot token, WEBSITE_API_SECRET, or
// database detail - none of the reused DTOs carry any of those (enforced by
// TestNoSensitiveFieldsInAPIResponses).
type HubSummary struct {
	Organization  OrganizationSummary         `json:"organization"`
	Subscription  *SubscriptionSummary        `json:"subscription,omitempty"`
	Installation  InstallationSummary         `json:"installation"`
	Discord       *HubDiscordSummary          `json:"discord,omitempty"`
	DayZServer    *DayZServerSummary          `json:"dayzServer,omitempty"`
	ChannelRoutes map[string]ChannelRouteInfo `json:"channelRoutes"`
	Settings      InstallationGeneralSettings `json:"settings"`
}

// handleGetInstallationHub is GET .../installations/{installationID}/hub
// (section 9). Any member may read. Every child section is best-effort and
// omitted (never a hard failure) if that piece isn't configured yet - a
// customer mid-setup can still open something without a 404, though the
// website is expected to route a non-READY installation back to onboarding
// per section 15/16, not rely on the hub to enforce that.
func (a *App) handleGetInstallationHub(w http.ResponseWriter, r *http.Request) {
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
	role, ok := a.requireOrganizationMember(w, r, organizationID, user.ID)
	if !ok {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSInstallations == nil || a.SaaSOrganizations == nil || a.SaaSGuildConnections == nil || a.SaaSChannelRoutes == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
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

	org, err := a.getOrganizationRow(ctx, organizationID)
	if err != nil || org == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}

	hub := HubSummary{
		Organization:  toOrganizationSummary(*org, role),
		Installation:  a.buildInstallationSummary(ctx, organizationID, *inst),
		ChannelRoutes: map[string]ChannelRouteInfo{},
	}

	if a.SaaSSubscriptions != nil {
		if sub, err := a.SaaSSubscriptions.GetForOrganization(ctx, organizationID); err == nil && sub != nil {
			hub.Subscription = &SubscriptionSummary{
				Plan: sub.Plan, Status: sub.Status,
				TrialEndsAt: nullableTimeStr(sub.TrialEndsAt), CurrentPeriod: nullableTimeStr(sub.CurrentPeriodEnd),
			}
		}
	}

	// discordGuildID is resolved best-effort here (not via
	// loadInstallationGuildSnowflake, which hard-fails if the guild
	// connection is missing) - the hub must still return everything else
	// it has for an installation that hasn't connected Discord yet.
	discordGuildID := ""
	if conn, err := a.SaaSGuildConnections.GetScoped(ctx, organizationID, inst.DiscordGuildConnectionID); err == nil && conn != nil {
		hub.Discord = &HubDiscordSummary{GuildName: conn.GuildName, GuildIcon: conn.GuildIcon, BotInstalled: conn.BotInstalled}
		if a.Guilds != nil {
			if guildRecord, err := a.Guilds.GetGuildByID(ctx, conn.GuildID); err == nil && guildRecord != nil {
				discordGuildID = guildRecord.DiscordGuildID
			}
		}
	}

	if inst.GameServerID != nil && a.SaaSServers != nil {
		if srv, err := a.SaaSServers.GetScoped(ctx, organizationID, *inst.GameServerID); err == nil && srv != nil {
			summary := toDayZServerSummary(*srv)
			hub.DayZServer = &summary
		}
	}

	hub.ChannelRoutes = a.buildChannelRoutesResponse(ctx, organizationID, installationID, discordGuildID).Routes

	if a.SaaSInstallations != nil {
		if settings, err := a.SaaSInstallations.GetSettings(ctx, organizationID, installationID); err == nil && settings != nil {
			hub.Settings = toInstallationGeneralSettings(*settings)
		}
	}

	writeSaaSJSON(w, http.StatusOK, hub)
}

// --- general settings (sections 10-11) --------------------------------------

// InstallationGeneralSettings is the customer-editable general settings
// surface - GET/PUT .../settings's shape, and the hub's "settings" field.
// Deliberately separate from the channel-specific settings surfaces
// (InstallationChannelSettings/ChannelRoutesResponse) - never overloaded
// onto them (section 10).
type InstallationGeneralSettings struct {
	Timezone             string `json:"timezone"`
	DistanceUnit         string `json:"distanceUnit"`
	OnlineDisplayEnabled bool   `json:"onlineDisplayEnabled"`
	LeaderboardEnabled   bool   `json:"leaderboardEnabled"`
}

func toInstallationGeneralSettings(s repository.InstallationSettings) InstallationGeneralSettings {
	return InstallationGeneralSettings{
		Timezone:             s.Timezone,
		DistanceUnit:         s.DistanceUnit,
		OnlineDisplayEnabled: s.OnlineDisplayEnabled,
		LeaderboardEnabled:   s.LeaderboardEnabled,
	}
}

// validDistanceUnits is the only two values this SaaS surface currently
// persists (section 10 audit: this column exists but isn't yet wired into
// killfeed embed rendering, which computes distance in meters natively -
// internal/presentation/story_engine.go - so this setting is forward-
// looking, not yet runtime-authoritative).
var validDistanceUnits = map[string]bool{"METERS": true, "FEET": true}

// handleGetInstallationSettings is GET
// .../installations/{installationID}/settings (section 10). Any member may
// read.
func (a *App) handleGetInstallationSettings(w http.ResponseWriter, r *http.Request) {
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
	settings, err := a.SaaSInstallations.GetSettings(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get installation settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load settings")
		return
	}
	if settings == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toInstallationGeneralSettings(*settings))
}

// handleSaveInstallationSettings is PUT
// .../installations/{installationID}/settings (section 10/11). Only
// OWNER/ADMIN may save. Timezone/distance unit/display toggles are
// deliberately non-critical (section 13) - this never touches setup
// progress, validation, or installation status, unlike the channel-routing
// endpoints.
func (a *App) handleSaveInstallationSettings(w http.ResponseWriter, r *http.Request) {
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

	var req InstallationGeneralSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.Timezone = strings.TrimSpace(req.Timezone)
	req.DistanceUnit = strings.ToUpper(strings.TrimSpace(req.DistanceUnit))
	if req.Timezone == "" {
		writeSaaSError(w, codeInvalidRequest, "timezone is required")
		return
	}
	if !validDistanceUnits[req.DistanceUnit] {
		writeSaaSError(w, codeInvalidRequest, "distanceUnit must be METERS or FEET")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	current, err := a.SaaSInstallations.GetSettings(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get installation settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load settings")
		return
	}
	if current == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}

	// Mutate only the general fields - channel IDs/setup source/category
	// stay exactly as they were.
	current.Timezone = req.Timezone
	current.DistanceUnit = req.DistanceUnit
	current.OnlineDisplayEnabled = req.OnlineDisplayEnabled
	current.LeaderboardEnabled = req.LeaderboardEnabled

	if err := a.SaaSInstallations.UpdateSettings(ctx, organizationID, installationID, *current); err != nil {
		slog.Warn("component=saas_api", "msg", "update installation settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save settings")
		return
	}

	writeSaaSJSON(w, http.StatusOK, toInstallationGeneralSettings(*current))
}
