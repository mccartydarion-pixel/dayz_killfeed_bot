package entitlements

import "testing"

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
	keys := Resolve("PLAN_A")
	keys[0] = "mutated"
	if Resolve("PLAN_B")[0] == "mutated" {
		t.Fatal("expected Resolve to return an independent copy, not shared backing state")
	}
}
