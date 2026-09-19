//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Real-PostgreSQL tests for the Faction Hub repository (docs/FACTIONS.md). Every fixture is
// created with unique names and removed by t.Cleanup, against the throwaway integration
// database only (TEST_DATABASE_URL + ALLOW_INTEGRATION_DB_TESTS=true).

type hubWorld struct {
	t    *testing.T
	ctx  context.Context
	db   *database.DB
	repo *FactionHubRepository

	org1, inst1, inst1b int64 // org 1 has two installations (two DayZ servers of one guild)
	org2, inst2         int64
	instNoServer        int64 // org 1, no DayZ server selected
	instSuspended       int64 // org 1, suspended
	suffix              int64
	nextUser            int
}

func newHubWorld(t *testing.T) *hubWorld {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	w := &hubWorld{t: t, ctx: ctx, db: db, repo: NewFactionHubRepository(db.Pool), suffix: time.Now().UnixNano()}

	owner1, owner2 := w.newUser(), w.newUser()
	w.org1 = w.newOrg(owner1, "a")
	w.org2 = w.newOrg(owner2, "b")
	guild1, guild2 := w.newGuild("g1"), w.newGuild("g2")
	conn1, conn2 := w.newConnection(w.org1, guild1), w.newConnection(w.org2, guild2)
	server1a, server1b := w.newServer(guild1, "s1a"), w.newServer(guild1, "s1b")
	server2 := w.newServer(guild2, "s2")
	w.inst1 = w.newInstallation(w.org1, conn1, &server1a, "READY")
	w.inst1b = w.newInstallation(w.org1, conn1, &server1b, "READY")
	w.inst2 = w.newInstallation(w.org2, conn2, &server2, "READY")
	w.instNoServer = w.newInstallation(w.org1, conn1, nil, "CONFIGURING")
	guild3 := w.newGuild("g3")
	conn3 := w.newConnection(w.org1, guild3)
	server3 := w.newServer(guild3, "s3")
	w.instSuspended = w.newInstallation(w.org1, conn3, &server3, "SUSPENDED")

	t.Cleanup(func() {
		// Cascades to installations, factions, members, applications, connections, servers.
		for _, org := range []int64{w.org1, w.org2} {
			_, _ = db.Pool.Exec(ctx, `DELETE FROM organizations WHERE id=$1`, org)
		}
		_, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE discord_guild_id LIKE $1`, fmt.Sprintf("hub-%d-%%", w.suffix))
		_, _ = db.Pool.Exec(ctx, `DELETE FROM app_users WHERE discord_user_id LIKE $1`, fmt.Sprintf("hub-%d-%%", w.suffix))
	})
	return w
}

func (w *hubWorld) one(sql string, args ...any) int64 {
	w.t.Helper()
	var id int64
	if err := w.db.Pool.QueryRow(w.ctx, sql, args...).Scan(&id); err != nil {
		w.t.Fatalf("fixture %q: %v", sql, err)
	}
	return id
}

func (w *hubWorld) newUser() int64 {
	w.nextUser++
	return w.one(`INSERT INTO app_users(discord_user_id, discord_username, discord_global_name) VALUES($1,$2,$3) RETURNING id`,
		fmt.Sprintf("hub-%d-u%d", w.suffix, w.nextUser), fmt.Sprintf("user%d", w.nextUser), fmt.Sprintf("User %d", w.nextUser))
}

func (w *hubWorld) newUsers(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = w.newUser()
	}
	return out
}

func (w *hubWorld) newOrg(owner int64, tag string) int64 {
	return w.one(`INSERT INTO organizations(name, slug, owner_user_id) VALUES($1,$2,$3) RETURNING id`, "Hub Org "+tag, fmt.Sprintf("hub-%d-%s", w.suffix, tag), owner)
}

func (w *hubWorld) newGuild(tag string) int64 {
	return w.one(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("hub-%d-%s", w.suffix, tag))
}

func (w *hubWorld) newConnection(org, guild int64) int64 {
	return w.one(`INSERT INTO discord_guild_connections(organization_id, guild_id) VALUES($1,$2) RETURNING id`, org, guild)
}

func (w *hubWorld) newServer(guild int64, tag string) int64 {
	return w.one(`INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		guild, fmt.Sprintf("hub-%d-%s", w.suffix, tag))
}

func (w *hubWorld) newInstallation(org, conn int64, server *int64, status string) int64 {
	return w.one(`INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,$4) RETURNING id`, org, conn, server, status)
}

func (w *hubWorld) create(inst, user int64, name, tag, status string) *HubFaction {
	w.t.Helper()
	org := w.org1
	if inst == w.inst2 {
		org = w.org2
	}
	f, err := w.repo.CreateFaction(w.ctx, org, inst, user, HubFactionInput{Name: name, Tag: tag, Description: "desc of " + name, RecruitmentStatus: status})
	if err != nil {
		w.t.Fatalf("create faction %q: %v", name, err)
	}
	return f
}

func wantErr(t *testing.T, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s: want %v, got %v", what, want, got)
	}
}

func (w *hubWorld) memberIDOf(factionID, userID int64) int64 {
	return w.one(`SELECT id FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2`, factionID, userID)
}

func (w *hubWorld) roleOf(factionID, userID int64) string {
	w.t.Helper()
	role, err := w.repo.MemberRole(w.ctx, w.orgOf(factionID), w.instOf(factionID), factionID, userID)
	if err != nil {
		w.t.Fatal(err)
	}
	return role
}

func (w *hubWorld) orgOf(factionID int64) int64 {
	return w.one(`SELECT organization_id FROM hub_factions WHERE id=$1`, factionID)
}
func (w *hubWorld) instOf(factionID int64) int64 {
	return w.one(`SELECT installation_id FROM hub_factions WHERE id=$1`, factionID)
}

func (w *hubWorld) count(sql string, args ...any) int {
	w.t.Helper()
	return int(w.one(sql, args...))
}

// --- schema ------------------------------------------------------------------------------------

