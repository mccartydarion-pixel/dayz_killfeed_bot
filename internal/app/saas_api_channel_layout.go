package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Channel System V2 finalization: one layout runner shared by one-click
// setup, Repair Champion Discord Layout and the Discord /setup command, plus
// a read-only layout status and the explicit, confirmed retired-channel
// cleanup. There is exactly one channel blueprint (championDestinations).

// ChannelLayoutStatusResponse is GET .../channels/layout: the current state
// without changing anything.
type ChannelLayoutStatusResponse struct {
	Routes       map[string]ChannelRouteInfo `json:"routes"`
	Destinations []ChannelDestinationReport  `json:"destinations"`
	Retirable    []RetirableChannel          `json:"retirable"`
}

type cleanupRetiredChannelsRequest struct {
	// ChannelIDs are the retirable channels the customer confirmed. Anything
	// not proven Champion-owned and unreferenced is refused, never deleted.
	ChannelIDs []string `json:"channelIds"`
}

// CleanupRetiredChannelsResponse reports what the confirmed cleanup did.
type CleanupRetiredChannelsResponse struct {
	Deleted []RetirableChannel      `json:"deleted"`
	Skipped []CleanupSkippedChannel `json:"skipped"`
}

type CleanupSkippedChannel struct {
	ChannelID string `json:"channelId"`
	Reason    string `json:"reason"` // NOT_RETIRABLE | REFERENCED | NOT_EMPTY | DISCORD_ERROR | GONE
}

var errMissingManageChannels = errors.New("missing manage channels")

// legacySetupReplacements maps each channel the legacy /setup created to the
// V2 route that replaces it. The welcome channel has no V2 replacement and is
// never retired.
var legacySetupReplacements = []struct {
	field, routeKey string
	get             func(*discord.GuildSetup) string
	clear           func(*discord.GuildSetup)
}{
	{"KillfeedChannelID", "KILLFEED", func(g *discord.GuildSetup) string { return g.KillfeedChannelID }, func(g *discord.GuildSetup) { g.KillfeedChannelID = "" }},
	{"DeathChannelID", "KILLFEED", func(g *discord.GuildSetup) string { return g.DeathChannelID }, func(g *discord.GuildSetup) { g.DeathChannelID = "" }},
	{"LeaderboardsChannelID", "AUTO_LEADERBOARD", func(g *discord.GuildSetup) string { return g.LeaderboardsChannelID }, func(g *discord.GuildSetup) { g.LeaderboardsChannelID, g.LeaderboardMessageID = "", "" }},
	{"PlayerStatsChannelID", "STATS_LEADERBOARDS", func(g *discord.GuildSetup) string { return g.PlayerStatsChannelID }, func(g *discord.GuildSetup) { g.PlayerStatsChannelID, g.PlayerStatsInfoMessageID = "", "" }},
	{"LinkPanelChannelID", "LINK_GAMERTAG", func(g *discord.GuildSetup) string { return g.LinkPanelChannelID }, func(g *discord.GuildSetup) { g.LinkPanelChannelID, g.LinkPanelMessageID = "", "" }},
	{"ADMMonitorChannelID", "ADMIN_LOGS", func(g *discord.GuildSetup) string { return g.ADMMonitorChannelID }, func(g *discord.GuildSetup) { g.ADMMonitorChannelID = "" }},
	{"ServerStatusChannelID", "SERVER_STATUS", func(g *discord.GuildSetup) string { return g.ServerStatusChannelID }, func(g *discord.GuildSetup) { g.ServerStatusChannelID, g.ServerStatusMessageID = "", "" }},
	{"OnlinePlayersChannelID", "ONLINE_COUNTER", func(g *discord.GuildSetup) string { return g.OnlinePlayersChannelID }, func(g *discord.GuildSetup) { g.OnlinePlayersChannelID = "" }},
}

func (a *App) legacySetupStore() discord.SetupStore {
	if a.Guilds == nil {
		return nil
	}
	return discord.NewPostgresSetupStore(a.Guilds)
}

