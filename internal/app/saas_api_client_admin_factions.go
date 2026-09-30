package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Faction Hub moderation for server staff (docs/CLIENT_ADMIN.md "Factions"). These act on the
// web Faction Hub (hub_factions) - the factions players see in the Player Hub - as opposed to
// the legacy Discord-side factions behind GET .../admin/factions. Every write takes a required
// reason and records an admin_audit_log row; FACTION_MODERATE (Moderator) covers edits, member
// removal, leadership transfer and releasing a claimed flag/armband; FACTION_DISSOLVE
// (Administrator) is the only route that deletes a faction, behind typed confirmation.

const maxModerationReason = 300

func (a *App) registerClientAdminFactionHubRoutes(base string) {
	h := a.HTTPServer.Handle
	h("GET "+base+"/hub-factions", a.handleModerationListFactions)
	h("PUT "+base+"/hub-factions/{factionID}", a.handleModerationUpdateFaction)
	h("DELETE "+base+"/hub-factions/{factionID}/members/{memberID}", a.handleModerationRemoveMember)
	h("POST "+base+"/hub-factions/{factionID}/transfer-leadership", a.handleModerationTransferLeadership)
	h("POST "+base+"/hub-factions/{factionID}/dissolve", a.handleModerationDissolveFaction)
}

type moderatedFactionDTO struct {
	factionSummaryDTO
	Description         string            `json:"description"`
	Leader              *factionMemberDTO `json:"leader"`
	PendingApplications int               `json:"pendingApplications"`
	CreatedAt           string            `json:"createdAt"`
	UpdatedAt           string            `json:"updatedAt"`
	LastActivityAt      *string           `json:"lastActivityAt"`
}

func (a *App) toModeratedFaction(m repository.HubModeratedFaction) moderatedFactionDTO {
	out := moderatedFactionDTO{factionSummaryDTO: toFactionSummary(m.HubFaction, a.assetBaseURL()), Description: m.Description,
		PendingApplications: m.PendingApplicant, CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: m.UpdatedAt.UTC().Format(time.RFC3339),
		LastActivityAt: nullableTimeStr(m.LastActivityAt)}
	if m.Leader != nil {
		l := toFactionMember(*m.Leader)
		out.Leader = &l
	}
	return out
}

// moderationContext is requireCapability plus the Faction Hub availability check.
func (a *App) moderationContext(w http.ResponseWriter, r *http.Request, capability permissions.Capability) (adminActor, int64, bool) {
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return adminActor{}, 0, false
	}
	factionID, good := pathInt64(w, r, "factionID")
	if !good {
		return adminActor{}, 0, false
	}
	if a.FactionHub == nil {
		writeSaaSError(w, codeInternalError, "faction service unavailable")
		return adminActor{}, 0, false
	}
	return ac, factionID, true
}

// moderationReason validates the required, bounded free-text reason. It is stored in the audit
// log only; it is never shown to the faction.
func moderationReason(w http.ResponseWriter, raw string) (string, bool) {
	reason := strings.Join(strings.Fields(raw), " ")
	if reason == "" {
		writeSaaSError(w, codeInvalidRequest, "reason is required")
		return "", false
	}
	if len(reason) > maxModerationReason {
		writeSaaSError(w, codeInvalidRequest, "reason is too long")
		return "", false
	}
	return reason, true
}