func TestHubMigrationSeedsRolesAndEnforcesTenantIntegrity(t *testing.T) {
	w := newHubWorld(t)
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_roles WHERE faction_id IS NULL AND is_system`); n != 3 {
		t.Fatalf("expected the 3 built-in system roles, got %d", n)
	}
	// Re-running the migration set is idempotent (seed uses ON CONFLICT DO NOTHING).
	if err := w.db.Migrate(w.ctx); err != nil {
		t.Fatal(err)
	}
	users := w.newUsers(2)
	f := w.create(w.inst1, users[0], "Schema Test", "SCH", "OPEN")

	// A faction cannot claim an organization that does not own its installation.
	_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO hub_factions(organization_id, installation_id, game_server_id, name, tag, slug, created_by_user_id)
SELECT $1, $2, game_server_id, 'Wrong Org', 'WO', 'wrong-org', $3 FROM installations WHERE id=$2`, w.org2, w.inst1, users[0])
	if err == nil {
		t.Fatal("a faction with a mismatched organization/installation must be rejected by the schema")
	}
	// A member cannot sit in a different installation than its faction.
	_, err = w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_members(faction_id, installation_id, user_id, role_key) VALUES($1,$2,$3,'MEMBER')`, f.ID, w.inst1b, users[1])
	if err == nil {
		t.Fatal("a member with a mismatched installation must be rejected by the schema")
	}
	// Same for an application.
	_, err = w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_applications(faction_id, installation_id, user_id) VALUES($1,$2,$3)`, f.ID, w.inst1b, users[1])
	if err == nil {
		t.Fatal("an application with a mismatched installation must be rejected by the schema")
	}
	// At most one LEADER per faction.
	_, err = w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_members(faction_id, installation_id, user_id, role_key) VALUES($1,$2,$3,'LEADER')`, f.ID, w.inst1, users[1])
	if err == nil {
		t.Fatal("a second LEADER must be rejected by the schema")
	}
	// One active faction per user per installation, even bypassing the service.
	other := w.create(w.inst1, w.newUser(), "Schema Other", "SCO", "OPEN")
	_, err = w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_members(faction_id, installation_id, user_id, role_key) VALUES($1,$2,$3,'MEMBER')`, other.ID, w.inst1, users[0])
	if err == nil {
		t.Fatal("the schema must forbid a second faction for the same user on one installation")
	}
	// Invalid enumerated values are rejected.
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE hub_factions SET recruitment_status='PUBLIC' WHERE id=$1`, f.ID); err == nil {
		t.Fatal("invalid recruitment status must be rejected")
	}
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE hub_factions SET primary_color='red' WHERE id=$1`, f.ID); err == nil {
		t.Fatal("invalid color must be rejected")
	}
}

// --- faction creation ---------------------------------------------------------------------------

