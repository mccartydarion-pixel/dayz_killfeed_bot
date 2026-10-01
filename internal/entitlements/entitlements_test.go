package entitlements

import "testing"

func withEnforced(t *testing.T, on bool) {
	t.Helper()
	prev := Enforced()
	SetEnforced(on)
	t.Cleanup(func() { SetEnforced(prev) })
}

func TestResolveReturnsKnownKeys(t *testing.T) {
	keys := Resolve("ANY_PLAN")
	if len(keys) == 0 {
		t.Fatal("expected at least one entitlement key")
	}
	if !Has("ANY_PLAN", Killfeed) {
		t.Fatal("expected killfeed to be resolvable")
	}
}

func TestResolveReturnsACopyNotSharedState(t *testing.T) {
	for _, on := range []bool{false, true} {
		withEnforced(t, on)
		for _, plan := range []string{"PREMIUM", PlanSurvivor} {
			keys := Resolve(plan)
			keys[0] = "mutated"
			if Resolve(plan)[0] == "mutated" {
				t.Fatalf("enforced=%v plan=%s: Resolve must return an independent copy", on, plan)
			}
		}
	}
}

// Gating off (the default) must behave exactly as before gating existed: every plan,
// Survivor included, gets every feature.
func TestNotEnforcedGrantsEverythingToEveryPlan(t *testing.T) {
	withEnforced(t, false)
	for _, plan := range []string{PlanSurvivor, "PREMIUM", "TRIAL", "LOW", "", "NONE"} {
		if len(Resolve(plan)) != len(allKeys) {
			t.Fatalf("plan %q: got %d keys, want all %d", plan, len(Resolve(plan)), len(allKeys))
		}
		for _, k := range allKeys {
			if !Has(plan, k) {
				t.Fatalf("plan %q: missing %s with gating off", plan, k)
			}
		}
		if FactionLimit(plan) != 0 {
			t.Fatalf("plan %q: faction limit %d with gating off, want unlimited", plan, FactionLimit(plan))
		}
	}
}

func TestEnforcedRestrictsOnlySurvivor(t *testing.T) {
	withEnforced(t, true)
	premiumOnly := []Key{RankedSeasons, Bounties, Heatmaps, Economy, CustomEmbeds, UnlimitedFactions, MultipleServers, PrioritySupport}
	for _, k := range premiumOnly {
		if Has(PlanSurvivor, k) {
			t.Fatalf("Survivor must not include %s", k)
		}
		if Has("normal", k) {
			t.Fatalf("plan keys are case-insensitive: normal must not include %s", k)
		}
	}
	for _, k := range []Key{Killfeed, Leaderboards, LivePlayers, WebsiteDashboard, DiscordActivity, SpecialKills, AdvancedStats} {
		if !Has(PlanSurvivor, k) {
			t.Fatalf("Survivor must include %s", k)
		}
	}
	if FactionLimit(PlanSurvivor) != SurvivorFactionLimit {
		t.Fatalf("Survivor faction limit = %d", FactionLimit(PlanSurvivor))
	}
	// Champion, the trial, retired plans and unknown/empty plans keep everything.
	for _, plan := range []string{"PREMIUM", "TRIAL", "LOW", "MEDIUM", "HIGH", "", "NONE", "SOMETHING_NEW"} {
		for _, k := range allKeys {
			if !Has(plan, k) {
				t.Fatalf("plan %q lost %s; only Survivor may be restricted", plan, k)
			}
		}
		if FactionLimit(plan) != 0 {
			t.Fatalf("plan %q: faction limit %d, want unlimited", plan, FactionLimit(plan))
		}
	}
}

func TestRouteAllowed(t *testing.T) {
	withEnforced(t, true)
	gated := map[string]Key{"BOUNTY": Bounties, "BOUNTY_TRACKING": Bounties, "HEATMAPS": Heatmaps, "ECONOMY": Economy, "SHOP": Economy, "SERVER_RANKS": RankedSeasons}
	for route, want := range gated {
		if got, ok := RouteFeature(route); !ok || got != want {
			t.Fatalf("RouteFeature(%s) = %s,%v want %s", route, got, ok, want)
		}
		if RouteAllowed(PlanSurvivor, route) {
			t.Fatalf("Survivor must not receive the %s route", route)
		}
		if !RouteAllowed("PREMIUM", route) {
			t.Fatalf("Champion must receive the %s route", route)
		}
	}
	for _, route := range []string{"KILLFEED", "SERVER_STATUS", "LEADERBOARDS", ""} {
		if !RouteAllowed(PlanSurvivor, route) {
			t.Fatalf("ungated route %q must be allowed on every plan", route)
		}
	}
}