func (a *App) handleModerationListFactions(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapFactionModerate)
	if !ok {
		return
	}
	if a.FactionHub == nil {
		writeSaaSError(w, codeInternalError, "faction service unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.FactionHub.ListFactionsForModeration(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, 500)
	if err != nil {
		factionFailed(w, "list factions", err)
		return
	}
	out := make([]moderatedFactionDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, a.toModeratedFaction(m))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

type moderationUpdateRequest struct {
	updateFactionRequest
	// ReleaseFlag / ReleaseArmband free a claimed key (a moderator never assigns one).
	ReleaseFlag    bool   `json:"releaseFlag"`
	ReleaseArmband bool   `json:"releaseArmband"`
	Reason         string `json:"reason"`
}

func (a *App) handleModerationUpdateFaction(w http.ResponseWriter, r *http.Request) {
	ac, factionID, ok := a.moderationContext(w, r, permissions.CapFactionModerate)
	if !ok {
		return
	}
	var req moderationUpdateRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	reason, ok := moderationReason(w, req.Reason)
	if !ok {
		return
	}
	// Staff correct what players wrote; they do not pick branding for them.
	req.FlagKey, req.ArmbandKey, req.PrimaryColor, req.SecondaryColor, req.Requirements = nil, nil, nil, nil, nil
	upd, err := normalizeFactionUpdate(req.updateFactionRequest)
	if err != nil {
		factionFailed(w, "update faction", err)
		return
	}
	empty := ""
	if req.ReleaseFlag {
		upd.FlagKey = &empty
	}
	if req.ReleaseArmband {
		upd.ArmbandKey = &empty
	}
	if upd.Name == nil && upd.Tag == nil && upd.Description == nil && upd.RecruitmentStatus == nil && upd.FlagKey == nil && upd.ArmbandKey == nil {
		writeSaaSError(w, codeInvalidRequest, "nothing to change")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	before, err := a.FactionHub.Get(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, factionID)
	if err != nil {
		factionFailed(w, "load faction", err)
		return
	}
	after, err := a.FactionHub.ModerateFaction(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, factionID, ac.user.ID, upd)
	if err != nil {
		factionFailed(w, "update faction", err)
		return
	}
	a.moderationStatsChanged(ac, factionID)
	a.recordAudit(ctx, ac, "FACTION_MODERATED", factionTarget(factionID), reason, "success", moderationSnapshot(*before), moderationSnapshot(*after))
	writeSaaSJSON(w, http.StatusOK, toFactionSummary(*after, a.assetBaseURL()))
}

type moderationMemberRequest struct {
	Reason string `json:"reason"`
}

func (a *App) handleModerationRemoveMember(w http.ResponseWriter, r *http.Request) {
	ac, factionID, ok := a.moderationContext(w, r, permissions.CapFactionModerate)
	if !ok {
		return
	}
	memberID, good := pathInt64(w, r, "memberID")
	if !good {
		return
	}
	var req moderationMemberRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	reason, ok := moderationReason(w, req.Reason)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	removed, err := a.FactionHub.ModerateRemoveMember(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, factionID, memberID, ac.user.ID)
	if err != nil {
		factionFailed(w, "remove member", err)
		return
	}
	a.moderationStatsChanged(ac, factionID)
	a.recordAudit(ctx, ac, "FACTION_MEMBER_REMOVED", factionTarget(factionID), reason, "success", map[string]any{"memberId": memberID, "userId": removed.User.ID, "role": removed.RoleKey}, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"removed": toFactionMember(*removed)})
}

type moderationTransferRequest struct {
	MemberID int64  `json:"memberId"`
	Reason   string `json:"reason"`
}

func (a *App) handleModerationTransferLeadership(w http.ResponseWriter, r *http.Request) {
	ac, factionID, ok := a.moderationContext(w, r, permissions.CapFactionModerate)
	if !ok {
		return
	}
	var req moderationTransferRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	if req.MemberID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "memberId is required")
		return
	}
	reason, ok := moderationReason(w, req.Reason)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	leader, err := a.FactionHub.ModerateTransferLeadership(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, factionID, req.MemberID, ac.user.ID)
	if err != nil {
		factionFailed(w, "transfer leadership", err)
		return
	}
	a.recordAudit(ctx, ac, "FACTION_LEADERSHIP_TRANSFERRED", factionTarget(factionID), reason, "success", nil, map[string]any{"memberId": leader.ID, "userId": leader.User.ID})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"leader": toFactionMember(leader)})
}

type moderationDissolveRequest struct {
	Confirm string `json:"confirm"`
	Reason  string `json:"reason"`
}

func (a *App) handleModerationDissolveFaction(w http.ResponseWriter, r *http.Request) {
	ac, factionID, ok := a.moderationContext(w, r, permissions.CapFactionDissolve)
	if !ok {
		return
	}
	var req moderationDissolveRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	if !requireConfirmation(w, req.Confirm, "DISSOLVE") {
		return
	}
	reason, ok := moderationReason(w, req.Reason)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	gone, err := a.FactionHub.DissolveFaction(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, factionID)
	if err != nil {
		factionFailed(w, "dissolve faction", err)
		return
	}
	a.moderationStatsChanged(ac, factionID)
	a.recordAudit(ctx, ac, "FACTION_HUB_DISSOLVED", factionTarget(factionID), reason, "success", moderationSnapshot(*gone), nil)
	slog.Info("component=saas_api", "event", "faction_dissolved_by_staff", "organization_id", ac.scope.OrganizationID, "installation_id", ac.scope.InstallationID, "faction_id", factionID, "acting_user_id", ac.user.ID)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"dissolved": true, "faction": toFactionSummary(*gone, a.assetBaseURL())})
}

// moderationSnapshot is the safe before/after shape for the audit log: identifiers and the
// moderated fields, never member lists or application text.
func moderationSnapshot(f repository.HubFaction) map[string]any {
	return map[string]any{"id": f.ID, "name": f.Name, "tag": f.Tag, "recruitmentStatus": f.RecruitmentStatus, "flagKey": f.FlagKey, "armbandKey": f.ArmbandKey, "memberCount": f.MemberCount}
}

// moderationStatsChanged drops the faction's cached figures the way a player edit does.
func (a *App) moderationStatsChanged(ac adminActor, factionID int64) {
	if a.FactionHubStats != nil {
		a.FactionHubStats.Invalidate(ac.scope.OrganizationID, ac.scope.InstallationID, factionID)
	}
}
