package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion SaaS setup Step 5 - full channel routing.
//
// Upgrades the four-field installation_settings channel model to a scalable
// feature -> Discord channel table (installation_channel_routes, migration
// 0027): one row per (installation, route_key), so adding a future route
// never means another schema column. installation_settings' four legacy
// columns and their GET/PUT .../channels endpoints (saas_api_channels.go)
// are untouched and keep working - this file only adds the new
// GET/PUT .../channel-routes surface and upgrades POST .../channels/auto-setup
// to configure the complete blueprint instead of just four channels.

// --- route blueprint (sections 1-4/10/17) -----------------------------------

// RouteRequirement classifies how essential a route is to Champion's
// current runtime behavior - audited against the actual bot code (section
// 17), never assumed from the blueprint alone.
type RouteRequirement string

const (
	// RouteRequired: Champion cannot function as configured without it.
	RouteRequired RouteRequirement = "REQUIRED"
	// RouteOptional: no runtime publisher currently posts here (the
	// blueprint still reserves the channel for when one is built), or the
	// feature is implemented but genuinely non-essential.
	RouteOptional RouteRequirement = "OPTIONAL"
	// RouteFeatureDependent: fully implemented, but only matters for guilds
	// that actually use that specific feature.
	RouteFeatureDependent RouteRequirement = "FEATURE_DEPENDENT"
)

// championRouteDefault is one entry in Champion's default channel blueprint.
type championRouteDefault struct {
	Key         string
	ChannelName string
	Requirement RouteRequirement
}

// championManagedCategoryName is Champion's recommended default category
// (section 1/8). All sixteen default channels are created under it; the API
// deliberately keeps category resolution (ensureManagedCategory) separate
// from channel resolution so a later product change to split channels
// across multiple categories doesn't require a schema change (section 8).
const championManagedCategoryName = "CHAMPION KILLFEED"

