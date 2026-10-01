//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Plan tiers for the opt-in features: hot zones, the retention dashboard, fight replay and the
// feed identity are Champion-only. Lives, the Champion Card and the network listing are part of
// every plan.

func planGated(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code != http.StatusForbidden || !strings.Contains(body, codePlanFeatureRequired) {
		t.Fatalf("Survivor %s: want 403 %s, got %d %s", what, codePlanFeatureRequired, code, body)
	}
}

// Survivor cannot switch a Champion-only feature on, but can always read its settings and switch
// one off; the network listing is not gated at all.
func TestPlanGatingFeatureSettings(t *testing.T) {
	w := newStandoutWorld(t)
	owner := w.f.OwnerDiscordID
	hotZones := hotZoneSettingsDTO{Enabled: true, WindowMinutes: 60, MinKills: 6, RadiusMeters: 500, DurationMinutes: 30, CooldownMinutes: 120, FirstPoints: 500, SecondPoints: 250, ThirdPoints: 100}
	identity := feedIdentitySettingsDTO{Enabled: true, Name: "Deadzone Feed"}
	replay := fightReplaySettingsDTO{Public: true, DelayMinutes: 30}

	// Saved while on Champion.
	setOrgPlan(t, w.a, w.f.OrgID, "PREMIUM")
	enforcePlanGating(t)
	for what, save := range map[string]func() (int, map[string]any){
		"hot zones": func() (int, map[string]any) {
			return w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", owner, hotZones)
		},
		"feed identity": func() (int, map[string]any) {
			return w.put(w.a.handlePutFeedIdentitySettings, "/features/feed-identity", owner, identity)
		},
		"fight replay": func() (int, map[string]any) {
			return w.put(w.a.handlePutFightReplaySettings, "/features/fight-replay", owner, replay)
		},
	} {
		if code, resp := save(); code != http.StatusOK {
			t.Fatalf("Champion enables %s: %d %v", what, code, resp)
		}
	}

	setOrgPlan(t, w.a, w.f.OrgID, entitlements.PlanSurvivor)
	rr := w.call(w.a.handlePutHotZoneSettings, http.MethodPut, w.path("/features/hot-zones"), owner, hotZones, nil)
	planGated(t, "enables hot zones", rr.Code, rr.Body.String())
	rr = w.call(w.a.handlePutFeedIdentitySettings, http.MethodPut, w.path("/features/feed-identity"), owner, identity, nil)
	planGated(t, "enables a feed identity", rr.Code, rr.Body.String())
	rr = w.call(w.a.handlePutFightReplaySettings, http.MethodPut, w.path("/features/fight-replay"), owner, replay, nil)
	planGated(t, "makes replays public", rr.Code, rr.Body.String())

	// Reading stays open and shows what was saved on Champion: nothing is lost by a downgrade.
	rr = w.call(w.a.handleGetFeatureSettings, http.MethodGet, w.path("/features"), owner, nil, nil)
	saved := decodeBody[featureSettingsDTO](t, rr)
	if rr.Code != http.StatusOK || !saved.HotZones.Enabled || !saved.FeedIdentity.Enabled || !saved.FightReplay.Public {
		t.Fatalf("Survivor reads its settings: %d %+v", rr.Code, saved)
	}
	// Switching off is always allowed.
	hotZones.Enabled, identity.Enabled, replay.Public = false, false, false
	if code, resp := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", owner, hotZones); code != http.StatusOK {
		t.Fatalf("Survivor switches hot zones off: %d %v", code, resp)
	}
	if code, resp := w.put(w.a.handlePutFeedIdentitySettings, "/features/feed-identity", owner, identity); code != http.StatusOK {
		t.Fatalf("Survivor switches the feed identity off: %d %v", code, resp)
	}
	if code, resp := w.put(w.a.handlePutFightReplaySettings, "/features/fight-replay", owner, replay); code != http.StatusOK {
		t.Fatalf("Survivor makes replays private: %d %v", code, resp)
	}
	// The network listing is part of every plan.
	if code, resp := w.put(w.a.handlePutNetworkSettings, "/features/network", owner, networkSettingsDTO{Listed: true, Description: "Survivor server"}); code != http.StatusOK {
		t.Fatalf("Survivor lists its server: %d %v", code, resp)
	}
	t.Cleanup(func() {
		_, _ = w.a.FeatureSettings.SaveNetwork(context.Background(), w.f.InstallationID, 0, repository.NetworkSettings{})
	})

	// Gating off: Survivor behaves exactly as before plan tiers.
	entitlements.SetEnforced(false)
	hotZones.Enabled = true
	if code, resp := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", owner, hotZones); code != http.StatusOK {
		t.Fatalf("gating off: %d %v", code, resp)
	}
}

