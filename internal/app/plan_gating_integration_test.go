//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Plan tiers over the real HTTP routes: Survivor gets PLAN_FEATURE_REQUIRED on
// Champion-only routes and FACTION_LIMIT_REACHED past 5 factions; Champion and
// gating-off keep everything.

func setOrgPlan(t *testing.T, a *App, organizationID int64, plan string) {
	t.Helper()
	if _, err := a.DB.Pool.Exec(context.Background(), `
INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,$2,'ACTIVE')
ON CONFLICT (organization_id) DO UPDATE SET plan = EXCLUDED.plan`, organizationID, plan); err != nil {
		t.Fatal(err)
	}
}

func (w *factionWorld) setPlan(f installationFixture, plan string) {
	w.t.Helper()
	setOrgPlan(w.t, w.a, f.OrgID, plan)
}

func planErrorCode(t *testing.T, r *apiResult) string {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func enforcePlanGating(t *testing.T) {
	t.Helper()
	prev := entitlements.Enforced()
	entitlements.SetEnforced(true)
	t.Cleanup(func() { entitlements.SetEnforced(prev) })
}

func TestPlanGatingChampionOnlyRoutes(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	w.linkPlayer(w.a1, player, "Gated Player")
	shop := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/shop/products", w.a1.OrgID, w.a1.InstallationID)

	enforcePlanGating(t)
	w.setPlan(w.a1, entitlements.PlanSurvivor)
	for _, path := range []string{w.eco(w.a1, "/me"), shop} {
		r := w.do(http.MethodGet, path, player, nil)
		if r.Status != http.StatusForbidden || planErrorCode(t, r) != codePlanFeatureRequired {
			t.Fatalf("Survivor %s: want 403 %s, got %d %s", path, codePlanFeatureRequired, r.Status, r.Body)
		}
	}
	w.setPlan(w.a1, "PREMIUM")
	w.expect(w.do(http.MethodGet, w.eco(w.a1, "/me"), player, nil), http.StatusOK, "Champion economy")
	w.expect(w.do(http.MethodGet, shop, player, nil), http.StatusOK, "Champion shop")

	// Gating off: Survivor behaves exactly as before plan tiers.
	w.setPlan(w.a1, entitlements.PlanSurvivor)
	entitlements.SetEnforced(false)
	w.expect(w.do(http.MethodGet, w.eco(w.a1, "/me"), player, nil), http.StatusOK, "gating off")
}

func TestPlanGatingFactionCapOverAPI(t *testing.T) {
	w := newFactionWorld(t)
	enforcePlanGating(t)
	w.setPlan(w.a1, entitlements.PlanSurvivor)
	for i := 0; i < entitlements.SurvivorFactionLimit; i++ {
		w.createFaction(w.a1, w.players[i], fmt.Sprintf("Survivor Crew %d", i), fmt.Sprintf("SV%d", i), "OPEN")
	}
	last := w.players[entitlements.SurvivorFactionLimit]
	body := map[string]any{"name": "One Too Many", "tag": "OTM", "description": "over the cap", "recruitmentStatus": "OPEN"}
	r := w.do(http.MethodPost, w.path(w.a1, ""), last, body)
	if r.Status != http.StatusConflict || planErrorCode(t, r) != codeFactionLimitReached {
		t.Fatalf("sixth faction on Survivor: want 409 %s, got %d %s", codeFactionLimitReached, r.Status, r.Body)
	}

	w.setPlan(w.a1, "PREMIUM")
	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), last, body), http.StatusCreated, "Champion has no faction cap")
}

// Starting a ranked season is Champion-only; reading the current one stays open.
func TestPlanGatingRankedSeasonStart(t *testing.T) {
	w := newClientAdminWorld(t)
	w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
	enforcePlanGating(t)
	setOrgPlan(t, w.a, w.f.OrgID, entitlements.PlanSurvivor)
	start := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}}
	path := w.path("/ranked/server-season")
	if rr := w.call(w.a.handleGetServerRankedSeason, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("Survivor may still read the season: %d %s", rr.Code, rr.Body.String())
	}
	rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), codePlanFeatureRequired) {
		t.Fatalf("Survivor start: want 403 %s, got %d %s", codePlanFeatureRequired, rr.Code, rr.Body.String())
	}
	rr = w.call(w.a.handleResetServerRankedSeason, http.MethodPost, w.path("/ranked/server-season/reset"), w.f.OwnerDiscordID, start, nil)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), codePlanFeatureRequired) {
		t.Fatalf("Survivor reset: want 403 %s, got %d %s", codePlanFeatureRequired, rr.Code, rr.Body.String())
	}
	setOrgPlan(t, w.a, w.f.OrgID, "PREMIUM")
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusOK {
		t.Fatalf("Champion start: %d %s", rr.Code, rr.Body.String())
	}
}

// Survivor can't save a custom embed, but can still read, delete and switch a route back to
// the default card after a downgrade.
func TestPlanGatingEmbedSaveButNotCleanup(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.a1.OrgID, w.a1.InstallationID
	// Saved while on Champion.
	setOrgPlan(t, w.a, org, "PREMIUM")
	if rr := w.put(org, inst, "KILLFEED", w.admin, goodTemplate("Saved on Champion")); rr.Code != http.StatusOK {
		t.Fatalf("Champion save: %d %s", rr.Code, rr.Body.String())
	}

	enforcePlanGating(t)
	setOrgPlan(t, w.a, org, entitlements.PlanSurvivor)
	rr := w.put(org, inst, "KILLFEED", w.admin, goodTemplate("Saved on Survivor"))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), codePlanFeatureRequired) {
		t.Fatalf("Survivor save: want 403 %s, got %d %s", codePlanFeatureRequired, rr.Code, rr.Body.String())
	}
	if rr := w.get(org, inst, "KILLFEED", w.admin); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Saved on Champion") {
		t.Fatalf("Survivor can still read the saved design: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.activate(org, inst, "KILLFEED", w.admin, "DEFAULT"); rr.Code != http.StatusOK {
		t.Fatalf("Survivor can switch back to the default card: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.del(org, inst, "KILLFEED", w.admin); rr.Code != http.StatusOK {
		t.Fatalf("Survivor can delete a saved design: %d %s", rr.Code, rr.Body.String())
	}
}