func TestHubCreateFactionRulesAndSlugs(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(6)

	f := w.create(w.inst1, u[0], "Unit Zero", "uz", "OPEN")
	if f.Slug != "unit-zero" || f.Tag != "uz" || f.MemberCount != 1 || f.RecruitmentStatus != "OPEN" || f.CreatedByUserID != u[0] {
		t.Fatalf("unexpected faction: %+v", f)
	}
	if f.OrganizationID != w.org1 || f.InstallationID != w.inst1 || f.GameServerID == 0 {
		t.Fatalf("scope not recorded: %+v", f)
	}
	if role := w.roleOf(f.ID, u[0]); role != factionhub.RoleLeader {
		t.Fatalf("creator must be LEADER, got %q", role)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_settings WHERE faction_id=$1`, f.ID); n != 1 {
		t.Fatal("settings row must be created with the faction")
	}

	// Same name / tag on the same installation (case-insensitive) is a conflict.
	_, err := w.repo.CreateFaction(w.ctx, w.org1, w.inst1, u[1], HubFactionInput{Name: "UNIT ZERO", Tag: "XX", RecruitmentStatus: "OPEN"})
	wantErr(t, "duplicate name", err, factionhub.ErrNameTaken)
	_, err = w.repo.CreateFaction(w.ctx, w.org1, w.inst1, u[1], HubFactionInput{Name: "Other Name", Tag: "UZ", RecruitmentStatus: "OPEN"})
	wantErr(t, "duplicate tag", err, factionhub.ErrTagTaken)
	if n := w.count(`SELECT COUNT(*) FROM hub_factions WHERE installation_id=$1`, w.inst1); n != 1 {
		t.Fatalf("a failed create must leave nothing behind, got %d factions", n)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE user_id=$1`, u[1]); n != 0 {
		t.Fatal("a failed create must not leave a membership")
	}

	// The same name and tag are fine on another installation of the same organization
	// (another DayZ server) and on another organization's installation.
	if g := w.create(w.inst1b, u[1], "Unit Zero", "UZ", "OPEN"); g.Slug != "unit-zero" || g.InstallationID != w.inst1b {
		t.Fatalf("same name on a different installation must work with its own slug: %+v", g)
	}
	if g := w.create(w.inst2, u[2], "Unit Zero", "UZ", "OPEN"); g.OrganizationID != w.org2 {
		t.Fatalf("same name on another organization must work: %+v", g)
	}

	// A different name that slugs the same gets a suffix; a third gets the next.
	g2 := w.create(w.inst1, u[3], "Unit-Zero", "UZ2", "CLOSED")
	if g2.Slug != "unit-zero-2" {
		t.Fatalf("slug collision must be suffixed, got %q", g2.Slug)
	}
	g3 := w.create(w.inst1, u[4], "unit zero!", "UZ3", "CLOSED")
	if g3.Slug != "unit-zero-3" {
		t.Fatalf("second collision, got %q", g3.Slug)
	}

	// One faction per user per installation.
	_, err = w.repo.CreateFaction(w.ctx, w.org1, w.inst1, u[0], HubFactionInput{Name: "Second", Tag: "SEC", RecruitmentStatus: "OPEN"})
	wantErr(t, "leader founding a second faction", err, factionhub.ErrAlreadyInFaction)
	// ...but a user may lead a faction on a different installation.
	if g := w.create(w.inst1b, u[0], "Leader Elsewhere", "LE", "OPEN"); g.InstallationID != w.inst1b {
		t.Fatal("the rule is per installation")
	}

	// Installation preconditions.
	_, err = w.repo.CreateFaction(w.ctx, w.org1, w.instNoServer, u[5], HubFactionInput{Name: "No Server", Tag: "NS", RecruitmentStatus: "OPEN"})
	wantErr(t, "installation without a DayZ server", err, factionhub.ErrNoServer)
	_, err = w.repo.CreateFaction(w.ctx, w.org1, w.instSuspended, u[5], HubFactionInput{Name: "Suspended", Tag: "SU", RecruitmentStatus: "OPEN"})
	wantErr(t, "suspended installation", err, factionhub.ErrInstallationInert)
	// The installation must belong to the organization.
	_, err = w.repo.CreateFaction(w.ctx, w.org2, w.inst1, u[5], HubFactionInput{Name: "Wrong Org", Tag: "WO", RecruitmentStatus: "OPEN"})
	wantErr(t, "installation of another organization", err, factionhub.ErrNotFound)

	// Creating a faction cancels the founder's pending applications on that installation.
	lead := w.newUser()
	target := w.create(w.inst1, lead, "Recruiter", "REC", "OPEN")
	founder := w.newUser()
	app, err := w.repo.Apply(w.ctx, w.org1, w.inst1, target.ID, founder, "let me in")
	if err != nil {
		t.Fatal(err)
	}
	w.create(w.inst1, founder, "Founder Faction", "FF", "OPEN")
	var status string
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT status FROM hub_faction_applications WHERE id=$1`, app.ID).Scan(&status); err != nil || status != "CANCELLED" {
		t.Fatalf("pending application must be CANCELLED after founding a faction, got %q (%v)", status, err)
	}
}

func TestHubDirectoryPaginationSearchRecruitingAndCounts(t *testing.T) {
	w := newHubWorld(t)
	users := w.newUsers(8)
	names := []struct{ name, tag, status string }{
		{"Alpha Wolves", "AW", "OPEN"}, {"Bravo Company", "BC", "CLOSED"}, {"Charlie 100%", "C100", "OPEN"},
		{"Delta_Force", "DF", "INVITE_ONLY"}, {"Echo Team", "ET", "OPEN"},
	}
	var ids []int64
	for i, n := range names {
		ids = append(ids, w.create(w.inst1, users[i], n.name, n.tag, n.status).ID)
	}
	// Two extra members in Echo Team, to check counts are per faction and correct.
	for _, u := range users[5:7] {
		app, err := w.repo.Apply(w.ctx, w.org1, w.inst1, ids[4], u, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, ids[4], app.ID, users[4]); err != nil {
			t.Fatal(err)
		}
	}
	// A faction on another installation never shows up.
	w.create(w.inst1b, users[7], "Alpha Elsewhere", "AE", "OPEN")

	page1, more, err := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 2})
	if err != nil || len(page1) != 2 || !more {
		t.Fatalf("page1: %d items more=%v err=%v", len(page1), more, err)
	}
	if page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("directory is newest first: got %d,%d", page1[0].ID, page1[1].ID)
	}
	if page1[0].MemberCount != 3 || page1[1].MemberCount != 1 {
		t.Fatalf("member counts: %d,%d", page1[0].MemberCount, page1[1].MemberCount)
	}
	page2, more, err := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 2, AfterID: page1[1].ID})
	if err != nil || len(page2) != 2 || !more || page2[0].ID != ids[2] || page2[1].ID != ids[1] {
		t.Fatalf("page2: %+v more=%v err=%v", page2, more, err)
	}
	page3, more, err := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 2, AfterID: page2[1].ID})
	if err != nil || len(page3) != 1 || more || page3[0].ID != ids[0] {
		t.Fatalf("page3: %+v more=%v err=%v", page3, more, err)
	}

	rec, _, err := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 50, RecruitingOnly: true})
	if err != nil || len(rec) != 3 {
		t.Fatalf("recruiting=OPEN only: got %d (%v)", len(rec), err)
	}
	for _, f := range rec {
		if f.RecruitmentStatus != "OPEN" {
			t.Fatalf("non-OPEN faction in recruiting filter: %+v", f)
		}
	}

	for search, want := range map[string][]int64{
		"wolves": {ids[0]}, "WOLVES": {ids[0]}, "bc": {ids[1]}, "Delta": {ids[3]}, "team": {ids[4]},
		"100%": {ids[2]}, "%": {ids[2]}, "_": {ids[3]}, // LIKE wildcards are literal characters
		"nothing-matches": {},
	} {
		got, _, err := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 50, Search: search})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) || (len(want) == 1 && got[0].ID != want[0]) {
			t.Errorf("search %q: got %d results, want %v", search, len(got), want)
		}
	}
	// Name OR tag.
	if got, _, _ := w.repo.Directory(w.ctx, w.org1, w.inst1, HubDirectoryQuery{Limit: 50, Search: "c100"}); len(got) != 1 || got[0].ID != ids[2] {
		t.Fatal("tag search must match")
	}
	// Foreign organization / installation.
	_, _, err = w.repo.Directory(w.ctx, w.org2, w.inst1, HubDirectoryQuery{Limit: 10})
	wantErr(t, "directory across organizations", err, factionhub.ErrNotFound)
}

func TestHubUpdateFactionLeaderOnly(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(4)
	f := w.create(w.inst1, u[0], "Editable", "ED", "CLOSED")
	// promote u[1] to officer, u[2] stays plain member
	for _, m := range []int64{u[1], u[2]} {
		app, _ := w.repo.Apply(w.ctx, w.org1, w.inst1, w.openFor(f), m, "")
		if _, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, f.ID, app.ID, u[0]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.repo.PromoteMember(w.ctx, w.org1, w.inst1, f.ID, w.memberIDOf(f.ID, u[1]), u[0]); err != nil {
		t.Fatal(err)
	}

	name, tag, desc, status, color := "Renamed Faction", "RN", "new description", "OPEN", "#c0ffee"
	upd := HubFactionUpdate{Name: &name, Tag: &tag, Description: &desc, RecruitmentStatus: &status, PrimaryColor: &color,
		Settings: &factionhub.Settings{PvPRequired: true, MicRequired: true, CustomRequirements: "be nice"}}
	for who, user := range map[string]int64{"officer": u[1], "member": u[2], "outsider": u[3]} {
		_, err := w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, user, upd)
		wantErr(t, who+" updating", err, factionhub.ErrForbidden)
	}
	got, err := w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, u[0], upd)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name || got.Tag != tag || got.Description != desc || got.RecruitmentStatus != "OPEN" || got.PrimaryColor == nil || *got.PrimaryColor != "#c0ffee" {
		t.Fatalf("update not applied: %+v", got)
	}
	if got.Slug != "editable" {
		t.Fatalf("slug must stay stable across renames, got %q", got.Slug)
	}
	if got.OrganizationID != w.org1 || got.InstallationID != w.inst1 {
		t.Fatal("scope must never change")
	}
	if !got.Settings.PvPRequired || !got.Settings.MicRequired || got.Settings.CustomRequirements != "be nice" {
		t.Fatalf("settings not applied: %+v", got.Settings)
	}
	// A partial update leaves everything else untouched; "" clears a color.
	empty := ""
	only := "Only Name"
	got, err = w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, u[0], HubFactionUpdate{Name: &only, PrimaryColor: &empty})
	if err != nil || got.Name != "Only Name" || got.Tag != "RN" || got.PrimaryColor != nil || !got.Settings.PvPRequired {
		t.Fatalf("partial update wrong: %+v %v", got, err)
	}
	// Renaming onto another faction's name/tag conflicts.
	w.create(w.inst1, w.newUser(), "Taken Name", "TK", "OPEN")
	taken, takenTag := "taken name", "tk"
	_, err = w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, u[0], HubFactionUpdate{Name: &taken})
	wantErr(t, "rename onto existing name", err, factionhub.ErrNameTaken)
	_, err = w.repo.UpdateFaction(w.ctx, w.org1, w.inst1, f.ID, u[0], HubFactionUpdate{Tag: &takenTag})
	wantErr(t, "retag onto existing tag", err, factionhub.ErrTagTaken)
	// The visual keys are placeholders: nothing in the update path writes them.
	if got.LogoKey != nil || got.FlagKey != nil || got.ArmbandKey != nil {
		t.Fatal("visual keys must stay unset")
	}
}

// openFor reopens recruiting on f directly so helper applications work regardless of its status.
func (w *hubWorld) openFor(f *HubFaction) int64 {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE hub_factions SET recruitment_status='OPEN' WHERE id=$1`, f.ID); err != nil {
		w.t.Fatal(err)
	}
	return f.ID
}