// championRouteBlueprint is Champion's full sixteen-route default channel
// structure (sections 1-4). Every entry's requirement/runtime-status comment
// reflects an actual codebase audit (section 17) - never invented:
//
//   - ALREADY_IMPLEMENTED entries name the exact GuildSetup field/publisher
//     that already posts to a channel serving this purpose today.
//   - NOT_IMPLEMENTED_YET entries have no runtime publisher at all; the
//     blueprint still creates/reserves the channel (section 3 asks for all
//     sixteen), but nothing posts there until that feature is built.
var championRouteBlueprint = []championRouteDefault{
	// ALREADY_IMPLEMENTED: internal/discord/killfeed.go KillfeedPublisher.
	// PublishKill (GuildSetup.KillfeedChannelID). PvP kill/death/special-kill
	// feed - the bot cannot function as a killfeed bot without this.
	{"KILLFEED", "killfeed", RouteRequired},
	// NOT_IMPLEMENTED_YET: no infected/environment/PvE event type exists in
	// internal/killfeed/event.go; no publisher posts here yet.
	{"PVE_FEED", "pvefeed", RouteOptional},
	// ALREADY_IMPLEMENTED: internal/discord/public_panels.go
	// PublicPanelHandler.handleLink (GuildSetup.LinkPanelChannelID). Matters
	// only for guilds that use Discord<->game identity linking.
	{"LINK_GAMERTAG", "link-gamertag", RouteFeatureDependent},
	// ALREADY_IMPLEMENTED: internal/discord/public_panels.go handleMyStats/
	// handleSearch (GuildSetup.PlayerStatsChannelID) - an on-demand "My
	// Stats / Search Player" lookup panel, distinct from the auto-refreshing
	// leaderboard below.
	{"STATS_LEADERBOARDS", "stats-leaderboards", RouteOptional},
	// ALREADY_IMPLEMENTED: internal/discord/leaderboard_scheduler.go
	// LeaderboardScheduler (GuildSetup.LeaderboardsChannelID) - edits one
	// persistent ranked-leaderboard message on a fixed refresh cycle.
	{"AUTO_LEADERBOARD", "auto-leaderboard", RouteOptional},
	// IMPLEMENTED / RUNTIME ROUTED: internal/discord/hitfeed.go
	// HitfeedPublisher publishes aggregated, rate-capped PLAYER_HIT cards to
	// this route. There is no legacy channel and no KILLFEED fallback: with no
	// route configured hits are simply not published.
	{"HITFEED", "hitfeed", RouteOptional},
	// NOT_IMPLEMENTED_YET (as a channel feed): internal/discord/
	// competitive_commands.go BountyCommandHandler only replies to /bounty
	// ephemerally; there is no standing public bounty-board channel post.
	{"BOUNTY", "bounty", RouteOptional},
	// NOT_IMPLEMENTED_YET (as its own channel): bounty progression today
	// renders as a "MOST WANTED" section inside the shared live-panels
	// message (internal/discord/panels), not a dedicated channel.
	{"BOUNTY_TRACKING", "bounty-tracking", RouteOptional},
	// NOT_IMPLEMENTED_YET: no heatmap code anywhere in the repo.
	{"HEATMAPS", "heatmaps", RouteOptional},
	// NOT_IMPLEMENTED_YET: no in-game economy/credits system exists
	// (internal/entitlements is a subscription-tier feature-flag map, not
	// an in-game economy).
	{"ECONOMY", "economy", RouteOptional},
	// NOT_IMPLEMENTED_YET: no casino code anywhere in the repo.
	{"CASINO", "casino", RouteOptional},
	// NOT_IMPLEMENTED_YET: no shop/store code anywhere in the repo.
	{"SHOP", "shop", RouteOptional},
	// IMPLEMENTED / RUNTIME ROUTED: internal/discord/connections.go
	// ConnectionsPublisher publishes bounded, batched connect/disconnect
	// notices to this route. No legacy channel and no fallback: with no route
	// nothing is published. The unrelated voice counter
	// (GuildSetup.OnlinePlayersChannelID) is untouched.
	{"CONNECTIONS", "connections", RouteOptional},
	// NOT_IMPLEMENTED_YET: no building/base-related event type or
	// publisher exists.
	{"BUILD_FEED", "build-feed", RouteOptional},
	// NOT_IMPLEMENTED_YET: no separate moderation-alert publisher exists,
	// distinct from the diagnostic ADM monitor below.
	{"ADMIN_ALERTS", "admin-alerts", RouteOptional},
	// ALREADY_IMPLEMENTED: internal/discord/adm_monitor.go
	// ADMMonitorPublisher (GuildSetup.ADMMonitorChannelID) - ADM download
	// health/diagnostic embeds. Matters only for guilds relying on ADM
	// health monitoring, not every installation.
	{"ADMIN_LOGS", "admin-logs", RouteFeatureDependent},
}

// championRouteKeys is the fixed set of valid route_key values - never an
// arbitrary client-supplied string (mirrors setupSteps' validation pattern
// in saas_api_installations.go).
var championRouteKeys = func() map[string]bool {
	out := make(map[string]bool, len(championRouteBlueprint))
	for _, r := range championRouteBlueprint {
		out[r.Key] = true
	}
	return out
}()

// --- DTOs (section 6) -------------------------------------------------------

// ChannelRouteInfo is one configured route's customer-safe view - the value
// type of ChannelRoutesResponse.Routes, keyed by route_key.
type ChannelRouteInfo struct {
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName,omitempty"`
	// ManagedByChampion is true only for a channel Champion itself created
	// or reused via one-click auto-setup; false once a customer explicitly
	// points a route at a channel via a manual save (section 15) - any
	// future "reset Champion channels" feature must only ever touch
	// channels where this is true (section 16).
	ManagedByChampion bool `json:"managedByChampion"`
}

// ChannelRoutesResponse is the response of GET .../channel-routes and
// PUT .../channel-routes (section 6/11) - a keyed map, not an array, so the
// website never has to search for a route by key.
type ChannelRoutesResponse struct {
	Routes map[string]ChannelRouteInfo `json:"routes"`
}

