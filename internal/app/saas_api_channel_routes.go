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

// The route vocabulary and the default Discord layout live in
// saas_channel_layout.go (Champion Channel System V2).

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
	Key  string `json:"key,omitempty"` // LIVE | HUB | STAFF
}

// AutoSetupChannelsResponse is POST .../channels/auto-setup's response
// (section 13). Configured=false with a Reason is a safe, expected outcome
// the website must branch on (section 5/14) - never a raw Discord error and
// never the standard {"error":...} envelope, since neither case is a
// failure to retry blindly, they're states the UI renders directly ("grant
// Manage Channels" / "you already customized this").
type AutoSetupChannelsResponse struct {
	Configured bool   `json:"configured"`
	Reason     string `json:"reason,omitempty"`
	// Category is the LIVE category, kept for callers that read one category.
	Category   *ChannelCategorySummary     `json:"category,omitempty"`
	Categories []ChannelCategorySummary    `json:"categories,omitempty"`
	Routes     map[string]ChannelRouteInfo `json:"routes,omitempty"`
	// Destinations is the per-channel setup and verification report,
	// including destinations that were skipped (BLOCKED/BROKEN/DISABLED).
	Destinations []ChannelDestinationReport `json:"destinations,omitempty"`
	// Retirable lists Champion-managed channels/categories no route uses any
	// more. Champion never deletes them on its own (see .../channels/cleanup).
	Retirable []RetirableChannel `json:"retirable,omitempty"`
	// Summary condenses what this run did.
	Summary *LayoutSummary `json:"summary,omitempty"`
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
	// requires every route): a present key with a non-empty channel ID
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
// verifies the bot is installed and holds Manage Channels, then applies
// Champion's V2 layout (saas_channel_layout.go): only destinations with a
// working producer get a channel, every created channel is verified, and
// repeat calls reuse channels by ID/name so they never produce "-1"/"-2"
// duplicates.
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

	// Panel sync and per-channel verification run inside this request.
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	_, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
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

	resp, err := a.runChannelLayout(ctx, organizationID, installationID, discordGuildID, false)
	if err != nil && resp.Reason == "" {
		slog.Warn("component=saas_api", "msg", "apply channel layout failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not set up Champion's Discord channels")
		return
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// syncRoutedPanelsNow posts or restores the persistent panels for freshly
// written routes before auto-setup verifies them. Both syncs share
// RoutePanels' lock with their background loops, so this can never post a
// second copy of a panel.
func (a *App) syncRoutedPanelsNow(ctx context.Context) {
	a.ChannelRoutes.InvalidateAll()
	a.RouteSyncer.SyncOnce(ctx)
	a.BountyBoard.SyncOnce(ctx)
	a.HeatmapBoard.SyncOnce(ctx)
	a.ServerStatusBoard.SyncOnce(ctx)
	a.syncOnlineCounterRoute(ctx)
}

// syncOnlineCounterRoute binds the online-players counter to the
// ONLINE_COUNTER route of the configured guild, when one exists. Without a
// route the legacy GuildSetup channel (bound at startup) stays in place.
func (a *App) syncOnlineCounterRoute(ctx context.Context) {
	if a.onlineCounter == nil {
		return
	}
	channelID, ok := a.onlineCounterRouteChannel(ctx)
	if !ok {
		return
	}
	if a.onlineCounter.ChannelID() != channelID {
		a.onlineCounter.SetChannelID(channelID)
		// A fresh channel's name is unknown; the counter loop reads it and
		// reconciles it to the current count on its next evaluation.
		a.pokeOnlineCounter()
	}
}

// onlineCounterRouteChannel resolves the ONLINE_COUNTER route channel,
// preferring the public counter server's own route.
func (a *App) onlineCounterRouteChannel(ctx context.Context) (string, bool) {
	if a.ChannelRoutes == nil || a.guildServers == nil {
		return "", false
	}
	guildRowID, serverIDs, err := a.guildServers(ctx)
	if err != nil {
		return "", false
	}
	a.counterOwnerMu.RLock()
	preferred := a.publicCounterServerID
	a.counterOwnerMu.RUnlock()
	order := serverIDs
	if preferred != 0 {
		order = append([]int64{preferred}, serverIDs...)
	}
	for _, serverID := range order {
		channelID, found, err := a.ChannelRoutes.Resolve(ctx, guildRowID, serverID, "ONLINE_COUNTER")
		if err != nil || !found || channelID == "" {
			continue
		}
		return channelID, true
	}
	return "", false
}