// --- applications ---------------------------------------------------------------------------------

func TestHubApplicationRules(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(8)
	open := w.create(w.inst1, u[0], "Open House", "OH", "OPEN")
	inviteOnly := w.create(w.inst1, u[1], "By Invitation", "BI", "INVITE_ONLY")
	closed := w.create(w.inst1, u[2], "Closed Shop", "CS", "CLOSED")
	elsewhere := w.create(w.inst1b, u[3], "Other Server", "OS", "OPEN")

	apply := func(inst, faction, user int64) (*HubApplication, error) {
		return w.repo.Apply(w.ctx, w.org1, inst, faction, user, "hello")
	}

	// Recruitment status gates applications.
	_, err := apply(w.inst1, inviteOnly.ID, u[4])
	wantErr(t, "INVITE_ONLY", err, factionhub.ErrRecruitmentClosed)
	_, err = apply(w.inst1, closed.ID, u[4])
	wantErr(t, "CLOSED", err, factionhub.ErrRecruitmentClosed)

	app, err := apply(w.inst1, open.ID, u[4])
	if err != nil || app.Status != "PENDING" || app.Message != "hello" || app.User.ID != u[4] || app.FactionSlug != "open-house" {
		t.Fatalf("apply: %+v %v", app, err)
	}
	// No duplicate pending application to the same faction.
	_, err = apply(w.inst1, open.ID, u[4])
	wantErr(t, "duplicate pending", err, factionhub.ErrAlreadyApplied)
	// A member cannot apply (the leader of the faction itself, or of another).
	_, err = apply(w.inst1, open.ID, u[0])
	wantErr(t, "leader applying to own faction", err, factionhub.ErrAlreadyInFaction)
	_, err = apply(w.inst1, open.ID, u[1])
	wantErr(t, "leader of another faction applying", err, factionhub.ErrAlreadyInFaction)
	// The faction must exist on THAT installation.
	_, err = apply(w.inst1b, open.ID, u[5])
	wantErr(t, "faction id on the wrong installation", err, factionhub.ErrNotFound)
	_, err = w.repo.Apply(w.ctx, w.org2, w.inst2, open.ID, u[5], "")
	wantErr(t, "faction id on another organization", err, factionhub.ErrNotFound)
	// A user may apply on another installation; the rule is per installation.
	if _, err := apply(w.inst1b, elsewhere.ID, u[0]); err != nil {
		t.Fatalf("applying on another installation must work: %v", err)
	}
	// Suspended installation.
	_, err = w.repo.Apply(w.ctx, w.org1, w.instSuspended, open.ID, u[5], "")
	wantErr(t, "suspended installation", err, factionhub.ErrInstallationInert)

	// Withdraw: only the applicant, only while pending; history is kept.
	if _, err := w.repo.WithdrawApplication(w.ctx, w.org1, w.inst1, open.ID, app.ID, u[6]); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("someone else's application must look like it does not exist, got %v", err)
	}
	withdrawn, err := w.repo.WithdrawApplication(w.ctx, w.org1, w.inst1, open.ID, app.ID, u[4])
	if err != nil || withdrawn.Status != "WITHDRAWN" {
		t.Fatalf("withdraw: %+v %v", withdrawn, err)
	}
	_, err = w.repo.WithdrawApplication(w.ctx, w.org1, w.inst1, open.ID, app.ID, u[4])
	wantErr(t, "withdrawing twice", err, factionhub.ErrNotPending)
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_applications WHERE id=$1`, app.ID); n != 1 {
		t.Fatal("application history must not be deleted")
	}
	// After withdrawing, applying again is allowed (a new row).
	again, err := apply(w.inst1, open.ID, u[4])
	if err != nil || again.ID == app.ID {
		t.Fatalf("re-apply after withdraw: %+v %v", again, err)
	}

	// Deny: leader/officer only; records reviewer; not repeatable.
	if _, err := w.repo.DenyApplication(w.ctx, w.org1, w.inst1, open.ID, again.ID, u[7]); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("an outsider must not deny, got %v", err)
	}
	denied, err := w.repo.DenyApplication(w.ctx, w.org1, w.inst1, open.ID, again.ID, u[0])
	if err != nil || denied.Status != "DENIED" || denied.ReviewedByUserID == nil || *denied.ReviewedByUserID != u[0] || denied.ReviewedAt == nil {
		t.Fatalf("deny: %+v %v", denied, err)
	}
	_, err = w.repo.DenyApplication(w.ctx, w.org1, w.inst1, open.ID, again.ID, u[0])
	wantErr(t, "denying twice", err, factionhub.ErrNotPending)
	_, _, err = w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, open.ID, again.ID, u[0])
	wantErr(t, "accepting a denied application", err, factionhub.ErrNotPending)
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE user_id=$1`, u[4]); n != 0 {
		t.Fatal("no membership may exist for a denied applicant")
	}
}