// ChannelCategorySummary is the Champion-managed category auto-setup
// created or reused.
type ChannelCategorySummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AutoSetupChannelsResponse is POST .../channels/auto-setup's response
// (section 13). Configured=false with a Reason is a safe, expected outcome
// the website must branch on (section 5/14) - never a raw Discord error and
// never the standard {"error":...} envelope, since neither case is a
// failure to retry blindly, they're states the UI renders directly ("grant
// Manage Channels" / "you already customized this").
type AutoSetupChannelsResponse struct {
	Configured bool                        `json:"configured"`
	Reason     string                      `json:"reason,omitempty"`
	Category   *ChannelCategorySummary     `json:"category,omitempty"`
	Routes     map[string]ChannelRouteInfo `json:"routes,omitempty"`
}

type autoSetupChannelsRequest struct {
	// Force, when true, lets auto-setup proceed even though the
	// installation already has customer-owned routing (section 14) - an
	// explicit, deliberate "restore Champion defaults" action (section 9),
	// never the default.
	Force bool `json:"force"`
}

type saveChannelRoutesRequest struct {
	// Routes is a partial merge keyed by route_key (section 9 - never
	// requires all sixteen): a present key with a non-empty channel ID
	// upserts that route; a present key with an empty string explicitly
	// disables/removes it; an absent key is left untouched.
	Routes map[string]string `json:"routes"`
}

// --- shared helpers ----------------------------------------------------------

// hasCustomChannelConfiguration reports whether s represents a legacy
// four-field channel configuration auto-setup must not silently overwrite
// (section 14): any channel already set, UNLESS the only thing that ever
// set them was auto-setup itself (source "AUTO"). Kept for the legacy
// GET/PUT .../channels surface (saas_api_channels.go), which this file's
// auto-setup still consults so a customer who customized via that older
// endpoint is protected exactly like one who used the new routes endpoint.
func hasCustomChannelConfiguration(s repository.InstallationSettings) bool {
	if s.ChannelSetupSource == "AUTO" {
		return false
	}
	return s.KillfeedChannelID != "" || s.LeaderboardChannelID != "" || s.PlayerStatusChannelID != "" || s.AdminLogChannelID != ""
}

// hasCustomerOwnedRoutes reports whether any already-configured route was
// NOT created/reused by Champion itself (section 14/15) - the routes-table
// half of the "do not silently overwrite" guard.
func hasCustomerOwnedRoutes(routes []repository.ChannelRoute) bool {
	for _, r := range routes {
		if !r.ManagedByChampion {
			return true
		}
	}
	return false
}

// buildChannelRoutesResponse loads installationID's persisted routes and
// best-effort enriches each with its current Discord channel name (never
// fails the response if that lookup fails - channelId is always accurate
// either way).
func (a *App) buildChannelRoutesResponse(ctx context.Context, organizationID, installationID int64, discordGuildID string) ChannelRoutesResponse {
	routes, err := a.SaaSChannelRoutes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list channel routes failed", "err", err.Error())
		return ChannelRoutesResponse{Routes: map[string]ChannelRouteInfo{}}
	}
	names := map[string]string{}
	if discordGuildID != "" && a.saasDiscordVerifier != nil {
		if channels, err := a.saasDiscordVerifier.ListGuildChannels(discordGuildID); err == nil {
			for _, c := range channels {
				names[c.ID] = c.Name
			}
		}
	}
	out := make(map[string]ChannelRouteInfo, len(routes))
	for _, r := range routes {
		out[r.RouteKey] = ChannelRouteInfo{ChannelID: r.ChannelID, ChannelName: names[r.ChannelID], ManagedByChampion: r.ManagedByChampion}
	}
	return ChannelRoutesResponse{Routes: out}
}

// --- get/save channel routes (section 11) -----------------------------------