// The retention dashboard and staff fight replays answer PLAN_FEATURE_REQUIRED on Survivor.
func TestPlanGatingRetentionAndStaffFights(t *testing.T) {
	w := newStandoutWorld(t)
	owner := w.f.OwnerDiscordID
	now := time.Now().UTC()
	ace, bob := w.player("Ace"), w.player("Bob")
	kill := w.fightKill(ace, bob, now.Add(-2*time.Hour), 100, 100, 120, 120)
	pv := map[string]string{"killID": strconv.FormatInt(kill, 10)}
	routes := []struct {
		what    string
		handler http.HandlerFunc
		path    string
		pv      map[string]string
	}{
		{"retention", w.a.handleRetention, w.path("/retention"), nil},
		{"lapsed players", w.a.handleRetentionLapsed, w.path("/retention/lapsed"), nil},
		{"fights", w.a.handleAdminFights, w.path("/fights"), nil},
		{"fight replay", w.a.handleAdminFightReplay, w.path("/fights/x"), pv},
	}

	enforcePlanGating(t)
	setOrgPlan(t, w.a, w.f.OrgID, entitlements.PlanSurvivor)
	for _, route := range routes {
		rr := w.call(route.handler, http.MethodGet, route.path, owner, nil, route.pv)
		planGated(t, "reads "+route.what, rr.Code, rr.Body.String())
	}
	// The plan is checked after the capability: a Moderator learns nothing about the plan.
	mod := syncUser(t, w.a, fmt.Sprintf("plan-mod-%d", standoutSeq.Add(1)), "Mod")
	w.mapRole(mod.DiscordUserID, "plan-role-mod", "MODERATOR")
	if rr := w.call(w.a.handleRetention, http.MethodGet, w.path("/retention"), mod.DiscordUserID, nil, nil); rr.Code != http.StatusForbidden || strings.Contains(rr.Body.String(), codePlanFeatureRequired) {
		t.Fatalf("a Moderator was told about the plan: %d %s", rr.Code, rr.Body.String())
	}

	setOrgPlan(t, w.a, w.f.OrgID, "PREMIUM")
	for _, route := range routes {
		if rr := w.call(route.handler, http.MethodGet, route.path, owner, nil, route.pv); rr.Code != http.StatusOK {
			t.Fatalf("Champion reads %s: %d %s", route.what, rr.Code, rr.Body.String())
		}
	}
}

// What a Survivor installation saved while on Champion stops working at runtime and comes back on
// upgrade: no new hot zone opens, feeds post as the bot, and players see replays as switched off.
func TestPlanGatingRuntimeFollowsThePlan(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()
	hunter, victim := w.player("Hunter"), w.player("Victim")
	for i := 0; i < 6; i++ {
		w.killAt(hunter, victim, now.Add(-time.Duration(10+i)*time.Minute), 7600+float64(i*20), 12700)
	}
	old := w.fightKill(hunter, victim, now.Add(-3*time.Hour), 100, 100, 120, 120)
	w.fightKill(victim, hunter, now.Add(-3*time.Hour+20*time.Second), 120, 120, 100, 100)
	if _, err := w.a.FeatureSettings.SaveHotZones(ctx, w.f.InstallationID, 0, repository.HotZoneSettings{Enabled: true, WindowMinutes: 60, MinKills: 6, RadiusM: 500,
		DurationMinutes: 30, CooldownMinutes: 120, FirstPoints: 500, SecondPoints: 250, ThirdPoints: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.FeatureSettings.SaveFeedIdentity(ctx, w.f.InstallationID, 0, repository.FeedIdentitySettings{Enabled: true, Name: "Deadzone Feed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.FeatureSettings.SaveFightReplay(ctx, w.f.InstallationID, 0, repository.FightReplaySettings{Public: true, DelayMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	fan := syncUser(t, w.a, fmt.Sprintf("plan-fan-%d", standoutSeq.Add(1)), "Fan")
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, w.guildID, hunter, fan.DiscordUserID); err != nil {
		t.Fatal(err)
	}
	pv := func(kill int64) map[string]string {
		return map[string]string{"installationID": strconv.FormatInt(w.f.InstallationID, 10), "killID": strconv.FormatInt(kill, 10)}
	}
	type list struct {
		Enabled bool              `json:"enabled"`
		Items   []fightSummaryDTO `json:"items"`
	}

	enforcePlanGating(t)
	setOrgPlan(t, w.a, w.f.OrgID, entitlements.PlanSurvivor)
	w.tick(now)
	if got := w.hotZoneEvents(); len(got) != 0 {
		t.Fatalf("a hot zone opened on Survivor: %+v", got)
	}
	if identity, ok, err := w.a.FeatureSettings.FeedIdentityForServer(ctx, w.serverID); err != nil || ok {
		t.Fatalf("Survivor feed identity = %+v %v %v", identity, ok, err)
	}
	// To the player the feature is off: an ordinary empty answer, never a plan error.
	rr := w.call(w.a.handlePlayerFights, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(0))
	if closed := decodeBody[list](t, rr); rr.Code != http.StatusOK || closed.Enabled || len(closed.Items) != 0 {
		t.Fatalf("Survivor player fights: %d %+v", rr.Code, closed)
	}
	if rr := w.call(w.a.handlePlayerFightReplay, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(old)); rr.Code != http.StatusNotFound {
		t.Fatalf("Survivor player replay: %d", rr.Code)
	}

	// Upgrading brings back exactly what was saved.
	setOrgPlan(t, w.a, w.f.OrgID, "PREMIUM")
	w.tick(now)
	if got := w.hotZoneEvents(); len(got) != 1 {
		t.Fatalf("Champion: expected one hot zone, got %+v", got)
	}
	if identity, ok, err := w.a.FeatureSettings.FeedIdentityForServer(ctx, w.serverID); err != nil || !ok || identity.Name != "Deadzone Feed" {
		t.Fatalf("Champion feed identity = %+v %v %v", identity, ok, err)
	}
	rr = w.call(w.a.handlePlayerFights, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(0))
	if open := decodeBody[list](t, rr); rr.Code != http.StatusOK || !open.Enabled || len(open.Items) != 1 {
		t.Fatalf("Champion player fights: %d %+v", rr.Code, open)
	}
}