// runChannelLayout applies the V2 layout to one installation. preserve keeps
// customer-owned routes (repair, /setup); without it (one-click setup) every
// route is mapped to Champion's channel.
func (a *App) runChannelLayout(ctx context.Context, organizationID, installationID int64, discordGuildID string, preserve bool) (AutoSetupChannelsResponse, error) {
	settings, err := a.SaaSInstallations.GetSettings(ctx, organizationID, installationID)
	if err != nil {
		return AutoSetupChannelsResponse{}, fmt.Errorf("load channel settings: %w", err)
	}
	if settings == nil {
		return AutoSetupChannelsResponse{}, fmt.Errorf("installation not found")
	}
	inst, err := a.SaaSInstallations.GetScoped(ctx, organizationID, installationID)
	if err != nil || inst == nil {
		return AutoSetupChannelsResponse{}, fmt.Errorf("load installation: %v", err)
	}
	existingRoutes, err := a.SaaSChannelRoutes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		return AutoSetupChannelsResponse{}, fmt.Errorf("load channel routes: %w", err)
	}
	perms, err := a.saasDiscordVerifier.GuildPermissions(discordGuildID)
	if err != nil {
		return AutoSetupChannelsResponse{}, fmt.Errorf("guild permissions: %w", err)
	}
	if perms&discordgo.PermissionManageChannels == 0 {
		return AutoSetupChannelsResponse{Configured: false, Reason: "MISSING_MANAGE_CHANNELS"}, errMissingManageChannels
	}

	layout, err := applyChannelLayout(ctx, a.saasDiscordVerifier, a.SaaSChannelRoutes, channelLayoutInput{
		OrganizationID: organizationID,
		InstallationID: installationID,
		GuildID:        discordGuildID,
		Existing:       existingRoutes,
		Producers:      a.channelRouteProducers(),
		Preserve:       preserve,
		SyncPanels:     a.syncRoutedPanelsNow,
	})
	if errors.Is(err, errKillfeedUnavailable) {
		return AutoSetupChannelsResponse{Configured: false, Reason: "KILLFEED_UNAVAILABLE"}, err
	}
	if err != nil {
		return AutoSetupChannelsResponse{}, err
	}
	routes := layout.Routes

	resp := AutoSetupChannelsResponse{Configured: true, Routes: routes, Destinations: layout.Destinations}
	isLayoutCategory := map[string]bool{}
	for _, cat := range championCategories {
		ch, ok := layout.Categories[cat.Key]
		if !ok {
			continue
		}
		isLayoutCategory[ch.ID] = true
		summary := ChannelCategorySummary{ID: ch.ID, Name: ch.Name, Key: cat.Key}
		resp.Categories = append(resp.Categories, summary)
		if cat.Key == categoryLive {
			resp.Category = &summary
		}
	}

	// Record every Champion-owned channel no route uses any more: routes this
	// run moved away from, the pre-V2 SaaS category, and channels the legacy
	// /setup created that a V2 route now replaces.
	retired := make([]repository.RetiredChannel, 0, len(layout.Retirable))
	for _, rc := range layout.Retirable {
		retired = append(retired, repository.RetiredChannel{ChannelID: rc.ChannelID, Kind: rc.Kind, Source: "ROUTE", FormerRoutes: rc.FormerRoutes})
	}
	if old := settings.ChampionCategoryID; old != "" && !isLayoutCategory[old] {
		retired = append(retired, repository.RetiredChannel{ChannelID: old, Kind: "CATEGORY", Source: "ROUTE"})
	}
	retired = append(retired, a.legacySetupRetirables(discordGuildID, routes, isLayoutCategory)...)
	if a.SaaSRetiredChannels != nil && len(retired) > 0 {
		if err := a.SaaSRetiredChannels.Record(ctx, organizationID, installationID, retired); err != nil {
			slog.Warn("component=saas_api", "msg", "record retired channels failed", "err", err.Error())
		}
	}
	resp.Retirable = a.currentRetirables(ctx, organizationID, installationID, discordGuildID)
	summary := layout.Summary
	summary.Retirable = len(resp.Retirable)
	resp.Summary = &summary

	// Mirror the three routes with a direct legacy equivalent back onto
	// installation_settings (section 5 - "keep old fields operational until
	// all runtime consumers have migrated").
	updatedSettings := *settings
	updatedSettings.KillfeedChannelID = routes["KILLFEED"].ChannelID
	updatedSettings.LeaderboardChannelID = routes["STATS_LEADERBOARDS"].ChannelID
	updatedSettings.AdminLogChannelID = routes["ADMIN_LOGS"].ChannelID
	if len(summary.Preserved) == 0 {
		// Only a layout that owns every route counts as Champion-configured;
		// preserved customer routes keep one-click setup protected.
		updatedSettings.ChannelSetupSource = "AUTO"
	}
	if resp.Category != nil {
		updatedSettings.ChampionCategoryID = resp.Category.ID
	}
	if err := a.SaaSInstallations.UpdateSettings(ctx, organizationID, installationID, updatedSettings); err != nil {
		return AutoSetupChannelsResponse{}, fmt.Errorf("save channel settings: %w", err)
	}

	persistedKillfeed := ""
	for _, rt := range existingRoutes {
		if rt.RouteKey == "KILLFEED" {
			persistedKillfeed = rt.ChannelID
		}
	}
	// Setup-completion task, section 13: moving KILLFEED on an already-READY
	// installation invalidates its permission verification.
	criticalChange := inst.Status == repository.InstallationReady && persistedKillfeed != "" && persistedKillfeed != routes["KILLFEED"].ChannelID
	a.completeChannelsStep(ctx, organizationID, installationID, inst.Status, criticalChange)

	slog.Info("component=saas_api", "event", "saas_channel_layout_applied", "installation_id", installationID, "preserve", preserve,
		"created", len(summary.Created), "reused", len(summary.Reused), "remapped", len(summary.Remapped), "preserved", len(summary.Preserved),
		"broken", len(summary.Broken), "retirable", summary.Retirable)
	return resp, nil
}