// handleListChannelRoutes is GET .../installations/{installationID}/channel-routes
// (section 11). Any member may read.
func (a *App) handleListChannelRoutes(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSChannelRoutes == nil || a.SaaSGuildConnections == nil || a.Guilds == nil || a.SaaSInstallations == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	_, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	writeSaaSJSON(w, http.StatusOK, a.buildChannelRoutesResponse(ctx, organizationID, installationID, discordGuildID))
}

// handleSaveChannelRoutes is PUT .../installations/{installationID}/channel-routes
// (section 11/12). Only OWNER/ADMIN may save. Every supplied non-empty
// channel ID is re-verified live against the installation's own Discord
// guild before anything is persisted (section 12) - a cross-guild ID is
// rejected outright. Every route this call touches is marked customer-owned
// (managedByChampion=false), even if the chosen channel happens to match one
// Champion previously created (section 9/15) - an explicit customer
// selection is never re-labeled as Champion-managed.
func (a *App) handleSaveChannelRoutes(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSChannelRoutes == nil || a.SaaSGuildConnections == nil || a.Guilds == nil || a.SaaSInstallations == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	var req saveChannelRoutesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	if len(req.Routes) == 0 {
		writeSaaSError(w, codeInvalidRequest, "routes is required")
		return
	}

	normalized := make(map[string]string, len(req.Routes))
	for key, value := range req.Routes {
		key = strings.ToUpper(strings.TrimSpace(key))
		if !championRouteKeys[key] {
			writeSaaSError(w, codeInvalidRequest, "unknown route key: "+key)
			return
		}
		normalized[key] = strings.TrimSpace(value)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	loaded, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	channels, err := a.saasDiscordVerifier.ListGuildChannels(discordGuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list guild channels failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not verify Discord channels")
		return
	}
	valid := make(map[string]bool, len(channels))
	for _, c := range channels {
		valid[c.ID] = true
	}
	for _, value := range normalized {
		if value != "" && !valid[value] {
			// Section 12: cross-guild (or nonexistent, or non-text-capable)
			// channel IDs are never accepted.
			writeSaaSError(w, codeInvalidRequest, "one or more selected channels do not belong to this Discord server")
			return
		}
	}

	existing, err := a.SaaSChannelRoutes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list channel routes failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load channel routes")
		return
	}

	// Section 10: KILLFEED is required - validate the WOULD-BE merged state
	// before persisting anything, so a request that would clear it is
	// rejected atomically rather than partially applied.
	oldKillfeed := ""
	merged := make(map[string]string, len(existing))
	for _, rt := range existing {
		merged[rt.RouteKey] = rt.ChannelID
		if rt.RouteKey == "KILLFEED" {
			oldKillfeed = rt.ChannelID
		}
	}
	for key, value := range normalized {
		merged[key] = value
	}
	if merged["KILLFEED"] == "" {
		writeSaaSError(w, codeInvalidRequest, "a killfeed channel is required")
		return
	}
	// Setup-completion task, section 13: changing KILLFEED's channel on an
	// already-READY installation invalidates its permission verification -
	// stale results for the OLD channel can never be trusted for a new one.
	criticalChange := loaded.Status == repository.InstallationReady && oldKillfeed != "" && oldKillfeed != merged["KILLFEED"]

	for key, value := range normalized {
		if value == "" {
			if err := a.SaaSChannelRoutes.DeleteRoute(ctx, organizationID, installationID, key); err != nil {
				slog.Warn("component=saas_api", "msg", "delete channel route failed", "err", err.Error())
				writeSaaSError(w, codeInternalError, "could not save channel routes")
				return
			}
			continue
		}
		if err := a.SaaSChannelRoutes.UpsertRoute(ctx, organizationID, installationID, key, value, false); err != nil {
			slog.Warn("component=saas_api", "msg", "upsert channel route failed", "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not save channel routes")
			return
		}
	}

	a.completeChannelsStep(ctx, organizationID, installationID, loaded.Status, criticalChange)

	slog.Info("component=saas_api", "event", "saas_channel_routes_saved", "installation_id", installationID, "routes_touched", len(normalized))

	writeSaaSJSON(w, http.StatusOK, a.buildChannelRoutesResponse(ctx, organizationID, installationID, discordGuildID))
}

