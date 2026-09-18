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

// Champion SaaS setup Step 5 - Discord channel configuration.
//
// Loads the installation's guild channels, lets the customer pick which
// ones Champion posts to, persists the selection onto the existing
// installation_settings row (section 1 - no new table), and advances setup
// from CHANNELS to VALIDATION once a killfeed channel is saved. Step 6
// (verify-permissions, saas_api_discord.go) remains the only place that
// performs full permission verification - this file never duplicates that.

// --- shared DTOs (section 2/6) ---------------------------------------------

// DiscordChannelSummary is one Champion-selectable Discord channel - only
// text/announcement channels the bot can currently view are ever returned
// (see discord.Client.ListGuildChannels). Internal Discord detail
// (permission overwrites, raw type integers) never leaves this DTO.
type DiscordChannelSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Position int    `json:"position"`
	// CanSend is a safe, best-effort hint for the website (section 5) - it
	// is never a substitute for Step 6's full permission verification.
	CanSend bool `json:"canSend"`
}

// channelTypeLabel maps discordgo's raw channel type to the small, stable
// vocabulary this API exposes - never the raw integer (section 2).
func channelTypeLabel(t discordgo.ChannelType) string {
	switch t {
	case discordgo.ChannelTypeGuildNews:
		return "ANNOUNCEMENT"
	default:
		return "TEXT"
	}
}

func toDiscordChannelSummary(c discord.GuildChannelInfo) DiscordChannelSummary {
	return DiscordChannelSummary{
		ID:       c.ID,
		Name:     c.Name,
		Type:     channelTypeLabel(c.Type),
		Position: c.Position,
		CanSend:  c.CanSend,
	}
}

// InstallationChannelSettings is the persisted channel selection for one
// installation - both the GET (#7) and PUT (#8) request/response shape.
// Only killfeedChannelId is required (section 10); the rest may be empty
// strings, meaning "not configured yet."
type InstallationChannelSettings struct {
	KillfeedChannelID     string `json:"killfeedChannelId"`
	LeaderboardChannelID  string `json:"leaderboardChannelId"`
	PlayerStatusChannelID string `json:"playerStatusChannelId"`
	AdminLogChannelID     string `json:"adminLogChannelId"`
}

func toInstallationChannelSettings(s repository.InstallationSettings) InstallationChannelSettings {
	return InstallationChannelSettings{
		KillfeedChannelID:     s.KillfeedChannelID,
		LeaderboardChannelID:  s.LeaderboardChannelID,
		PlayerStatusChannelID: s.PlayerStatusChannelID,
		AdminLogChannelID:     s.AdminLogChannelID,
	}
}

// --- list guild channels (section 3) ---------------------------------------

// handleListDiscordChannels is GET
// .../installations/{installationID}/discord/channels (section 3): any
// member may read. Resolves installation -> guild connection -> guild
// snowflake (the same chain handleVerifyInstallation/handleVerifyPermissions
// already use) and returns that guild's Champion-selectable channels.
func (a *App) handleListDiscordChannels(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	_, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	channels, err := a.saasDiscordVerifier.ListGuildChannels(discordGuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list guild channels failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not load Discord channels")
		return
	}

	out := make([]DiscordChannelSummary, 0, len(channels))
	for _, c := range channels {
		out = append(out, toDiscordChannelSummary(c))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

// --- get persisted channel settings (section 7) -----------------------------

// handleGetChannelSettings is GET
// .../installations/{installationID}/channels (section 7): any member may
// read. Required for refresh persistence (section 16) - a page reload must
// restore whatever was last saved.
func (a *App) handleGetChannelSettings(w http.ResponseWriter, r *http.Request) {
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
		slog.Warn("component=saas_api", "msg", "get channel settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load channel settings")
		return
	}
	if settings == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toInstallationChannelSettings(*settings))
}

// --- save channel settings (section 8) --------------------------------------

