package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/vip"
)

// Supporter & VIP tiers (docs/CLIENT_HUB_GROWTH.md): a tier gives its members a killfeed badge, an
// optional Discord role and a multiplier on automatic Champion Point rewards. Staff grant and end
// memberships; expired ones end on their own and lose the role.

// vipRoleAPI is the Discord role calls VIP sync makes (a *discordgo.Session in production).
type vipRoleAPI interface {
	GuildMemberRoleAdd(guildID, userID, roleID string, options ...discordgo.RequestOption) error
	GuildMemberRoleRemove(guildID, userID, roleID string, options ...discordgo.RequestOption) error
}

// vipDMAPI is the Discord calls a tier notice makes (a *discordgo.Session in production).
type vipDMAPI interface {
	UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

func (a *App) registerVIPRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/vip", a.handleVIP)
	h("PUT "+adminBase+"/vip/tiers", a.handleSaveVIPTier)
	h("DELETE "+adminBase+"/vip/tiers/{tierID}", a.handleDeleteVIPTier)
	h("POST "+adminBase+"/vip/members", a.handleGrantVIP)
	h("POST "+adminBase+"/vip/members/{memberID}/revoke", a.handleRevokeVIP)
}

func (a *App) handleVIP(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapVIPView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.VIP == nil {
		writeSaaSError(w, codeInternalError, "VIP tiers unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	tiers, err := a.VIP.ListTiers(ctx, ac.scope.GuildID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load VIP tiers")
		return
	}
	members, err := a.VIP.ListMembers(ctx, ac.scope.GuildID, 200)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load VIP members")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"tiers": tiers, "members": members})
}

