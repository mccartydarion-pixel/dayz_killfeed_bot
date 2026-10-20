//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/owneraccess"
)

func (w *adminWorld) do(method string, h adminHandler, target, acting string, pv map[string]string, body any) *httptest.ResponseRecorder {
	raw := ""
	if body != nil {
		raw = mustJSON(w.t, body)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("Content-Type", "application/json")
	if acting != "" {
		req.Header.Set(actingUserHeader, acting)
	}
	for k, v := range pv {
		req.SetPathValue(k, v)
	}
	rr := httptest.NewRecorder()
	w.a.adminRoute(h)(rr, req)
	return rr
}

type staffListBody struct {
	Staff []staffMemberDTO `json:"staff"`
}

// Platform staff against the real database: the owner adds a member, the member reads every
// admin route and is refused every write, the changes are in the audit log with their reasons,
// and removal ends access at once.
func TestPlatformStaffEndToEnd(t *testing.T) {
	w := newOwnerWorld(t)
	staffID := fmt.Sprintf("8%017d", time.Now().UnixNano()%100000000000000000)
	t.Cleanup(func() {
		_, _ = w.a.DB.Pool.Exec(context.Background(), `DELETE FROM platform_staff WHERE discord_user_id=$1`, staffID)
	})
	inst := strconv.FormatInt(w.a1.InstallationID, 10)

	// Before being added: 403 everywhere, including /me.
	if rr := w.get(w.a.handleAdminMe, "/api/admin/me", staffID, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("me before being added: %d", rr.Code)
	}
	// A staff member cannot add themselves; a stranger cannot either.
	if rr := w.do(http.MethodPost, w.a.handleAdminAddStaff, "/api/admin/staff", staffID, nil, map[string]any{"discordId": staffID, "reason": "let me in"}); rr.Code != http.StatusForbidden {
		t.Fatalf("self-add: %d", rr.Code)
	}

	rr := w.do(http.MethodPost, w.a.handleAdminAddStaff, "/api/admin/staff", adminFounderID, nil, map[string]any{"discordId": staffID, "note": "Support", "reason": "joined the support team"})
	if rr.Code != http.StatusOK {
		t.Fatalf("add staff: %d %s", rr.Code, rr.Body.String())
	}
	var list staffListBody
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	var mine *staffMemberDTO
	for i := range list.Staff {
		if list.Staff[i].DiscordID == staffID {
			mine = &list.Staff[i]
		}
	}
	if mine == nil || mine.Note != "Support" || mine.AddedBy != adminFounderID {
		t.Fatalf("staff list after add: %s", rr.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, mine.AddedAt); err != nil {
		t.Fatalf("addedAt is not RFC3339: %q", mine.AddedAt)
	}

	// /me for both roles.
	for acting, role := range map[string]string{adminFounderID: "OWNER", staffID: "STAFF"} {
		rr := w.get(w.a.handleAdminMe, "/api/admin/me", acting, nil)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"role":"`+role+`"`) || !strings.Contains(rr.Body.String(), `"discordId":"`+acting+`"`) {
			t.Fatalf("me as %s: %d %s", acting, rr.Code, rr.Body.String())
		}
	}
	// Staff read every admin read route the world knows, plus the staff list, flags and audit log.
	for name, call := range w.allRoutes() {
		if rr := call(staffID); rr.Code != http.StatusOK {
			t.Fatalf("staff GET %s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"staff": w.get(w.a.handleAdminListStaff, "/api/admin/staff", staffID, nil),
		"audit": w.get(w.a.handleOwnerAudit, "/api/admin/audit", staffID, nil),
		"flags": w.get(w.a.handleOwnerInstallationFlags, "/api/admin/installations/"+inst+"/flags", staffID, map[string]string{"installationID": inst}),
	} {
		if rr.Code != http.StatusOK {
			t.Fatalf("staff GET %s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	// Staff writes are refused and change nothing.
	denied := map[string]*httptest.ResponseRecorder{
		"suspend": w.do(http.MethodPost, w.a.handleOwnerSuspendInstallation, "/api/admin/installations/"+inst+"/suspend", staffID, map[string]string{"installationID": inst}, map[string]any{"reason": "abuse"}),
		"flag":    w.do(http.MethodPut, w.a.handleOwnerSetInstallationFlag, "/api/admin/installations/"+inst+"/flags/map_rotation", staffID, map[string]string{"installationID": inst, "flag": "map_rotation"}, map[string]any{"reason": "x", "enabled": true}),
		"grant":   w.do(http.MethodPost, w.a.handleOwnerGrantPlan, "/api/admin/organizations/1/grant", staffID, map[string]string{"organizationID": strconv.FormatInt(w.a1.OrgID, 10)}, map[string]any{"reason": "x", "plan": "PREMIUM", "days": 30}),
		"remove":  w.do(http.MethodDelete, w.a.handleAdminRemoveStaff, "/api/admin/staff/"+staffID, staffID, map[string]string{"discordID": staffID}, map[string]any{"reason": "x"}),
	}
	for name, rr := range denied {
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "platform staff can view but not change this") {
			t.Fatalf("staff %s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	var status string
	var overrides int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT status, (SELECT COUNT(*) FROM installation_feature_flags WHERE installation_id=$1) FROM installations WHERE id=$1`, w.a1.InstallationID).Scan(&status, &overrides); err != nil {
		t.Fatal(err)
	}
	if status == "SUSPENDED" || overrides != 0 {
		t.Fatalf("a refused staff write changed something: status=%s overrides=%d", status, overrides)
	}

	// Note update, owner refusal, validation.
	if rr := w.do(http.MethodPost, w.a.handleAdminAddStaff, "/api/admin/staff", adminFounderID, nil, map[string]any{"discordId": staffID, "note": "Billing", "reason": "moved teams"}); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"note":"Billing"`) {
		t.Fatalf("note update: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.do(http.MethodPost, w.a.handleAdminAddStaff, "/api/admin/staff", adminFounderID, nil, map[string]any{"discordId": adminFounderID, "reason": "x"}); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "already has full access") {
		t.Fatalf("adding the owner: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.do(http.MethodPost, w.a.handleAdminAddStaff, "/api/admin/staff", adminFounderID, nil, map[string]any{"discordId": "abc", "reason": "x"}); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rr.Code)
	}
	if rr := w.do(http.MethodPost, w.a.handleAdminAddStaff, "/api/admin/staff", adminFounderID, nil, map[string]any{"discordId": staffID}); rr.Code != http.StatusBadRequest {
		t.Fatalf("no reason: %d", rr.Code)
	}

	// Remove: 200 with the list, then access is gone, then 404.
	rr = w.do(http.MethodDelete, w.a.handleAdminRemoveStaff, "/api/admin/staff/"+staffID, adminFounderID, map[string]string{"discordID": staffID}, map[string]any{"reason": "left the company"})
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), staffID) || !strings.Contains(rr.Body.String(), `"staff":[`) {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.get(w.a.handleAdminOverview, "/api/admin/overview", staffID, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("removed staff: %d", rr.Code)
	}
	if rr := w.do(http.MethodDelete, w.a.handleAdminRemoveStaff, "/api/admin/staff/"+staffID, adminFounderID, map[string]string{"discordID": staffID}, map[string]any{"reason": "again"}); rr.Code != http.StatusNotFound {
		t.Fatalf("remove unknown: %d", rr.Code)
	}

	// The audit log has the three changes, newest first, with the reasons and who was changed.
	rows, err := w.a.DB.Pool.Query(context.Background(), `
SELECT action, actor_discord_id, reason, COALESCE(before_state->>'discordId',''), COALESCE(after_state->>'discordId',''), COALESCE(after_state->>'note','')
FROM platform_audit_log WHERE target_type='platform_staff' AND (before_state->>'discordId'=$1 OR after_state->>'discordId'=$1) ORDER BY id`, staffID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, actor, reason, beforeID, afterID, note string
		if err := rows.Scan(&action, &actor, &reason, &beforeID, &afterID, &note); err != nil {
			t.Fatal(err)
		}
		if actor != adminFounderID {
			t.Fatalf("audit actor %s", actor)
		}
		got = append(got, action+"|"+reason+"|"+note)
	}
	want := "staff.added|joined the support team|Support,staff.note_updated|moved teams|Billing,staff.removed|left the company|"
	if strings.Join(got, ",") != want {
		t.Fatalf("audit rows:\n got %s\nwant %s", strings.Join(got, ","), want)
	}
	if actions := w.auditActions(t, "?targetType=platform_staff"); len(actions) < 3 {
		t.Fatalf("the audit endpoint must list the staff changes: %v", actions)
	}
}

// ownerAccessWorld makes organization A's owner a platform owner, with the real cached lookup
// wired exactly as New wires it, and plan gating enforced.
func ownerAccessWorld(t *testing.T) *adminWorld {
	t.Helper()
	w := newOwnerWorld(t)
	w.a.Config.AdminDiscordIDs = []string{adminFounderID, w.a1.OwnerDiscordID}
	w.a.OwnerAccess = owneraccess.New(w.a.PlatformOwner, w.a.Config.AdminDiscordIDs, time.Hour)
	if err := w.a.OwnerAccess.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	entitlements.SetOwnerOrganizations(w.a.OwnerAccess.Organization)
	t.Cleanup(func() { entitlements.SetOwnerOrganizations(nil) })
	w.a.FeatureFlags = featureflags.New(w.a.PlatformOwner, time.Hour)
	w.a.FeatureFlags.SetOwnerInstallations(w.a.OwnerAccess.Installation)
	if err := w.a.FeatureFlags.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	enforcePlanGating(t)
	return w
}

func setOrgSubscription(t *testing.T, a *App, organizationID int64, plan, status string) {
	t.Helper()
	if _, err := a.DB.Pool.Exec(context.Background(), `
INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,$2,$3)
ON CONFLICT (organization_id) DO UPDATE SET plan = EXCLUDED.plan, status = EXCLUDED.status, trial_ends_at = NULL`, organizationID, plan, status); err != nil {
		t.Fatal(err)
	}
}

type flagPage struct {
	Flags []struct {
		Key              string `json:"key"`
		Effective        bool   `json:"effective"`
		Default          bool   `json:"default"`
		Source           string `json:"source"`
		SourceLabel      string `json:"sourceLabel"`
		OwnerAccess      bool   `json:"ownerAccess"`
		OwnerDefaultOn   bool   `json:"ownerDefaultOn"`
		OwnerDefaultNote string `json:"ownerDefaultNote"`
		Override         *bool  `json:"override"`
	} `json:"flags"`
}

func (w *adminWorld) flags(t *testing.T, installationID int64) map[string]struct {
	Effective, OwnerAccess bool
	Source, Label          string
	Override               *bool
} {
	t.Helper()
	inst := strconv.FormatInt(installationID, 10)
	rr := w.get(w.a.handleOwnerInstallationFlags, "/api/admin/installations/"+inst+"/flags", adminFounderID, map[string]string{"installationID": inst})
	if rr.Code != http.StatusOK {
		t.Fatalf("flags: %d %s", rr.Code, rr.Body.String())
	}
	var page flagPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct {
		Effective, OwnerAccess bool
		Source, Label          string
		Override               *bool
	}{}
	for _, f := range page.Flags {
		out[f.Key] = struct {
			Effective, OwnerAccess bool
			Source, Label          string
			Override               *bool
		}{f.Effective, f.OwnerAccess, f.Source, f.SourceLabel, f.Override}
	}
	return out
}

// Platform owner access end to end on a real database: the owner's own organization on an ended
// Survivor subscription passes the plan gates, the billing wall and the feature switches, and
// the customer next to it - which the same person merely administers - is exactly as before.
func TestPlatformOwnerAccessEndToEnd(t *testing.T) {
	w := ownerAccessWorld(t)
	a, ctx := w.a, context.Background()
	setOrgSubscription(t, a, w.a1.OrgID, entitlements.PlanSurvivor, "CANCELED")
	setOrgSubscription(t, a, w.b1.OrgID, entitlements.PlanSurvivor, "CANCELED")
	// The platform owner is also an ADMIN of the customer organization: that unlocks nothing.
	if _, err := a.DB.Pool.Exec(ctx, `
INSERT INTO organization_members(organization_id, user_id, role)
SELECT $1, o.owner_user_id, 'ADMIN' FROM organizations o WHERE o.id=$2 ON CONFLICT DO NOTHING`, w.b1.OrgID, w.a1.OrgID); err != nil {
		t.Fatal(err)
	}
	if err := a.OwnerAccess.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.OwnerAccess.Organization(w.a1.OrgID) || a.OwnerAccess.Organization(w.b1.OrgID) || !a.OwnerAccess.Installation(w.a1.InstallationID) || a.OwnerAccess.Installation(w.b1.InstallationID) {
		t.Fatal("owner access must follow the organization owner, not an ADMIN membership")
	}

	// Plan gates.
	ownerPlan, err := a.organizationPlan(ctx, w.a1.OrgID)
	if err != nil || !ownerPlan.OwnerAccess() || ownerPlan.Key() != entitlements.PlanSurvivor {
		t.Fatalf("owner plan: %+v %v", ownerPlan, err)
	}
	customerPlan, err := a.organizationPlan(ctx, w.b1.OrgID)
	if err != nil || customerPlan.OwnerAccess() {
		t.Fatalf("customer plan: %+v %v", customerPlan, err)
	}
	for _, key := range []entitlements.Key{entitlements.MapRotation, entitlements.Economy, entitlements.FightReplay, entitlements.Retention, entitlements.Heatmaps, entitlements.PerkStore, entitlements.RankedSeasons} {
		if !entitlements.Has(ownerPlan, key) {
			t.Fatalf("the owner's organization is missing %s", key)
		}
		if entitlements.Has(customerPlan, key) {
			t.Fatalf("the Survivor customer must not have %s", key)
		}
		for org, wantOK := range map[int64]bool{w.a1.OrgID: true, w.b1.OrgID: false} {
			rec := httptest.NewRecorder()
			ok := a.requirePlanFeature(rec, httptest.NewRequest(http.MethodGet, "/x", nil), org, key)
			if ok != wantOK || (!ok && rec.Code != http.StatusForbidden) {
				t.Fatalf("requirePlanFeature(org %d, %s) = %v (status %d), want %v", org, key, ok, rec.Code, wantOK)
			}
		}
	}
	if entitlements.FactionLimit(ownerPlan) != 0 || entitlements.FactionLimit(customerPlan) != entitlements.SurvivorFactionLimit {
		t.Fatal("faction limits")
	}

	// Map rotation: available for the owner's installation with the environment switch off and
	// no override; its own per-server "enabled" setting is untouched (nothing was saved).
	if a.Config.MapRotationEnabled {
		t.Fatal("the environment default must be off")
	}
	if reason, blocked, err := a.mapRotationAvailable(ctx, w.a1.OrgID, w.a1.InstallationID); err != nil || reason != "" || blocked {
		t.Fatalf("owner map rotation: %q %v %v", reason, blocked, err)
	}
	if reason, _, err := a.mapRotationAvailable(ctx, w.b1.OrgID, w.b1.InstallationID); err != nil || reason != mapRotationReasonFlag {
		t.Fatalf("customer map rotation: %q %v", reason, err)
	}
	var enabledRows int
	if err := a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM map_rotation_settings WHERE installation_id=$1 AND enabled`, w.a1.InstallationID).Scan(&enabledRows); err != nil {
		t.Fatal(err)
	}
	if enabledRows != 0 {
		t.Fatal("owner access must not enable map rotation on a server by itself")
	}

	// The admin flags view says where the value comes from.
	mine := w.flags(t, w.a1.InstallationID)
	if f := mine[featureflags.MapRotation]; !f.Effective || f.Source != "owner_access" || !f.OwnerAccess || f.Override != nil || !strings.Contains(f.Label, "owner access") {
		t.Fatalf("owner map_rotation flag: %+v", f)
	}
	for _, key := range []string{featureflags.ShopCanary, featureflags.CaseEvidence, featureflags.CaseBuildEvidence} {
		if f := mine[key]; f.Effective || f.OwnerAccess || f.Source != "default" {
			t.Fatalf("%s must not be switched on by owner access: %+v", key, f)
		}
	}
	if a.ShopCanaryGate.Allows(w.a1.InstallationID) {
		t.Fatal("owner access must not open the shop canary")
	}
	// No embed renderer in this world: custom embeds stay off, and the view says so.
	if f := mine[featureflags.CustomEmbeds]; f.Effective || f.OwnerAccess || a.customEmbedsFor(w.a1.InstallationID) {
		t.Fatalf("custom embeds without a renderer: %+v", f)
	}
	for key, f := range w.flags(t, w.b1.InstallationID) {
		if f.Effective || f.OwnerAccess || f.Source != "default" {
			t.Fatalf("customer flag %s changed: %+v", key, f)
		}
	}
	// An explicit OFF override still wins, and clearing it brings owner access back.
	inst := strconv.FormatInt(w.a1.InstallationID, 10)
	pv := map[string]string{"installationID": inst, "flag": featureflags.MapRotation}
	if rr := w.do(http.MethodPut, a.handleOwnerSetInstallationFlag, "/api/admin/installations/"+inst+"/flags/map_rotation", adminFounderID, pv, map[string]any{"reason": "not on my server", "enabled": false}); rr.Code != http.StatusOK {
		t.Fatalf("set override: %d %s", rr.Code, rr.Body.String())
	}
	if f := w.flags(t, w.a1.InstallationID)[featureflags.MapRotation]; f.Effective || f.Source != "override" || !f.OwnerAccess || f.Override == nil || *f.Override {
		t.Fatalf("after OFF override: %+v", f)
	}
	if reason, _, _ := a.mapRotationAvailable(ctx, w.a1.OrgID, w.a1.InstallationID); reason != mapRotationReasonFlag {
		t.Fatalf("an OFF override must switch map rotation off for the owner's own server, reason=%q", reason)
	}
	if rr := w.do(http.MethodPut, a.handleOwnerSetInstallationFlag, "/api/admin/installations/"+inst+"/flags/map_rotation", adminFounderID, pv, map[string]any{"reason": "back to normal"}); rr.Code != http.StatusOK {
		t.Fatalf("clear override: %d %s", rr.Code, rr.Body.String())
	}
	if reason, _, _ := a.mapRotationAvailable(ctx, w.a1.OrgID, w.a1.InstallationID); reason != "" {
		t.Fatalf("clearing the override brings owner access back, reason=%q", reason)
	}

	// Billing state: the owner's organization is not walled by its ended subscription.
	rec := httptest.NewRecorder()
	if limit, ok := a.installationCapacity(ctx, rec, w.a1.OrgID); !ok || limit != billing.OwnerInstallations {
		t.Fatalf("owner installation capacity: %d %v %s", limit, ok, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	if _, ok := a.installationCapacity(ctx, rec, w.b1.OrgID); ok || rec.Code != http.StatusPaymentRequired {
		t.Fatalf("customer installation capacity must still need billing: %v %d", ok, rec.Code)
	}

	// The Owner Hub read model reports the real plan and status, and says owner access applies.
	orgA, err := a.adminSaaS.GetOrganization(ctx, w.a1.OrgID)
	if err != nil || orgA == nil || orgA.Subscription == nil {
		t.Fatalf("organization A: %+v %v", orgA, err)
	}
	orgB, err := a.adminSaaS.GetOrganization(ctx, w.b1.OrgID)
	if err != nil || orgB == nil || orgB.Subscription == nil {
		t.Fatalf("organization B: %+v %v", orgB, err)
	}
	all := len(entitlements.Resolve(ownerPlan))
	if s := orgA.Subscription; !s.PlatformOwnerAccess || s.Plan != entitlements.PlanSurvivor || s.Status != "CANCELED" || len(s.Entitlements) != all {
		t.Fatalf("organization A subscription view: %+v", s)
	}
	if s := orgB.Subscription; s.PlatformOwnerAccess || len(s.Entitlements) >= all {
		t.Fatalf("organization B subscription view: %+v", s)
	}

	// A suspended installation stays suspended: owner access never reinstates anything.
	if rr := w.do(http.MethodPost, a.handleOwnerSuspendInstallation, "/api/admin/installations/"+inst+"/suspend", adminFounderID, map[string]string{"installationID": inst}, map[string]any{"reason": "testing suspension"}); rr.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", rr.Code, rr.Body.String())
	}
	if err := a.OwnerAccess.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = a.organizationPlan(ctx, w.a1.OrgID)
	_, _, _ = a.mapRotationAvailable(ctx, w.a1.OrgID, w.a1.InstallationID)
	var status string
	if err := a.DB.Pool.QueryRow(ctx, `SELECT status FROM installations WHERE id=$1`, w.a1.InstallationID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "SUSPENDED" {
		t.Fatalf("the installation must stay suspended, status=%s", status)
	}

	// Taken off the allowlist: within one refresh the organization is an ordinary customer again.
	a.Config.AdminDiscordIDs = []string{adminFounderID}
	a.OwnerAccess = owneraccess.New(a.PlatformOwner, a.Config.AdminDiscordIDs, time.Hour)
	if err := a.OwnerAccess.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	entitlements.SetOwnerOrganizations(a.OwnerAccess.Organization)
	if plan, _ := a.organizationPlan(ctx, w.a1.OrgID); plan.OwnerAccess() || entitlements.Has(plan, entitlements.MapRotation) {
		t.Fatal("without the allowlist entry the Survivor plan applies again")
	}
}

// A platform owner's brand-new organization is unlocked at once: creating it refreshes the
// cached lookup instead of waiting for the TTL.
func TestPlatformOwnerNewOrganizationIsUnlockedAtOnce(t *testing.T) {
	w := ownerAccessWorld(t)
	a := w.a
	before := a.OwnerAccess.Organization(w.a1.OrgID)
	org := mustCreateOrg(t, a, w.a1.OwnerDiscordID, "Owner Second Org", fmt.Sprintf("owner-second-%d", time.Now().UnixNano()))
	if !before || !a.OwnerAccess.Organization(org.ID) {
		t.Fatalf("the new organization must have owner access without waiting for the cache (before=%v)", before)
	}
	customer := mustCreateOrg(t, a, w.b1.OwnerDiscordID, "Customer Second Org", fmt.Sprintf("customer-second-%d", time.Now().UnixNano()))
	if a.OwnerAccess.Organization(customer.ID) {
		t.Fatal("a customer's new organization must not have owner access")
	}
}
