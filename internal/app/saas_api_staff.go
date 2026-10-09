package app

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Server Hub access for staff by Discord role (docs/CLIENT_ADMIN.md "Staff access"). Someone who
// is not a member of an organization, but holds a Discord role the Staff page maps to a level on
// one of its installations, can open that installation's Server Hub: the read routes below accept
// them with the effective role STAFF, and the client-admin routes gate each action by their level
// as they always have. Removing the role in Discord removes the access (roles are re-read every
// discordRoleCacheTTL).

// staffLevels returns the installations among candidates on which user holds a staff
// level, with that level. Lookups run against each guild's live roles (cached briefly).
func (a *App) staffLevels(ctx context.Context, user *repository.AppUser, candidates []repository.StaffInstallation) map[int64]permissions.Level {
	out := map[int64]permissions.Level{}
	for _, c := range candidates {
		scope := repository.AdminScope{OrganizationID: c.OrganizationID, InstallationID: c.InstallationID, GuildID: c.GuildID, DiscordGuildID: c.DiscordGuildID}
		level, _, err := a.actorLevel(ctx, scope, user)
		if err != nil {
			// Not in that guild, or Discord unavailable: no access through it.
			continue
		}
		if level > permissions.LevelNone {
			out[c.InstallationID] = level
		}
	}
	return out
}

// staffInstallationsForOrganization is the set of the organization's installations user staffs.
func (a *App) staffInstallationsForOrganization(ctx context.Context, organizationID int64, user *repository.AppUser) (map[int64]permissions.Level, error) {
	if a.ClientAdmin == nil || user == nil || user.DiscordUserID == "" {
		return nil, nil
	}
	candidates, err := a.ClientAdmin.StaffInstallationsForOrganization(ctx, organizationID)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	return a.staffLevels(ctx, user, candidates), nil
}

// requireOrganizationViewer is requireOrganizationMember for read routes that staff may use too.
// A member gets their role. A non-member who staffs one of the organization's installations gets
// RoleStaff; on a route with an {installationID}, only for an installation they staff.
func (a *App) requireOrganizationViewer(w http.ResponseWriter, r *http.Request, organizationID int64, user *repository.AppUser) (role string, ok bool) {
	if a.SaaSOrganizations == nil {
		writeSaaSError(w, codeInternalError, "organization directory unavailable")
		return "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	role, member, err := a.SaaSOrganizations.VerifyMembership(ctx, organizationID, user.ID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "membership check failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify membership")
		return "", false
	}
	if member {
		return role, true
	}
	staffed, err := a.staffInstallationsForOrganization(ctx, organizationID, user)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "staff access check failed", "err", err.Error())
	}
	if len(staffed) > 0 {
		if raw := r.PathValue("installationID"); raw == "" || staffedInstallation(staffed, raw) {
			return repository.RoleStaff, true
		}
	}
	writeSaaSError(w, codeForbidden, "not a member of this organization")
	return "", false
}

func staffedInstallation(staffed map[int64]permissions.Level, raw string) bool {
	id, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && staffed[id] > permissions.LevelNone
}

// StaffInstallationSummary is one Server Hub the acting user can open as staff.
type StaffInstallationSummary struct {
	OrganizationID   int64  `json:"organizationId"`
	OrganizationName string `json:"organizationName"`
	InstallationID   int64  `json:"installationId"`
	DiscordGuildID   string `json:"discordGuildId"`
	GuildName        string `json:"guildName"`
	Level            string `json:"level"`
}

type staffInstallationsRequest struct {
	// Guilds are the Discord guild IDs the user's Discord sign-in reported: only installations in
	// those guilds are checked, so the lookup never scans every customer.
	Guilds []string `json:"guilds"`
}

const maxStaffGuilds = 200

// handleStaffInstallations is POST /api/saas/staff/installations: the Server Hubs the acting user
// may open because of their Discord roles, excluding organizations they are a member of (those
// come from GET /api/saas/organizations).
func (a *App) handleStaffInstallations(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	req, good := decodeJSONBody[staffInstallationsRequest](w, r)
	if !good {
		return
	}
	guilds := make([]string, 0, len(req.Guilds))
	seen := map[string]bool{}
	for _, g := range req.Guilds {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] || len(g) > 32 {
			continue
		}
		seen[g] = true
		guilds = append(guilds, g)
		if len(guilds) == maxStaffGuilds {
			break
		}
	}
	out := []StaffInstallationSummary{}
	if a.ClientAdmin == nil || a.SaaSOrganizations == nil || len(guilds) == 0 {
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	candidates, err := a.ClientAdmin.StaffInstallationsInGuilds(ctx, guilds)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "staff installation lookup failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not look up staff access")
		return
	}
	memberOf := map[int64]bool{}
	var open []repository.StaffInstallation
	for _, c := range candidates {
		member, checked := memberOf[c.OrganizationID]
		if !checked {
			_, member, err = a.SaaSOrganizations.VerifyMembership(ctx, c.OrganizationID, user.ID)
			if err != nil {
				continue
			}
			memberOf[c.OrganizationID] = member
		}
		if !member {
			open = append(open, c)
		}
	}
	levels := a.staffLevels(ctx, user, open)
	for _, c := range open {
		level, ok := levels[c.InstallationID]
		if !ok {
			continue
		}
		out = append(out, StaffInstallationSummary{OrganizationID: c.OrganizationID, OrganizationName: c.OrganizationName, InstallationID: c.InstallationID,
			DiscordGuildID: c.DiscordGuildID, GuildName: c.GuildName, Level: level.String()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GuildName < out[j].GuildName })
	writeSaaSJSON(w, http.StatusOK, out)
}