// --- one-click channel auto-setup (sections 1-7) ----------------------------

// handleAutoSetupChannels is POST
// .../installations/{installationID}/channels/auto-setup (section 2/7). Only
// OWNER/ADMIN may run it. Resolves the installation's exact Discord guild,
// verifies the bot is installed and holds Manage Channels, then
// creates-or-reuses the Champion default category and all sixteen default
// channels - entirely ID-driven once they exist (section 4), so repeat
// calls are idempotent (section 3) and never produce "-1"/"-2" duplicates.
func (a *App) handleAutoSetupChannels(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil || a.SaaSChannelRoutes == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	var req autoSetupChannelsRequest
	if r.Body != nil {
		// force is optional - an empty/absent body just means force=false,
		// never a request-format error.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	loaded, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	if !a.saasDiscordVerifier.Verify(discordGuildID, "").GuildFound {
		writeSaaSError(w, codeDiscordUnavailable, "Champion is not installed in this Discord server")
		return
	}

	settings, err := a.SaaSInstallations.GetSettings(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get channel settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load channel settings")
		return
	}
	if settings == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	existingRoutes, err := a.SaaSChannelRoutes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list channel routes failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load channel routes")
		return
	}

	// Section 14: never silently overwrite a customer's own customization,
	// whether it was made via the legacy four-field endpoint or the new
	// routes endpoint.
	if !req.Force && (hasCustomChannelConfiguration(*settings) || hasCustomerOwnedRoutes(existingRoutes)) {
		writeSaaSJSON(w, http.StatusOK, AutoSetupChannelsResponse{Configured: false, Reason: "CUSTOM_CONFIGURATION_EXISTS"})
		return
	}

	// Section 5: verify Champion can actually create channels here before
	// attempting anything.
	perms, err := a.saasDiscordVerifier.GuildPermissions(discordGuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get guild permissions failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not verify Discord permissions")
		return
	}
	if perms&discordgo.PermissionManageChannels == 0 {
		writeSaaSJSON(w, http.StatusOK, AutoSetupChannelsResponse{Configured: false, Reason: "MISSING_MANAGE_CHANNELS"})
		return
	}

	allChannels, err := a.saasDiscordVerifier.ListAllGuildChannels(discordGuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list all guild channels failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not load Discord channels")
		return
	}

	category, err := a.ensureManagedCategory(discordGuildID, allChannels, settings.ChampionCategoryID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "ensure managed category failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not set up Champion's channel category")
		return
	}

	persisted := make(map[string]string, len(existingRoutes))
	for _, rt := range existingRoutes {
		persisted[rt.RouteKey] = rt.ChannelID
	}

	routes := make(map[string]ChannelRouteInfo, len(championRouteBlueprint))
	for _, spec := range championRouteBlueprint {
		ch, err := a.ensureManagedChannel(discordGuildID, allChannels, category.ID, spec.ChannelName, persisted[spec.Key])
		if err != nil {
			slog.Warn("component=saas_api", "msg", "ensure managed channel failed", "err", err.Error(), "route", spec.Key)
			writeSaaSError(w, codeDiscordUnavailable, "could not set up Champion's Discord channels")
			return
		}
		if err := a.SaaSChannelRoutes.UpsertRoute(ctx, organizationID, installationID, spec.Key, ch.ID, true); err != nil {
			slog.Warn("component=saas_api", "msg", "upsert channel route failed", "err", err.Error(), "route", spec.Key)
			writeSaaSError(w, codeInternalError, "could not save channel routes")
			return
		}
		routes[spec.Key] = ChannelRouteInfo{ChannelID: ch.ID, ChannelName: ch.Name, ManagedByChampion: true}
	}

	// Mirror the three routes with a direct legacy equivalent back onto
	// installation_settings (section 5 - "keep old fields operational until
	// all runtime consumers have migrated"): a caller still using
	// GET .../channels must keep seeing accurate data after an auto-setup
	// run, not stale/empty legacy columns.
	updatedSettings := *settings
	updatedSettings.KillfeedChannelID = routes["KILLFEED"].ChannelID
	updatedSettings.LeaderboardChannelID = routes["STATS_LEADERBOARDS"].ChannelID
	updatedSettings.AdminLogChannelID = routes["ADMIN_LOGS"].ChannelID
	updatedSettings.ChannelSetupSource = "AUTO"
	updatedSettings.ChampionCategoryID = category.ID
	if err := a.SaaSInstallations.UpdateSettings(ctx, organizationID, installationID, updatedSettings); err != nil {
		slog.Warn("component=saas_api", "msg", "update channel settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save channel settings")
		return
	}

	// Setup-completion task, section 13: force=true restoring defaults over
	// customer-owned routing on an already-READY installation is exactly the
	// "critical config change" case - never silently keeps READY against a
	// channel that was never actually verified.
	criticalChange := loaded.Status == repository.InstallationReady && persisted["KILLFEED"] != "" && persisted["KILLFEED"] != routes["KILLFEED"].ChannelID
	a.completeChannelsStep(ctx, organizationID, installationID, loaded.Status, criticalChange)

	slog.Info("component=saas_api", "event", "saas_channels_auto_setup", "installation_id", installationID, "category_id", category.ID, "route_count", len(routes))

	writeSaaSJSON(w, http.StatusOK, AutoSetupChannelsResponse{
		Configured: true,
		Category:   &ChannelCategorySummary{ID: category.ID, Name: category.Name},
		Routes:     routes,
	})
}

