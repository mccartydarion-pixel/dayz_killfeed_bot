//go:build integration

package repository

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/owneraccess"
)

func (w *hubWorld) ownerDiscordID(org int64) string {
	w.t.Helper()
	var id string
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT u.discord_user_id FROM organizations o JOIN app_users u ON u.id=o.owner_user_id WHERE o.id=$1`, org).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// The platform_staff table (migration 0125) through the repository: add, update the note, list,
// remove, and the table's own checks.
func TestPlatformStaffRepository(t *testing.T) {
	w := newHubWorld(t)
	repo := NewPlatformOwnerRepository(w.db.Pool)
	// A unique snowflake per run, so reruns on a kept database never collide.
	id := fmt.Sprintf("9%017d", time.Now().UnixNano()%100000000000000000)
	t.Cleanup(func() { _, _ = w.db.Pool.Exec(w.ctx, `DELETE FROM platform_staff WHERE discord_user_id=$1`, id) })

	if m, err := repo.GetPlatformStaff(w.ctx, id); err != nil || m != nil {
		t.Fatalf("unknown id: %+v %v", m, err)
	}
	before, added, err := repo.UpsertPlatformStaff(w.ctx, id, "Support", "900000000000000777")
	if err != nil || before != nil {
		t.Fatalf("add: before=%+v err=%v", before, err)
	}
	if added.DiscordID != id || added.Note != "Support" || added.AddedBy != "900000000000000777" || added.AddedAt.IsZero() {
		t.Fatalf("added row: %+v", added)
	}
	before, updated, err := repo.UpsertPlatformStaff(w.ctx, id, "Billing", "900000000000000888")
	if err != nil || before == nil || before.Note != "Support" {
		t.Fatalf("update: before=%+v err=%v", before, err)
	}
	if updated.Note != "Billing" || updated.AddedBy != "900000000000000777" || !updated.AddedAt.Equal(added.AddedAt) {
		t.Fatalf("an update changes the note only: %+v", updated)
	}
	got, err := repo.GetPlatformStaff(w.ctx, id)
	if err != nil || got == nil || got.Note != "Billing" {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := repo.ListPlatformStaff(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list {
		found = found || m.DiscordID == id
	}
	if !found {
		t.Fatalf("the member is missing from the list: %+v", list)
	}
	// The table refuses what the API refuses.
	for _, bad := range []string{"not-a-snowflake", "12345", strings.Repeat("1", 21)} {
		if _, _, err := repo.UpsertPlatformStaff(w.ctx, bad, "", "x"); err == nil {
			t.Fatalf("the table accepted the id %q", bad)
		}
	}
	if _, _, err := repo.UpsertPlatformStaff(w.ctx, id, strings.Repeat("n", 121), "x"); err == nil {
		t.Fatal("the table accepted a 121-character note")
	}
	removed, err := repo.RemovePlatformStaff(w.ctx, id)
	if err != nil || removed.DiscordID != id || removed.Note != "Billing" {
		t.Fatalf("remove: %+v %v", removed, err)
	}
	if _, err := repo.RemovePlatformStaff(w.ctx, id); !errors.Is(err, ErrPlatformStaffNotFound) {
		t.Fatalf("removing again: want ErrPlatformStaffNotFound, got %v", err)
	}
}

// PlatformOwnerScope finds an organization by its OWNER only: an ADMIN membership in somebody
// else's organization never puts that organization in the result.
func TestPlatformOwnerScopeFollowsTheOrganizationOwnerOnly(t *testing.T) {
	w := newHubWorld(t)
	repo := NewPlatformOwnerRepository(w.db.Pool)
	owner1 := w.ownerDiscordID(w.org1)
	if owner1 == w.ownerDiscordID(w.org2) {
		t.Fatal("the fixture's two organizations must have different owners")
	}
	// The platform owner is also an ADMIN of organization 2.
	if _, err := w.db.Pool.Exec(w.ctx, `
INSERT INTO organization_members(organization_id, user_id, role)
SELECT $1, o.owner_user_id, 'ADMIN' FROM organizations o WHERE o.id=$2
ON CONFLICT DO NOTHING`, w.org2, w.org1); err != nil {
		t.Fatal(err)
	}
	orgs, insts, err := repo.PlatformOwnerScope(w.ctx, []string{owner1, "900000000000000999"})
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0] != w.org1 {
		t.Fatalf("organizations = %v, want only %d", orgs, w.org1)
	}
	has := map[int64]bool{}
	for _, id := range insts {
		has[id] = true
	}
	for _, id := range []int64{w.inst1, w.inst1b, w.instNoServer, w.instSuspended} {
		if !has[id] {
			t.Fatalf("installation %d of the owner's organization is missing from %v", id, insts)
		}
	}
	if has[w.inst2] {
		t.Fatal("an installation of an organization the owner only administers must not be included")
	}
	if orgs, insts, err := repo.PlatformOwnerScope(w.ctx, nil); err != nil || orgs != nil || insts != nil {
		t.Fatalf("no owners: %v %v %v", orgs, insts, err)
	}
	if orgs, _, err := repo.PlatformOwnerScope(w.ctx, []string{"900000000000000999"}); err != nil || len(orgs) != 0 {
		t.Fatalf("an owner with no organization: %v %v", orgs, err)
	}
}

// The runtime lookups in this package (feed routes, custom embeds) on a real database: a
// platform owner's own organization on Survivor, with gating enforced, keeps everything, and the
// customer next to it on the same plan is still restricted.
func TestPlatformOwnerOrganizationPassesRepositoryPlanGates(t *testing.T) {
	w := newHubWorld(t)
	access := owneraccess.New(NewPlatformOwnerRepository(w.db.Pool), []string{w.ownerDiscordID(w.org1)}, time.Hour)
	if err := access.Refresh(w.ctx); err != nil {
		t.Fatal(err)
	}
	if !access.Organization(w.org1) || access.Organization(w.org2) || !access.Installation(w.inst1) || access.Installation(w.inst2) {
		t.Fatal("owner access snapshot is wrong")
	}
	entitlements.SetOwnerOrganizations(access.Organization)
	t.Cleanup(func() { entitlements.SetOwnerOrganizations(nil) })
	enforcePlans(t, true)
	w.setPlan(w.org1, "NORMAL")
	w.setPlan(w.org2, "NORMAL")

	routes := NewChannelRouteRepository(w.db.Pool)
	for _, inst := range []int64{w.inst1, w.inst2} {
		for _, route := range []string{"BOUNTY", "HEATMAPS", "ECONOMY", "SERVER_RANKS", "KILLFEED"} {
			if _, err := w.db.Pool.Exec(w.ctx, `INSERT INTO installation_channel_routes(installation_id, route_key, channel_id) VALUES($1,$2,$3)`, inst, route, "ch-"+route); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolve := func(inst int64, route string) bool {
		t.Helper()
		guildRow, server := w.guildAndServer(inst)
		_, found, err := routes.ResolveChannel(w.ctx, guildRow, server, route)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	for _, route := range []string{"BOUNTY", "HEATMAPS", "ECONOMY", "SERVER_RANKS", "KILLFEED"} {
		if !resolve(w.inst1, route) {
			t.Fatalf("the owner's installation must resolve the %s route on any plan", route)
		}
	}
	for _, route := range []string{"BOUNTY", "HEATMAPS", "ECONOMY", "SERVER_RANKS"} {
		if resolve(w.inst2, route) {
			t.Fatalf("a Survivor customer must still not resolve the %s route", route)
		}
	}
	if !resolve(w.inst2, "KILLFEED") {
		t.Fatal("a Survivor customer keeps the killfeed route")
	}

	// Feed identity and hot zones read the plan in SQL too.
	settings := NewFeatureSettingsRepository(w.db.Pool)
	for _, inst := range []int64{w.inst1, w.inst2} {
		if _, err := w.db.Pool.Exec(w.ctx, `
INSERT INTO installation_feature_settings(installation_id, feed_identity_enabled, feed_identity_name, hot_zones_enabled)
VALUES($1, TRUE, 'My Server', TRUE)
ON CONFLICT (installation_id) DO UPDATE SET feed_identity_enabled=TRUE, feed_identity_name='My Server', hot_zones_enabled=TRUE`, inst); err != nil {
			t.Fatal(err)
		}
	}
	_, server1 := w.guildAndServer(w.inst1)
	_, server2 := w.guildAndServer(w.inst2)
	if s, ok, err := settings.FeedIdentityForServer(w.ctx, server1); err != nil || !ok || s.Name != "My Server" {
		t.Fatalf("owner feed identity: %+v %v %v", s, ok, err)
	}
	if _, ok, err := settings.FeedIdentityForServer(w.ctx, server2); err != nil || ok {
		t.Fatalf("a Survivor customer must not get a feed identity: %v %v", ok, err)
	}
	hot := func(inst int64) bool {
		t.Helper()
		guildRow, _ := w.guildAndServer(inst)
		list, err := settings.HotZoneInstallations(w.ctx, guildRow)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range list {
			if h.InstallationID == inst {
				return true
			}
		}
		return false
	}
	if !hot(w.inst1) || hot(w.inst2) {
		t.Fatalf("hot zones: owner=%v customer=%v, want true/false", hot(w.inst1), hot(w.inst2))
	}

	// Losing ownership of the allowlist (the id is taken off CHAMPION_ADMIN_DISCORD_IDS) locks it again.
	entitlements.SetOwnerOrganizations(nil)
	if resolve(w.inst1, "BOUNTY") {
		t.Fatal("without owner access the Survivor plan applies again")
	}
}

func TestPlatformOwnerOrganizationRendersCustomEmbedsOnAnyPlan(t *testing.T) {
	w := newEmbedWorld(t)
	org, first := w.installation()
	inst := w.secondInstallation(org, first)
	var guildRow, server int64
	w.must(w.pool.QueryRow(w.ctx, `SELECT c.guild_id, i.game_server_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, inst).Scan(&guildRow, &server))
	if _, err := w.repo.Upsert(w.ctx, org, inst, sampleConfig("KILLFEED", "custom-card")); err != nil {
		t.Fatal(err)
	}
	w.must(w.repo.SetActivation(w.ctx, org, inst, "KILLFEED", EmbedModeCustom, 0))
	_, err := w.pool.Exec(w.ctx, `INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,'NORMAL','CANCELED')
ON CONFLICT (organization_id) DO UPDATE SET plan = EXCLUDED.plan, status = EXCLUDED.status`, org)
	w.must(err)
	enforcePlans(t, true)
	if _, cfg, err := w.repo.ResolveTemplate(w.ctx, guildRow, server, "KILLFEED"); err != nil || cfg != nil {
		t.Fatalf("a Survivor customer keeps the default card: %+v %v", cfg, err)
	}
	entitlements.SetOwnerOrganizations(func(id int64) bool { return id == org })
	t.Cleanup(func() { entitlements.SetOwnerOrganizations(nil) })
	gotInst, cfg, err := w.repo.ResolveTemplate(w.ctx, guildRow, server, "KILLFEED")
	w.must(err)
	if gotInst != inst || cfg == nil || cfg.Title.Template != "custom-card" {
		t.Fatalf("the owner's own organization renders its template on any plan: %d %+v", gotInst, cfg)
	}
}