func (a *App) handleSaveVIPTier(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapVIPManage)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.Economy) {
		return
	}
	req, ok := decodeJSONBody[vip.Tier](w, r)
	if !ok {
		return
	}
	tier, err := vip.Normalize(req)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	if a.VIP == nil {
		writeSaaSError(w, codeInternalError, "VIP tiers unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	saved, err := a.VIP.SaveTier(ctx, ac.scope.GuildID, tier)
	switch {
	case errors.Is(err, repository.ErrVIPTierNotFound):
		writeSaaSError(w, codeNotFound, "VIP tier not found")
		return
	case errors.Is(err, repository.ErrVIPTierNameTaken):
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	case err != nil:
		slog.Warn("component=saas_api", "event", "vip_tier_save_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save the tier")
		return
	}
	a.recordAudit(ctx, ac, "VIP_TIER_SAVE", fmt.Sprintf("vip_tier:%d", saved.ID), saved.Name, "success", nil, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

func (a *App) handleDeleteVIPTier(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapVIPManage)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "tierID")
	if !ok {
		return
	}
	if a.VIP == nil {
		writeSaaSError(w, codeInternalError, "VIP tiers unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	err := a.VIP.DeleteTier(ctx, ac.scope.GuildID, id)
	switch {
	case errors.Is(err, repository.ErrVIPTierNotFound):
		writeSaaSError(w, codeNotFound, "VIP tier not found")
		return
	case errors.Is(err, repository.ErrVIPTierInUse):
		writeSaaSError(w, codeInvalidRequest, "end this tier's active memberships before deleting it")
		return
	case err != nil:
		writeSaaSError(w, codeInternalError, "could not delete the tier")
		return
	}
	a.recordAudit(ctx, ac, "VIP_TIER_DELETE", fmt.Sprintf("vip_tier:%d", id), "", "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

type grantVIPRequest struct {
	PlayerID int64  `json:"playerId"`
	TierID   int64  `json:"tierId"`
	Days     int    `json:"days"` // 0 = no end date
	Note     string `json:"note"`
}

func (a *App) handleGrantVIP(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapVIPManage)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.Economy) {
		return
	}
	req, ok := decodeJSONBody[grantVIPRequest](w, r)
	if !ok {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.PlayerID <= 0 || req.TierID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "pick a player and a tier")
		return
	}
	if req.Days < 0 || req.Days > vip.MaxDays {
		writeSaaSError(w, codeInvalidRequest, "length is 0 (no end) to 3650 days")
		return
	}
	if len([]rune(req.Note)) > 200 {
		writeSaaSError(w, codeInvalidRequest, "keep the note under 200 characters")
		return
	}
	if a.VIP == nil {
		writeSaaSError(w, codeInternalError, "VIP tiers unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var expires *time.Time
	if req.Days > 0 {
		at := time.Now().UTC().AddDate(0, 0, req.Days)
		expires = &at
	}
	member, err := a.VIP.Grant(ctx, ac.scope.GuildID, req.TierID, req.PlayerID, expires, req.Note, ac.user.DiscordUserID)
	switch {
	case errors.Is(err, repository.ErrVIPTierNotFound), errors.Is(err, repository.ErrVIPPlayerNotFound):
		writeSaaSError(w, codeNotFound, err.Error())
		return
	case errors.Is(err, repository.ErrVIPAlreadyMember):
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	case err != nil:
		slog.Warn("component=saas_api", "event", "vip_grant_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not grant the tier")
		return
	}
	member.RoleError = a.syncVIPRole(ctx, member, true)
	serverID := int64(0)
	if ac.scope.ServerID != nil {
		serverID = *ac.scope.ServerID
	}
	member.NoticeError = a.notifyVIPGranted(ctx, ac.scope.GuildID, serverID, member)
	a.recordAudit(ctx, ac, "VIP_GRANT", playerTarget(member.PlayerID), member.TierName, "success", nil, map[string]any{"tierId": member.TierID, "expiresAt": member.ExpiresAt})
	writeSaaSJSON(w, http.StatusOK, member)
}

func (a *App) handleRevokeVIP(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapVIPManage)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "memberID")
	if !ok {
		return
	}
	if a.VIP == nil {
		writeSaaSError(w, codeInternalError, "VIP tiers unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	member, err := a.VIP.Revoke(ctx, ac.scope.GuildID, id, "REVOKED")
	if errors.Is(err, repository.ErrVIPMemberNotFound) {
		writeSaaSError(w, codeNotFound, err.Error())
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not end the membership")
		return
	}
	member.RoleError = a.syncVIPRole(ctx, member, false)
	a.recordAudit(ctx, ac, "VIP_REVOKE", playerTarget(member.PlayerID), member.TierName, "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, member)
}

// syncVIPRole adds or removes the tier's Discord role and records the outcome. It returns the
// owner-facing problem ("" when the role changed or there is nothing to do).
func (a *App) syncVIPRole(ctx context.Context, m repository.VIPMember, add bool) string {
	if m.RoleID == "" || a.VIPRoles == nil {
		return ""
	}
	problem := ""
	switch {
	case m.DiscordUserID == "":
		problem = "No Discord role change: this player has no verified Discord link."
	default:
		var err error
		if add {
			err = a.VIPRoles.GuildMemberRoleAdd(m.DiscordGuild, m.DiscordUserID, m.RoleID)
		} else {
			err = a.VIPRoles.GuildMemberRoleRemove(m.DiscordGuild, m.DiscordUserID, m.RoleID)
		}
		if err != nil {
			problem = "Discord refused the role change. Check the bot has Manage Roles and its role is above the VIP role."
			slog.Warn("component=vip", "msg", "role sync failed", "member_id", m.ID, "err", err.Error())
		}
	}
	if err := a.VIP.RecordRoleSync(ctx, m.ID, problem); err != nil {
		slog.Warn("component=vip", "msg", "role sync stamp failed", "member_id", m.ID, "err", err.Error())
	}
	return problem
}

// runVIPExpiry ends memberships whose time ran out and removes their Discord role.
func (a *App) runVIPExpiry(ctx context.Context, guildID int64, now time.Time) {
	if a.VIP == nil {
		return
	}
	expired, err := a.VIP.ExpireDue(ctx, guildID, now)
	if err != nil {
		slog.Warn("component=vip", "msg", "expiry sweep failed", "err", err.Error())
		return
	}
	for _, m := range expired {
		a.syncVIPRole(ctx, m, false)
	}
}

// buildVIPNotice is the direct message a player gets when they receive a tier.
func buildVIPNotice(t repository.PlayerTier, serverName, hubURL string) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{
		Author:      presentation.BrandAuthor("Supporter Tiers"),
		Title:       "💎 You received the " + presentation.SafeName(t.Name, 60) + " tier",
		Description: "From " + serverWord(serverName) + ". Thank you for supporting the server!",
		Color:       presentation.Gold, // unless the owner gave the tier its own colour (below)
	}
	if len(t.Color) == 7 {
		if c, err := strconv.ParseInt(t.Color[1:], 16, 32); err == nil {
			embed.Color = int(c)
		}
	}
	perks := []string{"Killfeed badge: " + t.Badge}
	if t.DiscordRole {
		perks = append(perks, "A Discord role in the server")
	}
	if t.RewardMultiplier > 1 {
		perks = append(perks, "×"+strconv.FormatFloat(t.RewardMultiplier, 'f', -1, 64)+" Champion Points on automatic rewards")
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "What you get", Value: "• " + strings.Join(perks, "\n• ")})
	until := "No end date"
	if t.ExpiresAt != nil {
		until = presentation.Timestamp(*t.ExpiresAt, 'f')
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Yours until", Value: until, Inline: true})
	if hubURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "See it in the Player Hub", Value: hubURL})
	}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// notifyVIPGranted tells the player by direct message that they received a tier. It returns the
// staff-facing reason the message was not sent ("" when it was). A tier is never undone over it.
func (a *App) notifyVIPGranted(ctx context.Context, guildID, serverID int64, m repository.VIPMember) string {
	if m.DiscordUserID == "" {
		return "No direct message: this player has no verified Discord link."
	}
	if a.VIPNotices == nil || a.VIP == nil {
		return "No direct message: Discord is unavailable."
	}
	tier, err := a.VIP.ActiveForPlayer(ctx, guildID, m.PlayerID)
	if err != nil || tier == nil {
		return "No direct message: the tier could not be read back."
	}
	serverName := ""
	if serverID > 0 {
		serverName = a.serverName(serverID)
	}
	ch, err := a.VIPNotices.UserChannelCreate(m.DiscordUserID)
	if err == nil {
		_, err = a.VIPNotices.ChannelMessageSendComplex(ch.ID, buildVIPNotice(*tier, serverName, a.siteURL()+"/dashboard/player"))
	}
	if err != nil {
		slog.Warn("component=vip", "event", "grant_dm_failed", "member_id", m.ID, "err", err.Error())
		return "No direct message: the player does not accept direct messages from this server."
	}
	return ""
}

// handleMyVIP is GET .../vip/me: the supporter tier the signed-in player holds, for the Player Hub.
func (a *App) handleMyVIP(w http.ResponseWriter, r *http.Request) {
	er, ok := a.scopedContext(w, r, "")
	if !ok {
		return
	}
	out := map[string]any{"linked": false, "tier": nil}
	if a.VIP == nil {
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if errors.Is(err, economy.ErrIdentityRequired) {
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		economyFailed(w, "load supporter tier", err)
		return
	}
	tier, err := a.VIP.ActiveForPlayer(ctx, er.scope.GuildID, acct.AccountID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load your supporter tier")
		return
	}
	out["linked"] = true
	if tier != nil {
		out["tier"] = tier
	}
	writeSaaSJSON(w, http.StatusOK, out)
}
