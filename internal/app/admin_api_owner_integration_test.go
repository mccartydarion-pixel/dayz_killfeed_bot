//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/admin"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/canaryops"
)

// ownerWorld adds the Owner Hub write model to the admin world.
func newOwnerWorld(t *testing.T) *adminWorld {
	t.Helper()
	w := newAdminWorld(t)
	w.a.PlatformOwner = repository.NewPlatformOwnerRepository(w.a.DB.Pool)
	return w
}

func (w *adminWorld) post(h adminHandler, target, acting string, pv map[string]string, body any) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(http.MethodPost, target, reader)
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

func (w *adminWorld) auditActions(t *testing.T, query string) []string {
	t.Helper()
	rr := w.get(w.a.handleOwnerAudit, "/api/admin/audit"+query, adminFounderID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("audit list: %d %s", rr.Code, rr.Body.String())
	}
	var page struct {
		Items []struct {
			Action string `json:"action"`
			Reason string `json:"reason"`
		} `json:"items"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &page)
	out := make([]string, 0, len(page.Items))
	for _, it := range page.Items {
		out = append(out, it.Action)
	}
	return out
}

// Owner writes are gated exactly like reads: a tenant owner is 403, a missing reason is 400,
// and nothing is audited for a refused request.
func TestOwnerWritesAreGatedAndNeedAReason(t *testing.T) {
	w := newOwnerWorld(t)
	org := strconv.FormatInt(w.a1.OrgID, 10)
	pv := map[string]string{"organizationID": org}
	if rr := w.post(w.a.handleOwnerSetTrial, "/api/admin/organizations/"+org+"/trial", w.a1.OwnerDiscordID, pv, map[string]any{"reason": "x", "days": 7}); rr.Code != http.StatusForbidden {
		t.Fatalf("tenant owner must be 403, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.post(w.a.handleOwnerSetTrial, "/api/admin/organizations/"+org+"/trial", adminFounderID, pv, map[string]any{"days": 7}); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing reason must be 400, got %d %s", rr.Code, rr.Body.String())
	}
	if got := w.auditActions(t, "?organizationId="+org); len(got) != 0 {
		t.Fatalf("refused requests must not be audited, got %v", got)
	}
}

// Trial -> grant -> revoke on an organization without Stripe, each audited, visible through the
// read model, and refused once Stripe owns the row.
func TestOwnerTrialGrantRevokeLifecycle(t *testing.T) {
	w := newOwnerWorld(t)
	org := strconv.FormatInt(w.a1.OrgID, 10)
	pv := map[string]string{"organizationID": org}

	rr := w.post(w.a.handleOwnerSetTrial, "/api/admin/organizations/"+org+"/trial", adminFounderID, pv, map[string]any{"reason": "goodwill", "days": 30})
	if rr.Code != http.StatusOK {
		t.Fatalf("set trial: %d %s", rr.Code, rr.Body.String())
	}
	sub := decodeBody[map[string]any](t, rr)["subscription"].(map[string]any)
	if sub["status"] != "TRIAL" || sub["billingRequired"] != false {
		t.Fatalf("trial subscription: %v", sub)
	}
	ends, _ := time.Parse(time.RFC3339, sub["trialEndsAt"].(string))
	if d := time.Until(ends); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("trial should end in ~30 days, ends %v", ends)
	}

	rr = w.post(w.a.handleOwnerGrantPlan, "/api/admin/organizations/"+org+"/grant", adminFounderID, pv, map[string]any{"reason": "partner server", "plan": "PRO", "days": 90})
	if rr.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rr.Code, rr.Body.String())
	}
	sub = decodeBody[map[string]any](t, rr)["subscription"].(map[string]any)
	if sub["status"] != "ACTIVE" || sub["plan"] != "PRO" || sub["externallyBilled"] != false || sub["ownerGrantReason"] != "partner server" || sub["billingRequired"] != false {
		t.Fatalf("granted subscription: %v", sub)
	}

	// The read model shows the grant on the organization detail.
	rr = w.get(w.a.handleAdminGetOrganization, "/api/admin/organizations/"+org, adminFounderID, pv)
	detail := decodeBody[map[string]any](t, rr)
	readSub := detail["subscription"].(map[string]any)
	if readSub["plan"] != "PRO" || readSub["externallyBilled"] != false || readSub["ownerGrantUntil"] == nil {
		t.Fatalf("organization detail subscription: %v", readSub)
	}

	// A dated grant lapses on its own: backdate it and the state machine says billing required.
	if _, err := w.a.DB.Pool.Exec(t.Context(), `UPDATE subscriptions SET owner_grant_until=NOW()-INTERVAL '1 day' WHERE organization_id=$1`, w.a1.OrgID); err != nil {
		t.Fatal(err)
	}
	if lapsed, _ := w.a.SaaSSubscriptions.GetForOrganization(t.Context(), w.a1.OrgID); !ownerSubscription(lapsed).BillingRequired {
		t.Fatalf("lapsed grant must require billing")
	}

	rr = w.post(w.a.handleOwnerRevokeAccess, "/api/admin/organizations/"+org+"/revoke", adminFounderID, pv, map[string]any{"reason": "partnership ended"})
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rr.Code, rr.Body.String())
	}
	sub = decodeBody[map[string]any](t, rr)["subscription"].(map[string]any)
	if sub["status"] != "INACTIVE" || sub["plan"] != "NONE" || sub["billingRequired"] != true {
		t.Fatalf("revoked subscription: %v", sub)
	}

	// Once Stripe owns the row, the owner-only mutations refuse it.
	if _, err := w.a.SaaSSubscriptions.ApplyProviderState(t.Context(), w.a1.OrgID, repository.ProviderState{Provider: "stripe", ProviderCustomerID: "cus_x", ProviderSubscriptionID: "sub_x", Plan: "PRO", Status: repository.SubscriptionActive}); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]adminHandler{"trial": w.a.handleOwnerSetTrial, "grant": w.a.handleOwnerGrantPlan, "revoke": w.a.handleOwnerRevokeAccess} {
		rr = w.post(h, "/api/admin/organizations/"+org+"/"+name, adminFounderID, pv, map[string]any{"reason": "r", "days": 7, "plan": "PRO"})
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s on a stripe-managed org must be 409, got %d %s", name, rr.Code, rr.Body.String())
		}
	}

	got := w.auditActions(t, "?organizationId="+org)
	want := []string{"organization.access_revoked", "organization.plan_granted", "organization.trial_set"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	// Audit rows are scoped by filter: the other tenant has none.
	if other := w.auditActions(t, "?organizationId="+strconv.FormatInt(w.b1.OrgID, 10)); len(other) != 0 {
		t.Fatalf("other tenant must have no audit rows, got %v", other)
	}
}

// Suspend parks the installation and remembers where it came from; reinstate restores it.
func TestOwnerSuspendAndReinstateInstallation(t *testing.T) {
	w := newOwnerWorld(t)
	inst := strconv.FormatInt(w.a1.InstallationID, 10)
	pv := map[string]string{"installationID": inst}
	before, _ := w.a.PlatformOwner.GetInstallation(t.Context(), w.a1.InstallationID)

	rr := w.post(w.a.handleOwnerSuspendInstallation, "/api/admin/installations/"+inst+"/suspend", adminFounderID, pv, map[string]any{"reason": "chargeback"})
	if rr.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[map[string]any](t, rr)["installation"].(map[string]any)
	if got["status"] != "SUSPENDED" || got["suspendedReason"] != "chargeback" || got["statusBeforeSuspend"] != before.Status {
		t.Fatalf("suspended installation: %v (before %q)", got, before.Status)
	}
	// Twice is a no-op conflict, not a second audit row.
	if rr = w.post(w.a.handleOwnerSuspendInstallation, "/api/admin/installations/"+inst+"/suspend", adminFounderID, pv, map[string]any{"reason": "again"}); rr.Code != http.StatusConflict {
		t.Fatalf("second suspend must be 409, got %d", rr.Code)
	}
	// Restarting a suspended installation's worker is refused.
	if rr = w.post(w.a.handleOwnerRestartWorker, "/api/admin/installations/"+inst+"/restart-worker", adminFounderID, pv, map[string]any{"reason": "r"}); rr.Code != http.StatusConflict {
		t.Fatalf("restart on suspended must be 409, got %d %s", rr.Code, rr.Body.String())
	}

	// The read model shows it, and the customer-facing summary reads OFFLINE.
	rr = w.get(w.a.handleAdminGetInstallation, "/api/admin/installations/"+inst, adminFounderID, pv)
	detail := decodeBody[map[string]any](t, rr)
	if detail["status"] != "SUSPENDED" || detail["suspension"] == nil {
		t.Fatalf("installation detail after suspend: status=%v suspension=%v", detail["status"], detail["suspension"])
	}

	rr = w.post(w.a.handleOwnerReinstateInstallation, "/api/admin/installations/"+inst+"/reinstate", adminFounderID, pv, map[string]any{"reason": "resolved"})
	if rr.Code != http.StatusOK {
		t.Fatalf("reinstate: %d %s", rr.Code, rr.Body.String())
	}
	got = decodeBody[map[string]any](t, rr)["installation"].(map[string]any)
	if got["status"] != before.Status || got["suspendedAt"] != nil {
		t.Fatalf("reinstated installation: %v (want status %q)", got, before.Status)
	}
	if actions := w.auditActions(t, "?targetType=installation&targetId="+inst); strings.Join(actions, ",") != "installation.reinstated,installation.suspended" {
		t.Fatalf("audit = %v", actions)
	}
}

// A banned user is refused by every SaaS route that resolves an acting user; unban restores
// them; platform admins cannot be banned.
func TestOwnerBanRefusesTheUserEverywhere(t *testing.T) {
	w := newOwnerWorld(t)
	victim, err := w.a.SaaSUsers.GetByDiscordID(t.Context(), w.a1.OwnerDiscordID)
	if err != nil || victim == nil {
		t.Fatalf("fixture owner: %v %v", victim, err)
	}
	uid := strconv.FormatInt(victim.ID, 10)
	pv := map[string]string{"userID": uid}

	saasList := func(acting string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/saas/organizations", nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set(actingUserHeader, acting)
		rr := httptest.NewRecorder()
		w.a.handleListOrganizations(rr, req)
		return rr.Code
	}
	if code := saasList(w.a1.OwnerDiscordID); code != http.StatusOK {
		t.Fatalf("before ban: %d", code)
	}

	rr := w.post(w.a.handleOwnerBanUser, "/api/admin/users/"+uid+"/ban", adminFounderID, pv, map[string]any{"reason": "abuse"})
	if rr.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rr.Code, rr.Body.String())
	}
	if u := decodeBody[map[string]any](t, rr)["user"].(map[string]any); u["bannedAt"] == nil || u["banReason"] != "abuse" {
		t.Fatalf("banned user: %v", u)
	}
	if code := saasList(w.a1.OwnerDiscordID); code != http.StatusForbidden {
		t.Fatalf("banned user must be 403 on saas routes, got %d", code)
	}
	// The organization detail marks the member.
	org := strconv.FormatInt(w.a1.OrgID, 10)
	rr = w.get(w.a.handleAdminGetOrganization, "/api/admin/organizations/"+org, adminFounderID, map[string]string{"organizationID": org})
	members := decodeBody[map[string]any](t, rr)["members"].([]any)
	if m := members[0].(map[string]any); m["bannedAt"] == nil {
		t.Fatalf("member must show bannedAt: %v", m)
	}

	rr = w.post(w.a.handleOwnerUnbanUser, "/api/admin/users/"+uid+"/unban", adminFounderID, pv, map[string]any{"reason": "appeal accepted"})
	if rr.Code != http.StatusOK {
		t.Fatalf("unban: %d %s", rr.Code, rr.Body.String())
	}
	if code := saasList(w.a1.OwnerDiscordID); code != http.StatusOK {
		t.Fatalf("after unban: %d", code)
	}

	// An allowlisted admin cannot be banned.
	admin := syncUser(t, w.a, adminFounderID, "Founder")
	aid := strconv.FormatInt(admin.ID, 10)
	rr = w.post(w.a.handleOwnerBanUser, "/api/admin/users/"+aid+"/ban", adminFounderID, map[string]string{"userID": aid}, map[string]any{"reason": "oops"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("banning an admin must be 409, got %d %s", rr.Code, rr.Body.String())
	}
}

// The operations console: status is gated like every read, and the maintenance actions are
// audited owner writes that report the service's outcome.
func TestOwnerOpsStatusAndActions(t *testing.T) {
	w := newOwnerWorld(t)
	w.a.AdminService = admin.NewService(nil, nil)
	refreshed := 0
	w.a.AdminService.SetLeaderboardRefresh(func(context.Context) error { refreshed++; return nil })
	w.a.AdminService.SetADMSourceScan(func(context.Context) (map[string]any, error) { return nil, errors.New("nitrado unreachable") })

	if rr := w.get(w.a.handleAdminOpsStatus, "/api/admin/ops/status", w.a1.OwnerDiscordID, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("tenant owner must be 403 on ops status, got %d", rr.Code)
	}
	rr := w.get(w.a.handleAdminOpsStatus, "/api/admin/ops/status", adminFounderID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("ops status: %d %s", rr.Code, rr.Body.String())
	}
	status := decodeBody[map[string]any](t, rr)
	if status["generatedAt"] == nil || status["workerManager"] == nil || status["uptime"] == nil {
		t.Fatalf("ops status shape: %v", status)
	}

	rr = w.post(w.a.handleAdminOpsLeaderboardRefresh, "/api/admin/ops/leaderboard-refresh", adminFounderID, nil, map[string]any{"reason": "panel looked stale"})
	if rr.Code != http.StatusOK || refreshed != 1 {
		t.Fatalf("leaderboard refresh: %d %s (refreshed=%d)", rr.Code, rr.Body.String(), refreshed)
	}
	rr = w.post(w.a.handleAdminOpsADMSourceScan, "/api/admin/ops/adm-source-scan", adminFounderID, nil, map[string]any{"reason": "kills missing"})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("failed scan must be 500, got %d %s", rr.Code, rr.Body.String())
	}
	got := w.auditActions(t, "?targetType=platform")
	if len(got) < 2 || got[0] != "ops.adm_source_scanned" || got[1] != "ops.leaderboard_refreshed" {
		t.Fatalf("ops audit = %v", got)
	}
}

// Feature flags: an override changes what the consumers see, clearing it restores the
// environment default, and every change is audited.
func TestOwnerFeatureFlagsOverrideConsumers(t *testing.T) {
	w := newOwnerWorld(t)
	w.a.FeatureFlags = featureflags.New(w.a.PlatformOwner, time.Hour)
	caseFlags = w.a.FeatureFlags
	t.Cleanup(func() { caseFlags = nil })
	w.a.ShopCanaryGate = canaryops.NewGate(false, nil).WithOverride(func(id int64) (bool, bool) {
		v, ok := w.a.FeatureFlags.Overrides(id)[featureflags.ShopCanary]
		return v, ok
	})
	inst := strconv.FormatInt(w.a1.InstallationID, 10)
	pv := map[string]string{"installationID": inst}

	rr := w.get(w.a.handleOwnerInstallationFlags, "/api/admin/installations/"+inst+"/flags", adminFounderID, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("flags: %d %s", rr.Code, rr.Body.String())
	}
	var page struct {
		Flags []struct {
			Key       string `json:"key"`
			Effective bool   `json:"effective"`
			Override  *bool  `json:"override"`
		} `json:"flags"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &page)
	if len(page.Flags) != len(featureflags.Catalog) {
		t.Fatalf("catalog length: %d", len(page.Flags))
	}
	for _, f := range page.Flags {
		if f.Override != nil || f.Effective {
			t.Fatalf("fresh installation must have no overrides and env-off defaults: %+v", f)
		}
	}
	if w.a.ShopCanaryGate.Allows(w.a1.InstallationID) {
		t.Fatal("canary must be locked before the override")
	}

	put := func(flag string, body map[string]any) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/admin/installations/"+inst+"/flags/"+flag, strings.NewReader(mustJSON(t, body)))
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set(actingUserHeader, adminFounderID)
		req.SetPathValue("installationID", inst)
		req.SetPathValue("flag", flag)
		rec := httptest.NewRecorder()
		w.a.adminRoute(w.a.handleOwnerSetInstallationFlag)(rec, req)
		return rec
	}
	if rr := put("nope", map[string]any{"reason": "r", "enabled": true}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown flag must be 404, got %d", rr.Code)
	}
	if rr := put(featureflags.ShopCanary, map[string]any{"reason": "pilot customer", "enabled": true}); rr.Code != http.StatusOK {
		t.Fatalf("set flag: %d %s", rr.Code, rr.Body.String())
	}
	if !w.a.ShopCanaryGate.Allows(w.a1.InstallationID) {
		t.Fatalf("canary gate must honour the override; overrides=%v resp=%s", w.a.FeatureFlags.Overrides(w.a1.InstallationID), rr.Body.String())
	}
	if w.a.ShopCanaryGate.Allows(w.b1.InstallationID) {
		t.Fatal("the other tenant stays locked")
	}
	if !w.a.customEmbedsFor(w.a1.InstallationID) == false {
		// no renderer in this world: custom embeds stay off whatever the flag says
		t.Fatal("custom embeds need a renderer")
	}
	if rr := put(featureflags.ShopCanary, map[string]any{"reason": "pilot over"}); rr.Code != http.StatusOK {
		t.Fatalf("clear flag: %d %s", rr.Code, rr.Body.String())
	}
	if w.a.ShopCanaryGate.Allows(w.a1.InstallationID) {
		t.Fatal("clearing restores the environment default (locked)")
	}
	if got := w.auditActions(t, "?targetType=installation&targetId="+inst); strings.Join(got, ",") != "installation.flag_cleared,installation.flag_set" {
		t.Fatalf("audit = %v", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