// handleSaveChannelSettings is PUT
// .../installations/{installationID}/channels (section 8). Only OWNER/ADMIN
// may save. Every supplied, non-empty channel ID is re-verified against the
// installation's own Discord guild before anything is persisted (section 9)
// - never a client-supplied channel ID trusted blindly. killfeedChannelId
// is the only required selection (section 10); the same channel may be
// reused across multiple purposes (section 11). Preserves every other
// settings field (section 12) and advances setup CHANNELS -> VALIDATION on
// success (section 13).
func (a *App) handleSaveChannelSettings(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	var req InstallationChannelSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.KillfeedChannelID = strings.TrimSpace(req.KillfeedChannelID)
	req.LeaderboardChannelID = strings.TrimSpace(req.LeaderboardChannelID)
	req.PlayerStatusChannelID = strings.TrimSpace(req.PlayerStatusChannelID)
	req.AdminLogChannelID = strings.TrimSpace(req.AdminLogChannelID)

	if req.KillfeedChannelID == "" {
		writeSaaSError(w, codeInvalidRequest, "killfeedChannelId is required")
		return
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

	// Section 9: every supplied, non-empty channel ID must exist, belong to
	// THIS installation's guild, and be a supported text-capable channel -
	// exactly what the guild's own channel list (just verified live) proves.
	for _, id := range []string{req.KillfeedChannelID, req.LeaderboardChannelID, req.PlayerStatusChannelID, req.AdminLogChannelID} {
		if id != "" && !valid[id] {
			writeSaaSError(w, codeInvalidRequest, "one or more selected channels do not belong to this Discord server")
			return
		}
	}

	current, err := a.SaaSInstallations.GetSettings(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get channel settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load channel settings")
		return
	}
	if current == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}

	// Setup-completion task, section 13: a killfeed channel CHANGE on an
	// already-READY installation invalidates its permission verification -
	// computed before mutating current, so it reflects the real old value.
	criticalChange := loaded.Status == repository.InstallationReady && current.KillfeedChannelID != "" && current.KillfeedChannelID != req.KillfeedChannelID

	// Section 12: mutate only the channel fields, preserving
	// timezone/distance unit/display toggles exactly as they were.
	current.KillfeedChannelID = req.KillfeedChannelID
	current.LeaderboardChannelID = req.LeaderboardChannelID
	current.PlayerStatusChannelID = req.PlayerStatusChannelID
	current.AdminLogChannelID = req.AdminLogChannelID
	// An explicit manual save always marks this installation's channel
	// configuration as customer-owned (see hasCustomChannelConfiguration) -
	// a later one-click auto-setup call must never silently overwrite it
	// without force=true (section 13), even if the values happen to match
	// what auto-setup would have produced.
	current.ChannelSetupSource = "MANUAL"

	if err := a.SaaSInstallations.UpdateSettings(ctx, organizationID, installationID, *current); err != nil {
		slog.Warn("component=saas_api", "msg", "update channel settings failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save channel settings")
		return
	}

	a.completeChannelsStep(ctx, organizationID, installationID, loaded.Status, criticalChange)

	slog.Info("component=saas_api", "event", "saas_channels_saved", "installation_id", installationID,
		"has_leaderboard", req.LeaderboardChannelID != "", "has_player_status", req.PlayerStatusChannelID != "", "has_admin_log", req.AdminLogChannelID != "")

	writeSaaSJSON(w, http.StatusOK, InstallationChannelSettings{
		KillfeedChannelID:     current.KillfeedChannelID,
		LeaderboardChannelID:  current.LeaderboardChannelID,
		PlayerStatusChannelID: current.PlayerStatusChannelID,
		AdminLogChannelID:     current.AdminLogChannelID,
	})
}

// completeChannelsStep advances setup progress to VALIDATION and, if the
// installation hasn't gotten further than Discord/Nitrado yet, moves it into
// CONFIGURING (sections 14/15) - shared by the manual save and the one-click
// auto-setup paths, since a successfully saved channel configuration means
// the same thing regardless of which path produced it. Never sets
// validationCompleted true itself - only POST .../setup/complete's live,
// multi-channel permission check does that (saas_api_setup_completion.go).
//
// criticalChange marks a save that actually changed the KILLFEED route's
// channel on an installation that was already READY (setup-completion task,
// section 13): stale permission-verification results for the OLD channel
// can never be trusted for a different one, so this forces validation back
// to incomplete and the installation back to CONFIGURING - never silently
// keeps an installation READY against a channel it was never actually
// verified against. A non-critical save (first-time configuration, or
// re-saving the same KILLFEED channel) leaves READY/validationCompleted
// alone entirely.
func (a *App) completeChannelsStep(ctx context.Context, organizationID, installationID int64, currentStatus string, criticalChange bool) {
	if err := a.advanceSetupProgress(ctx, organizationID, installationID, false, func(p *repository.InstallationSetupProgress) {
		p.ChannelsCompleted = true
		p.CurrentStep = "VALIDATION"
		if criticalChange {
			p.ValidationCompleted = false
		}
	}); err != nil {
		slog.Warn("component=saas_api", "msg", "advance setup progress after channel save failed", "err", err.Error())
	}

	nextStatus := ""
	switch {
	case criticalChange && currentStatus == repository.InstallationReady:
		nextStatus = repository.InstallationConfiguring
	case currentStatus == repository.InstallationDiscordConnected, currentStatus == repository.InstallationNitradoConnected:
		nextStatus = repository.InstallationConfiguring
	}
	if nextStatus != "" {
		if err := a.SaaSInstallations.UpdateStatus(ctx, organizationID, installationID, nextStatus); err != nil {
			slog.Warn("component=saas_api", "msg", "update installation status failed", "err", err.Error())
		}
	}
}