func TestHubAcceptIsTransactionalAndClosesOtherApplications(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(5)
	fa := w.create(w.inst1, u[0], "Faction A", "FA", "OPEN")
	fb := w.create(w.inst1, u[1], "Faction B", "FB", "OPEN")
	fc := w.create(w.inst1, u[2], "Faction C", "FC", "OPEN")
	fOther := w.create(w.inst1b, u[3], "Faction Other", "FO", "OPEN")
	applicant := u[4]

	appA, _ := w.repo.Apply(w.ctx, w.org1, w.inst1, fa.ID, applicant, "a")
	appB, _ := w.repo.Apply(w.ctx, w.org1, w.inst1, fb.ID, applicant, "b")
	appC, _ := w.repo.Apply(w.ctx, w.org1, w.inst1, fc.ID, applicant, "c")
	appOther, err := w.repo.Apply(w.ctx, w.org1, w.inst1b, fOther.ID, applicant, "other installation")
	if err != nil {
		t.Fatal(err)
	}

	// Only leader/officer of THAT faction may accept.
	if _, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, fa.ID, appA.ID, u[1]); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("a leader of another faction must not accept, got %v", err)
	}
	// An application belongs to its own faction: the wrong faction path is a 404.
	if _, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, fb.ID, appA.ID, u[1]); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("application under the wrong faction must be not found, got %v", err)
	}

	app, member, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, fa.ID, appA.ID, u[0])
	if err != nil {
		t.Fatal(err)
	}
	if app.Status != "ACCEPTED" || app.ReviewedByUserID == nil || *app.ReviewedByUserID != u[0] || app.ReviewedAt == nil {
		t.Fatalf("accepted application: %+v", app)
	}
	if member.RoleKey != factionhub.RoleMember || member.User.ID != applicant || member.FactionID != fa.ID {
		t.Fatalf("new membership: %+v", member)
	}
	status := func(id int64) string {
		var s string
		if err := w.db.Pool.QueryRow(w.ctx, `SELECT status FROM hub_faction_applications WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if status(appB.ID) != "CANCELLED" || status(appC.ID) != "CANCELLED" {
		t.Fatalf("other pending applications on the installation must be CANCELLED: B=%s C=%s", status(appB.ID), status(appC.ID))
	}
	if status(appOther.ID) != "PENDING" {
		t.Fatalf("an application on ANOTHER installation must be untouched, got %s", status(appOther.ID))
	}
	// A cancelled application can no longer be accepted (and the applicant is a member anyway).
	_, _, err = w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, fb.ID, appB.ID, u[1])
	wantErr(t, "accepting a cancelled application", err, factionhub.ErrNotPending)
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2`, w.inst1, applicant); n != 1 {
		t.Fatalf("exactly one membership expected, got %d", n)
	}

	// Eligibility is re-validated at accept time: an applicant who became a member through a
	// route that does not cancel applications (direct insert) is refused, with nothing written.
	late := w.newUser()
	lateApp, _ := w.repo.Apply(w.ctx, w.org1, w.inst1, fb.ID, late, "")
	if _, err := w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_members(faction_id, installation_id, user_id, role_key) VALUES($1,$2,$3,'MEMBER')`, fc.ID, w.inst1, late); err != nil {
		t.Fatal(err)
	}
	_, _, err = w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, fb.ID, lateApp.ID, u[1])
	wantErr(t, "accept of an already-member applicant", err, factionhub.ErrAlreadyInFaction)
	if status(lateApp.ID) != "PENDING" {
		t.Fatalf("a refused accept must leave the application PENDING, got %s", status(lateApp.ID))
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2`, fb.ID, late); n != 0 {
		t.Fatal("a refused accept must not create a membership")
	}
}

func TestHubListApplicationsAuthorization(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(5)
	f := w.create(w.inst1, u[0], "Review Board", "RB", "OPEN")
	for _, user := range u[1:3] {
		if _, err := w.repo.Apply(w.ctx, w.org1, w.inst1, f.ID, user, "msg"); err != nil {
			t.Fatal(err)
		}
	}
	list := func(user int64, viewer bool) ([]HubApplication, error) {
		items, _, err := w.repo.ListApplications(w.ctx, w.org1, w.inst1, f.ID, HubAccess{UserID: user, OrgViewer: viewer}, "", 10, 0)
		return items, err
	}
	if items, err := list(u[0], false); err != nil || len(items) != 2 {
		t.Fatalf("leader: %d %v", len(items), err)
	}
	// A plain member and an outsider are forbidden; an organization OWNER/ADMIN gets the read-only view.
	if _, err := list(u[3], false); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("outsider must be forbidden, got %v", err)
	}
	if items, err := list(u[3], true); err != nil || len(items) != 2 {
		t.Fatalf("org viewer must see applications read-only: %d %v", len(items), err)
	}
	// The org viewer flag grants no mutation.
	app := w.one(`SELECT id FROM hub_faction_applications WHERE faction_id=$1 ORDER BY id LIMIT 1`, f.ID)
	if _, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, f.ID, app, u[3]); !errors.Is(err, factionhub.ErrForbidden) {
		t.Fatalf("an org admin who is not a faction officer must not accept, got %v", err)
	}
	// Status filter + paging.
	if _, err := w.repo.DenyApplication(w.ctx, w.org1, w.inst1, f.ID, app, u[0]); err != nil {
		t.Fatal(err)
	}
	pending, _, err := w.repo.ListApplications(w.ctx, w.org1, w.inst1, f.ID, HubAccess{UserID: u[0]}, "PENDING", 10, 0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending filter: %d %v", len(pending), err)
	}
	first, more, err := w.repo.ListApplications(w.ctx, w.org1, w.inst1, f.ID, HubAccess{UserID: u[0]}, "", 1, 0)
	if err != nil || len(first) != 1 || !more {
		t.Fatalf("paging: %d more=%v %v", len(first), more, err)
	}
	second, more, err := w.repo.ListApplications(w.ctx, w.org1, w.inst1, f.ID, HubAccess{UserID: u[0]}, "", 1, first[0].ID)
	if err != nil || len(second) != 1 || more || second[0].ID >= first[0].ID {
		t.Fatalf("second page: %+v more=%v %v", second, more, err)
	}
}

// --- members ---------------------------------------------------------------------------------------

func (w *hubWorld) join(f *HubFaction, leader, user int64) int64 {
	w.t.Helper()
	w.openFor(f)
	app, err := w.repo.Apply(w.ctx, w.org1, w.instOf(f.ID), f.ID, user, "")
	if err != nil {
		w.t.Fatal(err)
	}
	if _, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.instOf(f.ID), f.ID, app.ID, leader); err != nil {
		w.t.Fatal(err)
	}
	return w.memberIDOf(f.ID, user)
}

