//go:build integration

package app

import (
	"context"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Server Hub access for staff by Discord role: someone who is not a member of the organization
// but holds a mapped Discord role can find the installation, read its dashboard and hub, and
// nothing that is OWNER/ADMIN-only or outside the installations they staff.
func TestStaffByDiscordRoleSeesTheServerHub(t *testing.T) {
	w := newClientAdminWorld(t)
	suffix := time.Now().UnixNano()
	staff := syncUser(t, w.a, fmt.Sprintf("staff-%d", suffix), "Staffer")
	stranger := syncUser(t, w.a, fmt.Sprintf("stranger-%d", suffix), "Stranger")
	org := strconv.FormatInt(w.f.OrgID, 10)

	lookup := func(actor string) []StaffInstallationSummary {
		t.Helper()
		rr := w.call(w.a.handleStaffInstallations, http.MethodPost, "/api/saas/staff/installations", actor, staffInstallationsRequest{Guilds: []string{w.f.DiscordGuildID, "some-other-guild"}}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("staff lookup: %d %s", rr.Code, rr.Body.String())
		}
		return decodeBody[[]StaffInstallationSummary](t, rr)
	}
	dashboard := func(actor string) (int, DashboardSummary) {
		t.Helper()
		rr := w.call(w.a.handleDashboard, http.MethodGet, "/api/saas/organizations/"+org+"/dashboard", actor, nil, map[string]string{"installationID": ""})
		if rr.Code != http.StatusOK {
			return rr.Code, DashboardSummary{}
		}
		return rr.Code, decodeBody[DashboardSummary](t, rr)
	}

	// No mapped role yet: nothing to open, and the dashboard is refused.
	if got := lookup(staff.DiscordUserID); len(got) != 0 {
		t.Fatalf("unmapped user found %+v", got)
	}
	if code, _ := dashboard(staff.DiscordUserID); code != http.StatusForbidden {
		t.Fatalf("unmapped user dashboard: %d", code)
	}

	w.mapRole(staff.DiscordUserID, fmt.Sprintf("role-mod-%d", suffix), "MODERATOR")

	got := lookup(staff.DiscordUserID)
	if len(got) != 1 || got[0].InstallationID != w.f.InstallationID || got[0].OrganizationID != w.f.OrgID || got[0].Level != "MODERATOR" {
		t.Fatalf("staff lookup = %+v", got)
	}
	// The owner is a member: their own organization never shows up as a staff hub.
	if owner := lookup(w.f.OwnerDiscordID); len(owner) != 0 {
		t.Fatalf("owner listed as staff: %+v", owner)
	}

	code, dash := dashboard(staff.DiscordUserID)
	if code != http.StatusOK || dash.Organization.Role != repository.RoleStaff || len(dash.Installations) != 1 || dash.Installations[0].ID != w.f.InstallationID {
		t.Fatalf("staff dashboard: %d %+v", code, dash)
	}
	hub := w.call(w.a.handleGetInstallationHub, http.MethodGet, "/hub", staff.DiscordUserID, nil, nil)
	if hub.Code != http.StatusOK {
		t.Fatalf("staff hub: %d %s", hub.Code, hub.Body.String())
	}
	me := w.call(w.a.handleClientAdminMe, http.MethodGet, w.path("/me"), staff.DiscordUserID, nil, nil)
	if me.Code != http.StatusOK || decodeBody[clientAdminMeResponse](t, me).Level != "MODERATOR" {
		t.Fatalf("staff /admin/me: %d %s", me.Code, me.Body.String())
	}

	// Owner/admin-only routes stay closed to staff.
	memberOnly := httptest.NewRecorder()
	if _, ok := w.a.requireOrganizationMember(memberOnly, withActingUser(saasRequest(http.MethodGet, "/", nil), staff.DiscordUserID), w.f.OrgID, w.a.mustUser(t, staff.DiscordUserID)); ok || memberOnly.Code != http.StatusForbidden {
		t.Fatalf("member-only route let staff in: %d", memberOnly.Code)
	}
	// An installation of the same organization they don't staff is refused.
	other := w.call(w.a.handleGetInstallationHub, http.MethodGet, "/hub", staff.DiscordUserID, nil, map[string]string{"installationID": "999999999"})
	if other.Code != http.StatusForbidden {
		t.Fatalf("unstaffed installation: %d %s", other.Code, other.Body.String())
	}
	// Someone without the role gets nothing.
	if code, _ := dashboard(stranger.DiscordUserID); code != http.StatusForbidden {
		t.Fatalf("stranger dashboard: %d", code)
	}
}

// The Staff page lists the guild's roles to pick from, and shows mapped roles by name.
func TestStaffPageListsDiscordRolesByName(t *testing.T) {
	w := newClientAdminWorld(t)
	w.verifier.guildRoles = map[string][]discordRoleFixture{w.f.DiscordGuildID: {{ID: "r-mod", Name: "Moderator", Position: 2}, {ID: "r-bot", Name: "Champion", Position: 3, Managed: true}}}
	rr := w.call(w.a.handleListDiscordRoles, http.MethodGet, w.path("/discord-roles"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("discord roles: %d %s", rr.Code, rr.Body.String())
	}
	roles := decodeBody[struct {
		Items []discordRoleDTO `json:"items"`
	}](t, rr).Items
	if len(roles) != 1 || roles[0].ID != "r-mod" || roles[0].Name != "Moderator" {
		t.Fatalf("roles = %+v (bot-managed roles must be left out)", roles)
	}
	rr = w.call(w.a.handleSetPermission, http.MethodPut, w.path("/permissions/r-mod"), w.f.OwnerDiscordID, setPermissionRequest{Level: "MODERATOR"}, map[string]string{"discordRoleID": "r-mod"})
	if rr.Code != http.StatusOK {
		t.Fatalf("map role: %d %s", rr.Code, rr.Body.String())
	}
	rr = w.call(w.a.handleListPermissions, http.MethodGet, w.path("/permissions"), w.f.OwnerDiscordID, nil, nil)
	items := decodeBody[struct {
		Items []rolePermissionDTO `json:"items"`
	}](t, rr).Items
	if len(items) != 1 || items[0].DiscordRoleName != "Moderator" {
		t.Fatalf("permissions = %+v", items)
	}
}

func (a *App) mustUser(t *testing.T, discordUserID string) int64 {
	t.Helper()
	user, err := a.SaaSUsers.GetByDiscordID(context.Background(), discordUserID)
	if err != nil || user == nil {
		t.Fatalf("user %s: %v", discordUserID, err)
	}
	return user.ID
}

// A staff Administrator (a non-member whose Discord role is mapped to ADMINISTRATOR) runs the
// server like the owner: the organization-manager routes behind the website's "Discord &
// channels" tab admit them; a staff Moderator is still refused, and billing stays the owner's.
func TestStaffAdministratorIsAnOrganizationManager(t *testing.T) {
	w := newClientAdminWorld(t)
	suffix := time.Now().UnixNano()
	admin := syncUser(t, w.a, fmt.Sprintf("staff-admin-%d", suffix), "Staff admin")
	mod := syncUser(t, w.a, fmt.Sprintf("staff-mod-%d", suffix), "Staff mod")
	w.mapRole(admin.DiscordUserID, fmt.Sprintf("role-admin-%d", suffix), "ADMINISTRATOR")
	w.mapRole(mod.DiscordUserID, fmt.Sprintf("role-mod-%d", suffix), "MODERATOR")

	// Saving channel routes: a manager route with an {installationID}. The fake Discord knows
	// one channel in the installation's guild for the killfeed.
	if w.verifier.channelFound == nil {
		w.verifier.channelFound = map[string]bool{}
	}
	w.verifier.guildFound[w.f.DiscordGuildID] = true
	w.verifier.channelFound[w.f.DiscordGuildID+"|staff-kf-chan"] = true
	seedGuildChannels(w.verifier, w.f.DiscordGuildID, fakeGuildChannel{ID: "staff-kf-chan", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	routes := map[string]string{"KILLFEED": "staff-kf-chan"}
	if rr := saveChannelRoutes(t, w.a, w.f.OrgID, w.f.InstallationID, mod.DiscordUserID, routes); rr.Code != http.StatusForbidden {
		t.Fatalf("staff moderator saved channel routes: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, w.a, w.f.OrgID, w.f.InstallationID, admin.DiscordUserID, routes); rr.Code != http.StatusOK {
		t.Fatalf("staff administrator could not save channel routes: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, w.a, w.f.OrgID, w.f.InstallationID+1000, admin.DiscordUserID, routes); rr.Code != http.StatusForbidden {
		t.Fatalf("staff administrator reached an installation they do not staff: %d %s", rr.Code, rr.Body.String())
	}

	// The plan is the owner's: billing refuses a staff administrator like any non-owner.
	ctx := context.Background()
	role, ok, err := w.a.organizationManager(ctx, w.f.OrgID, &repository.AppUser{ID: admin.ID, DiscordUserID: admin.DiscordUserID}, "")
	if err != nil || !ok || role != repository.RoleStaff {
		t.Fatalf("organizationManager for a staff administrator: %q %v %v", role, ok, err)
	}
	if _, ok, _ := w.a.organizationManager(ctx, w.f.OrgID, &repository.AppUser{ID: mod.ID, DiscordUserID: mod.DiscordUserID}, ""); ok {
		t.Fatal("a staff moderator is not an organization manager")
	}
	org := strconv.FormatInt(w.f.OrgID, 10)
	rr := w.call(w.a.handleStartTrial, http.MethodPost, "/api/saas/organizations/"+org+"/trial/start", admin.DiscordUserID, map[string]any{}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("staff administrator reached trial start (the plan is the owner's): %d %s", rr.Code, rr.Body.String())
	}
}
