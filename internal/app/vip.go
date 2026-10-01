package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
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