func TestHubMemberManagementMatrix(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(8)
	leader, officer, officer2, member, member2, outsider := u[0], u[1], u[2], u[3], u[4], u[5]
	f := w.create(w.inst1, leader, "Chain Of Command", "COC", "OPEN")
	mOfficer, mOfficer2 := w.join(f, leader, officer), w.join(f, leader, officer2)
	mMember, mMember2 := w.join(f, leader, member), w.join(f, leader, member2)
	mLeader := w.memberIDOf(f.ID, leader)
	do := func(fn func(ctx context.Context, org, inst, faction, m, actor int64) (*HubMember, error), target, actor int64) (*HubMember, error) {
		return fn(w.ctx, w.org1, w.inst1, f.ID, target, actor)
	}
	promote, demote, remove := w.repo.PromoteMember, w.repo.DemoteMember, w.repo.RemoveMember

	// Promote two members to officer (join() always creates plain MEMBERs).
	if m, err := do(promote, mOfficer, leader); err != nil || m.RoleKey != factionhub.RoleOfficer {
		t.Fatalf("leader promoting a member: %+v %v", m, err)
	}
	if m, err := do(promote, mOfficer2, leader); err != nil || m.RoleKey != factionhub.RoleOfficer {
		t.Fatalf("leader promoting a member: %+v %v", m, err)
	}
	if role := w.roleOf(f.ID, officer); role != factionhub.RoleOfficer {
		t.Fatalf("role not persisted: %q", role)
	}
	// Only the LEADER promotes; nobody else can (a plain member, an outsider, or an officer).
	for who, actor := range map[string]int64{"officer": officer, "member": member, "outsider": outsider} {
		_, err := do(promote, mMember2, actor)
		wantErr(t, who+" promoting", err, factionhub.ErrForbidden)
	}
	_, err := do(promote, mLeader, leader)
	wantErr(t, "promoting the leader", err, factionhub.ErrLeaderProtected)
	// An officer cannot promote a member or another officer.
	_, err = do(promote, mMember, officer)
	wantErr(t, "officer promoting a member", err, factionhub.ErrForbidden)
	_, err = do(promote, mOfficer2, officer)
	wantErr(t, "officer promoting an officer", err, factionhub.ErrForbidden)
	// The leader cannot be promoted (already top) or demoted.
	_, err = do(promote, mOfficer, leader)
	wantErr(t, "promoting an existing officer", err, factionhub.ErrInvalidTransition)
	_, err = do(demote, mLeader, leader)
	wantErr(t, "demoting the leader", err, factionhub.ErrLeaderProtected)
	_, err = do(demote, mMember, leader)
	wantErr(t, "demoting a plain member", err, factionhub.ErrInvalidTransition)
	// An officer cannot demote another officer.
	_, err = do(demote, mOfficer2, officer)
	wantErr(t, "officer demoting an officer", err, factionhub.ErrForbidden)

	// Removal: officer may remove MEMBER only; leader may remove MEMBER and OFFICER; nobody the LEADER.
	_, err = do(remove, mOfficer2, officer)
	wantErr(t, "officer removing an officer", err, factionhub.ErrForbidden)
	_, err = do(remove, mLeader, officer)
	wantErr(t, "officer removing the leader", err, factionhub.ErrLeaderProtected)
	_, err = do(remove, mLeader, leader)
	wantErr(t, "leader removing themselves", err, factionhub.ErrLeaderProtected)
	_, err = do(remove, mMember, member)
	wantErr(t, "member removing anyone", err, factionhub.ErrForbidden)
	_, err = do(remove, mMember, outsider)
	wantErr(t, "outsider removing", err, factionhub.ErrForbidden)
	if m, err := do(remove, mMember, officer); err != nil || m.User.ID != member {
		t.Fatalf("officer removing a member: %+v %v", m, err)
	}
	if m, err := do(demote, mOfficer2, leader); err != nil || m.RoleKey != factionhub.RoleMember {
		t.Fatalf("leader demoting an officer: %+v %v", m, err)
	}
	if m, err := do(promote, mOfficer2, leader); err != nil || m.RoleKey != factionhub.RoleOfficer {
		t.Fatal("re-promote")
	}
	if m, err := do(remove, mOfficer2, leader); err != nil || m.RoleKey != factionhub.RoleOfficer {
		t.Fatalf("leader removing an officer: %+v %v", m, err)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1`, f.ID); n != 3 { // leader, officer, member2
		t.Fatalf("expected 3 members left, got %d", n)
	}
	// A removed user is free to join or found another faction on the installation.
	if g := w.create(w.inst1, member, "Fresh Start", "FS", "OPEN"); g == nil {
		t.Fatal("a removed member must be able to found a faction")
	}
	// A member id from another faction is not found (scoped to the faction in the path).
	other := w.create(w.inst1, w.newUser(), "Somewhere Else", "SE", "OPEN")
	_, err = w.repo.RemoveMember(w.ctx, w.org1, w.inst1, other.ID, mMember2, w.ownerOf(other))
	wantErr(t, "member of another faction", err, factionhub.ErrNotFound)
	// Exactly one leader, always.
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID); n != 1 {
		t.Fatalf("exactly one leader expected, got %d", n)
	}
	// Member list ordering: leader, officers, members.
	members, total, err := w.repo.Members(w.ctx, w.org1, w.inst1, f.ID, 50)
	if err != nil || total != 3 || len(members) != 3 || members[0].RoleKey != "LEADER" || members[1].RoleKey != "OFFICER" || members[2].RoleKey != "MEMBER" {
		t.Fatalf("members ordering: %+v total=%d err=%v", members, total, err)
	}
	if capped, total, _ := w.repo.Members(w.ctx, w.org1, w.inst1, f.ID, 2); len(capped) != 2 || total != 3 {
		t.Fatal("limit caps the list but total stays real")
	}
}

func (w *hubWorld) ownerOf(f *HubFaction) int64 { return f.CreatedByUserID }

func TestHubMyFaction(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(4)
	f := w.create(w.inst1, u[0], "Mine", "MN", "OPEN")
	g := w.create(w.inst1, u[1], "Yours", "YR", "OPEN")

	none, err := w.repo.MyFaction(w.ctx, w.org1, w.inst1, u[2])
	if err != nil || none.Faction != nil || none.Member != nil || len(none.Pending) != 0 {
		t.Fatalf("no faction: %+v %v", none, err)
	}
	if _, err := w.repo.Apply(w.ctx, w.org1, w.inst1, f.ID, u[2], "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repo.Apply(w.ctx, w.org1, w.inst1, g.ID, u[2], "two"); err != nil {
		t.Fatal(err)
	}
	pending, err := w.repo.MyFaction(w.ctx, w.org1, w.inst1, u[2])
	if err != nil || pending.Faction != nil || len(pending.Pending) != 2 || pending.Pending[0].FactionName == "" {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	lead, err := w.repo.MyFaction(w.ctx, w.org1, w.inst1, u[0])
	if err != nil || lead.Faction == nil || lead.Faction.ID != f.ID || lead.Member.RoleKey != "LEADER" || lead.Faction.MemberCount != 1 {
		t.Fatalf("leader: %+v %v", lead, err)
	}
	// The same user has an independent standing on another installation.
	elsewhere, err := w.repo.MyFaction(w.ctx, w.org1, w.inst1b, u[0])
	if err != nil || elsewhere.Faction != nil {
		t.Fatalf("other installation must be independent: %+v %v", elsewhere, err)
	}
	_, err = w.repo.MyFaction(w.ctx, w.org2, w.inst1, u[0])
	wantErr(t, "my faction across organizations", err, factionhub.ErrNotFound)
}

func TestHubVerifiedGamertagLinkIsRecordedOnMembership(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(2)
	f := w.create(w.inst1, u[0], "Linked Squad", "LS", "OPEN")
	// u[1] links a gamertag on the installation's guild.
	guildID := w.one(`SELECT c.guild_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, w.inst1)
	playerID := w.one(`INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'LinkedGamer') RETURNING id`, guildID, fmt.Sprintf("hub-%d-dz", w.suffix))
	var discordID string
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT discord_user_id FROM app_users WHERE id=$1`, u[1]).Scan(&discordID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Pool.Exec(w.ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, guildID, playerID, discordID); err != nil {
		t.Fatal(err)
	}
	w.join(f, u[0], u[1])
	members, _, err := w.repo.Members(w.ctx, w.org1, w.inst1, f.ID, 10)
	if err != nil || len(members) != 2 {
		t.Fatal(err)
	}
	var linked, unlinked *HubMember
	for i := range members {
		if members[i].User.ID == u[1] {
			linked = &members[i]
		} else {
			unlinked = &members[i]
		}
	}
	if linked.PlayerID == nil || *linked.PlayerID != playerID || linked.Gamertag == nil || *linked.Gamertag != "LinkedGamer" {
		t.Fatalf("verified link must be recorded: %+v", linked)
	}
	if unlinked.PlayerID != nil || unlinked.Gamertag != nil {
		t.Fatal("a user without a link has no player identity (never invented)")
	}
	// A PENDING (unverified) link is not used.
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE player_links SET status='PENDING' WHERE player_id=$1`, playerID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.repo.RemoveMember(w.ctx, w.org1, w.inst1, f.ID, w.memberIDOf(f.ID, u[1]), u[0]); err != nil {
		t.Fatal(err)
	}
	w.join(f, u[0], u[1])
	if pid := w.one(`SELECT COALESCE(player_id,0) FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2`, f.ID, u[1]); pid != 0 {
		t.Fatal("an unverified link must not be recorded")
	}
}