// --- optional custom channel creation (section 10) --------------------------

type createDiscordChannelRequest struct {
	Name       string `json:"name"`
	CategoryID string `json:"categoryId"`
}

// normalizeChannelName lowercases and hyphenates raw into a Discord-safe
// channel name (section 12): letters, digits, hyphens and underscores are
// kept, runs of whitespace/hyphens collapse to a single hyphen, and every
// other character (emoji, punctuation, etc.) is dropped outright. Reports
// ok=false for anything that normalizes to empty or exceeds Discord's
// 100-character channel name limit - this API never trusts a client-supplied
// name to already be safe, even though Discord itself also sanitizes names.
func normalizeChannelName(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if b.Len() > 0 && !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		default:
			// Drop unsupported characters entirely rather than rejecting
			// the whole name outright.
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if name == "" || len(name) > 100 {
		return "", false
	}
	return name, true
}

// handleCreateDiscordChannel is POST
// .../installations/{installationID}/discord/channels (section 10): an
// optional, explicit "create a new channel" action for Customize mode. Only
// OWNER/ADMIN may call it. categoryId, when supplied, must already belong to
// THIS installation's own guild (section 19) - never trusted blindly, and
// never used to create a channel in another guild (structurally impossible
// here anyway, since the target guild always comes from the installation,
// never from client input).
func (a *App) handleCreateDiscordChannel(w http.ResponseWriter, r *http.Request) {
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
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	var req createDiscordChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	name, ok := normalizeChannelName(req.Name)
	if !ok {
		writeSaaSError(w, codeInvalidRequest, "a valid channel name is required")
		return
	}
	categoryID := strings.TrimSpace(req.CategoryID)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	_, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	perms, err := a.saasDiscordVerifier.GuildPermissions(discordGuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get guild permissions failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not verify Discord permissions")
		return
	}
	if perms&discordgo.PermissionManageChannels == 0 {
		writeSaaSError(w, codeDiscordUnavailable, "Champion needs Manage Channels permission in this Discord server")
		return
	}

	if categoryID != "" {
		channels, err := a.saasDiscordVerifier.ListAllGuildChannels(discordGuildID)
		if err != nil {
			slog.Warn("component=saas_api", "msg", "list all guild channels failed", "err", err.Error())
			writeSaaSError(w, codeDiscordUnavailable, "could not load Discord channels")
			return
		}
		found := false
		for _, ch := range channels {
			if ch.ID == categoryID && ch.Type == discordgo.ChannelTypeGuildCategory {
				found = true
				break
			}
		}
		if !found {
			// Section 19: never create against a category ID that isn't
			// actually part of this installation's own guild.
			writeSaaSError(w, codeInvalidRequest, "categoryId does not belong to this Discord server")
			return
		}
	}

	created, err := a.saasDiscordVerifier.CreateGuildTextChannel(discordGuildID, name, categoryID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "create discord channel failed", "err", err.Error())
		writeSaaSError(w, codeDiscordUnavailable, "could not create Discord channel")
		return
	}

	slog.Info("component=saas_api", "event", "saas_discord_channel_created", "installation_id", installationID)
	writeSaaSJSON(w, http.StatusCreated, DiscordChannelSummary{
		ID:      created.ID,
		Name:    created.Name,
		Type:    channelTypeLabel(created.Type),
		CanSend: true,
	})
}
