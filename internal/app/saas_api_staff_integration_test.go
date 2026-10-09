//go:build integration

package app

import (
	"fmt"
	"context"
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