// --- tenant isolation ---------------------------------------------------------------------------------

func TestHubTenantIsolationEveryMethod(t *testing.T) {
	w := newHubWorld(t)
	u := w.newUsers(4)
	f := w.create(w.inst1, u[0], "Isolated", "ISO", "OPEN")
	member := w.join(f, u[0], u[1])
	app, err := w.repo.Apply(w.ctx, w.org1, w.inst1, f.ID, u[2], "hi")
	if err != nil {
		t.Fatal(err)
	}

	type call struct {
		name string
		fn   func(org, inst int64) error
	}
	name := "Hijack"
	calls := []call{
		{"Get", func(o, i int64) error { _, err := w.repo.Get(w.ctx, o, i, f.ID); return err }},
		{"Members", func(o, i int64) error { _, _, err := w.repo.Members(w.ctx, o, i, f.ID, 10); return err }},
		{"MemberRole", func(o, i int64) error { _, err := w.repo.MemberRole(w.ctx, o, i, f.ID, u[0]); return err }},
		{"UpdateFaction", func(o, i int64) error {
			_, err := w.repo.UpdateFaction(w.ctx, o, i, f.ID, u[0], HubFactionUpdate{Name: &name})
			return err
		}},
		{"Apply", func(o, i int64) error { _, err := w.repo.Apply(w.ctx, o, i, f.ID, u[3], ""); return err }},
		{"ListApplications", func(o, i int64) error {
			_, _, err := w.repo.ListApplications(w.ctx, o, i, f.ID, HubAccess{UserID: u[0]}, "", 10, 0)
			return err
		}},
		{"AcceptApplication", func(o, i int64) error {
			_, _, err := w.repo.AcceptApplication(w.ctx, o, i, f.ID, app.ID, u[0])
			return err
		}},
		{"DenyApplication", func(o, i int64) error { _, err := w.repo.DenyApplication(w.ctx, o, i, f.ID, app.ID, u[0]); return err }},
		{"WithdrawApplication", func(o, i int64) error {
			_, err := w.repo.WithdrawApplication(w.ctx, o, i, f.ID, app.ID, u[2])
			return err
		}},
		{"PromoteMember", func(o, i int64) error { _, err := w.repo.PromoteMember(w.ctx, o, i, f.ID, member, u[0]); return err }},
		{"DemoteMember", func(o, i int64) error { _, err := w.repo.DemoteMember(w.ctx, o, i, f.ID, member, u[0]); return err }},
		{"RemoveMember", func(o, i int64) error { _, err := w.repo.RemoveMember(w.ctx, o, i, f.ID, member, u[0]); return err }},
	}
	// Wrong organization, wrong installation (same org), and both wrong: always "not found".
	for _, scope := range []struct {
		label     string
		org, inst int64
	}{
		{"other organization, real installation", w.org2, w.inst1},
		{"real organization, other installation of the org", w.org1, w.inst1b},
		{"other organization and installation", w.org2, w.inst2},
	} {
		for _, c := range calls {
			if err := c.fn(scope.org, scope.inst); !errors.Is(err, factionhub.ErrNotFound) {
				t.Errorf("%s via %s: want ErrNotFound, got %v", c.name, scope.label, err)
			}
		}
	}
	// Nothing changed.
	got, err := w.repo.Get(w.ctx, w.org1, w.inst1, f.ID)
	if err != nil || got.Name != "Isolated" || got.MemberCount != 2 {
		t.Fatalf("isolation probes must not change data: %+v %v", got, err)
	}
	if s := w.one(`SELECT COUNT(*) FROM hub_faction_applications WHERE id=$1 AND status='PENDING'`, app.ID); s != 1 {
		t.Fatal("the application must still be pending")
	}
}

// --- concurrency ------------------------------------------------------------------------------------------