// legacySetupRetirables lists channels the legacy /setup created whose job a
// V2 route now does (in a different channel). The legacy GuildSetup only ever
// held channels the legacy /setup command created, so they are provably
// Champion-owned.
func (a *App) legacySetupRetirables(discordGuildID string, routes map[string]ChannelRouteInfo, layoutCategory map[string]bool) []repository.RetiredChannel {
	store := a.legacySetupStore()
	if store == nil {
		return nil
	}
	gs, err := store.Get(discordGuildID)
	if err != nil || gs == nil {
		return nil
	}
	return legacyRetirablesFor(gs, routes, layoutCategory)
}

// legacyRetirablesFor is legacySetupRetirables' pure core.
func legacyRetirablesFor(gs *discord.GuildSetup, routes map[string]ChannelRouteInfo, layoutCategory map[string]bool) []repository.RetiredChannel {
	var out []repository.RetiredChannel
	inUse := map[string]bool{}
	for _, r := range routes {
		inUse[r.ChannelID] = true
	}
	seen := map[string]bool{}
	for _, lr := range legacySetupReplacements {
		id := lr.get(gs)
		route, routed := routes[lr.routeKey]
		if id == "" || !routed || route.ChannelID == id || inUse[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, repository.RetiredChannel{ChannelID: id, Kind: "CHANNEL", Source: "LEGACY_SETUP", FormerRoutes: []string{lr.routeKey}, LegacyField: lr.field})
	}
	if gs.CategoryID != "" && len(layoutCategory) > 0 && !layoutCategory[gs.CategoryID] {
		out = append(out, repository.RetiredChannel{ChannelID: gs.CategoryID, Kind: "CATEGORY", Source: "LEGACY_SETUP", LegacyField: "CategoryID"})
	}
	return out
}

// currentRetirables returns the recorded retired channels that still exist
// and that no route references. Rows for channels gone from Discord, or used
// by a route again, are forgotten.
func (a *App) currentRetirables(ctx context.Context, organizationID, installationID int64, discordGuildID string) []RetirableChannel {
	out := []RetirableChannel{}
	if a.SaaSRetiredChannels == nil {
		return out
	}
	rows, err := a.SaaSRetiredChannels.List(ctx, organizationID, installationID)
	if err != nil || len(rows) == 0 {
		return out
	}
	channels, err := a.saasDiscordVerifier.ListAllGuildChannels(discordGuildID)
	if err != nil {
		return out
	}
	byID := make(map[string]discord.RawGuildChannel, len(channels))
	for _, ch := range channels {
		byID[ch.ID] = ch
	}
	for _, row := range rows {
		ch, exists := byID[row.ChannelID]
		referenced, rerr := a.SaaSRetiredChannels.ChannelReferenced(ctx, row.ChannelID)
		if rerr != nil {
			continue
		}
		if !exists || referenced {
			_ = a.SaaSRetiredChannels.Forget(ctx, organizationID, installationID, row.ChannelID)
			continue
		}
		out = append(out, RetirableChannel{ChannelID: row.ChannelID, ChannelName: ch.Name, Kind: row.Kind, Source: row.Source, FormerRoutes: row.FormerRoutes, ManagedByChampion: true})
	}
	return out
}

