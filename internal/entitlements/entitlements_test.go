package entitlements

import "testing"

func withEnforced(t *testing.T, on bool) {
	t.Helper()
	prev := Enforced()
	SetEnforced(on)
	t.Cleanup(func() { SetEnforced(prev) })
}

func TestResolveReturnsKnownKeys(t *testing.T) {
	keys := resolve("ANY_PLAN")
	if len(keys) == 0 {
		t.Fatal("expected at least one entitlement key")
	}
	if !has("ANY_PLAN", Killfeed) {
		t.Fatal("expected killfeed to be resolvable")
	}
}

func TestResolveReturnsACopyNotSharedState(t *testing.T) {
	for _, on := range []bool{false, true} {
		withEnforced(t, on)
		for _, plan := range []string{"PREMIUM", PlanSurvivor} {
			keys := resolve(plan)
			keys[0] = "mutated"
			if resolve(plan)[0] == "mutated" {
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
		if len(resolve(plan)) != len(allKeys) {
			t.Fatalf("plan %q: got %d keys, want all %d", plan, len(resolve(plan)), len(allKeys))
		}
		for _, k := range allKeys {
			if !has(plan, k) {
				t.Fatalf("plan %q: missing %s with gating off", plan, k)
			}
		}
		if factionLimit(plan) != 0 {
			t.Fatalf("plan %q: faction limit %d with gating off, want unlimited", plan, factionLimit(plan))
		}
	}
}

func TestEnforcedRestrictsOnlySurvivor(t *testing.T) {
	withEnforced(t, true)
	premiumOnly := []Key{RankedSeasons, Bounties, Heatmaps, Economy, CustomEmbeds, UnlimitedFactions, MultipleServers, PrioritySupport,
		HotZones, Retention, FightReplay, FeedIdentity}
	for _, k := range premiumOnly {
		if has(PlanSurvivor, k) {
			t.Fatalf("Survivor must not include %s", k)
		}
		if has("normal", k) {
			t.Fatalf("plan keys are case-insensitive: normal must not include %s", k)
		}
	}
	for _, k := range []Key{Killfeed, Leaderboards, LivePlayers, WebsiteDashboard, DiscordActivity, SpecialKills, AdvancedStats} {
		if !has(PlanSurvivor, k) {
			t.Fatalf("Survivor must include %s", k)
		}
	}
	if factionLimit(PlanSurvivor) != SurvivorFactionLimit {
		t.Fatalf("Survivor faction limit = %d", factionLimit(PlanSurvivor))
	}
	// Champion, the trial, retired plans and unknown/empty plans keep everything.
	for _, plan := range []string{"PREMIUM", "TRIAL", "LOW", "MEDIUM", "HIGH", "", "NONE", "SOMETHING_NEW"} {
		for _, k := range allKeys {
			if !has(plan, k) {
				t.Fatalf("plan %q lost %s; only Survivor may be restricted", plan, k)
			}
		}
		if factionLimit(plan) != 0 {
			t.Fatalf("plan %q: faction limit %d, want unlimited", plan, factionLimit(plan))
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
		if routeAllowed(PlanSurvivor, route) {
			t.Fatalf("Survivor must not receive the %s route", route)
		}
		if !routeAllowed("PREMIUM", route) {
			t.Fatalf("Champion must receive the %s route", route)
		}
	}
	for _, route := range []string{"KILLFEED", "SERVER_STATUS", "LEADERBOARDS", ""} {
		if !routeAllowed(PlanSurvivor, route) {
			t.Fatalf("ungated route %q must be allowed on every plan", route)
		}
	}
}

func withOwnerOrganizations(t *testing.T, ids ...int64) {
	t.Helper()
	owned := map[int64]bool{}
	for _, id := range ids {
		owned[id] = true
	}
	SetOwnerOrganizations(func(id int64) bool { return owned[id] })
	t.Cleanup(func() { SetOwnerOrganizations(nil) })
}

// A platform owner's own organization has every feature, on any plan, with gating enforced.
func TestPlatformOwnerOrganizationHasEverything(t *testing.T) {
	withEnforced(t, true)
	withOwnerOrganizations(t, 7)
	for _, key := range []string{PlanSurvivor, "", "NONE", "TRIAL", "PREMIUM", "whatever"} {
		p := ForOrganization(7, key)
		if !p.OwnerAccess() || p.Key() != key {
			t.Fatalf("plan %q: owner=%v key=%q", key, p.OwnerAccess(), p.Key())
		}
		if got := Resolve(p); len(got) != len(allKeys) {
			t.Fatalf("plan %q: %d keys, want all %d", key, len(got), len(allKeys))
		}
		for _, k := range allKeys {
			if !Has(p, k) {
				t.Fatalf("plan %q: owner organization is missing %s", key, k)
			}
		}
		if FactionLimit(p) != 0 {
			t.Fatalf("plan %q: owner organization has a faction limit", key)
		}
		for _, route := range []string{"BOUNTY", "BOUNTY_TRACKING", "HEATMAPS", "ECONOMY", "SHOP", "SERVER_RANKS", "DONATION_PERKS", "KILLFEED"} {
			if !RouteAllowed(p, route) {
				t.Fatalf("plan %q: owner organization lost route %s", key, route)
			}
		}
	}
	if Has(ForOrganization(7, PlanSurvivor), "not_a_feature") {
		t.Fatal("owner access must not invent features")
	}
	keys := Resolve(ForOrganization(7, PlanSurvivor))
	keys[0] = "mutated"
	if Resolve(ForOrganization(7, PlanSurvivor))[0] == "mutated" {
		t.Fatal("Resolve must return an independent copy for an owner organization too")
	}
}

// Nothing changes for anybody else: every other organization gets exactly what its plan key gets.
func TestCustomerOrganizationsAreUnchangedByOwnerAccess(t *testing.T) {
	withOwnerOrganizations(t, 7)
	routes := []string{"BOUNTY", "BOUNTY_TRACKING", "HEATMAPS", "ECONOMY", "SHOP", "SERVER_RANKS", "DONATION_PERKS", "KILLFEED", "UNKNOWN"}
	for _, on := range []bool{false, true} {
		withEnforced(t, on)
		for _, org := range []int64{0, -1, 8, 70} {
			for _, key := range []string{PlanSurvivor, "normal", "", "NONE", "TRIAL", "PREMIUM"} {
				p := ForOrganization(org, key)
				if p.OwnerAccess() {
					t.Fatalf("organization %d must not have owner access", org)
				}
				if len(Resolve(p)) != len(resolve(key)) || FactionLimit(p) != factionLimit(key) {
					t.Fatalf("enforced=%v org=%d plan=%q: resolve/faction limit differ from the plan's own", on, org, key)
				}
				for _, k := range allKeys {
					if Has(p, k) != has(key, k) {
						t.Fatalf("enforced=%v org=%d plan=%q key=%s differs from the plan's own answer", on, org, key, k)
					}
				}
				for _, route := range routes {
					if RouteAllowed(p, route) != routeAllowed(key, route) {
						t.Fatalf("enforced=%v org=%d plan=%q route=%s differs from the plan's own answer", on, org, key, route)
					}
				}
			}
		}
	}
	withEnforced(t, true)
	if Has(ForOrganization(8, PlanSurvivor), MapRotation) {
		t.Fatal("a Survivor customer must still be restricted")
	}
}

func TestNoOwnerLookupMeansNoOwnerAccess(t *testing.T) {
	SetOwnerOrganizations(nil)
	if OwnerOrganization(7) || ForOrganization(7, PlanSurvivor).OwnerAccess() {
		t.Fatal("without a lookup nobody has owner access")
	}
	withOwnerOrganizations(t, 7)
	if OwnerOrganization(0) || OwnerOrganization(-7) {
		t.Fatal("a non-positive organization id is never an owner organization")
	}
}