// runConcurrently starts n goroutines together and collects their errors.
func runConcurrently(n int, fn func(i int) error) []error {
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

func countOK(errs []error) (ok int) {
	for _, e := range errs {
		if e == nil {
			ok++
		}
	}
	return
}

func TestHubConcurrentCreatesBySameUser(t *testing.T) {
	w := newHubWorld(t)
	for round := 0; round < 8; round++ {
		user := w.newUser()
		errs := runConcurrently(6, func(i int) error {
			_, err := w.repo.CreateFaction(w.ctx, w.org1, w.inst1, user, HubFactionInput{
				Name: fmt.Sprintf("Racer %d %d", round, i), Tag: fmt.Sprintf("R%d%d", round, i), RecruitmentStatus: "OPEN"})
			return err
		})
		if ok := countOK(errs); ok != 1 {
			t.Fatalf("round %d: exactly one simultaneous create may win, got %d (errors: %v)", round, ok, errs)
		}
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrAlreadyInFaction) {
				t.Fatalf("round %d: losers must get ErrAlreadyInFaction, got %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2`, w.inst1, user); n != 1 {
			t.Fatalf("round %d: one membership expected, got %d", round, n)
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_factions WHERE created_by_user_id=$1`, user); n != 1 {
			t.Fatalf("round %d: a losing create must leave no orphan faction, got %d", round, n)
		}
	}
}

func TestHubConcurrentAcceptsOfTheSameApplication(t *testing.T) {
	w := newHubWorld(t)
	leader, officer := w.newUser(), w.newUser()
	f := w.create(w.inst1, leader, "Double Tap", "DT", "OPEN")
	w.join(f, leader, officer)
	if _, err := w.repo.PromoteMember(w.ctx, w.org1, w.inst1, f.ID, w.memberIDOf(f.ID, officer), leader); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 10; round++ {
		applicant := w.newUser()
		app, err := w.repo.Apply(w.ctx, w.org1, w.inst1, f.ID, applicant, "")
		if err != nil {
			t.Fatal(err)
		}
		errs := runConcurrently(4, func(i int) error {
			actor := leader
			if i%2 == 1 {
				actor = officer
			}
			_, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, f.ID, app.ID, actor)
			return err
		})
		if ok := countOK(errs); ok != 1 {
			t.Fatalf("round %d: exactly one accept may win, got %d (%v)", round, ok, errs)
		}
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrNotPending) {
				t.Fatalf("round %d: losers must see ErrNotPending, got %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2`, f.ID, applicant); n != 1 {
			t.Fatalf("round %d: one membership expected, got %d", round, n)
		}
	}
}

func TestHubTwoLeadersAcceptingTheSameApplicantIntoDifferentFactions(t *testing.T) {
	w := newHubWorld(t)
	leaderA, leaderB, leaderC := w.newUser(), w.newUser(), w.newUser()
	fa := w.create(w.inst1, leaderA, "Race A", "RA", "OPEN")
	fb := w.create(w.inst1, leaderB, "Race B", "RB", "OPEN")
	fc := w.create(w.inst1, leaderC, "Race C", "RC", "OPEN")
	targets := []struct {
		f      *HubFaction
		leader int64
	}{{fa, leaderA}, {fb, leaderB}, {fc, leaderC}}

	for round := 0; round < 15; round++ {
		applicant := w.newUser()
		appIDs := make([]int64, len(targets))
		for i, tgt := range targets {
			app, err := w.repo.Apply(w.ctx, w.org1, w.inst1, tgt.f.ID, applicant, "")
			if err != nil {
				t.Fatal(err)
			}
			appIDs[i] = app.ID
		}
		errs := runConcurrently(len(targets), func(i int) error {
			_, _, err := w.repo.AcceptApplication(w.ctx, w.org1, w.inst1, targets[i].f.ID, appIDs[i], targets[i].leader)
			return err
		})
		if ok := countOK(errs); ok != 1 {
			t.Fatalf("round %d: the applicant must join exactly one faction, %d accepts succeeded (%v)", round, ok, errs)
		}
		for _, e := range errs {
			// A loser sees its application already closed (cancelled by the winner) or the
			// applicant already placed - never a deadlock, never a raw database error.
			if e != nil && !errors.Is(e, factionhub.ErrNotPending) && !errors.Is(e, factionhub.ErrAlreadyInFaction) {
				t.Fatalf("round %d: unexpected loser error: %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2`, w.inst1, applicant); n != 1 {
			t.Fatalf("round %d: one-faction-per-installation violated: %d memberships", round, n)
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_applications WHERE installation_id=$1 AND user_id=$2 AND status='PENDING'`, w.inst1, applicant); n != 0 {
			t.Fatalf("round %d: no pending application may remain after the applicant joined, got %d", round, n)
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_applications WHERE installation_id=$1 AND user_id=$2 AND status='ACCEPTED'`, w.inst1, applicant); n != 1 {
			t.Fatalf("round %d: exactly one ACCEPTED application expected, got %d", round, n)
		}
	}
}

func TestHubConcurrentApplyAndFoundBySameUser(t *testing.T) {
	w := newHubWorld(t)
	target := w.create(w.inst1, w.newUser(), "Target House", "TH", "OPEN")
	for round := 0; round < 8; round++ {
		user := w.newUser()
		errs := runConcurrently(2, func(i int) error {
			if i == 0 {
				_, err := w.repo.Apply(w.ctx, w.org1, w.inst1, target.ID, user, "")
				return err
			}
			_, err := w.repo.CreateFaction(w.ctx, w.org1, w.inst1, user, HubFactionInput{Name: fmt.Sprintf("Founded %d", round), Tag: fmt.Sprintf("FN%d", round), RecruitmentStatus: "OPEN"})
			return err
		})
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrAlreadyInFaction) {
				t.Fatalf("round %d: unexpected error %v", round, e)
			}
		}
		// Whatever the order, the end state is consistent: at most one membership, and a
		// user who founded a faction has no pending application left.
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2`, w.inst1, user); n > 1 {
			t.Fatalf("round %d: %d memberships", round, n)
		}
		if w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2`, w.inst1, user) == 1 {
			if n := w.count(`SELECT COUNT(*) FROM hub_faction_applications WHERE installation_id=$1 AND user_id=$2 AND status='PENDING'`, w.inst1, user); n != 0 {
				t.Fatalf("round %d: a member must have no pending application, got %d", round, n)
			}
		}
	}
}

func TestHubConcurrentDemoteAndRemoveKeepInvariants(t *testing.T) {
	w := newHubWorld(t)
	leader := w.newUser()
	f := w.create(w.inst1, leader, "Mutiny", "MUT", "OPEN")
	for round := 0; round < 8; round++ {
		officer, victim := w.newUser(), w.newUser()
		mOfficer, mVictim := w.join(f, leader, officer), w.join(f, leader, victim)
		if _, err := w.repo.PromoteMember(w.ctx, w.org1, w.inst1, f.ID, mOfficer, leader); err != nil {
			t.Fatal(err)
		}
		// The leader demotes the officer while the officer removes a member.
		errs := runConcurrently(2, func(i int) error {
			if i == 0 {
				_, err := w.repo.DemoteMember(w.ctx, w.org1, w.inst1, f.ID, mOfficer, leader)
				return err
			}
			_, err := w.repo.RemoveMember(w.ctx, w.org1, w.inst1, f.ID, mVictim, officer)
			return err
		})
		for _, e := range errs {
			if e != nil && !errors.Is(e, factionhub.ErrForbidden) && !errors.Is(e, factionhub.ErrNotFound) {
				t.Fatalf("round %d: unexpected error %v", round, e)
			}
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_members WHERE faction_id=$1 AND role_key='LEADER'`, f.ID); n != 1 {
			t.Fatalf("round %d: leader count %d", round, n)
		}
		// Clean up for the next round.
		_, _ = w.db.Pool.Exec(w.ctx, `DELETE FROM hub_faction_members WHERE faction_id=$1 AND role_key<>'LEADER'`, f.ID)
	}
}