// requireLayoutScope runs the shared auth chain for the layout endpoints and
// returns the installation's guild. write requires OWNER/ADMIN.
func (a *App) requireLayoutScope(w http.ResponseWriter, r *http.Request, write bool) (organizationID, installationID int64, discordGuildID string, ok bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok = pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	if write {
		_, ok = a.requireOrganizationRole(w, r, organizationID, user.ID)
	} else {
		_, ok = a.requireOrganizationMember(w, r, organizationID, user.ID)
	}
	if !ok {
		return
	}
	installationID, ok = pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	ok = false
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil || a.SaaSChannelRoutes == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}
	_, guildID, errCode, errMsg := a.loadInstallationGuildSnowflake(r.Context(), organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}
	return organizationID, installationID, guildID, true
}

// handleChannelLayoutStatus is GET .../channels/layout. Any member may read.
func (a *App) handleChannelLayoutStatus(w http.ResponseWriter, r *http.Request) {
	organizationID, installationID, guildID, ok := a.requireLayoutScope(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	routes, err := a.SaaSChannelRoutes.ListForInstallation(ctx, organizationID, installationID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load channel routes")
		return
	}
	destinations, err := inspectChannelLayout(a.saasDiscordVerifier, guildID, routes, a.channelRouteProducers())
	if err != nil {
		slog.Warn("component=saas_api", "msg", "inspect channel layout failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not inspect Discord channels")
		return
	}
	writeSaaSJSON(w, http.StatusOK, ChannelLayoutStatusResponse{
		Routes:       a.buildChannelRoutesResponse(ctx, organizationID, installationID, guildID).Routes,
		Destinations: destinations,
		Retirable:    a.currentRetirables(ctx, organizationID, installationID, guildID),
	})
}

// handleRepairChannelLayout is POST .../channels/repair: Repair Champion
// Discord Layout. OWNER/ADMIN only. Unlike one-click setup it runs over
// customer routing, which it preserves untouched.
func (a *App) handleRepairChannelLayout(w http.ResponseWriter, r *http.Request) {
	organizationID, installationID, guildID, ok := a.requireLayoutScope(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if !a.saasDiscordVerifier.Verify(guildID, "").GuildFound {
		writeSaaSError(w, codeDiscordUnavailable, "Champion is not installed in this Discord server")
		return
	}
	resp, err := a.runChannelLayout(ctx, organizationID, installationID, guildID, true)
	if err != nil && resp.Reason == "" {
		slog.Warn("component=saas_api", "msg", "repair channel layout failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not repair Champion's Discord layout")
		return
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handleCleanupRetiredChannels is POST .../channels/cleanup: deletes only the
// confirmed channels the backend proves are Champion-owned (recorded as
// retired), unreferenced by any route, and still present. OWNER/ADMIN only.
func (a *App) handleCleanupRetiredChannels(w http.ResponseWriter, r *http.Request) {
	organizationID, installationID, guildID, ok := a.requireLayoutScope(w, r, true)
	if !ok {
		return
	}
	var req cleanupRetiredChannelsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.ChannelIDs) == 0 {
		writeSaaSError(w, codeInvalidRequest, "channelIds is required")
		return
	}
	if a.SaaSRetiredChannels == nil {
		writeSaaSError(w, codeInternalError, "cleanup unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := a.cleanupRetiredChannels(ctx, organizationID, installationID, guildID, req.ChannelIDs)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "cleanup retired channels failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not clean up retired channels")
		return
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// retiredChannelStore is cleanup's view of the retired-channel records;
// *repository.RetiredChannelRepository satisfies it.
type retiredChannelStore interface {
	List(ctx context.Context, organizationID, installationID int64) ([]repository.RetiredChannel, error)
	Forget(ctx context.Context, organizationID, installationID int64, channelID string) error
	ChannelReferenced(ctx context.Context, channelID string) (bool, error)
}

// channelCleanupDiscord is cleanup's Discord surface.
type channelCleanupDiscord interface {
	ListAllGuildChannels(guildID string) ([]discord.RawGuildChannel, error)
	DeleteGuildChannel(channelID string) error
}

func (a *App) cleanupRetiredChannels(ctx context.Context, organizationID, installationID int64, guildID string, channelIDs []string) (CleanupRetiredChannelsResponse, error) {
	resp, clearFields, err := cleanupRetired(ctx, a.SaaSRetiredChannels, a.saasDiscordVerifier, organizationID, installationID, guildID, channelIDs)
	if len(clearFields) > 0 {
		a.clearLegacySetupFields(guildID, clearFields)
	}
	if err == nil {
		slog.Info("component=saas_api", "event", "saas_retired_channels_cleaned", "installation_id", installationID, "deleted", len(resp.Deleted), "skipped", len(resp.Skipped))
	}
	return resp, err
}

// cleanupRetired deletes only confirmed channels that are recorded as
// retired for this installation, unreferenced by every route, still present
// with the recorded kind, and (for a category) empty. It returns the legacy
// GuildSetup fields to clear for the deleted channels.
func cleanupRetired(ctx context.Context, store retiredChannelStore, d channelCleanupDiscord, organizationID, installationID int64, guildID string, channelIDs []string) (CleanupRetiredChannelsResponse, []string, error) {
	resp := CleanupRetiredChannelsResponse{Deleted: []RetirableChannel{}, Skipped: []CleanupSkippedChannel{}}
	rows, err := store.List(ctx, organizationID, installationID)
	if err != nil {
		return resp, nil, err
	}
	recorded := map[string]repository.RetiredChannel{}
	for _, row := range rows {
		recorded[row.ChannelID] = row
	}
	channels, err := d.ListAllGuildChannels(guildID)
	if err != nil {
		return resp, nil, err
	}
	byID := map[string]discord.RawGuildChannel{}
	children := map[string]int{}
	for _, ch := range channels {
		byID[ch.ID] = ch
		if ch.ParentID != "" {
			children[ch.ParentID]++
		}
	}
	// Channels before categories, so a category emptied by this cleanup can
	// go too.
	ordered := make([]string, 0, len(channelIDs))
	queued := map[string]bool{}
	for _, pass := range []string{"CHANNEL", "CATEGORY"} {
		for _, id := range channelIDs {
			if row, ok := recorded[strings.TrimSpace(id)]; ok && row.Kind == pass && !queued[row.ChannelID] {
				queued[row.ChannelID] = true
				ordered = append(ordered, row.ChannelID)
			}
		}
	}
	for _, id := range channelIDs {
		if _, ok := recorded[strings.TrimSpace(id)]; !ok {
			resp.Skipped = append(resp.Skipped, CleanupSkippedChannel{ChannelID: id, Reason: "NOT_RETIRABLE"})
		}
	}
	var clearFields []string
	for _, id := range ordered {
		row := recorded[id]
		ch, exists := byID[id]
		if !exists {
			_ = store.Forget(ctx, organizationID, installationID, id)
			resp.Skipped = append(resp.Skipped, CleanupSkippedChannel{ChannelID: id, Reason: "GONE"})
			continue
		}
		referenced, err := store.ChannelReferenced(ctx, id)
		if err != nil {
			return resp, clearFields, err
		}
		if referenced {
			resp.Skipped = append(resp.Skipped, CleanupSkippedChannel{ChannelID: id, Reason: "REFERENCED"})
			continue
		}
		if row.Kind == "CATEGORY" && children[id] > 0 {
			// Never delete a category that still holds channels (a customer
			// may have moved their own channels into it).
			resp.Skipped = append(resp.Skipped, CleanupSkippedChannel{ChannelID: id, Reason: "NOT_EMPTY"})
			continue
		}
		if (ch.Type == discordgo.ChannelTypeGuildCategory) != (row.Kind == "CATEGORY") {
			resp.Skipped = append(resp.Skipped, CleanupSkippedChannel{ChannelID: id, Reason: "NOT_RETIRABLE"})
			continue
		}
		if err := d.DeleteGuildChannel(id); err != nil {
			slog.Warn("component=saas_api", "msg", "delete retired channel failed", "channel_id", id, "err", err.Error())
			resp.Skipped = append(resp.Skipped, CleanupSkippedChannel{ChannelID: id, Reason: "DISCORD_ERROR"})
			continue
		}
		if ch.ParentID != "" {
			children[ch.ParentID]--
		}
		_ = store.Forget(ctx, organizationID, installationID, id)
		if row.LegacyField != "" {
			clearFields = append(clearFields, row.LegacyField)
		}
		resp.Deleted = append(resp.Deleted, RetirableChannel{ChannelID: id, ChannelName: ch.Name, Kind: row.Kind, Source: row.Source, FormerRoutes: row.FormerRoutes, ManagedByChampion: true})
	}
	return resp, clearFields, nil
}

// clearLegacySetupFields drops legacy GuildSetup pointers at deleted channels
// so nothing falls back to a channel that no longer exists.
func (a *App) clearLegacySetupFields(discordGuildID string, fields []string) {
	store := a.legacySetupStore()
	if store == nil {
		return
	}
	gs, err := store.Get(discordGuildID)
	if err != nil || gs == nil {
		return
	}
	for _, f := range fields {
		if f == "CategoryID" {
			gs.CategoryID = ""
			continue
		}
		for _, lr := range legacySetupReplacements {
			if lr.field == f {
				lr.clear(gs)
			}
		}
	}
	if err := store.Save(*gs); err != nil {
		slog.Warn("component=saas_api", "msg", "clear legacy setup fields failed", "err", err.Error())
	}
}

// DiscordSetupLayout is the Discord /setup command's entry point: it applies
// the same V2 layout (repair semantics: customer routes preserved) to every
// installation connected to the guild. It never creates legacy channels.
func (a *App) DiscordSetupLayout(ctx context.Context, discordGuildID string) (discord.SetupLayoutResult, error) {
	var out discord.SetupLayoutResult
	if a.Guilds == nil || a.SaaSChannelRoutes == nil || a.SaaSInstallations == nil || a.saasDiscordVerifier == nil {
		return out, fmt.Errorf("channel setup is unavailable")
	}
	_, guildRowID, err := a.Guilds.GetGuild(ctx, discordGuildID)
	if err != nil || guildRowID == 0 {
		return out, discord.ErrNoInstallation
	}
	refs, err := a.SaaSChannelRoutes.ListInstallationsForGuild(ctx, guildRowID)
	if err != nil {
		return out, err
	}
	if len(refs) == 0 {
		return out, discord.ErrNoInstallation
	}
	for _, ref := range refs {
		resp, err := a.runChannelLayout(ctx, ref.OrganizationID, ref.InstallationID, discordGuildID, true)
		if errors.Is(err, errMissingManageChannels) {
			return out, discord.ErrMissingManageChannels
		}
		if err != nil {
			return out, err
		}
		out.Installations++
		addLayoutToSetupResult(&out, resp)
	}
	return out, nil
}

// addLayoutToSetupResult folds one installation's layout run into the /setup report. Several
// installations of one guild resolve to the same Discord channels, so everything is keyed by
// channel ID (discord.SetupLayoutResult deduplicates): a channel is reported once per run, with the
// most significant outcome any installation had for it.
func addLayoutToSetupResult(out *discord.SetupLayoutResult, resp AutoSetupChannelsResponse) {
	remapped := map[string]bool{}
	if resp.Summary != nil {
		for _, key := range resp.Summary.Remapped {
			remapped[key] = true
		}
	}
	for _, rep := range resp.Destinations {
		label := rep.Label
		if label == "" {
			label = rep.Key
		}
		switch {
		case rep.Checks == nil && (rep.Health == HealthBlocked || rep.Health == HealthDisabled || rep.Health == HealthNotRequired):
			out.AddBlockedSystem(label)
			continue
		case rep.ChannelID == "":
			out.AddFailedSystem(label)
			continue
		}
		outcome := discord.SetupChannelReused
		if rep.Created {
			outcome = discord.SetupChannelCreated
		} else {
			for _, key := range destinationByKey(rep.Key).Routes {
				if remapped[key] {
					outcome = discord.SetupChannelUpdated
					break
				}
			}
		}
		if rep.Health == HealthBroken || rep.Checks == nil || !rep.Checks.passed() {
			outcome = discord.SetupChannelFailed
		} else {
			out.AddVerifiedSystem(label)
		}
		out.AddChannel(discord.SetupChannel{ID: rep.ChannelID, Name: rep.ChannelName, System: label, Outcome: outcome})
	}
	for _, rc := range resp.Retirable {
		if rc.Kind != "CATEGORY" {
			out.AddLegacy(rc.ChannelID)
		}
	}
}