// ensureManagedCategory resolves Champion's managed category, preferring the
// persisted ID (authoritative - section 4), falling back to a name-based
// scan for recovery, and creating it only if neither is found (sections 2-4).
func (a *App) ensureManagedCategory(guildID string, channels []discord.RawGuildChannel, persistedCategoryID string) (discord.RawGuildChannel, error) {
	if persistedCategoryID != "" {
		for _, ch := range channels {
			if ch.ID == persistedCategoryID && ch.Type == discordgo.ChannelTypeGuildCategory {
				return ch, nil
			}
		}
		// Persisted ID no longer resolves (e.g. deleted in Discord) - fall
		// through to name-based recovery below.
	}
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildCategory && strings.EqualFold(strings.TrimSpace(ch.Name), championManagedCategoryName) {
			return ch, nil
		}
	}
	created, err := a.saasDiscordVerifier.CreateGuildCategory(guildID, championManagedCategoryName)
	if err != nil {
		return discord.RawGuildChannel{}, err
	}
	return *created, nil
}

// ensureManagedChannel resolves one default channel under categoryID, same
// ID-first-then-name-then-create precedence as ensureManagedCategory
// (sections 2-4). Reusing an existing channel never requires it to already
// sit under categoryID for the persisted-ID path (a customer may have moved
// it in Discord - the ID is still authoritative), but the name-based
// recovery scan does check the category, so recovery cannot accidentally
// adopt an unrelated same-named channel elsewhere in the guild.
func (a *App) ensureManagedChannel(guildID string, channels []discord.RawGuildChannel, categoryID, name, persistedChannelID string) (discord.RawGuildChannel, error) {
	if persistedChannelID != "" {
		for _, ch := range channels {
			if ch.ID == persistedChannelID && ch.Type == discordgo.ChannelTypeGuildText {
				return ch, nil
			}
		}
	}
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildText && ch.ParentID == categoryID && strings.EqualFold(ch.Name, name) {
			return ch, nil
		}
	}
	created, err := a.saasDiscordVerifier.CreateGuildTextChannel(guildID, name, categoryID)
	if err != nil {
		return discord.RawGuildChannel{}, err
	}
	return *created, nil
}
